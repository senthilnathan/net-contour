/*
Copyright 2020 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resources

import (
	"context"
	"fmt"

	v1 "github.com/projectcontour/contour/apis/projectcontour/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"knative.dev/net-contour/pkg/reconciler/contour/config"
	"knative.dev/net-contour/pkg/reconciler/contour/resources/names"
	"knative.dev/networking/pkg/apis/networking/v1alpha1"
	"knative.dev/pkg/kmeta"
	"knative.dev/pkg/logging"
)

// MakeEndpointProbeIngress creates a new child kingress resource with a
// bogus hostname per referenced service, which we will probe to ensure
// each service has been warmed in Envoy's EDS before changing any of the
// active RDS programming to reference those endpoints.
func MakeEndpointProbeIngress(ctx context.Context, ing *v1alpha1.Ingress, previousState []*v1.HTTPProxy) *v1alpha1.Ingress {
	childIng := &v1alpha1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.EndpointProbeIngress(ing),
			Namespace: ing.Namespace,
			Labels:    ing.Labels,
			Annotations: kmeta.UnionMaps(ing.Annotations, map[string]string{
				EndpointsProbeKey: "true",
			}),
			OwnerReferences: []metav1.OwnerReference{*kmeta.NewControllerRef(ing)},
		},
		Spec: v1alpha1.IngressSpec{
			// TODO: Probing against HTTP should be enough as it ensures Envoy's EDS?
			// Need to verify it by scale-N test with HTTPS.
			HTTPOption: v1alpha1.HTTPOptionEnabled,
		},
	}

	sns := ServiceNames(ctx, ing)

	// Reverse engineer our previous state from the prior generation's HTTP Proxy resources.
	for _, proxy := range previousState {
		// Skip probe when status is not valid. It happens when the previous revision was garbage collected.
		// see: https://github.com/knative/serving/issues/9582
		if proxy.Status.CurrentStatus != "valid" {
			logging.FromContext(ctx).Infof("Skip invalid proxy: %#v", proxy)
			continue
		}

		// Establish the visibility based on the class annotation.
		var vis v1alpha1.IngressVisibility
		for v, class := range config.FromContext(ctx).Contour.VisibilityClasses {
			if class == proxy.Annotations[ClassKey] {
				vis = v
			}
		}
		if vis == "" {
			continue
		}

		for _, route := range proxy.Spec.Routes {
			hasPath := false
			for _, cond := range route.Conditions {
				if cond.Prefix != "" {
					hasPath = true
				}
			}
			for _, svc := range route.Services {
				si, ok := sns[svc.Name]
				if !ok {
					si = ServiceInfo{
						Port:            intstr.FromInt(svc.Port),
						RawVisibilities: sets.NewString(),
						HasPath:         hasPath,
					}
				}
				si.RawVisibilities.Insert(string(vis))
				sns[svc.Name] = si
			}
		}
	}

	// Give the services a deterministic ordering.
	order := make(sets.String, len(sns))
	for key := range sns {
		order.Insert(key)
	}

	l := order.List()
	logging.FromContext(ctx).Debugf("Endpoints probe will cover services: %v", l)

	for _, name := range l {
		si := sns[name]
		if si.HasPath {
			// TODO(https://github.com/knative-sandbox/net-certmanager/issues/44): Remove this.
			continue
		}
		for _, vis := range si.Visibilities() {
			childIng.Spec.Rules = append(childIng.Spec.Rules, v1alpha1.IngressRule{
				Hosts:      []string{fmt.Sprintf("%s.gen-%d.%s.%s.net-contour.invalid", name, ing.Generation, ing.Name, ing.Namespace)},
				Visibility: vis,
				HTTP: &v1alpha1.HTTPIngressRuleValue{
					Paths: []v1alpha1.HTTPIngressPath{{
						RewriteHost: si.RewriteHost,
						Splits: []v1alpha1.IngressBackendSplit{{
							IngressBackend: v1alpha1.IngressBackend{
								ServiceName:      name,
								ServiceNamespace: ing.Namespace,
								ServicePort:      si.Port,
							},
							Percent: 100,
						}},
					}},
				},
			})
		}
	}

	return childIng
}
