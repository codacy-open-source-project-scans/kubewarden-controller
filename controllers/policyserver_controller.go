/*
Copyright 2021.

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
	"time"

	"github.com/go-logr/logr"
	"github.com/kubewarden/kubewarden-controller/internal/pkg/admission"
	"github.com/kubewarden/kubewarden-controller/internal/pkg/constants"
	policiesv1 "github.com/kubewarden/kubewarden-controller/pkg/apis/policies/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// PolicyServerReconciler reconciles a PolicyServer object
type PolicyServerReconciler struct {
	client.Client
	Log        logr.Logger
	Scheme     *runtime.Scheme
	Reconciler admission.Reconciler
}

// Warning: this controller is deployed by a helm chart which has its own
// templated RBAC rules. The rules are kept in sync between what is generated by
// `make manifests` and the helm chart by hand.
//
// We need access to these resources only inside of the namespace where the
// controller is deployed. Here we assume it's being deployed inside of the
// `kubewarden` namespace, this has to be parametrized in the helm chart
//+kubebuilder:rbac:namespace=kubewarden,groups=policies.kubewarden.io,resources=policyservers,verbs=get;list;watch;delete
//+kubebuilder:rbac:namespace=kubewarden,groups=policies.kubewarden.io,resources=policyservers/status,verbs=get;update;patch
//+kubebuilder:rbac:namespace=kubewarden,groups=policies.kubewarden.io,resources=policyservers/finalizers,verbs=update
//+kubebuilder:rbac:namespace=kubewarden,groups=core,resources=secrets;services;configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:namespace=kubewarden,groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:namespace=kubewarden,groups=apps,resources=replicasets,verbs=get;list;watch
//+kubebuilder:rbac:namespace=kubewarden,groups=core,resources=pods,verbs=get;list;watch

func (r *PolicyServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var policyServer policiesv1.PolicyServer
	if err := r.Get(ctx, req.NamespacedName, &policyServer); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("cannot retrieve policy server: %w", err)
	}

	policies, err := r.Reconciler.GetPolicies(ctx, &policyServer)
	if err != nil {
		return ctrl.Result{}, errors.Join(errors.New("could not get policies"), err)
	}

	if policyServer.ObjectMeta.DeletionTimestamp != nil {
		return r.reconcileDeletion(ctx, &policyServer, policies)
	}

	reconcileResult, reconcileErr := r.reconcile(ctx, &policyServer, policies)

	if err := r.Client.Status().Update(ctx, &policyServer); err != nil {
		return ctrl.Result{}, fmt.Errorf("update policy server status error: %w", err)
	}

	return reconcileResult, reconcileErr
}

func (r *PolicyServerReconciler) reconcile(ctx context.Context, policyServer *policiesv1.PolicyServer, policies []policiesv1.Policy) (ctrl.Result, error) {
	if err := r.Reconciler.Reconcile(ctx, policyServer, policies); err != nil {
		if admission.IsPolicyServerNotReady(err) {
			r.Log.Info("delaying policy registration since policy server is not yet ready")
			return ctrl.Result{
				Requeue:      true,
				RequeueAfter: time.Second * 5,
			}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reconciliation error: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *PolicyServerReconciler) reconcileDeletion(ctx context.Context, policyServer *policiesv1.PolicyServer, policies []policiesv1.Policy) (ctrl.Result, error) {
	if len(policies) != 0 {
		// There are still policies scheduled on the PolicyServer, we have to
		// wait for them to be completely removed before going further with the cleanup
		return r.deletePoliciesAndRequeue(ctx, policyServer, policies)
	}

	if err := r.Reconciler.ReconcileDeletion(ctx, policyServer); err != nil {
		return ctrl.Result{}, errors.Join(errors.New("could not reconcile policy server deletion"), err)
	}

	controllerutil.RemoveFinalizer(policyServer, constants.KubewardenFinalizer)
	if err := r.Update(ctx, policyServer); err != nil {
		// return if PolicyServer was previously deleted
		if apierrors.IsConflict(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("cannot update policy server: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *PolicyServerReconciler) deletePoliciesAndRequeue(ctx context.Context, policyServer *policiesv1.PolicyServer, policies []policiesv1.Policy) (ctrl.Result, error) {
	deleteError := make([]error, 0)
	for _, policy := range policies {
		if policy.GetDeletionTimestamp() != nil {
			// the policy is already pending deletion
			continue
		}
		if err := r.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
			deleteError = append(deleteError, err)
		}
	}

	if len(deleteError) != 0 {
		r.Log.Error(errors.Join(deleteError...), "could not remove all policies bound to policy server", "policy-server", policyServer.Name)
		return ctrl.Result{}, fmt.Errorf("could not remove all policies bound to policy server %s", policyServer.Name)
	}

	return ctrl.Result{Requeue: true}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PolicyServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := mgr.GetFieldIndexer().IndexField(context.Background(), &policiesv1.ClusterAdmissionPolicy{}, constants.PolicyServerIndexKey, func(object client.Object) []string {
		policy, ok := object.(*policiesv1.ClusterAdmissionPolicy)
		if !ok {
			r.Log.Error(nil, "object is not type of ClusterAdmissionPolicy: %#v", "policy", policy)
			return []string{}
		}
		return []string{policy.Spec.PolicyServer}
	})
	if err != nil {
		return fmt.Errorf("failed enrolling controller with manager: %w", err)
	}
	err = mgr.GetFieldIndexer().IndexField(context.Background(), &policiesv1.AdmissionPolicy{}, constants.PolicyServerIndexKey, func(object client.Object) []string {
		policy, ok := object.(*policiesv1.AdmissionPolicy)
		if !ok {
			r.Log.Error(nil, "object is not type of AdmissionPolicy: %#v", "policy", policy)
			return []string{}
		}
		return []string{policy.Spec.PolicyServer}
	})
	if err != nil {
		return fmt.Errorf("failed enrolling controller with manager: %w", err)
	}
	err = ctrl.NewControllerManagedBy(mgr).
		For(&policiesv1.PolicyServer{}).
		Watches(&policiesv1.AdmissionPolicy{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
			// The watch will trigger twice per object change; once with the old
			// object, and once the new object. We need to be mindful when doing
			// Updates since they will invalidate the newever versions of the
			// object.
			policy, ok := object.(*policiesv1.AdmissionPolicy)
			if !ok {
				r.Log.Info("object is not type of AdmissionPolicy: %+v", "policy", policy)
				return []ctrl.Request{}
			}

			return []ctrl.Request{
				{
					NamespacedName: client.ObjectKey{
						Name: policy.Spec.PolicyServer,
					},
				},
			}
		})).
		Watches(&policiesv1.ClusterAdmissionPolicy{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
			// The watch will trigger twice per object change; once with the old
			// object, and once the new object. We need to be mindful when doing
			// Updates since they will invalidate the newever versions of the
			// object.
			policy, ok := object.(*policiesv1.ClusterAdmissionPolicy)
			if !ok {
				r.Log.Info("object is not type of ClusterAdmissionPolicy: %+v", "policy", policy)
				return []ctrl.Request{}
			}

			return []ctrl.Request{
				{
					NamespacedName: client.ObjectKey{
						Name: policy.Spec.PolicyServer,
					},
				},
			}
		})).
		Complete(r)

	if err != nil {
		return errors.Join(errors.New("failed enrolling controller with manager"), err)
	}
	return nil
}