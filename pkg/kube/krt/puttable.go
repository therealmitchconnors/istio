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

package krt

import (
	"fmt"
	"sync"

	"istio.io/istio/pkg/kube/informerfactory"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/kubetypes"
	"istio.io/istio/pkg/ptr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
)

type puttable[T any] interface {
	Put(obj *T)
}

type PuttableCollection[T any] interface {
	puttable[T]
	Collection[T]
}

func NewPuttableCollection[T any]() PuttableCollection[T] {
	// TODO: can't call NewCollection with this as input, because it isn't an internalCollection.  Might need to move this into krt to get typing right.
	result := &manualListerWatcher[T]{
		broadcaster: watch.NewBroadcaster(10, watch.WaitIfChannelFull),
		state:       map[Key[T]]*T{},
	}
	example := &roWrapper[T]{
		ObjectMeta: metav1.ObjectMeta{
			Name: "example",
		},
		i: ptr.Empty[T](),
	}
	inf := cache.NewSharedIndexInformer(result, example, 0, cache.Indexers{})
	si := informerfactory.StartableInformer{
		Informer: inf,
		// TODO: what if this is unstartable?  will nil start be called?
		// start: func(stopCh <-chan struct{}) {
		// 	// f.startOne(stopCh, key)
		// },
	}
	kinf := kclient.NewInformerClient[*roWrapper[T]](si, kubetypes.Filter{})
	kkinf := WrapClient(kinf)
	kout := NewCollection[*roWrapper[T], T](kkinf, func(ctx HandlerContext, obj *roWrapper[T]) *T {
		return &obj.i
	})
	out := puttableCollectionImpl[T]{
		puttable:   result,
		Collection: kout,
	}
	return out
}

type puttableCollectionImpl[T any] struct {
	puttable[T]
	Collection[T]
}

var _ cache.ListerWatcher = &manualListerWatcher[any]{}

// manualListerWatcher provides a ListerWatcher that can have arbitrary objects added and removed from it.
// TODO: add a remove function.
type manualListerWatcher[T any] struct {
	broadcaster *watch.Broadcaster
	state       map[Key[T]]*T
	mu          sync.RWMutex
}

func (t *manualListerWatcher[T]) List(_ metav1.ListOptions) (runtime.Object, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := newCollectionList[T]()
	for _, v := range t.state {
		result.Items = append(result.Items, newWrapper[T](*v))
	}
	return result, nil
}

func (t *manualListerWatcher[T]) Watch(options metav1.ListOptions) (watch.Interface, error) {
	return t.broadcaster.Watch()
}

func (t *manualListerWatcher[T]) Put(obj *T) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state[GetKey[T](*obj)] = obj
	t.broadcaster.Action(watch.Added, newWrapper(obj))
}

// collectionList is a helper to allow arbitrary collections that are usable
// by k8s informers.
type collectionList[T any] struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	Items           []*roWrapper[T] `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// DeepCopyObject implements runtime.Object.
func (*collectionList[T]) DeepCopyObject() runtime.Object {
	panic("unimplemented")
}

func newCollectionList[T any]() *collectionList[T] {
	return &collectionList[T]{
		TypeMeta: metav1.TypeMeta{
			Kind:       fmt.Sprintf("%TList", *new(T)),
			APIVersion: "istio.io/v1alpha1",
		},
		Items: []*roWrapper[T]{},
	}
}

var _ runtime.Object = &collectionList[any]{}
var _ ResourceNamer = &roWrapper[any]{}
var _ runtime.Object = &roWrapper[any]{}

// roWrapper makes any object a runtime.Object.  This allows us to leverage the k8s informer
// logic for distributing events about arbitrary objects.
type roWrapper[T any] struct {
	metav1.ObjectMeta
	i T
}

// ResourceName implements ResourceNamer.
func (rw *roWrapper[T]) ResourceName() string {
	return string(GetKey[T](rw.i))
}

func (rw *roWrapper[T]) GetObjectKind() schema.ObjectKind {
	return &metav1.TypeMeta{
		Kind:       fmt.Sprintf("roWrapper[%T]", rw.i),
		APIVersion: "istio.io/v1alpha1",
	}
}

func (rw *roWrapper[T]) DeepCopyObject() runtime.Object {
	// TODO: might need to implement this
	panic("implement roWrapper DeepCopyObject")
	// return rw
}

func newWrapper[T any](i T) *roWrapper[T] {
	p := &corev1.Namespace{}
	var ma metav1.ObjectMetaAccessor = p
	ma.GetObjectMeta()

	om := metav1.ObjectMeta{}
	if o, f := any(i).(metav1.ObjectMetaAccessor); f {
		// TODO test that pod is ObjectMeta...
		in := o.GetObjectMeta()
		// TODO: there should be client-go logic for this...
		om = metav1.ObjectMeta{
			Name:              in.GetName(),
			Namespace:         in.GetNamespace(),
			Labels:            in.GetLabels(),
			Annotations:       in.GetAnnotations(),
			OwnerReferences:   in.GetOwnerReferences(),
			Finalizers:        in.GetFinalizers(),
			GenerateName:      in.GetGenerateName(),
			Generation:        in.GetGeneration(),
			ResourceVersion:   in.GetResourceVersion(),
			UID:               in.GetUID(),
			CreationTimestamp: in.GetCreationTimestamp(),
			DeletionTimestamp: in.GetDeletionTimestamp(),
		}
	} else {
		om.Name = string(GetKey(i))
	}
	return &roWrapper[T]{
		ObjectMeta: om,
		i:          i,
	}
}
