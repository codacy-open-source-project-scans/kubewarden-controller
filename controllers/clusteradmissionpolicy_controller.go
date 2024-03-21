/*
Copyright 2022.

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

package controllers

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/kubewarden/kubewarden-controller/internal/pkg/admission"
	"github.com/kubewarden/kubewarden-controller/internal/pkg/constants"
	"github.com/kubewarden/kubewarden-controller/internal/pkg/naming"
	policiesv1 "github.com/kubewarden/kubewarden-controller/pkg/apis/policies/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Warning: this controller is deployed by a helm chart which has its own
// templated RBAC rules. The rules are kept in sync between what is generated by
// `make manifests` and the helm chart by hand.
//
// We need access to these resources inside of all the namespaces -> a ClusterRole
// is needed
//+kubebuilder:rbac:groups=policies.kubewarden.io,resources=clusteradmissionpolicies,verbs=get;list;watch;delete
//+kubebuilder:rbac:groups=policies.kubewarden.io,resources=clusteradmissionpolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=policies.kubewarden.io,resources=clusteradmissionpolicies/finalizers,verbs=update
//
// We need access to these resources only inside of the namespace where the
// controller is deployed. Here we assume it's being deployed inside of the
// `kubewarden` namespace, this has to be parametrized in the helm chart
//+kubebuilder:rbac:namespace=kubewarden,groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:namespace=kubewarden,groups=apps,resources=replicasets;deployments,verbs=get;list;watch

// ClusterAdmissionPolicyReconciler reconciles a ClusterAdmissionPolicy object
type ClusterAdmissionPolicyReconciler struct {
	client.Client
	Log        logr.Logger
	Scheme     *runtime.Scheme
	Reconciler admission.Reconciler
}

// Reconcile reconciles admission policies
func (r *ClusterAdmissionPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var clusterAdmissionPolicy policiesv1.ClusterAdmissionPolicy
	if err := r.Reconciler.APIReader.Get(ctx, req.NamespacedName, &clusterAdmissionPolicy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("cannot retrieve admission policy: %w", err)
	}

	return startReconciling(ctx, r.Reconciler.Client, r.Reconciler, &clusterAdmissionPolicy)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterAdmissionPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&policiesv1.ClusterAdmissionPolicy{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.findClusterAdmissionPoliciesForPod),
		).
		// Despite this policy server watch is not strictly necessary, we
		// include it for the integration tests, so that we identify
		// policy server creations even when the controller-manager is not
		// present (so no pods end up being created)
		Watches(
			&policiesv1.PolicyServer{},
			handler.EnqueueRequestsFromMapFunc(r.findClusterAdmissionPoliciesForPolicyServer),
		).
		Watches(
			&admissionregistrationv1.ValidatingWebhookConfiguration{},
			handler.EnqueueRequestsFromMapFunc(r.findClusterAdmissionPolicyForWebhookConfiguration),
		).
		Watches(
			&admissionregistrationv1.MutatingWebhookConfiguration{},
			handler.EnqueueRequestsFromMapFunc(r.findClusterAdmissionPolicyForWebhookConfiguration),
		).
		Complete(r)
	if err != nil {
		return errors.Join(errors.New("failed enrolling controller with manager"), err)
	}
	return nil
}

func (r *ClusterAdmissionPolicyReconciler) findClusterAdmissionPoliciesForConfigMap(object client.Object) []reconcile.Request {
	configMap, ok := object.(*corev1.ConfigMap)
	if !ok {
		return []reconcile.Request{}
	}
	policyMap, err := getPolicyMapFromConfigMap(configMap)
	if err != nil {
		return []reconcile.Request{}
	}
	return policyMap.ToClusterAdmissionPolicyReconcileRequests()
}

func (r *ClusterAdmissionPolicyReconciler) findClusterAdmissionPoliciesForPod(ctx context.Context, object client.Object) []reconcile.Request {
	pod, ok := object.(*corev1.Pod)
	if !ok {
		return []reconcile.Request{}
	}
	policyServerName, ok := pod.Labels[constants.PolicyServerLabelKey]
	if !ok {
		return []reconcile.Request{}
	}
	policyServerDeploymentName := naming.PolicyServerDeploymentNameForPolicyServerName(policyServerName)
	configMap := corev1.ConfigMap{}
	err := r.Reconciler.APIReader.Get(ctx, client.ObjectKey{
		Namespace: pod.ObjectMeta.Namespace,
		Name:      policyServerDeploymentName, // As the deployment name matches the name of the ConfigMap
	}, &configMap)
	if err != nil {
		return []reconcile.Request{}
	}
	return r.findClusterAdmissionPoliciesForConfigMap(&configMap)
}

func (r *ClusterAdmissionPolicyReconciler) findClusterAdmissionPoliciesForPolicyServer(ctx context.Context, object client.Object) []reconcile.Request {
	policyServer, ok := object.(*policiesv1.PolicyServer)
	if !ok {
		return []reconcile.Request{}
	}
	policyServerDeploymentName := naming.PolicyServerDeploymentNameForPolicyServerName(policyServer.Name)
	configMap := corev1.ConfigMap{}
	err := r.Reconciler.APIReader.Get(ctx, client.ObjectKey{
		Namespace: r.Reconciler.DeploymentsNamespace,
		Name:      policyServerDeploymentName, // As the deployment name matches the name of the ConfigMap
	}, &configMap)
	if err != nil {
		return []reconcile.Request{}
	}
	return r.findClusterAdmissionPoliciesForConfigMap(&configMap)
}

func (r *ClusterAdmissionPolicyReconciler) findClusterAdmissionPolicyForWebhookConfiguration(_ context.Context, webhookConfiguration client.Object) []reconcile.Request {
	if _, found := webhookConfiguration.GetLabels()["kubewarden"]; !found {
		return []reconcile.Request{}
	}

	policyScope, found := webhookConfiguration.GetLabels()[constants.WebhookConfigurationPolicyScopeLabelKey]
	if !found {
		r.Log.Info("Found a webhook configuration without a scope label, reconciling it", "name", webhookConfiguration.GetName())
		return []reconcile.Request{}
	}

	// Filter out AdmissionPolicies
	if policyScope != "cluster" {
		return []reconcile.Request{}
	}

	policyName, found := webhookConfiguration.GetAnnotations()[constants.WebhookConfigurationPolicyNameAnnotationKey]
	if !found {
		r.Log.Info("Found a webhook configuration without a policy name annotation, reconciling it", "name", webhookConfiguration.GetName())
		return []reconcile.Request{}
	}

	return []reconcile.Request{
		{
			NamespacedName: client.ObjectKey{
				Name: policyName,
			},
		},
	}
}
