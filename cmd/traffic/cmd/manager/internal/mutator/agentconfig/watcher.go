package agentconfig

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/v2/pkg/install"
	"github.com/telepresenceio/telepresence/v2/pkg/install/agent"
	"github.com/telepresenceio/telepresence/v2/pkg/k8sapi"
)

type agentInjectorConfig struct {
	Namespaced bool     `json:"namespaced"`
	Namespaces []string `json:"namespaces,omitempty"`
}

type Map interface {
	GetInto(string, string, interface{}) (bool, error)
	Run(context.Context) error
	Store(context.Context, *agent.Config, bool) error
}

func decode(v string, into interface{}) error {
	return yaml.NewDecoder(strings.NewReader(v)).Decode(into)
}

func Load(ctx context.Context, namespace string) (m Map, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		if err != nil {
			cancel()
		}
	}()

	ac := agentInjectorConfig{}
	cm, err := k8sapi.GetK8sInterface(ctx).CoreV1().ConfigMaps(namespace).Get(ctx, agent.ConfigMap, meta.GetOptions{})
	if err == nil {
		if v, ok := cm.Data[agent.InjectorKey]; ok {
			err = decode(v, &ac)
			if err != nil {
				return nil, err
			}
			dlog.Infof(ctx, "using %q entry from ConfigMap %s", agent.InjectorKey, agent.ConfigMap)
		}
	}

	dlog.Infof(ctx, "Loading ConfigMaps from %v", ac.Namespaces)
	return NewWatcher(agent.ConfigMap, ac.Namespaces...), nil
}

func (e *entry) workload(ctx context.Context) (*agent.Config, k8sapi.Workload, error) {
	ac := &agent.Config{}
	if err := decode(e.value, ac); err != nil {
		return nil, nil, fmt.Errorf("failed to decode ConfigMap entry %q into an agent config", e.value)
	}
	wl, err := k8sapi.GetWorkload(ctx, ac.WorkloadName, ac.Namespace, ac.WorkloadKind)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to get %s %s.%s: %v", ac.WorkloadKind, ac.WorkloadName, ac.Namespace, err)
	}
	return ac, wl, nil
}

func triggerRollout(ctx context.Context, wl k8sapi.Workload) {
	restartAnnotation := fmt.Sprintf(
		`{"spec": {"template": {"metadata": {"annotations": {"%srestartedAt": "%s"}}}}}`,
		install.DomainPrefix,
		time.Now().Format(time.RFC3339),
	)
	if err := wl.Patch(ctx, types.StrategicMergePatchType, []byte(restartAnnotation)); err != nil {
		dlog.Errorf(ctx, "unable to patch %s %s.%s: %v", wl.GetKind(), wl.GetName(), wl.GetNamespace(), err)
		return
	}
	dlog.Infof(ctx, "Successfully rolled out %s.%s", wl.GetName(), wl.GetNamespace())
}

func NewWatcher(name string, namespaces ...string) *configWatcher {
	return &configWatcher{
		name:       name,
		namespaces: namespaces,
		data:       make(map[string]map[string]string),
	}
}

type configWatcher struct {
	sync.RWMutex
	cancel     context.CancelFunc
	name       string
	namespaces []string
	data       map[string]map[string]string
	modCh      chan entry
	delCh      chan entry
}

type entry struct {
	name      string
	namespace string
	value     string
}

func (c *configWatcher) Run(ctx context.Context) error {
	ctx, c.cancel = context.WithCancel(ctx)
	addCh, delCh, err := c.Start(ctx)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-delCh:
			dlog.Infof(ctx, "del %s.%s: %s", e.name, e.namespace, e.value)
			ac, wl, err := e.workload(ctx)
			if err != nil {
				dlog.Error(ctx, err)
				continue
			}
			if ac.Create {
				// Deleted before it was generated, just ignore
				continue
			}
			triggerRollout(ctx, wl)
		case e := <-addCh:
			dlog.Infof(ctx, "add %s.%s: %s", e.name, e.namespace, e.value)
			ac, wl, err := e.workload(ctx)
			if err != nil {
				dlog.Error(ctx, err)
				continue
			}
			if ac.Create {
				if ac, err = Generate(ctx, wl, wl.GetPodTemplate()); err != nil {
					dlog.Error(ctx, err)
				} else if err = c.Store(ctx, ac.Namespace, ac, false); err != nil {
					dlog.Error(ctx, err)
				}
				continue // Calling Store() will generate a new event, so we skip rollout here
			}
			triggerRollout(ctx, wl)
		}
	}
}

func (c *configWatcher) GetInto(key, ns string, into interface{}) (bool, error) {
	c.RLock()
	var v string
	vs, ok := c.data[ns]
	if ok {
		v, ok = vs[key]
	}
	c.RUnlock()
	if !ok {
		return false, nil
	}
	if err := decode(v, into); err != nil {
		return false, err
	}
	return true, nil
}

// Store will store an agent config in the agents ConfigMap for the given namespace. It will
// also update the current snapshot if the updateSnapshot is true. This update will prevent
// the rollout that otherwise occur when the ConfigMap is updated.
func (c *configWatcher) Store(ctx context.Context, ac *agent.Config, updateSnapshot bool) error {
	bf := bytes.Buffer{}
	if err := yaml.NewEncoder(&bf).Encode(ac); err != nil {
		return err
	}
	yml := bf.String()

	create := false
	ns := ac.Namespace
	api := k8sapi.GetK8sInterface(ctx).CoreV1().ConfigMaps(ns)
	cm, err := api.Get(ctx, agent.ConfigMap, meta.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("unable to get ConfigMap %s: %w", agent.ConfigMap, err)
		}
		create = true
	}

	eq := false
	c.Lock()
	nm, ok := c.data[ns]
	if ok {
		if old, ok := nm[ac.AgentName]; ok {
			eq = old == yml
		}
	} else {
		nm = make(map[string]string)
		c.data[ns] = nm
	}
	if updateSnapshot && !eq {
		nm[ac.AgentName] = yml
	}
	c.Unlock()
	if eq {
		return nil
	}

	if create {
		cm = &core.ConfigMap{
			TypeMeta: meta.TypeMeta{
				Kind:       "ConfigMap",
				APIVersion: "v1",
			},
			ObjectMeta: meta.ObjectMeta{
				Name:      agent.ConfigMap,
				Namespace: ns,
			},
			Data: map[string]string{
				ac.AgentName: yml,
			},
		}
		dlog.Infof(ctx, "creating new ConfigMap %s.%s", agent.ConfigMap, ns)
		_, err = api.Create(ctx, cm, meta.CreateOptions{})
	} else {
		dlog.Infof(ctx, "updating ConfigMap %s.%s", agent.ConfigMap, ns)
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data[ac.AgentName] = yml
		_, err = api.Update(ctx, cm, meta.UpdateOptions{})
	}
	return err
}

func (c *configWatcher) Start(ctx context.Context) (modCh <-chan entry, delCh <-chan entry, err error) {
	c.Lock()
	c.modCh = make(chan entry)
	c.delCh = make(chan entry)
	c.Unlock()

	api := k8sapi.GetK8sInterface(ctx).CoreV1()
	do := func(ns string) {
		dlog.Infof(ctx, "Started watcher for ConfigMap %s.%s", agent.ConfigMap, ns)
		defer dlog.Infof(ctx, "Ended watcher for ConfigMap %s.%s", agent.ConfigMap, ns)

		// The Watch will perform a http GET call to the kubernetes API server, and that connection will not remain open forever
		// so when it closes, the watch must start over. This goes on until the context is cancelled.
		for ctx.Err() == nil {
			w, err := api.ConfigMaps(ns).Watch(ctx, meta.SingleObject(meta.ObjectMeta{
				Name: agent.ConfigMap,
			}))
			if err != nil {
				dlog.Errorf(ctx, "unable to create watcher: %v", err)
				return
			}
			if !c.eventHandler(ctx, w.ResultChan()) {
				return
			}
		}
	}

	if len(c.namespaces) == 0 {
		go do("")
	} else {
		for _, ns := range c.namespaces {
			go do(ns)
		}
	}
	return c.modCh, c.delCh, nil
}

func (c *configWatcher) eventHandler(ctx context.Context, evCh <-chan watch.Event) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case event, ok := <-evCh:
			if !ok {
				return true // restart watcher
			}
			switch event.Type {
			case watch.Deleted:
				if m, ok := event.Object.(*core.ConfigMap); ok {
					dlog.Infof(ctx, "%s %s.%s", event.Type, m.Name, m.Namespace)
					c.update(ctx, m.Namespace, nil)
				}
			case watch.Added, watch.Modified:
				if m, ok := event.Object.(*core.ConfigMap); ok {
					dlog.Infof(ctx, "%s %s.%s", event.Type, m.Name, m.Namespace)
					if m.Name != agent.ConfigMap {
						continue
					}
					c.update(ctx, m.Namespace, m.Data)
				}
			}
		}
	}
}

func writeToChan(ctx context.Context, es []entry, ch chan<- entry) {
	for _, e := range es {
		if e.name == agent.InjectorKey {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case ch <- e:
		}
	}
}

func (c *configWatcher) update(ctx context.Context, ns string, m map[string]string) {
	var dels []entry
	c.Lock()
	data, ok := c.data[ns]
	if !ok {
		data = make(map[string]string, len(m))
		c.data[ns] = data
	}
	for k, v := range data {
		if _, ok := m[k]; !ok {
			delete(data, k)
			dels = append(dels, entry{name: k, namespace: ns, value: v})
		}
	}
	var mods []entry
	for k, v := range m {
		if ov, ok := data[k]; !ok || ov != v {
			mods = append(mods, entry{name: k, namespace: ns, value: v})
			data[k] = v
		}
	}
	c.Unlock()
	go writeToChan(ctx, dels, c.delCh)
	go writeToChan(ctx, mods, c.modCh)
}