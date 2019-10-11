/*
Copyright 2019 kubeflow.org.

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

package knative

import (
	"context"
	"fmt"

	"github.com/kubeflow/kfserving/pkg/apis/serving/v1alpha2"
	"github.com/kubeflow/kfserving/pkg/constants"
	"github.com/kubeflow/kfserving/pkg/controller/inferenceservice/resources/knative"
	"knative.dev/pkg/kmp"
	knservingv1alpha1 "knative.dev/serving/pkg/apis/serving/v1alpha1"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
)

var log = logf.Log.WithName("Reconciler")

type ServiceReconciler struct {
	client         client.Client
	scheme         *runtime.Scheme
	serviceBuilder *knative.ServiceBuilder
}

func NewServiceReconciler(client client.Client, scheme *runtime.Scheme, config *v1.ConfigMap) *ServiceReconciler {
	return &ServiceReconciler{
		client:         client,
		scheme:         scheme,
		serviceBuilder: knative.NewServiceBuilder(client, config),
	}
}

func (r *ServiceReconciler) Reconcile(isvc *v1alpha2.InferenceService) error {
	if err := r.reconcileDefault(isvc); err != nil {
		return err
	}

	if err := r.reconcileCanary(isvc); err != nil {
		return err
	}
	return nil
}

func (r *ServiceReconciler) reconcileDefault(isvc *v1alpha2.InferenceService) error {
	for _, endpoint := range []constants.InferenceServiceEndpoint{constants.Predictor, constants.Transformer, constants.Explainer} {
		if err := r.reconcileEndpoint(isvc, endpoint, false); err != nil {
			return err
		}
	}
	return nil
}

func (r *ServiceReconciler) reconcileCanary(isvc *v1alpha2.InferenceService) error {
	for _, endpoint := range []constants.InferenceServiceEndpoint{constants.Predictor, constants.Transformer, constants.Explainer} {
		if err := r.reconcileEndpoint(isvc, endpoint, true); err != nil {
			return err
		}
	}
	return nil
}

func (r *ServiceReconciler) reconcileEndpoint(isvc *v1alpha2.InferenceService, endpoint constants.InferenceServiceEndpoint, isCanary bool) error {
	if isCanary {
		if isvc.Spec.Canary == nil {
			if err := r.finalizeCanaryService(isvc, endpoint); err != nil {
				return err
			}
			isvc.Status.PropagateCanaryStatus(endpoint, nil)
			return nil
		}
	}

	service, err := r.serviceBuilder.CreateEndpointService(
		isvc,
		endpoint,
		isCanary,
	)

	if err != nil {
		return err
	}

	if service == nil {
		return nil
	}

	status, err := r.reconcileService(isvc, service)
	if err != nil {
		return err
	}
	if isCanary {
		isvc.Status.PropagateCanaryStatus(endpoint, status)
	} else {
		isvc.Status.PropagateDefaultStatus(endpoint, status)
	}
	return nil
}

func (r *ServiceReconciler) finalizeCanaryService(isvc *v1alpha2.InferenceService, endpoint constants.InferenceServiceEndpoint) error {
	canaryServiceName := constants.CanaryServiceName(isvc.Name, endpoint)
	existing := &knservingv1alpha1.Service{}
	if err := r.client.Get(context.TODO(), types.NamespacedName{Name: canaryServiceName, Namespace: isvc.Namespace}, existing); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	} else {
		log.Info("Deleting service", "namespace", isvc.Namespace, "name", canaryServiceName)
		if err := r.client.Delete(context.TODO(), existing, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
			if !errors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func (r *ServiceReconciler) reconcileService(isvc *v1alpha2.InferenceService, desired *knservingv1alpha1.Service) (*knservingv1alpha1.ServiceStatus, error) {
	if err := controllerutil.SetControllerReference(isvc, desired, r.scheme); err != nil {
		return nil, err
	}
	// Create service if does not exist
	existing := &knservingv1alpha1.Service{}
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("Creating Knative Service", "namespace", desired.Namespace, "name", desired.Name)
			return &desired.Status, r.client.Create(context.TODO(), desired)
		}
		return nil, err
	}

	// Return if no differences to reconcile.
	if semanticEquals(desired, existing) {
		return &existing.Status, nil
	}

	// Reconcile differences and update
	diff, err := kmp.SafeDiff(desired.Spec.ConfigurationSpec, existing.Spec.ConfigurationSpec)
	if err != nil {
		return &existing.Status, fmt.Errorf("failed to diff service: %v", err)
	}
	log.Info("Reconciling service diff (-desired, +observed):", "diff", diff)
	log.Info("Updating service", "namespace", desired.Namespace, "name", desired.Name)
	existing.Spec.ConfigurationSpec = desired.Spec.ConfigurationSpec
	existing.ObjectMeta.Labels = desired.ObjectMeta.Labels
	if err := r.client.Update(context.TODO(), existing); err != nil {
		return &existing.Status, err
	}

	return &existing.Status, nil
}

func semanticEquals(desiredService, service *knservingv1alpha1.Service) bool {
	return equality.Semantic.DeepEqual(desiredService.Spec.ConfigurationSpec, service.Spec.ConfigurationSpec) &&
		equality.Semantic.DeepEqual(desiredService.ObjectMeta.Labels, service.ObjectMeta.Labels)
}