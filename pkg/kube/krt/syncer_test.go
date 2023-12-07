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

package krt_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	cgtesting "k8s.io/client-go/testing"

	"istio.io/istio/pkg/config/schema/collections"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/retry"
)

func TestApplyToK8s(t *testing.T) {
	g := gomega.NewWithT(t)
	c := kube.NewFakeClient()

	timeout := time.Hour
	Secrets := krt.NewInformer[*corev1.Secret](c)
	GeneratedConfigMap := krt.NewCollection[*corev1.Secret, corev1ac.ConfigMapApplyConfiguration](Secrets, func(ctx krt.HandlerContext, i *corev1.Secret) *corev1ac.ConfigMapApplyConfiguration {
		m := map[string]string{}
		for k, v := range i.Data {
			m[k] = string(v)
		}
		return corev1ac.ConfigMap(i.Name, i.Namespace).
			WithData(m)
	})
	krt.ApplyToK8s(GeneratedConfigMap, c)

	log.Warn("creating first secret")
	c.RunAndWait(test.NewStop(t))
	c.Kube().CoreV1().Secrets("default").Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "name"},
		Data: map[string][]byte{
			"key": []byte("value"),
		},
	}, metav1.CreateOptions{})
	check := func(name, namespace string, data map[string]any) {
		retry.UntilSuccessOrFail(t, func() error {
			lcmp, _ := c.Dynamic().Resource(collections.ConfigMap.GroupVersionResource()).
				Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
			if lcmp == nil {
				return fmt.Errorf("configmap not found")
			}
			lcm := *lcmp
			if !reflect.DeepEqual(lcm.Object["data"], data) {
				return fmt.Errorf("unexpected data %+v", lcm.Object["data"])
			}
			return nil
		}, retry.Timeout(timeout))
	}
	patchCount := func() int {
		res := 0
		for _, act := range c.Dynamic().(cgtesting.FakeClient).Actions() {
			if act.Matches("patch", collections.ConfigMap.GroupVersionResource().Resource) && act.GetNamespace() == "default" {
				res++
			}
		}
		return res
	}
	g.SetDefaultEventuallyTimeout(timeout)
	g.Eventually(patchCount).Should(gomega.Equal(1))
	check("name", "default", map[string]any{"key": "value"})
	g.Consistently(patchCount).Should(gomega.Equal(1))

	log.Warn("update input secret")
	c.Kube().CoreV1().Secrets("default").Update(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "name"},
		Data: map[string][]byte{
			"key": []byte("value2"),
		},
	}, metav1.UpdateOptions{})
	g.Eventually(patchCount).Should(gomega.Equal(2))
	check("name", "default", map[string]any{"key": "value2"})
	g.Consistently(patchCount).Should(gomega.Equal(2))
	time.Sleep(time.Second)
}

func TestSynchronizer(t *testing.T) {
	c := kube.NewFakeClient()
	g := gomega.NewWithT(t)
	timeout := time.Second
	Secrets := krt.NewInformer[*corev1.Secret](c)
	GeneratedConfigMap := krt.NewCollection[*corev1.Secret, corev1ac.ConfigMapApplyConfiguration](Secrets, func(ctx krt.HandlerContext, i *corev1.Secret) *corev1ac.ConfigMapApplyConfiguration {
		m := map[string]string{}
		for k, v := range i.Data {
			m[k] = string(v)
		}
		return corev1ac.ConfigMap(i.Name, i.Namespace).
			WithData(m)
	})
	LiveConfigMaps := krt.NewInformer[*corev1.ConfigMap](c)
	krt.NewSyncer(
		GeneratedConfigMap,
		LiveConfigMaps,
		func(gen corev1ac.ConfigMapApplyConfiguration, live *corev1.ConfigMap) bool {
			liveac, err := corev1ac.ExtractConfigMap(live, "istio")
			if err != nil {
				t.Fatal(err)
			}

			result := reflect.DeepEqual(gen, *liveac)
			return result
		},
		func(gen corev1ac.ConfigMapApplyConfiguration) {
			log.Warnf("apply to cm: %v", gen.Data["key"])
			_, err := c.Kube().CoreV1().ConfigMaps(*gen.Namespace).Apply(context.Background(), &gen, metav1.ApplyOptions{
				DryRun:       nil,
				Force:        true,
				FieldManager: "istio",
			})
			if err != nil {
				t.Fatal(err)
			}
		},
	)
	log.Warn("creating first secret")
	c.RunAndWait(test.NewStop(t))
	c.Kube().CoreV1().Secrets("default").Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "name"},
		Data: map[string][]byte{
			"key": []byte("value"),
		},
	}, metav1.CreateOptions{})
	check := func(key string, data map[string]string) {
		retry.UntilSuccessOrFail(t, func() error {
			lcmp := LiveConfigMaps.GetKey(krt.Key[*corev1.ConfigMap](key))
			if lcmp == nil {
				return fmt.Errorf("configmap not found")
			}
			lcm := *lcmp
			if !reflect.DeepEqual(lcm.Data, data) {
				return fmt.Errorf("unexpected data %+v", lcm.Data)
			}
			return nil
		}, retry.Timeout(timeout))
	}
	patchCount := func() int {
		res := 0
		for _, act := range c.Kube().(*fake.Clientset).Actions() {
			if act.Matches("patch", collections.ConfigMap.GroupVersionResource().Resource) && act.GetNamespace() == "default" {
				res++
			}
		}
		return res
	}
	g.SetDefaultEventuallyTimeout(timeout)
	g.Eventually(patchCount).Should(gomega.Equal(1))
	check("default/name", map[string]string{"key": "value"})
	g.Consistently(patchCount).Should(gomega.Equal(1))

	log.Warn("update input secret")
	c.Kube().CoreV1().Secrets("default").Update(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "name"},
		Data: map[string][]byte{
			"key": []byte("value2"),
		},
	}, metav1.UpdateOptions{})
	g.Eventually(patchCount).Should(gomega.Equal(2))
	check("default/name", map[string]string{"key": "value2"})
	g.Consistently(patchCount).Should(gomega.Equal(2))
	time.Sleep(time.Second)
}

func getType[T any]() reflect.Type {
	return reflect.TypeOf(ptr.Empty[T]())
}
