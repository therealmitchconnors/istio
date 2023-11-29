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

package gateway

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"go.uber.org/atomic"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/apis/v1alpha2"
	"sigs.k8s.io/gateway-api/apis/v1beta1"
	"sigs.k8s.io/yaml"

	istioio_networking_v1beta1 "istio.io/api/networking/v1beta1"
	istio_type_v1beta1 "istio.io/api/type/v1beta1"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/test/util"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/config/schema/gvr"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/inject"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/kclient/clienttest"
	istiolog "istio.io/istio/pkg/log"
	"istio.io/istio/pkg/revisions"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/env"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/test/util/file"
	"istio.io/istio/pkg/test/util/retry"
)

func TestConfigureIstioGateway(t *testing.T) {
	defaultNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	customClass := &v1beta1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "custom",
		},
		Spec: v1beta1.GatewayClassSpec{
			ControllerName: constants.ManagedGatewayController,
		},
	}
	defaultObjects := []runtime.Object{defaultNamespace}
	store := model.NewFakeStore()
	if _, err := store.Create(config.Config{
		Meta: config.Meta{
			GroupVersionKind: gvk.ProxyConfig,
			Name:             "test",
			Namespace:        "default",
		},
		Spec: &istioio_networking_v1beta1.ProxyConfig{
			Selector: &istio_type_v1beta1.WorkloadSelector{
				MatchLabels: map[string]string{
					"istio.io/gateway-name": "default",
				},
			},
			Image: &istioio_networking_v1beta1.ProxyImage{
				ImageType: "distroless",
			},
		},
	}); err != nil {
		t.Fatalf("failed to create ProxyConfigs: %s", err)
	}
	proxyConfig := model.GetProxyConfigs(store, mesh.DefaultMeshConfig())
	tests := []struct {
		name    string
		gw      v1beta1.Gateway
		objects []runtime.Object
		pcs     *model.ProxyConfigs
	}{
		{
			name: "simple",
			gw: v1beta1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default",
					Namespace: "default",
				},
				Spec: v1alpha2.GatewaySpec{
					GatewayClassName: defaultClassName,
				},
			},
			objects: defaultObjects,
		},
		{
			name: "manual-sa",
			gw: v1beta1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "default",
					Namespace:   "default",
					Annotations: map[string]string{gatewaySAOverride: "custom-sa"},
				},
				Spec: v1alpha2.GatewaySpec{
					GatewayClassName: defaultClassName,
				},
			},
			objects: defaultObjects,
		},
		{
			name: "manual-ip",
			gw: v1beta1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "default",
					Namespace:   "default",
					Annotations: map[string]string{gatewayNameOverride: "default"},
				},
				Spec: v1beta1.GatewaySpec{
					GatewayClassName: defaultClassName,
					Addresses: []v1beta1.GatewayAddress{{
						Type:  func() *v1beta1.AddressType { x := v1beta1.IPAddressType; return &x }(),
						Value: "1.2.3.4",
					}},
				},
			},
			objects: defaultObjects,
		},
		{
			name: "cluster-ip",
			gw: v1beta1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default",
					Namespace: "default",
					Annotations: map[string]string{
						"networking.istio.io/service-type": string(corev1.ServiceTypeClusterIP),
						gatewayNameOverride:                "default",
					},
				},
				Spec: v1beta1.GatewaySpec{
					GatewayClassName: defaultClassName,
					Listeners: []v1beta1.Listener{{
						Name:     "http",
						Port:     v1beta1.PortNumber(80),
						Protocol: k8sv1.HTTPProtocolType,
					}},
				},
			},
			objects: defaultObjects,
		},
		{
			name: "multinetwork",
			gw: v1beta1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "default",
					Namespace:   "default",
					Labels:      map[string]string{"topology.istio.io/network": "network-1"},
					Annotations: map[string]string{gatewayNameOverride: "default"},
				},
				Spec: v1beta1.GatewaySpec{
					GatewayClassName: defaultClassName,
					Listeners: []v1beta1.Listener{{
						Name:     "http",
						Port:     v1beta1.PortNumber(80),
						Protocol: k8sv1.HTTPProtocolType,
					}},
				},
			},
			objects: defaultObjects,
		},
		{
			name: "waypoint",
			gw: v1beta1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "namespace",
					Namespace: "default",
					Labels: map[string]string{
						"topology.istio.io/network": "network-1",
					},
				},
				Spec: v1beta1.GatewaySpec{
					GatewayClassName: constants.WaypointGatewayClassName,
					Listeners: []v1beta1.Listener{{
						Name:     "mesh",
						Port:     v1beta1.PortNumber(15008),
						Protocol: "ALL",
					}},
				},
			},
			objects: defaultObjects,
		},
		{
			name: "proxy-config-crd",
			gw: v1beta1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default",
					Namespace: "default",
				},
				Spec: v1alpha2.GatewaySpec{
					GatewayClassName: defaultClassName,
				},
			},
			objects: defaultObjects,
			pcs:     proxyConfig,
		},
		{
			name: "custom-class",
			gw: v1beta1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default",
					Namespace: "default",
				},
				Spec: v1beta1.GatewaySpec{
					GatewayClassName: v1beta1.ObjectName(customClass.Name),
				},
			},
			objects: defaultObjects,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pc := patchChecker{data: map[patchKey][][]byte{}}
			client := kube.NewFakeClient(tt.objects...)
			kclient.NewWriteClient[*v1beta1.GatewayClass](client).Create(customClass)
			kclient.NewWriteClient[*v1beta1.Gateway](client).Create(&tt.gw)
			stop := test.NewStop(t)
			env := model.NewEnvironment()
			env.PushContext().ProxyConfigs = tt.pcs
			tw := revisions.NewTagWatcher(client, "")
			go tw.Run(stop)
			d := NewDeploymentController(
				client, cluster.ID(features.ClusterName), env, testInjectionConfig(t), func(fn func()) {
				}, tw, "")
			d.patcher = pc.patcher
			client.RunAndWait(stop)
			go d.Run(stop)
			kube.WaitForCacheSync("test", stop, d.HasSynced)

			pc.CompareToGolden(t, filepath.Join("testdata", "deployment", tt.name+".yaml"))
		})
	}
}

func TestVersionManagement(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	log.SetOutputLevel(istiolog.DebugLevel)
	writes := make(chan string, 10)
	c := kube.NewFakeClient(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "default",
		},
	})
	tw := revisions.NewTagWatcher(c, "default")
	env := &model.Environment{}
	d := NewDeploymentController(c, "", env, testInjectionConfig(t), func(fn func()) {}, tw, "")
	reconciles := atomic.NewInt32(0)
	wantReconcile := int32(0)
	expectNotReconcield := func() {
		t.Helper()
		g.Consistently(reconciles.Load).Within(100 * time.Millisecond).Should(gomega.Equal(wantReconcile))
	}
	expectReconciled := func() {
		t.Helper()
		wantReconcile++
		assert.EventuallyEqual(t, reconciles.Load, wantReconcile, retry.Timeout(time.Second*5), retry.Message("no reconciliation"))
		expectNotReconcield()
	}

	d.patcher = func(g schema.GroupVersionResource, name string, namespace string, data []byte, subresources ...string) error {
		if g == gvr.KubernetesGateway {
			b, err := yaml.JSONToYAML(data)
			if err != nil {
				return err
			}
			writes <- string(b)
			reconciles.Inc()
		}
		return nil
	}
	stop := test.NewStop(t)
	gws := clienttest.Wrap(t, d.gateways)
	go tw.Run(stop)
	go d.Run(stop)
	c.RunAndWait(stop)
	kube.WaitForCacheSync("test", stop, d.HasSynced)
	// Create a gateway, we should mark our ownership
	defaultGateway := &v1beta1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gw",
			Namespace: "default",
		},
		Spec: v1beta1.GatewaySpec{GatewayClassName: defaultClassName},
	}
	gws.Create(defaultGateway)
	assert.Equal(t, assert.ChannelHasItem(t, writes), buildPatch(ControllerVersion))
	expectReconciled()
	assert.ChannelIsEmpty(t, writes)
	// Test fake doesn't actual do Apply, so manually do this
	defaultGateway.Annotations = map[string]string{ControllerVersionAnnotation: fmt.Sprint(ControllerVersion)}
	gws.Update(defaultGateway)
	// We shouldn't write in response to our write.
	assert.ChannelIsEmpty(t, writes)

	defaultGateway.Annotations["foo"] = "bar"
	gws.Update(defaultGateway)
	// We should not be updating the version, its already set. Setting it introduces a possible race condition
	// since we use SSA so there is no conflict checks.
	assert.ChannelIsEmpty(t, writes)

	// Somehow the annotation is removed - it should be added back
	defaultGateway.Annotations = map[string]string{}
	gws.Update(defaultGateway)
	// TODO uncomment after drift detection enabled
	expectReconciled()
	assert.Equal(t, assert.ChannelHasItem(t, writes), buildPatch(ControllerVersion))
	assert.ChannelIsEmpty(t, writes)
	// Test fake doesn't actual do Apply, so manually do this
	defaultGateway.Annotations = map[string]string{ControllerVersionAnnotation: fmt.Sprint(ControllerVersion)}
	gws.Update(defaultGateway)
	// We shouldn't write in response to our write.
	assert.ChannelIsEmpty(t, writes)

	// Somehow the annotation is set to an older version - it should be added back
	defaultGateway.Annotations = map[string]string{ControllerVersionAnnotation: fmt.Sprint(1)}
	gws.Update(defaultGateway)
	expectReconciled()
	assert.Equal(t, assert.ChannelHasItem(t, writes), buildPatch(ControllerVersion))
	assert.ChannelIsEmpty(t, writes)
	// Test fake doesn't actual do Apply, so manually do this
	defaultGateway.Annotations = map[string]string{ControllerVersionAnnotation: fmt.Sprint(ControllerVersion)}
	gws.Update(defaultGateway)
	// We shouldn't write in response to our write.
	assert.ChannelIsEmpty(t, writes)

	// Somehow the annotation is set to an new version - we should do nothing
	defaultGateway.Annotations = map[string]string{ControllerVersionAnnotation: fmt.Sprint(10)}
	gws.Update(defaultGateway)
	assert.ChannelIsEmpty(t, writes)
	// Do not expect reconcile
	expectNotReconcield()
}

func testInjectionConfig(t test.Failer) func() inject.WebhookConfig {
	vc, err := inject.NewValuesConfig(`
global:
  hub: test
  tag: test`)
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := inject.ParseTemplates(map[string]string{
		"kube-gateway": file.AsStringOrFail(t, filepath.Join(env.IstioSrc, "manifests/charts/istio-control/istio-discovery/files/kube-gateway.yaml")),
		"waypoint":     file.AsStringOrFail(t, filepath.Join(env.IstioSrc, "manifests/charts/istio-control/istio-discovery/files/waypoint.yaml")),
	})
	if err != nil {
		t.Fatal(err)
	}
	injConfig := func() inject.WebhookConfig {
		return inject.WebhookConfig{
			Templates:  tmpl,
			Values:     vc,
			MeshConfig: mesh.DefaultMeshConfig(),
		}
	}
	return injConfig
}

func buildPatch(version int) string {
	return fmt.Sprintf(`apiVersion: gateway.networking.k8s.io/v1beta1
kind: Gateway
metadata:
  annotations:
    gateway.istio.io/controller-version: "%d"
`, version)
}

type patchKey struct {
	gvr       schema.GroupVersionResource
	name      string
	namespace string
}

type patchChecker struct {
	data map[patchKey][][]byte
}

func (p *patchChecker) patcher(gvr schema.GroupVersionResource, name string, namespace string, data []byte, subresources ...string) error {
	key := patchKey{gvr: gvr, name: name, namespace: namespace}
	patches, ok := p.data[key]
	if !ok {
		patches = [][]byte{}
	}
	yamldata, err := yaml.JSONToYAML(data)
	if err != nil {
		return err
	}
	yamldata = timestampRegex.ReplaceAll(yamldata, []byte("lastTransitionTime: fake"))

	// ensure patch contents match gvr, name, and namespace vars
	datamap := map[string]any{}
	err = yaml.Unmarshal(yamldata, &datamap)
	if err != nil {
		return fmt.Errorf("Unable to parse golden file as yaml: %s", err)
	}
	us := unstructured.Unstructured{Object: datamap}
	ygvr, err := controllers.UnstructuredToGVR(us)
	if err != nil {
		return fmt.Errorf("Unable to parse golden file as unstructured: %s", err)
	}
	if ygvr != gvr {
		return fmt.Errorf("GVR mismatch: %s != %s", ygvr, gvr)
	}
	if len(us.GetName()) > 0 && us.GetName() != name {
		return fmt.Errorf("Name mismatch: %s != %s", us.GetName(), name)
	}
	if len(us.GetNamespace()) > 0 && us.GetNamespace() != namespace {
		return fmt.Errorf("Namespace mismatch: %s != %s", us.GetNamespace(), namespace)
	}
	us.SetName(name)
	us.SetNamespace(namespace)
	j, err := yaml.Marshal(us.Object)
	if err != nil {
		return err
	}

	if len(patches) == 0 || string(patches[len(patches)-1]) != string(j) {
		patches = append(patches, j)
		p.data[key] = patches
	}
	return nil
}

func (p *patchChecker) dump() *bytes.Buffer {
	result := &bytes.Buffer{}
	for key, patches := range p.data {
		for _, patch := range patches {
			result.WriteString(fmt.Sprintf("%s/%s/%s\n", key.gvr, key.namespace, key.name))
			result.Write(patch)
			result.WriteString("---\n")
		}
	}
	return result
}

func (p *patchChecker) CompareToGolden(t *testing.T, goldenFile string) {
	inputContent := p.dump().Bytes()
	g := gomega.NewGomegaWithT(t)
	goldenBytes := util.ReadGoldenFile(t, inputContent, goldenFile)
	for _, doc := range strings.Split(string(goldenBytes), "---\n") {
		if len(strings.TrimSpace(doc)) == 0 {
			continue
		}
		data := map[string]any{}
		err := yaml.Unmarshal([]byte(doc), &data)
		if err != nil {
			t.Fatalf("Unable to parse golden file as yaml: %s", err)
		}
		us := unstructured.Unstructured{Object: data}
		gvr, err := controllers.UnstructuredToGVR(us)

		if err != nil {
			t.Fatalf("Unable to parse golden file as unstructured: %s", err)
		}
		key := patchKey{gvr: gvr, name: us.GetName(), namespace: us.GetNamespace()}
		g.Eventually(func() [][]byte { return p.data[key] }).Should(
			gomega.And(
				gomega.HaveLen(1),
				gomega.ContainElement(gomega.MatchYAML(doc)),
			),
		)
	}
}
