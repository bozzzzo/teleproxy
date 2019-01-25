package k8s

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	pwatch "k8s.io/apimachinery/pkg/watch"

	"k8s.io/client-go/dynamic"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/cache"
)

type listWatchAdapter struct {
	resource dynamic.ResourceInterface
}

func (lw listWatchAdapter) List(options v1.ListOptions) (runtime.Object, error) {
	// silently coerce the returned *unstructured.UnstructuredList
	// struct to a runtime.Object interface.
	return lw.resource.List(options)
}

func (lw listWatchAdapter) Watch(options v1.ListOptions) (pwatch.Interface, error) {
	return lw.resource.Watch(options)
}

type Watcher struct {
	client  *Client
	watches map[watchKey]watch
	mutex   sync.Mutex
	started bool
	stop    chan struct{}
	wg      sync.WaitGroup
}

type watchKey struct {
	schema.GroupVersionResource
	Namespace string
}

type watch struct {
	resource  dynamic.NamespaceableResourceInterface
	hasSynced cache.InformerSynced
	store     cache.Store
	invoke    func()
	runner    func()
}

// NewWatcher returns a Kubernetes Watcher for the specified cluster
func NewWatcher(c *Client) *Watcher {
	w := &Watcher{
		client:  c,
		watches: make(map[watchKey]watch),
		stop:  make(chan struct{}),
	}

	return w
}

func (w *Watcher) Watch(resources string, listener func(*Watcher)) error {
	return w.WatchNamespace("", resources, listener)
}

func (w *Watcher) WatchNamespace(namespace, resources string, listener func(*Watcher)) error {
	ri := w.client.ResolveResourceType(resources)

	gvr := schema.GroupVersionResource{
		Group:    ri.Group,
		Version:  ri.Version,
		Resource: ri.Name,
	}

	return w.WatchInternal(gvr, namespace, listener)
}

func (w *Watcher) WatchInternal(gvr schema.GroupVersionResource, namespace string, listener func(*Watcher)) error {
	kubeclient, err := dynamic.NewForConfig(w.client.config)
	if err != nil {
		return err
	}

	resource := kubeclient.Resource(gvr)
	var watched dynamic.ResourceInterface = resource
	if namespace == "" {
		watched = resource.Namespace(namespace)
	}

	var hasSynced cache.InformerSynced
	invoke := func() {
		w.mutex.Lock()
		defer w.mutex.Unlock()
		if hasSynced() {
			listener(w)
		}
	}

	store, informerController := cache.NewInformer(
		listWatchAdapter{watched},
		nil,
		5*time.Minute,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				invoke()
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldUn := oldObj.(*unstructured.Unstructured)
				newUn := newObj.(*unstructured.Unstructured)
				// we ignore updates for objects
				// already in our store because we
				// assume this means we made the
				// change to them
				if oldUn.GetResourceVersion() != newUn.GetResourceVersion() {
					invoke()
				}
			},
			DeleteFunc: func(obj interface{}) {
				invoke()
			},
		},
	)

	hasSynced = informerController.HasSynced

	runner := func() {
		informerController.Run(w.stop)
		w.wg.Done()
	}

	w.watches[watchKey{gvr, namespace}] = watch{
		resource:  resource,
		hasSynced: informerController.HasSynced,
		store:     store,
		invoke:    invoke,
		runner:    runner,
	}

	return nil
}

func (w *Watcher) Start() {
	w.mutex.Lock()
	if w.started {
		w.mutex.Unlock()
		return
	} else {
		w.started = true
		w.mutex.Unlock()
	}

	w.wg.Add(len(w.watches))
	for _, watch := range w.watches {
		go watch.runner()
	}

	informerSynceds := make([]cache.InformerSynced, 0, len(w.watches))
	for _, watch := range w.watches {
		informerSynceds = append(informerSynceds, watch.hasSynced)
	}
	if !cache.WaitForCacheSync(w.stopCh, informerSynceds...) {
		log.Fatal("failed to sync")
	}

	for _, watch := range w.watches {
		watch.invoke()
	}
}

func (w *Watcher) List(kind string) []Resource {
	ri := w.client.resolve(w.client.Canonicalize(kind))

	gvr := schema.GroupVersionResource{
		Group:    ri.Group,
		Version:  ri.Version,
		Resource: ri.Name,
	}

	return w.ListInternal(gvr, "")
}

func (w *Watcher) ListInternal(gvr schema.GroupVersionResource, namespace string) []Resource {
	watch, ok := w.watches[watchKey{gvr, namespace}]
	if ok {
		objs := watch.store.List()
		result := make([]Resource, len(objs))
		for idx, obj := range objs {
			result[idx] = obj.(*unstructured.Unstructured).UnstructuredContent()
		}
		return result
	} else {
		return nil
	}
}

func (w *Watcher) UpdateStatus(resource Resource) (Resource, error) {
	ri := w.client.resolve(w.client.Canonicalize(resource.Kind()))

	gvr := schema.GroupVersionResource{
		Group:    ri.Group,
		Version:  ri.Version,
		Resource: ri.Name,
	}

	var uns unstructured.Unstructured
	uns.SetUnstructuredContent(resource)

	watch, ok := w.watches[watchKey{gvr, uns.GetNamespace()}]
	if !ok {
		return nil, fmt.Errorf("no watch: %v", gvr)
	}

	// XXX: should we have an if Namespaced here?
	result, err := watch.resource.Namespace(uns.GetNamespace()).UpdateStatus(&uns, v1.UpdateOptions{})
	if err != nil {
		return nil, err
	} else {
		watch.store.Update(result)
		return result.UnstructuredContent(), nil
	}
}

func (w *Watcher) Get(kind, qname string) Resource {
	resources := w.List(kind)
	for _, res := range resources {
		if strings.EqualFold(res.QName(), qname) {
			return res
		}
	}
	return Resource{}
}

func (w *Watcher) Exists(kind, qname string) bool {
	return w.Get(kind, qname).Name() != ""
}

func (w *Watcher) Stop() {
	close(w.stop)
	w.wg.Wait()
}

func (w *Watcher) Wait() {
	w.Start()
	w.wg.Wait()
}
