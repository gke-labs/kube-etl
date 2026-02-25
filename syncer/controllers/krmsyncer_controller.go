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

package controllers

import (
	"context"
	"fmt"
	krmv1alpha1 "github.com/gke-labs/kube-etl/syncer/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// KRMSyncerReconciler reconciles a KRMSyncer object
type KRMSyncerReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Manager ctrl.Manager

	WatcherManager *WatcherManager
}

//+kubebuilder:rbac:groups=syncer.gkelabs.io,resources=krmsyncers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=syncer.gkelabs.io,resources=krmsyncers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch

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
		logger.Info("Sync suspended")
		return ctrl.Result{}, nil
	}

	meta.SetStatusCondition(&krmSyncer.Status.Conditions, metav1.Condition{
		Type:    "Active",
		Status:  metav1.ConditionTrue,
		Reason:  "Active",
		Message: "Controller is active",
	})
	return r.reconcile(ctx, &krmSyncer)
}

func (r *KRMSyncerReconciler) reconcile(ctx context.Context, krmsyncer *krmv1alpha1.KRMSyncer) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	for _, rule := range krmsyncer.Spec.Rules {
		gvk := schema.GroupVersionKind{
			Group:   rule.Group,
			Version: rule.Version,
			Kind:    rule.Kind,
		}

		if krmsyncer.Spec.SyncMode == krmv1alpha1.Push {
			// PUSH Model: Use dynamic watchers on local cluster
			if err := r.WatcherManager.EnsureWatcher(ctx, nil, gvk); err != nil {
				logger.Error(err, "Failed to ensure local watcher for GVK", "gvk", gvk)
				continue
			}
		} else {
			// PULL Model: Use dynamic watchers on remote cluster
			remoteConfig, err := getRemoteConfig(ctx, r.Client, krmsyncer.Namespace, &krmsyncer.Spec.Remote)
			if err != nil {
				logger.Error(err, "Failed to get remote config for Pull")
				return ctrl.Result{}, err
			}
			if err := r.WatcherManager.EnsureWatcher(ctx, remoteConfig, gvk); err != nil {
				logger.Error(err, "Failed to ensure remote watcher for GVK", "gvk", gvk)
				continue
			}
		}
	}

	return ctrl.Result{}, nil
}

func (r *KRMSyncerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.WatcherManager = NewWatcherManager(mgr)
	return ctrl.NewControllerManagedBy(mgr).
		For(&krmv1alpha1.KRMSyncer{}).
		Complete(r)
}

// WatcherManager manages multiple clusters and their dynamic watchers
type WatcherManager struct {
	manager.Manager
	mu       sync.RWMutex
	clusters map[string]cluster.Cluster
	watchers map[string]bool
}

func NewWatcherManager(mgr ctrl.Manager) *WatcherManager {
	return &WatcherManager{
		Manager:  mgr,
		clusters: make(map[string]cluster.Cluster),
		watchers: make(map[string]bool),
	}
}

func (wm *WatcherManager) EnsureWatcher(ctx context.Context, restConfig *rest.Config, gvk schema.GroupVersionKind) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	clusterID := "local"
	if restConfig != nil {
		clusterID = restConfig.Host
	}

	watcherKey := fmt.Sprintf("%s/%s", clusterID, gvk.String())
	if wm.watchers[watcherKey] {
		return nil
	}

	// Get or Create Cluster
	var c cluster.Cluster
	if clusterID == "local" {
		c = wm.Manager
	} else {
		var ok bool
		c, ok = wm.clusters[clusterID]
		if !ok {
			var err error
			c, err = cluster.New(restConfig, func(o *cluster.Options) {
				o.Scheme = wm.GetScheme()
			})
			if err != nil {
				return fmt.Errorf("failed to create cluster for %s: %v", clusterID, err)
			}
			if err := wm.Add(c); err != nil {
				return fmt.Errorf("failed to add cluster to manager: %v", err)
			}
			wm.clusters[clusterID] = c
		}
	}

	// Create Controller
	reconciler := &DynamicResourceReconciler{
		LocalClient:  wm.GetClient(),
		SourceClient: c.GetClient(),
		SourceID:     clusterID,
		GVK:          gvk,
	}

	ctrlName := fmt.Sprintf("dynamic-watcher-%s-%s-%s-%s", clusterID, gvk.Group, gvk.Version, gvk.Kind)
	if len(ctrlName) > 63 {
		ctrlName = ctrlName[:63]
	}

	con, err := controller.New(ctrlName, wm.Manager, controller.Options{
		Reconciler: reconciler,
	})
	if err != nil {
		return fmt.Errorf("failed to create controller %s: %v", ctrlName, err)
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)

	err = con.Watch(source.Kind(c.GetCache(), u, &handler.TypedEnqueueRequestForObject[*unstructured.Unstructured]{}))
	if err != nil {
		return fmt.Errorf("failed to watch %v on cluster %s: %v", gvk, clusterID, err)
	}

	wm.watchers[watcherKey] = true
	return nil
}

// DynamicResourceReconciler handles events for dynamically watched resources
type DynamicResourceReconciler struct {
	LocalClient  client.Client
	SourceClient client.Client
	SourceID     string // "local" or Remote Host
	GVK          schema.GroupVersionKind
}

func (r *DynamicResourceReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the resource from Source cluster
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(r.GVK)
	err := r.SourceClient.Get(ctx, req.NamespacedName, u)
	isDeleted := false
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			isDeleted = true
		} else {
			return reconcile.Result{}, err
		}
	}

	// Fetch all Syncers from Local cluster to find matching rules (Active ones)
	var krmsyncers krmv1alpha1.KRMSyncerList
	if err := r.LocalClient.List(ctx, &krmsyncers); err != nil {
		return reconcile.Result{}, err
	}

	for _, krmsyncer := range krmsyncers.Items {
		if krmsyncer.Spec.Suspend {
			continue
		}

		var targetClient client.Client

		if krmsyncer.Spec.SyncMode == krmv1alpha1.Push && r.SourceID == "local" {
			// Push Mode: watch local, sync to remote
			targetClient, err = getRemoteClient(ctx, r.LocalClient, krmsyncer.Namespace, &krmsyncer.Spec.Remote)
			if err != nil {
				logger.Error(err, "Failed to get remote client for Push")
				continue
			}
		} else if krmsyncer.Spec.SyncMode == krmv1alpha1.Pull && r.SourceID != "local" {
			// Pull Mode: watch remote, sync to local
			remoteConfig, err := getRemoteConfig(ctx, r.LocalClient, krmsyncer.Namespace, &krmsyncer.Spec.Remote)
			if err != nil {
				logger.Error(err, "Failed to get remote config for Pull")
				continue
			}
			if remoteConfig.Host != r.SourceID {
				continue
			}
			targetClient = r.LocalClient
		} else {
			continue
		}

		for _, rule := range krmsyncer.Spec.Rules {
			// Check GVK match
			if rule.Group != r.GVK.Group || rule.Version != r.GVK.Version || rule.Kind != r.GVK.Kind {
				continue
			}

			// Check Namespace match
			if !isNamespaceMatched(req.Namespace, rule.Namespaces) {
				continue
			}

			// Handle Deletion
			if isDeleted {
				toDelete := &unstructured.Unstructured{}
				toDelete.SetGroupVersionKind(r.GVK)
				toDelete.SetName(req.Name)
				toDelete.SetNamespace(req.Namespace)

				logger.Info("Deleting resource on target cluster", "name", req.Name, "namespace", req.Namespace)
				if err := targetClient.Delete(ctx, toDelete); err != nil {
					if !errors.IsNotFound(err) {
						logger.Error(err, "Failed to delete resource on target cluster")
					}
				}
				continue
			}

			// Sync to Target Cluster
			logger.Info("Syncing resource to target cluster", "name", u.GetName(), "namespace", u.GetNamespace())
			filtered, err := filterFields(u, rule.SyncFields)
			if err != nil {
				logger.Error(err, "Failed to filter fields")
				continue
			}

			// Prepare for apply
			filtered.SetResourceVersion("")
			filtered.SetUID("")
			filtered.SetGeneration(0)
			filtered.SetManagedFields(nil)

			if err := applyToCluster(ctx, targetClient, filtered); err != nil {
				logger.Error(err, "Failed to sync to target cluster")
			} else {
				logger.Info("Successfully synced resource to target cluster", "name", u.GetName(), "namespace", u.GetNamespace())
			}
		}
	}
	return reconcile.Result{}, nil
}
