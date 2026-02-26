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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	toolswatch "k8s.io/client-go/tools/watch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sync"
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
	mu             sync.RWMutex
	dynamicClients map[string]dynamic.Interface
	watchers       map[string]context.CancelFunc
}

func NewWatcherManager(mgr ctrl.Manager) *WatcherManager {
	return &WatcherManager{
		Manager:        mgr,
		dynamicClients: make(map[string]dynamic.Interface),
		watchers:       make(map[string]context.CancelFunc),
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
	if _, ok := wm.watchers[watcherKey]; ok {
		return nil
	}

	// Get or Create Dynamic Client
	dynClient, ok := wm.dynamicClients[clusterID]
	if !ok {
		var err error
		if restConfig == nil {
			restConfig = wm.GetConfig()
		}
		dynClient, err = dynamic.NewForConfig(restConfig)
		if err != nil {
			return fmt.Errorf("failed to create dynamic client for %s: %v", clusterID, err)
		}
		wm.dynamicClients[clusterID] = dynClient
	}

	// Resolve GVR
	mapping, err := wm.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return fmt.Errorf("failed to resolve GVR for %v: %v", gvk, err)
	}
	gvr := mapping.Resource

	// Create Watcher
	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return dynClient.Resource(gvr).List(ctx, options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return dynClient.Resource(gvr).Watch(ctx, options)
		},
	}

	rw, err := toolswatch.NewRetryWatcher("1", lw)
	if err != nil {
		return fmt.Errorf("failed to create retry watcher for %v: %v", gvk, err)
	}

	watchCtx, cancel := context.WithCancel(context.Background())
	wm.watchers[watcherKey] = cancel

	go func() {
		defer rw.Stop()
		logger := log.FromContext(context.Background()).WithValues("gvk", gvk, "cluster", clusterID)
		logger.Info("Starting lightweight watcher")

		for {
			select {
			case <-watchCtx.Done():
				logger.Info("Stopping watcher")
				return
			case event, ok := <-rw.ResultChan():
				if !ok {
					logger.Info("Watcher channel closed, restarting")
					return
				}
				if event.Type == watch.Error {
					logger.Error(fmt.Errorf("watcher error: %v", event.Object), "Watcher error")
					continue
				}
				if event.Type == watch.Bookmark {
					continue
				}

				wm.handleEvent(watchCtx, event, clusterID, gvk)
			}
		}
	}()

	return nil
}

func (wm *WatcherManager) handleEvent(ctx context.Context, event watch.Event, sourceID string, gvk schema.GroupVersionKind) {
	logger := log.FromContext(ctx)

	u, ok := event.Object.(*unstructured.Unstructured)
	if !ok {
		logger.Error(fmt.Errorf("event object is not unstructured: %T", event.Object), "Invalid event object")
		return
	}

	// Fetch all Syncers from Local cluster to find matching rules (Active ones)
	var krmsyncers krmv1alpha1.KRMSyncerList
	if err := wm.GetClient().List(ctx, &krmsyncers); err != nil {
		logger.Error(err, "Failed to list KRMSyncers")
		return
	}

	for _, krmsyncer := range krmsyncers.Items {
		if krmsyncer.Spec.Suspend {
			continue
		}

		var targetClient client.Client
		var err error

		if krmsyncer.Spec.SyncMode == krmv1alpha1.Push && sourceID == "local" {
			// Push Mode: watch local, sync to remote
			targetClient, err = getRemoteClient(ctx, wm.GetClient(), krmsyncer.Namespace, &krmsyncer.Spec.Remote)
			if err != nil {
				logger.Error(err, "Failed to get remote client for Push")
				continue
			}
		} else if krmsyncer.Spec.SyncMode == krmv1alpha1.Pull && sourceID != "local" {
			// Pull Mode: watch remote, sync to local
			remoteConfig, err := getRemoteConfig(ctx, wm.GetClient(), krmsyncer.Namespace, &krmsyncer.Spec.Remote)
			if err != nil {
				logger.Error(err, "Failed to get remote config for Pull")
				continue
			}
			if remoteConfig.Host != sourceID {
				continue
			}
			targetClient = wm.GetClient()
		} else {
			continue
		}

		for _, rule := range krmsyncer.Spec.Rules {
			// Check GVK match
			if rule.Group != gvk.Group || rule.Version != gvk.Version || rule.Kind != gvk.Kind {
				continue
			}

			// Check Namespace match
			if !isNamespaceMatched(u.GetNamespace(), rule.Namespaces) {
				continue
			}

			// Handle Deletion
			if event.Type == watch.Deleted {
				toDelete := &unstructured.Unstructured{}
				toDelete.SetGroupVersionKind(gvk)
				toDelete.SetName(u.GetName())
				toDelete.SetNamespace(u.GetNamespace())

				logger.Info("Deleting resource on target cluster", "name", u.GetName(), "namespace", u.GetNamespace())
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
}
