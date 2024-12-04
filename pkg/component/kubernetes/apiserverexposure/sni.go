// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package apiserverexposure

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	istioapinetworkingv1beta1 "istio.io/api/networking/v1beta1"
	istionetworkingv1alpha3 "istio.io/client-go/pkg/apis/networking/v1alpha3"
	istionetworkingv1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/component"
	"github.com/gardener/gardener/pkg/component/kubernetes/apiserver"
	kubeapiserverconstants "github.com/gardener/gardener/pkg/component/kubernetes/apiserver/constants"
	"github.com/gardener/gardener/pkg/controllerutils"
	"github.com/gardener/gardener/pkg/features"
	"github.com/gardener/gardener/pkg/utils/istio"
	kubernetesutils "github.com/gardener/gardener/pkg/utils/kubernetes"
	"github.com/gardener/gardener/pkg/utils/managedresources"
	netutils "github.com/gardener/gardener/pkg/utils/net"
)

const managedResourceName = "kube-apiserver-sni"

// MutualTLSServiceNameSuffix is used to create a second service instance for
// use with mutual tls authentication from istio using the ca-front-proxy secrets
const MutualTLSServiceNameSuffix = "-mutual"

var (
	//go:embed templates/envoyfilter-apiserver-proxy.yaml
	envoyFilterAPIServerProxyTemplateContent string
	envoyFilterAPIServerProxyTemplate        *template.Template
	//go:embed templates/envoyfilter-istio-tls-termination.yaml
	envoyFilterIstioTLSTerminationTemplateContent string
	envoyFilterIstioTLSTerminationTemplate        *template.Template
)

func init() {
	envoyFilterAPIServerProxyTemplate = template.Must(template.
		New("envoy-filter-apiserver-proxy").
		Funcs(sprig.TxtFuncMap()).
		Parse(envoyFilterAPIServerProxyTemplateContent),
	)
	envoyFilterIstioTLSTerminationTemplate = template.Must(template.
		New("envoy-filter-istio-tls-termination").
		Funcs(sprig.TxtFuncMap()).
		Parse(envoyFilterIstioTLSTerminationTemplateContent),
	)
}

// SNIValues configure the kube-apiserver service SNI.
type SNIValues struct {
	Hosts               []string
	APIServerProxy      *APIServerProxy
	IstioIngressGateway IstioIngressGateway
}

// APIServerProxy contains values for the APIServer proxy protocol configuration.
type APIServerProxy struct {
	NamespaceUID       types.UID
	APIServerClusterIP string
}

// IstioIngressGateway contains the values for istio ingress gateway configuration.
type IstioIngressGateway struct {
	Namespace string
	Labels    map[string]string
}

// NewSNI creates a new instance of DeployWaiter which deploys Istio resources for
// kube-apiserver SNI access.
func NewSNI(
	client client.Client,
	name string,
	namespace string,
	valuesFunc func() *SNIValues,
) component.DeployWaiter {
	if valuesFunc == nil {
		valuesFunc = func() *SNIValues { return &SNIValues{} }
	}

	return &sni{
		client:     client,
		name:       name,
		namespace:  namespace,
		valuesFunc: valuesFunc,
	}
}

type sni struct {
	client     client.Client
	name       string
	namespace  string
	valuesFunc func() *SNIValues
}

type envoyFilterAPIServerProxyTemplateValues struct {
	*APIServerProxy
	IngressGatewayLabels        map[string]string
	Name                        string
	Namespace                   string
	OwnerRefNamespace           string
	Host                        string
	MutualTLSHost               string
	Port                        int
	APIServerClusterIPPrefixLen int
}

type envoyFilterIstioTLSTerminationTemplateValues struct {
	IngressGatewayLabels map[string]string
	Name                 string
	Namespace            string
	MutualTLSHost        string
	Port                 int
}

func (s *sni) Deploy(ctx context.Context) error {
	var (
		values = s.valuesFunc()

		destinationRule       = s.emptyDestinationRule()
		mutualDestinationRule = s.emptyMutualDestinationRule()
		gateway               = s.emptyGateway()
		virtualService        = s.emptyVirtualService()

		hostName                       = fmt.Sprintf("%s.%s.svc.%s", s.name, s.namespace, gardencorev1beta1.DefaultDomain)
		mTLSHostName                   = fmt.Sprintf("%s.%s.svc.%s", s.name+MutualTLSServiceNameSuffix, s.namespace, gardencorev1beta1.DefaultDomain)
		envoyFilterAPIServerProxy      bytes.Buffer
		envoyFilterIstioTLSTermination bytes.Buffer
	)

	registry := managedresources.NewRegistry(kubernetes.SeedScheme, kubernetes.SeedCodec, kubernetes.SeedSerializer)

	if values.APIServerProxy != nil {
		envoyFilter := s.emptyEnvoyFilterAPIServerProxy()
		apiServerClusterIPPrefixLen, err := netutils.GetBitLen(values.APIServerProxy.APIServerClusterIP)
		if err != nil {
			return err
		}

		if err := envoyFilterAPIServerProxyTemplate.Execute(&envoyFilterAPIServerProxy, envoyFilterAPIServerProxyTemplateValues{
			APIServerProxy:              values.APIServerProxy,
			IngressGatewayLabels:        values.IstioIngressGateway.Labels,
			Name:                        envoyFilter.Name,
			Namespace:                   envoyFilter.Namespace,
			OwnerRefNamespace:           s.namespace,
			Host:                        hostName,
			Port:                        kubeapiserverconstants.Port,
			MutualTLSHost:               mTLSHostName,
			APIServerClusterIPPrefixLen: apiServerClusterIPPrefixLen,
		}); err != nil {
			return err
		}

		filename := fmt.Sprintf("envoyfilter__%s__%s.yaml", envoyFilter.Namespace, envoyFilter.Name)
		registry.AddSerialized(filename, envoyFilterAPIServerProxy.Bytes())
	}

	if features.DefaultFeatureGate.Enabled(features.IstioTLSTermination) {
		envoyFilter := s.emptyEnvoyFilterIstioTLSTermination()

		if err := envoyFilterIstioTLSTerminationTemplate.Execute(&envoyFilterIstioTLSTermination, envoyFilterIstioTLSTerminationTemplateValues{
			IngressGatewayLabels: values.IstioIngressGateway.Labels,
			Name:                 envoyFilter.Name,
			Namespace:            envoyFilter.Namespace,
			Port:                 kubeapiserverconstants.Port,
			MutualTLSHost:        mTLSHostName,
		}); err != nil {
			return err
		}

		filename := fmt.Sprintf("envoyfilter__%s__%s.yaml", envoyFilter.Namespace, envoyFilter.Name)
		registry.AddSerialized(filename, envoyFilterIstioTLSTermination.Bytes())
	}

	if values.APIServerProxy != nil || features.DefaultFeatureGate.Enabled(features.IstioTLSTermination) {
		serializedObjects, err := registry.SerializedObjects()
		if err != nil {
			return err
		}

		if err := managedresources.CreateForSeed(ctx, s.client, s.namespace, managedResourceName, false, serializedObjects); err != nil {
			return err
		}
	}

	var destinationMutateFn func() error
	destinationMutateFn = istio.DestinationRuleWithLocalityPreference(destinationRule, getLabels(), hostName)
	if features.DefaultFeatureGate.Enabled(features.IstioTLSTermination) {
		destinationMutateFn = istio.DestinationRuleWithLocalityPreferenceAndTLSTermination(destinationRule, getLabels(), hostName, s.valuesFunc().Hosts[0], s.namespace+apiserver.IstioCASecretSuffix, istioapinetworkingv1beta1.ClientTLSSettings_SIMPLE)
	}

	if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, s.client, destinationRule, destinationMutateFn); err != nil {
		return err
	}

	if features.DefaultFeatureGate.Enabled(features.IstioTLSTermination) {
		destinationMutualMutateFn := istio.DestinationRuleWithLocalityPreferenceAndTLSTermination(mutualDestinationRule, getLabels(), mTLSHostName, s.valuesFunc().Hosts[0], s.namespace+apiserver.IstioCASecretSuffix, istioapinetworkingv1beta1.ClientTLSSettings_MUTUAL)
		if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, s.client, mutualDestinationRule, destinationMutualMutateFn); err != nil {
			return err
		}
	}

	var gatewayMutateFn func() error
	gatewayMutateFn = istio.GatewayWithTLSPassthrough(gateway, getLabels(), s.valuesFunc().IstioIngressGateway.Labels, s.valuesFunc().Hosts, kubeapiserverconstants.Port)
	if features.DefaultFeatureGate.Enabled(features.IstioTLSTermination) {
		gatewayMutateFn = istio.GatewayWithMutalTLS(gateway, getLabels(), s.valuesFunc().IstioIngressGateway.Labels, s.valuesFunc().Hosts, kubeapiserverconstants.Port, s.namespace+apiserver.IstioTLSSecretSuffix)
	}

	if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, s.client, gateway, gatewayMutateFn); err != nil {
		return err
	}

	var virtualServiceMutateFn func() error
	virtualServiceMutateFn = istio.VirtualServiceWithSNIMatch(virtualService, getLabels(), s.valuesFunc().Hosts, gateway.Name, kubeapiserverconstants.Port, hostName)
	if features.DefaultFeatureGate.Enabled(features.IstioTLSTermination) {
		virtualServiceMutateFn = istio.VirtualServiceForTLSTermination(virtualService, getLabels(), s.valuesFunc().Hosts, gateway.Name, kubeapiserverconstants.Port, hostName)
	}

	if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, s.client, virtualService, virtualServiceMutateFn); err != nil {
		return err
	}

	return nil
}

func (s *sni) Destroy(ctx context.Context) error {
	if err := managedresources.DeleteForSeed(ctx, s.client, s.namespace, managedResourceName); err != nil {
		return err
	}

	return kubernetesutils.DeleteObjects(
		ctx,
		s.client,
		s.emptyDestinationRule(),
		s.emptyMutualDestinationRule(),
		s.emptyEnvoyFilterAPIServerProxy(),
		s.emptyEnvoyFilterIstioTLSTermination(),
		s.emptyGateway(),
		s.emptyVirtualService(),
	)
}

func (s *sni) Wait(_ context.Context) error        { return nil }
func (s *sni) WaitCleanup(_ context.Context) error { return nil }

func (s *sni) emptyDestinationRule() *istionetworkingv1beta1.DestinationRule {
	return &istionetworkingv1beta1.DestinationRule{ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.namespace}}
}

func (s *sni) emptyMutualDestinationRule() *istionetworkingv1beta1.DestinationRule {
	return &istionetworkingv1beta1.DestinationRule{ObjectMeta: metav1.ObjectMeta{Name: s.name + "-mutual", Namespace: s.namespace}}
}

func (s *sni) emptyEnvoyFilterAPIServerProxy() *istionetworkingv1alpha3.EnvoyFilter {
	return &istionetworkingv1alpha3.EnvoyFilter{ObjectMeta: metav1.ObjectMeta{Name: s.namespace + "-apiserver-proxy", Namespace: s.valuesFunc().IstioIngressGateway.Namespace}}
}

func (s *sni) emptyEnvoyFilterIstioTLSTermination() *istionetworkingv1alpha3.EnvoyFilter {
	return &istionetworkingv1alpha3.EnvoyFilter{ObjectMeta: metav1.ObjectMeta{Name: s.namespace + "-istio-tls-termination", Namespace: s.valuesFunc().IstioIngressGateway.Namespace}}
}

func (s *sni) emptyGateway() *istionetworkingv1beta1.Gateway {
	return &istionetworkingv1beta1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.namespace}}
}

func (s *sni) emptyVirtualService() *istionetworkingv1beta1.VirtualService {
	return &istionetworkingv1beta1.VirtualService{ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.namespace}}
}
