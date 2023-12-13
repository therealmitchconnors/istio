// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kube

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"go.uber.org/atomic"
	istiofake "istio.io/client-go/pkg/clientset/versioned/fake"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/config/schema/kubeclient"
	"istio.io/istio/pkg/kube/informerfactory"
	"istio.io/istio/pkg/lazy"
	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/util/sets"
	extfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	kubeVersion "k8s.io/apimachinery/pkg/version"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/storage/names"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/testing"
	clienttesting "k8s.io/client-go/testing"
	gatewayapifake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
	"sigs.k8s.io/structured-merge-diff/v4/fieldpath"
)

// NewFakeClient creates a new, fake, client
func NewFakeClient(objects ...runtime.Object) CLIClient {
	c := &client{
		informerWatchesPending: atomic.NewInt32(0),
		clusterID:              "fake",
	}
	c.config = &rest.Config{
		Host: "server",
	}

	c.informerFactory = informerfactory.NewSharedInformerFactory()

	s := FakeIstioScheme
	fmf := newFieldManagerFacotry(s)
	merger := fakeMerger{
		scheme: s,
	}

	f := fake.NewSimpleClientset(objects...)
	insertPatchReactor(f, fmf)
	merger.Merge(f)
	c.kube = f

	c.metadata = metadatafake.NewSimpleMetadataClient(s)
	df := dynamicfake.NewSimpleDynamicClient(s)
	insertPatchReactor(df, fmf)
	merger.MergeDynamic(df)
	c.dynamic = df

	ifc := istiofake.NewSimpleClientset()
	insertPatchReactor(ifc, fmf)
	merger.Merge(ifc)
	c.istio = ifc

	gf := gatewayapifake.NewSimpleClientset()
	insertPatchReactor(gf, fmf)
	merger.Merge(gf)
	c.gatewayapi = gf
	c.extSet = extfake.NewSimpleClientset()

	// https://github.com/kubernetes/kubernetes/issues/95372
	// There is a race condition in the client fakes, where events that happen between the List and Watch
	// of an informer are dropped. To avoid this, we explicitly manage the list and watch, ensuring all lists
	// have an associated watch before continuing.
	// This would likely break any direct calls to List(), but for now our tests don't do that anyways. If we need
	// to in the future we will need to identify the Lists that have a corresponding Watch, possibly by looking
	// at created Informers
	// an atomic.Int is used instead of sync.WaitGroup because wg.Add and wg.Wait cannot be called concurrently
	listReactor := func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
		c.informerWatchesPending.Inc()
		return false, nil, nil
	}
	watchReactor := func(tracker clienttesting.ObjectTracker) func(action clienttesting.Action) (handled bool, ret watch.Interface, err error) {
		return func(action clienttesting.Action) (handled bool, ret watch.Interface, err error) {
			gvr := action.GetResource()
			ns := action.GetNamespace()
			watch, err := tracker.Watch(gvr, ns)
			if err != nil {
				return false, nil, err
			}
			c.informerWatchesPending.Dec()
			return true, watch, nil
		}
	}
	// https://github.com/kubernetes/client-go/issues/439
	createReactor := func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
		ret = action.(clienttesting.CreateAction).GetObject()
		meta, ok := ret.(metav1.Object)
		if !ok {
			return
		}

		if meta.GetName() == "" && meta.GetGenerateName() != "" {
			meta.SetName(names.SimpleNameGenerator.GenerateName(meta.GetGenerateName()))
		}

		return
	}
	for _, fc := range []fakeClient{
		c.kube.(*fake.Clientset),
		c.istio.(*istiofake.Clientset),
		c.gatewayapi.(*gatewayapifake.Clientset),
		c.dynamic.(*dynamicfake.FakeDynamicClient),
		c.metadata.(*metadatafake.FakeMetadataClient),
	} {
		fc.PrependReactor("list", "*", listReactor)
		fc.PrependWatchReactor("*", watchReactor(fc.Tracker()))
		fc.PrependReactor("create", "*", createReactor)
	}

	c.fastSync = true

	c.version = lazy.NewWithRetry(c.kube.Discovery().ServerVersion)

	if NewCrdWatcher != nil {
		c.crdWatcher = NewCrdWatcher(c)
	}

	return c
}

func NewFakeClientWithVersion(minor string, objects ...runtime.Object) CLIClient {
	c := NewFakeClient(objects...).(*client)
	c.Kube().Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &kubeVersion.Info{Major: "1", Minor: minor, GitVersion: fmt.Sprintf("v1.%v.0", minor)}
	return c
}

type fakeClient interface {
	PrependReactor(verb, resource string, reaction clienttesting.ReactionFunc)
	PrependWatchReactor(resource string, reaction clienttesting.WatchReactionFunc)
	Tracker() clienttesting.ObjectTracker
}

func insertPatchReactor(f testing.FakeClient, fmf *fieldManagerFactory) {
	f.PrependReactor(
		"patch",
		"*",
		func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			pa := action.(clienttesting.PatchAction)
			if pa.GetPatchType() == types.ApplyPatchType {
				// Apply patches are supposed to upsert, but fake client fails if the object doesn't exist,
				// if an apply patch occurs for a deployment that doesn't yet exist, create it.
				// However, we already hold the fakeclient lock, so we can't use the front door.
				// rfunc := clienttesting.ObjectReaction(f.Tracker())
				obj, err := f.Tracker().Get(pa.GetResource(), pa.GetNamespace(), pa.GetName())
				new := false
				mygvk := gvk.MustFromGVR(pa.GetResource()).Kubernetes()
				if kerrors.IsNotFound(err) || obj == nil {
					new = true
					obj = kubeclient.GVRToObject(pa.GetResource())
					d := obj.(metav1.Object)
					d.SetName(pa.GetName())
					d.SetNamespace(pa.GetNamespace())
					e := obj.(schema.ObjectKind)
					e.SetGroupVersionKind(mygvk)
				}

				appliedObj := &unstructured.Unstructured{Object: map[string]interface{}{}}
				if err := json.Unmarshal(pa.GetPatch(), &appliedObj.Object); err != nil {
					log.Errorf("error decoding YAML: %v", err)
					return false, nil, err
				}
				res, err := fmf.FieldManager(mygvk).Apply(obj, appliedObj, "istio", false)
				if err != nil {
					log.Errorf("error applying patch: %v", err)
					return false, res, err
				}

				if new {
					err = f.Tracker().Create(pa.GetResource(), res, pa.GetNamespace())
				} else if !reflect.DeepEqual(obj, res) {
					err = f.Tracker().Update(pa.GetResource(), res, pa.GetNamespace())
				}

				return true, res, err
			}
			return false, nil, nil
		},
	)
}

func actionKey(action testing.Action) string {
	out := strings.Builder{}
	out.WriteString(action.GetVerb())
	out.WriteString("/")
	out.WriteString(action.GetResource().String())
	out.WriteString("/")
	out.WriteString(action.GetNamespace())
	if n, ok := action.(named); ok {
		out.WriteString("/")
		out.WriteString(n.GetName())
	}
	return out.String()
}

type named interface {
	GetName() string
}
type fakeMerger struct {
	inProgress sets.Set[string]
	mergeLock  sync.RWMutex
	initOnce   sync.Once
	alertList  []testing.FakeClient
	scheme     *runtime.Scheme
	dynamic    *dynamicfake.FakeDynamicClient
}

func (fm *fakeMerger) init() {
	fm.initOnce.Do(func() {
		fm.mergeLock.Lock()
		defer fm.mergeLock.Unlock()
		fm.inProgress = sets.Set[string]{}
	})
}

func (fm *fakeMerger) propagateReactionSingle(destination testing.FakeClient, action testing.Action) (handled bool, ret runtime.Object, err error) {
	// prevent recursion with inProgress
	// TODO: this is probably excessive locking
	key := actionKey(action)
	fm.mergeLock.RLock()
	if fm.inProgress.Contains(key) {
		// this is an action 'echoing' back after we propagated it.  drop it.
		fm.mergeLock.RUnlock()
		return false, nil, nil
	}
	fm.mergeLock.RUnlock()
	fm.mergeLock.Lock()
	fm.inProgress.Insert(key)
	fm.mergeLock.Unlock()
	// if action is update or create, it includes a runtim.Object, which needs to be
	// passed to Invokes (either as unstructured [in the case of dynamic], or typed),
	// otherwise the obj var is ignored, and is traditionally defaulted to metav1.status
	var defaultObj runtime.Object
	defaultObj = &metav1.Status{Status: "metadata get fail"}
	if o, ok := action.(objectiveAction); ok {
		defaultObj = o.GetObject()
	}
	var typedDefault runtime.Object
	if _, ok := destination.(*dynamicfake.FakeDynamicClient); ok {
		typedDefault = &unstructured.Unstructured{}
	} else {
		typedDefault = kubeclient.GVRToObject(action.GetResource())
	}
	// convert the object to the required type
	err = fm.scheme.Convert(defaultObj, typedDefault, nil)
	if err != nil {
		log.Errorf("Cannot convert %v to the desired type for %v: %v", defaultObj, destination, err)
		return false, nil, nil
	}
	switch newAction := action.DeepCopy().(type) {
	case clienttesting.CreateActionImpl:
		newAction.Object = typedDefault
		_, err := destination.Invokes(newAction, typedDefault)
		if err != nil {
			log.Errorf("Propagating Invoke resulted in error for fake %v: %v", destination, err)
		}
	case clienttesting.UpdateActionImpl:
		newAction.Object = typedDefault
		_, err := destination.Invokes(newAction, typedDefault)
		if err != nil {
			log.Errorf("Propagating Invoke resulted in error for fake %v: %v", destination, err)
		}
	default:
		_, err := destination.Invokes(newAction, typedDefault)
		if err != nil {
			log.Errorf("Propagating Invoke resulted in error for fake %v: %v", destination, err)
		}
	}
	fm.mergeLock.Lock()
	fm.inProgress.Delete(key)
	fm.mergeLock.Unlock()
	// even if others return handled=true, we still want this client to react.
	return false, nil, nil
}

func (fm *fakeMerger) Merge(f testing.FakeClient) {
	fm.init()
	fm.mergeLock.Lock()
	defer fm.mergeLock.Unlock()
	fm.alertList = append(fm.alertList, f)
	myPropagator := func(action testing.Action) (bool, runtime.Object, error) {
		// propagate from typed client to dynamic
		return fm.propagateReactionSingle(fm.dynamic, action)
	}
	f.PrependReactor("*", "*", myPropagator)
}

func (fm *fakeMerger) MergeDynamic(df *dynamicfake.FakeDynamicClient) {
	fm.dynamic = df
	myPropagator := func(action testing.Action) (bool, runtime.Object, error) {
		for _, f := range fm.alertList {
			// propagate from dynamic to any typed client that cares.
			_, _, _ = fm.propagateReactionSingle(f, action)
		}
		// even if others return handled=true, we still want this client to react.
		return false, nil, nil
	}
	df.PrependReactor("*", "*", myPropagator)
}

type objectiveAction interface {
	GetObject() runtime.Object
}

type fieldManagerFactory struct {
	schema    *runtime.Scheme
	existing  map[schema.GroupVersionKind]*managedfields.FieldManager
	converter managedfields.TypeConverter
	mu        sync.Mutex
}

func newFieldManagerFacotry(myschema *runtime.Scheme) *fieldManagerFactory {
	return &fieldManagerFactory{
		schema:    myschema,
		converter: managedfields.NewDeducedTypeConverter(),
		existing:  make(map[schema.GroupVersionKind]*managedfields.FieldManager),
	}
}

func (fmf *fieldManagerFactory) FieldManager(gvk schema.GroupVersionKind) *managedfields.FieldManager {
	fmf.mu.Lock()
	defer fmf.mu.Unlock()
	if result, ok := fmf.existing[gvk]; ok {
		return result
	}
	result, err := managedfields.NewDefaultFieldManager(fmf.converter, fmf.schema, fmf.schema, fmf.schema, gvk, gvk.GroupVersion(), "", make(map[fieldpath.APIVersion]*fieldpath.Set))
	if err != nil {
		panic(err)
	}
	fmf.existing[gvk] = result
	return result
}
