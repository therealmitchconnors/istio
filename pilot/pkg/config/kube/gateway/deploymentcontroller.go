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
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
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
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/kube/kubetypes"
	"istio.io/istio/pkg/kube/namespace"
	"istio.io/istio/pkg/ptr"
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
	client       kube.Client
	clusterID    cluster.ID
	env          *model.Environment
	injectConfig krt.Singleton[inject.WebhookConfig]
	revision     string
	syncer       krt.Syncer
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
		defaultClassName: constants.ManagedGatewayController,
	}

	if features.MultiNetworkGatewayAPI {
		res[constants.RemoteGatewayClassName] = constants.UnmanagedGatewayController
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
	}

	if features.MultiNetworkGatewayAPI {
		m[constants.UnmanagedGatewayController] = classInfo{
			// This represents a gateway that our control plane cannot discover directly via the API server.
			// We shouldn't generate Istio resources for it. We aren't programming this gateway.
			controller:             constants.UnmanagedGatewayController,
			description:            "Remote to this cluster. Does not deploy or affect configuration.",
			disableRouteGeneration: true,
			addressType:            gateway.HostnameAddressType,
		}
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
	webhookConfig func() inject.WebhookConfig, injectionHandler func(fn func()), revision string,
	nsFilter namespace.DiscoveryNamespacesFilter,
) *DeploymentController {
	var filter namespace.DiscoveryFilter
	if nsFilter != nil {
		filter = nsFilter.Filter
	}
	krtInjectionConfig := krt.WrapHandler[inject.WebhookConfig](injectionHandler, func() *inject.WebhookConfig {
		result := webhookConfig()
		return &result
	})
	dc := &DeploymentController{
		client:       client,
		clusterID:    clusterID,
		env:          env,
		injectConfig: krtInjectionConfig,
		revision:     revision,
	}

	ntw := revisions.GetNewerTagWatcher(dc.client, dc.revision)
	kns := krt.NewInformerFiltered[*corev1.Namespace](client, kubetypes.Filter{ObjectFilter: filter})
	kgwc := krt.NewInformer[*gateway.GatewayClass](client)
	kgw := krt.NewInformerFiltered[*gateway.Gateway](client, kubetypes.Filter{ObjectFilter: filter})

	outSvc := krt.NewPuttableCollection[corev1ac.ServiceApplyConfiguration]()
	outGW := krt.NewPuttableCollection[patchInput]()
	outSA := krt.NewPuttableCollection[corev1ac.ServiceAccountApplyConfiguration]()

	var deployments krt.Collection[controllers.Object]
	services := krt.ApplyToK8s(outSvc, client)
	serviceAccounts := krt.ApplyToK8s(outSA, client)

	deploymentACs := krt.NewCollection[*gateway.Gateway, appsv1ac.DeploymentApplyConfiguration](kgw, func(ctx krt.HandlerContext, gw *gateway.Gateway) *appsv1ac.DeploymentApplyConfiguration {
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
			ClusterID:      dc.clusterID.String(),

			KubeVersion122: kube.IsAtLeastVersion(dc.client, 22),
			Revision:       dc.revision,
			ServiceType:    serviceType,
			ProxyUID:       proxyUID,
			ProxyGID:       proxyGID,
		}
		if overwriteControllerVersion {
			log.Debugf("write controller version, existing=%v", existingControllerVersion)
			outGW.Put(dc.setGatewayControllerVersion(*gw))
		} else {
			log.Debugf("controller version existing=%v, no action needed", existingControllerVersion)
		}

		rendered, err := dc.render(ctx, ci.templates, input)
		if err != nil {
			log.Errorf("failed to render template: %v", err)
		}
		svcac, saac, depac, err := unmarshalRendered(rendered)
		if err != nil {
			log.Errorf("failed to unmarshal rendered template: %v", err)
		}

		svc := krt.FetchOne(ctx, services, krt.FilterName(*svcac.Name, *svcac.Namespace))
		if ok, rv := dc.canManage(*svc); ok {
			outSvc.Put(svcac.WithResourceVersion(rv))
		}
		sa := krt.FetchOne(ctx, serviceAccounts, krt.FilterName(*saac.Name, *saac.Namespace))
		if ok, rv := dc.canManage(*sa); ok {
			outSA.Put(saac.WithResourceVersion(rv))
		}

		dep := krt.FetchOne(ctx, deployments, krt.FilterName(*depac.Name, *depac.Namespace))
		if ok, rv := dc.canManage(*dep); ok {
			return depac.WithResourceVersion(rv)
		}
		return nil
	})

	deployments = krt.ApplyToK8s(deploymentACs, client)

	krt.NewCollection[patchInput, string](outGW, func(ctx krt.HandlerContext, gw patchInput) *string {
		t := true
		_, err := client.Dynamic().Resource(gvr.KubernetesGateway).Namespace(gw.namespace).
			Patch(context.Background(), gw.name, types.ApplyPatchType, []byte(gw.patch), metav1.PatchOptions{
				Force:        &t,
				FieldManager: constants.ManagedGatewayController,
			})
		if err != nil {
			log.Error(err)
		}
		return ptr.Of[string]("")
	})

	dc.syncer = krt.NewMultiSyncer(deploymentACs.Synced(), outSvc.Synced(), outGW.Synced(), outSA.Synced())

	return dc
}

func unmarshalRendered(rendered []string) (svcac *corev1ac.ServiceApplyConfiguration,
	saac *corev1ac.ServiceAccountApplyConfiguration, depac *appsv1ac.DeploymentApplyConfiguration, err error) {
	for _, t := range rendered {
		data := map[string]any{}
		err = yaml.Unmarshal([]byte(t), &data)
		if err != nil {
			log.Error(err)
		}
		us := unstructured.Unstructured{Object: data}

		var mygvr schema.GroupVersionResource
		mygvr, err = controllers.UnstructuredToGVR(us)
		if err != nil {
			return
		}
		switch mygvr {
		case gvr.Service:
			err = yaml.Unmarshal([]byte(t), svcac)
			if err != nil {
				return
			}
		case gvr.ServiceAccount:
			err = yaml.Unmarshal([]byte(t), saac)
			if err != nil {
				return
			}
		case gvr.Deployment:
			err = yaml.Unmarshal([]byte(t), depac)
			if err != nil {
				return
			}
		default:
			panic("unknown gvr")
		}
	}
	return
}

func (d *DeploymentController) WaitUntilSynced(stop <-chan struct{}) {
	d.syncer.WaitUntilSynced(stop)
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

func (d *DeploymentController) setGatewayControllerVersion(gws gateway.Gateway) *patchInput {
	patch := fmt.Sprintf(`{"apiVersion":"gateway.networking.k8s.io/v1beta1","kind":"Gateway","metadata":{"annotations":{"%s":"%d"}}}`,
		ControllerVersionAnnotation, ControllerVersion)

	return &patchInput{
		gvr:       gvr.KubernetesGateway,
		name:      gws.GetName(),
		namespace: gws.GetNamespace(),
		patch:     patch,
	}
}

// canManage checks if a resource we are about to write should be managed by us. If the resource already exists
// but does not have the ManagedGatewayLabel, we won't overwrite it.
// This ensures we don't accidentally take over some resource we weren't supposed to, which could cause outages.
// Note K8s doesn't have a perfect way to "conditionally SSA", but its close enough (https://github.com/kubernetes/kubernetes/issues/116156).
func (d *DeploymentController) canManage(obj metav1.Object) (bool, string) {
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

type patchInput struct {
	name, namespace, patch string
	gvr                    schema.GroupVersionResource
}

func (pi *patchInput) ResourceName() string {
	return fmt.Sprintf("%s/%s/%s", pi.name, pi.namespace, pi.patch)
}
