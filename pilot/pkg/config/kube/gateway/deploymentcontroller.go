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
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	gateway "sigs.k8s.io/gateway-api/apis/v1beta1"
	"sigs.k8s.io/yaml"

	"istio.io/api/label"
	meshapi "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/config/schema/gvr"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/inject"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/revisions"
	"istio.io/istio/pkg/test/util/tmpl"
	"istio.io/istio/pkg/test/util/yml"
	"istio.io/istio/pkg/util/sets"
)

// DeploymentController implements a controller that materializes a Gateway into an in cluster gateway proxy
// to serve requests from. This is implemented with a Deployment and Service today.
// The implementation makes a few non-obvious choices - namely using Server Side Apply from go templates
// and not using controller-runtime.
//
// controller-runtime has a number of constraints that make it inappropriate for usage here, despite this
// seeming to be the bread and butter of the library:
// * It is not readily possible to bring existing Informers, which would require extra watches (#1668)
// * Goroutine leaks (#1655)
// * Excessive API-server calls at startup which have no benefit to us (#1603)
// * Hard to use with SSA (#1669)
// While these can be worked around, at some point it isn't worth the effort.
//
// Server Side Apply with go templates is an odd choice (no one likes YAML templating...) but is one of the few
// remaining options after all others are ruled out.
//   - Merge patch/Update cannot be used. If we always enforce that our object is *exactly* the same as
//     the in-cluster object we will get in endless loops due to other controllers that like to add annotations, etc.
//     If we chose to allow any unknown fields, then we would never be able to remove fields we added, as
//     we cannot tell if we created it or someone else did. SSA fixes these issues
//   - SSA using client-go Apply libraries is almost a good choice, but most third-party clients (Istio, MCS, and gateway-api)
//     do not provide these libraries.
//   - SSA using standard API types doesn't work well either: https://github.com/kubernetes-sigs/controller-runtime/issues/1669
//   - This leaves YAML templates, converted to unstructured types and Applied with the dynamic client.
type DeploymentController struct {
	client         kube.Client
	clusterID      cluster.ID
	env            *model.Environment
	patcher        patcher
	gateways       kclient.Client[*gateway.Gateway]
	namespaces     kclient.Client[*corev1.Namespace]
	gatewayClasses kclient.Client[*gateway.GatewayClass]

	clients      map[schema.GroupVersionResource]getter
	injectConfig krt.Singleton[inject.WebhookConfig]

	revision string
}

// Patcher is a function that abstracts patching logic. This is largely because client-go fakes do not handle patching
type patcher func(gvr schema.GroupVersionResource, name string, namespace string, data []byte, subresources ...string) error

// classInfo holds information about a gateway class
type classInfo struct {
	// controller name for this class
	controller string
	// description for this class
	description string
	// The key in the templates to use for this class
	templates string

	// defaultServiceType sets the default service type if one is not explicit set
	defaultServiceType corev1.ServiceType

	// disableRouteGeneration, if set, will make it so the controller ignores this class.
	disableRouteGeneration bool

	// addressType is the default address type to report
	addressType gateway.AddressType
}

var classInfos = getClassInfos()

var builtinClasses = getBuiltinClasses()

func getBuiltinClasses() map[gateway.ObjectName]gateway.GatewayController {
	res := map[gateway.ObjectName]gateway.GatewayController{
		defaultClassName:                 constants.ManagedGatewayController,
		constants.RemoteGatewayClassName: constants.UnmanagedGatewayController,
	}
	if features.EnableAmbientControllers {
		res[constants.WaypointGatewayClassName] = constants.ManagedGatewayMeshController
	}
	return res
}

func getClassInfos() map[gateway.GatewayController]classInfo {
	m := map[gateway.GatewayController]classInfo{
		constants.ManagedGatewayController: {
			controller:         constants.ManagedGatewayController,
			description:        "The default Istio GatewayClass",
			templates:          "kube-gateway",
			defaultServiceType: corev1.ServiceTypeLoadBalancer,
			addressType:        gateway.HostnameAddressType,
		},
		constants.UnmanagedGatewayController: {
			// This represents a gateway that our control plane cannot discover directly via the API server.
			// We shouldn't generate Istio resources for it. We aren't programming this gateway.
			controller:             constants.UnmanagedGatewayController,
			description:            "Remote to this cluster. Does not deploy or affect configuration.",
			disableRouteGeneration: true,
			addressType:            gateway.HostnameAddressType,
		},
	}
	if features.EnableAmbientControllers {
		m[constants.ManagedGatewayMeshController] = classInfo{
			controller:         constants.ManagedGatewayMeshController,
			description:        "The default Istio waypoint GatewayClass",
			templates:          "waypoint",
			defaultServiceType: corev1.ServiceTypeClusterIP,
			addressType:        gateway.IPAddressType,
		}
	}
	return m
}

// NewDeploymentController constructs a DeploymentController and registers required informers.
// The controller will not start until Run() is called.
func NewDeploymentController(client kube.Client, clusterID cluster.ID, env *model.Environment,
	webhookConfig func() inject.WebhookConfig, injectionHandler func(fn func()), _ revisions.TagWatcher, revision string,
) *DeploymentController {
	gateways := kclient.New[*gateway.Gateway](client)
	krtInjectionConfig := krt.WrapHandler[inject.WebhookConfig](injectionHandler, func(_ krt.HandlerContext) *inject.WebhookConfig {
		result := webhookConfig()
		return &result
	})
	dc := &DeploymentController{
		client:    client,
		clusterID: clusterID,
		clients:   map[schema.GroupVersionResource]getter{},
		env:       env,
		patcher: func(gvr schema.GroupVersionResource, name string, namespace string, data []byte, subresources ...string) error {
			c := client.Dynamic().Resource(gvr).Namespace(namespace)
			t := true
			_, err := c.Patch(context.Background(), name, types.ApplyPatchType, data, metav1.PatchOptions{
				Force:        &t,
				FieldManager: constants.ManagedGatewayController,
			}, subresources...)
			return err
		},
		gateways:       gateways,
		injectConfig:   krtInjectionConfig,
		revision:       revision,
		namespaces:     kclient.New[*corev1.Namespace](client),
		gatewayClasses: kclient.New[*gateway.GatewayClass](client),
	}

	// Set up a handler that will add the parent Gateway object onto the queue.
	// The queue will only handle Gateway objects; if child resources (Service, etc) are updated we re-add
	// the Gateway to the queue and reconcile the state of the world.
	// parentHandler := controllers.ObjectHandler(controllers.EnqueueForParentHandler(controllers.NewQueue("foo"), gvk.KubernetesGateway))

	services := kclient.New[*corev1.Service](dc.client)
	// services.AddEventHandler(parentHandler)
	dc.clients[gvr.Service] = NewUntypedWrapper(services)
	// // ksvc := krt.WrapClient[*corev1.Service](services)

	deployments := kclient.New[*appsv1.Deployment](dc.client)
	// deployments.AddEventHandler(parentHandler)
	dc.clients[gvr.Deployment] = NewUntypedWrapper(deployments)
	// // kdeps := krt.WrapClient[*appsv1.Deployment](deployments)

	serviceAccounts := kclient.New[*corev1.ServiceAccount](dc.client)
	// serviceAccounts.AddEventHandler(parentHandler)
	dc.clients[gvr.ServiceAccount] = NewUntypedWrapper(serviceAccounts)
	// ksa := krt.WrapClient[*corev1.ServiceAccount](serviceAccounts)

	return dc
}

func (d *DeploymentController) Run(stop <-chan struct{}) {

	ntw := revisions.GetNewerTagWatcher(d.client, d.revision)

	d.clients[gvr.Namespace] = NewUntypedWrapper(d.namespaces)
	kns := krt.WrapClient[*corev1.Namespace](d.namespaces)

	d.clients[gvr.GatewayClass] = NewUntypedWrapper(d.gatewayClasses)
	kgwc := krt.WrapClient(d.gatewayClasses)

	d.clients[gvr.KubernetesGateway] = NewUntypedWrapper(d.gateways)
	kgw := krt.WrapClient[*gateway.Gateway](d.gateways)

	iss := []cache.InformerSynced{}
	infs := []controllers.Shutdowner{}
	for _, i := range d.clients {
		iss = append(iss, i.HasSynced)
		infs = append(infs, i)
	}
	kube.WaitForCacheSync(
		"deployment controller",
		stop,
		iss...,
	)

	patches := krt.NewManyCollection[*gateway.Gateway, patchInput](kgw, func(ctx krt.HandlerContext, gw *gateway.Gateway) []patchInput {
		if !IsManaged(&gw.Spec) {
			return nil
		}
		var controller gateway.GatewayController
		if gc := krt.FetchOne(ctx, kgwc, krt.FilterName(string(gw.Spec.GatewayClassName), "")); gc != nil {
			controller = (*gc).Spec.ControllerName
		} else {
			if builtin, f := builtinClasses[gw.Spec.GatewayClassName]; f {
				controller = builtin
			}
		}
		ci, f := classInfos[controller]
		if !f {
			log.Debugf("skipping unknown controller %q", controller)
			return nil
		}
		// find the tag or revision indicated by the object
		selectedTag, ok := gw.Labels[label.IoIstioRev.Name]
		if !ok {
			ns := krt.FetchOne(ctx, kns, krt.FilterName(gw.Namespace, ""))
			if ns == nil {
				log.Debugf("gateway is not for this revision, skipping")
				return nil
			}
			selectedTag = (*ns).Labels[label.IoIstioRev.Name]
		}
		// TODO: after join() becomes comparable
		// myTags := sets.New(krt.Fetch[string](ctx, ntw)...)
		myTags := sets.New(ntw.List("")...)
		if !myTags.Contains(selectedTag) && !(selectedTag == "" && myTags.Contains("default")) {
			log.Debugf("gateway is not for this revision, skipping")
			return nil
		}
		// TODO: Here we could check if the tag is set and matches no known tags, and handle that if we are default.

		// Matched class, reconcile it
		if ci.templates == "" {
			log.Debug("skip gateway class without template")
			return nil
		}
		existingControllerVersion, overwriteControllerVersion, shouldHandle := ManagedGatewayControllerVersion(*gw)
		if !shouldHandle {
			log.Debugf("skipping gateway which is managed by controller version %v", existingControllerVersion)
			return nil
		}
		log.Info("reconciling")

		nsp := krt.FetchOne(ctx, kns, krt.FilterName(gw.Namespace, ""))
		ns := (*nsp)

		proxyUID, proxyGID := inject.GetProxyIDs(ns)

		defaultName := getDefaultName(gw.Name, &gw.Spec)

		serviceType := ci.defaultServiceType
		if o, f := gw.Annotations[serviceTypeOverride]; f {
			serviceType = corev1.ServiceType(o)
		}

		input := TemplateInput{
			Gateway:        gw,
			DeploymentName: model.GetOrDefault(gw.Annotations[gatewayNameOverride], defaultName),
			ServiceAccount: model.GetOrDefault(gw.Annotations[gatewaySAOverride], defaultName),
			Ports:          extractServicePorts(*gw),
			ClusterID:      d.clusterID.String(),

			KubeVersion122: kube.IsAtLeastVersion(d.client, 22),
			Revision:       d.revision,
			ServiceType:    serviceType,
			ProxyUID:       proxyUID,
			ProxyGID:       proxyGID,
		}
		result := []patchInput{}
		if overwriteControllerVersion {
			log.Debugf("write controller version, existing=%v", existingControllerVersion)
			result = append(result, d.setGatewayControllerVersion(*gw))
		} else {
			log.Debugf("controller version existing=%v, no action needed", existingControllerVersion)
		}

		rendered, err := d.render(ctx, ci.templates, input)
		if err != nil {
			log.Errorf("failed to render template: %v", err)
		}
		for _, t := range rendered {
			pi, err := d.apply(ci.controller, t)
			if err != nil {
				log.Errorf("apply failed: %v", err)
			}
			result = append(result, pi)
		}

		log.Info("gateway rendered, queueing for patch")

		return result // TODO: do we need a differenct contract?
	})
	// Do the applying in a single collection so that events that produce duplicate patches don't hit k8s
	krt.NewCollection[patchInput, string](patches, func(ctx krt.HandlerContext, patch patchInput) *string {
		log.Debugf("applying patch %v/%s/%s", patch.gvr, patch.namespace, patch.name)
		if err := d.patcher(patch.gvr, patch.name, patch.namespace, []byte(patch.patch)); err != nil {
			log.Errorf("patch failed: %v", err)
		}
		return nil
	})
	<-stop
	controllers.ShutdownAll(infs...)
}

func (d *DeploymentController) HasSynced() bool {
	for g, i := range d.clients {
		if !i.HasSynced() {
			log.Debugf("not synced %v", g)
			return false
		}
		log.Debugf("synced %v", g)
	}
	return true
}

const (
	// ControllerVersionAnnotation is an annotation added to the Gateway by the controller specifying
	// the "controller version". The original intent of this was to work around
	// https://github.com/istio/istio/issues/44164, where we needed to transition from a global owner
	// to a per-revision owner. The newer version number allows forcing ownership, even if the other
	// version was otherwise expected to control the Gateway.
	// The version number has no meaning other than "larger numbers win".
	// Numbers are used to future-proof in case we need to do another migration in the future.
	ControllerVersionAnnotation = "gateway.istio.io/controller-version"
	// ControllerVersion is the current version of our controller logic. Known versions are:
	//
	// * 1.17 and older: version 1 OR no version at all, depending on patch release
	// * 1.18+: version 5
	//
	// 2, 3, and 4 were intentionally skipped to allow for the (unlikely) event we need to insert
	// another version between these
	ControllerVersion = 5
)

// ManagedGatewayControllerVersion determines the version of the controller managing this Gateway,
// and if we should manage this.
// See ControllerVersionAnnotation for motivations.
func ManagedGatewayControllerVersion(gw gateway.Gateway) (existing string, takeOver bool, manage bool) {
	cur, f := gw.Annotations[ControllerVersionAnnotation]
	if !f {
		// No current owner, we should take it over.
		return "", true, true
	}
	curNum, err := strconv.Atoi(cur)
	if err != nil {
		// We cannot parse it - must be some new schema we don't know about. We should assume we do not manage it.
		// In theory, this should never happen, unless we decide a number was a bad idea in the future.
		return cur, false, false
	}
	if curNum > ControllerVersion {
		// A newer version owns this gateway, let them handle it
		return cur, false, false
	}
	if curNum == ControllerVersion {
		// We already manage this at this version
		// We will manage it, but no need to attempt to apply the version annotation, which could race with newer versions
		return cur, false, true
	}
	// We are either newer or the same version of the last owner - we can take over. We need to actually
	// re-apply the annotation
	return cur, true, true
}

type derivedInput struct {
	TemplateInput

	// Inserted from injection config
	ProxyImage  string
	ProxyConfig *meshapi.ProxyConfig
	MeshConfig  *meshapi.MeshConfig
	Values      map[string]any
}

func (d *DeploymentController) render(ctx krt.HandlerContext, templateName string, mi TemplateInput) ([]string, error) {

	cfg := krt.FetchOne(ctx, d.injectConfig.AsCollection())

	template := cfg.Templates[templateName]
	if template == nil {
		return nil, fmt.Errorf("no %q template defined", templateName)
	}

	labelToMatch := map[string]string{constants.GatewayNameLabel: mi.Name}
	proxyConfig := d.env.GetProxyConfigOrDefault(mi.Namespace, labelToMatch, nil, cfg.MeshConfig)
	input := derivedInput{
		TemplateInput: mi,
		ProxyImage: inject.ProxyImage(
			cfg.Values.Struct(),
			proxyConfig.GetImage(),
			mi.Annotations,
		),
		ProxyConfig: proxyConfig,
		MeshConfig:  cfg.MeshConfig,
		Values:      cfg.Values.Map(),
	}
	results, err := tmpl.Execute(template, input)
	if err != nil {
		return nil, err
	}

	return yml.SplitString(results), nil
}

func (d *DeploymentController) setGatewayControllerVersion(gws gateway.Gateway) patchInput {
	patch := fmt.Sprintf(`{"apiVersion":"gateway.networking.k8s.io/v1beta1","kind":"Gateway","metadata":{"annotations":{"%s":"%d"}}}`,
		ControllerVersionAnnotation, ControllerVersion)

	return patchInput{
		gvr:       gvr.KubernetesGateway,
		name:      gws.GetName(),
		namespace: gws.GetNamespace(),
		patch:     patch,
	}
}

// apply server-side applies a template to the cluster.
func (d *DeploymentController) apply(controller string, yml string) (result patchInput, err error) {
	data := map[string]any{}
	err = yaml.Unmarshal([]byte(yml), &data)
	if err != nil {
		return
	}
	us := unstructured.Unstructured{Object: data}
	// set managed-by label
	clabel := strings.ReplaceAll(controller, "/", "-")
	err = unstructured.SetNestedField(us.Object, clabel, "metadata", "labels", constants.ManagedGatewayLabel)
	if err != nil {
		return
	}
	gvr, err := controllers.UnstructuredToGVR(us)
	if err != nil {
		return
	}
	j, err := json.Marshal(us.Object)
	if err != nil {
		return
	}
	canManage, resourceVersion := d.canManage(gvr, us.GetName(), us.GetNamespace())
	if !canManage {
		log.Debugf("skipping %v/%v/%v, already managed", gvr, us.GetName(), us.GetNamespace())
		return
	}
	// Ensure our canManage assertion is not stale
	us.SetResourceVersion(resourceVersion)

	// log.Debugf("applying %v", string(j))
	result = patchInput{
		gvr:       gvr,
		name:      us.GetName(),
		namespace: us.GetNamespace(),
		patch:     string(j),
	}
	return
}

// canManage checks if a resource we are about to write should be managed by us. If the resource already exists
// but does not have the ManagedGatewayLabel, we won't overwrite it.
// This ensures we don't accidentally take over some resource we weren't supposed to, which could cause outages.
// Note K8s doesn't have a perfect way to "conditionally SSA", but its close enough (https://github.com/kubernetes/kubernetes/issues/116156).
func (d *DeploymentController) canManage(gvr schema.GroupVersionResource, name, namespace string) (bool, string) {
	store, f := d.clients[gvr]
	if !f {
		log.Warnf("unknown GVR %v", gvr)
		// Even though we don't know what it is, allow users to put the resource. We won't be able to
		// protect against overwrites though.
		return true, ""
	}
	obj := store.Get(name, namespace)
	if obj == nil {
		// no object, we can manage it
		return true, ""
	}
	_, managed := obj.GetLabels()[constants.ManagedGatewayLabel]
	// If object already exists, we can only manage it if it has the label
	return managed, obj.GetResourceVersion()
}

type TemplateInput struct {
	*gateway.Gateway
	DeploymentName string
	ServiceAccount string
	Ports          []corev1.ServicePort
	ServiceType    corev1.ServiceType
	ClusterID      string
	KubeVersion122 bool
	Revision       string
	ProxyUID       int64
	ProxyGID       int64
}

func extractServicePorts(gw gateway.Gateway) []corev1.ServicePort {
	tcp := strings.ToLower(string(protocol.TCP))
	svcPorts := make([]corev1.ServicePort, 0, len(gw.Spec.Listeners)+1)
	svcPorts = append(svcPorts, corev1.ServicePort{
		Name:        "status-port",
		Port:        int32(15021),
		AppProtocol: &tcp,
	})
	portNums := sets.New[int32]()
	for i, l := range gw.Spec.Listeners {
		if portNums.Contains(int32(l.Port)) {
			continue
		}
		portNums.Insert(int32(l.Port))
		name := string(l.Name)
		if name == "" {
			// Should not happen since name is required, but in case an invalid resource gets in...
			name = fmt.Sprintf("%s-%d", strings.ToLower(string(l.Protocol)), i)
		}
		appProtocol := strings.ToLower(string(l.Protocol))
		svcPorts = append(svcPorts, corev1.ServicePort{
			Name:        name,
			Port:        int32(l.Port),
			AppProtocol: &appProtocol,
		})
	}
	return svcPorts
}

// UntypedWrapper wraps a typed reader to an untyped one, since Go cannot do it automatically.
type UntypedWrapper[T controllers.ComparableObject] struct {
	reader kclient.Informer[T]
}
type getter interface {
	Get(name, namespace string) controllers.Object
	HasSynced() bool
	ShutdownHandlers()
}

func NewUntypedWrapper[T controllers.ComparableObject](c kclient.Client[T]) getter {
	return UntypedWrapper[T]{c}
}

func (u UntypedWrapper[T]) Get(name, namespace string) controllers.Object {
	// DO NOT return u.reader.Get directly, or we run into issues with https://go.dev/tour/methods/12
	res := u.reader.Get(name, namespace)
	if controllers.IsNil(res) {
		return nil
	}
	return res
}

func (u UntypedWrapper[T]) HasSynced() bool {
	return u.reader.HasSynced()
}

func (u UntypedWrapper[T]) ShutdownHandlers() {
	u.reader.ShutdownHandlers()
}

var _ getter = UntypedWrapper[*corev1.Service]{}

type patchInput struct {
	name, namespace, patch string
	gvr                    schema.GroupVersionResource
}

func (pi *patchInput) ResourceName() string {
	return fmt.Sprintf("%s/%s/%s", pi.name, pi.namespace, pi.patch)
}
