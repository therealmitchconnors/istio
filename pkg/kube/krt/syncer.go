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
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/config/schema/kubeclient"
	"istio.io/istio/pkg/config/schema/resource"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/kubetypes"
	"istio.io/istio/pkg/ptr"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8schema "k8s.io/apimachinery/pkg/runtime/schema"
)

func ApplyToK8s[T any](g Collection[T], c kube.Client) {

	myGVR := kubeclient.GetGVRFromApplyConfigType(reflect.TypeOf(ptr.Empty[T]()))

	i := kclient.NewUntypedInformer(c, myGVR, kubetypes.Filter{})
	ic := WrapClient[controllers.Object](i)

	NewSyncer[T, controllers.Object](g, ic,
		func(t T, live controllers.Object) bool {
			liveac, err := kubeclient.ExtractApplyConfig(live, "istio")
			if err != nil {
				log.Errorf("failed to extract applyconfig: %v", err)
			}
			return reflect.DeepEqual(liveac, t)
		},
		func(i T) {
			apiVersion, kindString := getVersionKind(i)
			// apiVersion := getField(i, "APIVersion")
			gv, _ := k8schema.ParseGroupVersion(apiVersion)
			// kindString := getField(i, "Kind")
			kind := gv.WithKind(kindString)
			igvk := resource.FromKubernetesGVK(&kind)
			gvr := gvk.MustToGVR(igvk)
			us := convertToUnstructured(i)
			// TODO: Dynamic fake doesn't support apply upsert
			_, err := c.Dynamic().Resource(gvr).Namespace(us.GetNamespace()).Apply(context.Background(), us.GetName(), &us, v1.ApplyOptions{
				DryRun:       nil,
				Force:        true,
				FieldManager: "istio",
			})
			if err != nil {
				panic(err)
			}
		},
	)
}

func convertToUnstructured(i any) unstructured.Unstructured {
	jbytes, err := json.Marshal(i)
	if err != nil {
		panic(err)
	}
	var imap map[string]any
	err = json.Unmarshal(jbytes, &imap)
	if err != nil {
		panic(err)
	}
	return unstructured.Unstructured{Object: imap}
}

func getVersionKind(v interface{}) (string, string) {
	r := reflect.Indirect(reflect.ValueOf(v)).FieldByName("TypeMetaApplyConfiguration")
	apiVersion := r.FieldByName("APIVersion").Elem()
	kind := r.FieldByName("Kind").Elem()
	return apiVersion.String(), kind.String()
}

func NewSyncer[G, L any](
	g Collection[G],
	l Collection[L],
	compare func(G, L) bool,
	apply func(G),
) {
	mu := sync.Mutex{}
	log := log.WithLabels("type", fmt.Sprintf("sync[%T]", *new(G)))
	NewCollection[G, bool](g, func(ctx HandlerContext, gen G) *bool {
		genKey := GetKey(gen)
		live := FetchOne(ctx, l, FilterKey(string(genKey)))
		mu.Lock()
		defer mu.Unlock()
		changed := true
		if live != nil {
			changed = !compare(gen, *live)
		}
		log.WithLabels("changed", changed).Infof("live update")
		if changed {
			apply(gen)
		}
		return nil
	})
}
