// Copyright 2026 Google LLC
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

package syncer

import (
	"context"
	"fmt"
	krmv1alpha1 "github.com/gke-labs/kube-etl/syncer/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// KRMSyncerReconciler reconciles a KRMSyncer object
type KRMSyncerReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Manager ctrl.Manager

	// WatchedGVKs tracks which GVKs are already being watched
	WatchedGVKs map[schema.GroupVersionKind]bool
	mu          sync.RWMutex
}

//+kubebuilder:rbac:groups=syncer.gkelabs.io,resources=krmsyncers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=syncer.gkelabs.io,resources=krmsyncers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

func (r *KRMSyncerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the KRMSyncer instance
	var krmSyncer krmv1alpha1.KRMSyncer
	if err := r.Get(ctx, req.NamespacedName, &krmSyncer); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Update Status if needed
	defer func() {
		if err := r.Status().Update(ctx, &krmSyncer); err != nil {
			logger.Error(err, "Failed to update KRMSyncer status")
		}
	}()

	if krmSyncer.Spec.Suspend {
		meta.SetStatusCondition(&krmSyncer.Status.Conditions, metav1.Condition{
			Type:    "Suspended",
			Status:  metav1.ConditionTrue,
			Reason:  "SuspendedBySpec",
			Message: "Controller is suspended",
		})
		// Wait for the source cluster to push resources to us.
		logger.Info("Sync suspended. Waiting for updates from active cluster...")
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	meta.SetStatusCondition(&krmSyncer.Status.Conditions, metav1.Condition{
		Type:    "Active",
		Status:  metav1.ConditionFalse,
		Reason:  "Active",
		Message: "Controller is active",
	})
	logger.Info("Syncing updates from active cluster...")
	return r.reconcile(ctx, &krmSyncer)
}

func (r *KRMSyncerReconciler) reconcile(ctx context.Context, syncer *krmv1alpha1.KRMSyncer) (ctrl.Result, error) {
	// logger := log.FromContext(ctx)

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, rule := range syncer.Spec.Rules {
		gvk := schema.GroupVersionKind{
			Group:   rule.Group,
			Version: rule.Version,
			Kind:    rule.Kind,
		}

		if !r.WatchedGVKs[gvk] {
			if err := r.startWatcher(ctx, gvk); err != nil {
				// log.Log.Error(err, "Failed to start watcher for GVK", "gvk", gvk)
				continue
			}

			r.WatchedGVKs[gvk] = true
		}
	}

	return ctrl.Result{}, nil
}

func (r *KRMSyncerReconciler) startWatcher(ctx context.Context, gvk schema.GroupVersionKind) error {
	dr := &DynamicResourceReconciler{
		Client: r.Client,
		GVK:    gvk,
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)

	return ctrl.NewControllerManagedBy(r.Manager).
		For(u).
		Named(fmt.Sprintf("dynamic-watcher-%s-%s-%s", gvk.Group, gvk.Version, gvk.Kind)).
		Complete(dr)
}

func (r *KRMSyncerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.WatchedGVKs = make(map[schema.GroupVersionKind]bool)
	return ctrl.NewControllerManagedBy(mgr).
		For(&krmv1alpha1.KRMSyncer{}).
		Complete(r)
}

// DynamicResourceReconciler handles events for dynamically watched resources
type DynamicResourceReconciler struct {
	client.Client
	GVK schema.GroupVersionKind
}

func (r *DynamicResourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the resource
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(r.GVK)
	err := r.Get(ctx, req.NamespacedName, u)
	isDeleted := false
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			isDeleted = true
		} else {
			return ctrl.Result{}, err
		}
	}

	// Fetch all Syncers to find matching rules (Active ones)
	var syncers krmv1alpha1.KRMSyncerList
	if err := r.List(ctx, &syncers); err != nil {
		return ctrl.Result{}, err
	}

	for _, syncer := range syncers.Items {
		if syncer.Spec.Suspend {
			continue
		}

		for _, rule := range syncer.Spec.Rules {
			// Check GVK match
			if rule.Group != r.GVK.Group || rule.Version != r.GVK.Version || rule.Kind != r.GVK.Kind {
				continue
			}

			// Check Namespace match
			if len(rule.Namespaces) > 0 {
				matched := false
				for _, ns := range rule.Namespaces {
					if ns == "*" || ns == req.Namespace {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}

			// Get Remote Client
			remoteClient, err := r.getRemoteClient(ctx, &syncer)
			if err != nil {
				logger.Error(err, "Failed to get remote client")
				continue
			}

			// Handle Deletion
			if isDeleted {
				toDelete := &unstructured.Unstructured{}
				toDelete.SetGroupVersionKind(r.GVK)
				toDelete.SetName(req.Name)
				toDelete.SetNamespace(req.Namespace)

				logger.Info("Deleting resource on remote cluster", "name", req.Name, "namespace", req.Namespace)
				if err := remoteClient.Delete(ctx, toDelete); err != nil {
					if !errors.IsNotFound(err) {
						logger.Error(err, "Failed to delete resource on remote cluster")
					}
				}
				continue
			}

			// Sync to Remote Cluster
			logger.Info("Syncing resource to remote cluster", "name", u.GetName(), "namespace", u.GetNamespace())
			remoteObj := u.DeepCopy()
			// Prepare object for remote: clear ResourceVersion, UID, etc.
			// Example error: "resourceVersion should not be set on objects to be created".
			remoteObj.SetResourceVersion("")
			remoteObj.SetUID("")
			remoteObj.SetGeneration(0)
			// TODO: filter status, managedFields?

			if err := r.applyToRemote(ctx, remoteClient, remoteObj); err != nil {
				logger.Error(err, "Failed to apply to remote cluster")
			} else {
				logger.Info("Successfully synced resource to remote cluster", "name", u.GetName(), "namespace", u.GetNamespace())
			}
		}
	}
	return ctrl.Result{}, nil
}

func (r *DynamicResourceReconciler) getRemoteClient(ctx context.Context, syncer *krmv1alpha1.KRMSyncer) (client.Client, error) {
	if syncer.Spec.Destination == nil || syncer.Spec.Destination.ClusterConfig == nil || syncer.Spec.Destination.ClusterConfig.KubeConfigSecretRef == nil {
		return nil, fmt.Errorf("KubeConfigSecretRef not specified")
	}

	secret := &corev1.Secret{}
	key := client.ObjectKey{
		Name:      syncer.Spec.Destination.ClusterConfig.KubeConfigSecretRef.Name,
		Namespace: syncer.Namespace, // Secret must be in the same namespace as Syncer
	}
	if err := r.Get(ctx, key, secret); err != nil {
		return nil, err
	}

	kubeconfig, ok := secret.Data["kubeconfig"]
	if !ok {
		return nil, fmt.Errorf("secret %s does not contain 'kubeconfig' key", key.Name)
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, err
	}

	return client.New(restConfig, client.Options{})
}

func (r *DynamicResourceReconciler) applyToRemote(ctx context.Context, remoteClient client.Client, obj *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(obj.GroupVersionKind())
	key := client.ObjectKeyFromObject(obj)

	if err := remoteClient.Get(ctx, key, existing); err != nil {
		if errors.IsNotFound(err) {
			return remoteClient.Create(ctx, obj)
		}
		return err
	}

	obj.SetResourceVersion(existing.GetResourceVersion())
	obj.SetUID(existing.GetUID())
	return remoteClient.Update(ctx, obj)
}
