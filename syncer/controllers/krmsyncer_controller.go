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
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	krmv1alpha1 "github.com/gke-labs/kube-etl/api/v1alpha1"
)

// KRMSyncerReconciler reconciles a KRMSyncer object
type KRMSyncerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Cache  cache.Cache

	// Ctrl is the controller interface to add watches dynamically
	Ctrl controller.Controller

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
		logger.Info("KRMSyncer is suspended", "name", krmSyncer.Name)
		return ctrl.Result{}, nil
	}

	meta.SetStatusCondition(&krmSyncer.Status.Conditions, metav1.Condition{
		Type:    "Suspended",
		Status:  metav1.ConditionFalse,
		Reason:  "Active",
		Message: "Controller is active",
	})

	// Dynamic Watch Registration
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, rule := range krmSyncer.Spec.Rules {
		gvk := schema.GroupVersionKind{
			Group:   rule.Group,
			Version: rule.Version,
			Kind:    rule.Kind,
		}

		if !r.WatchedGVKs[gvk] {
			logger.Info("Adding watch for GVK", "gvk", gvk)

			// Create a generic object for that GVK
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(gvk)

			// Create the handler
			// We use a custom EventHandler that handles the sync
			h := &SyncHandler{
				Client: r.Client,
				GVK:    gvk,
			}

			// Call controller.Watch
			// We watch the unstructured object type
			err := r.Ctrl.Watch(source.Kind(r.Cache, u, h))
			if err != nil {
				logger.Error(err, "Failed to watch GVK", "gvk", gvk)
				// Continue or return error? For MVP, log and continue.
				continue
			}

			r.WatchedGVKs[gvk] = true
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KRMSyncerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.WatchedGVKs = make(map[schema.GroupVersionKind]bool)

	// Build the controller
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&krmv1alpha1.KRMSyncer{}).
		Build(r)

	if err != nil {
		return err
	}

	r.Ctrl = c
	return nil
}

// SyncHandler handles events for dynamically watched resources
type SyncHandler struct {
	Client client.Client
	GVK    schema.GroupVersionKind
}

func (h *SyncHandler) Create(ctx context.Context, e event.TypedCreateEvent[*unstructured.Unstructured], q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
	h.handle(ctx, e.Object)
}

func (h *SyncHandler) Update(ctx context.Context, e event.TypedUpdateEvent[*unstructured.Unstructured], q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
	h.handle(ctx, e.ObjectNew)
}

func (h *SyncHandler) Delete(ctx context.Context, e event.TypedDeleteEvent[*unstructured.Unstructured], q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
	// MVP: No delete sync mandated, but good practice. Skipping for now as per "Sync resources from Local...".
}

func (h *SyncHandler) Generic(ctx context.Context, e event.TypedGenericEvent[*unstructured.Unstructured], q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
	h.handle(ctx, e.Object)
}

func (h *SyncHandler) handle(ctx context.Context, obj client.Object) {
	logger := log.FromContext(ctx).WithValues("gvk", h.GVK, "namespace", obj.GetNamespace(), "name", obj.GetName())

	// Find KRMSyncer(s) that match this object
	krmSyncerList := &krmv1alpha1.KRMSyncerList{}
	if err := h.Client.List(ctx, krmSyncerList); err != nil {
		logger.Error(err, "Failed to list KRMSyncers")
		return
	}

	for _, syncer := range krmSyncerList.Items {
		if syncer.Spec.Suspend {
			continue
		}

		matches := false
		for _, rule := range syncer.Spec.Rules {
			if rule.Group == h.GVK.Group && rule.Version == h.GVK.Version && rule.Kind == h.GVK.Kind {
				// Check Namespace filter
				if len(rule.Namespaces) > 0 {
					nsMatch := false
					for _, ns := range rule.Namespaces {
						if ns == obj.GetNamespace() {
							nsMatch = true
							break
						}
					}
					if !nsMatch {
						continue
					}
				}
				matches = true
				break
			}
		}

		if matches {
			// Perform Sync
			err := h.syncResource(ctx, &syncer, obj)
			if err != nil {
				logger.Error(err, "Failed to sync resource", "syncer", syncer.Name)
			} else {
				logger.Info("Successfully synced resource", "syncer", syncer.Name)
			}
		}
	}
}

func (h *SyncHandler) syncResource(ctx context.Context, syncer *krmv1alpha1.KRMSyncer, sourceObj client.Object) error {
	// Get Destination Config
	if syncer.Spec.Destination.KubeConfigSecretRef == nil {
		return fmt.Errorf("destination kubeconfig secret ref is nil")
	}

	secretRef := syncer.Spec.Destination.KubeConfigSecretRef
	secret := &corev1.Secret{}
	err := h.Client.Get(ctx, client.ObjectKey{Namespace: secretRef.Namespace, Name: secretRef.Name}, secret)
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig secret: %w", err)
	}

	kubeConfigData, ok := secret.Data["kubeconfig"] // Assuming key is 'kubeconfig'
	if !ok {
		// Fallback to standard keys like 'config' or check if there is only one key
		if v, ok := secret.Data["config"]; ok {
			kubeConfigData = v
		} else {
			return fmt.Errorf("secret does not contain 'kubeconfig' or 'config' key")
		}
	}

	// Create Dynamic Client
	clientConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeConfigData)
	if err != nil {
		return fmt.Errorf("failed to create rest config from secret: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(clientConfig)
	if err != nil {
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}

	// Prepare Source Object
	uObj, ok := sourceObj.(*unstructured.Unstructured)
	if !ok {
		// Convert if it's a typed object (though we watch unstructured, so it should be unstructured)
		// But client.Object interface implementation might be typed?
		// Since we used u.SetGroupVersionKind(gvk) in Watch, it should come as Unstructured if using dynamic cache or unstructured scheme?
		// Wait, we passed `source.Kind(r.Client.Cache(), u)`. The cache usually returns what's in the scheme.
		// If the Scheme has typed objects, it returns typed.
		// `r.Client` uses the manager's scheme.
		// `r.Scheme` (manager's scheme) likely has core types registered.

		// If we receive a typed object, we need to convert to Unstructured to strip fields easily.
		uObj = &unstructured.Unstructured{}

		// Use runtime converter
		unc, err := runtime.DefaultUnstructuredConverter.ToUnstructured(sourceObj)
		if err != nil {
			return fmt.Errorf("failed to convert to unstructured: %w", err)
		}
		uObj.SetUnstructuredContent(unc)
		uObj.SetGroupVersionKind(sourceObj.GetObjectKind().GroupVersionKind())
	}

	// Sanitize
	targetObj := uObj.DeepCopy()
	targetObj.SetUID("")
	targetObj.SetResourceVersion("")
	targetObj.SetGeneration(0)
	targetObj.SetOwnerReferences(nil)
	targetObj.SetManagedFields(nil)

	if syncer.Spec.Destination.Namespace != "" {
		targetObj.SetNamespace(syncer.Spec.Destination.Namespace)
	}

	// Apply to Destination
	gvr := schema.GroupVersionResource{
		Group:    h.GVK.Group,
		Version:  h.GVK.Version,
		Resource: getResourceName(h.GVK.Kind), // Fallback
	}

	// Use the Source Cluster's RESTMapper to find the GVR.
	// We assume Source and Destination have same API definitions for the watched resources.
	mapping, err := h.Client.RESTMapper().RESTMapping(h.GVK.GroupKind(), h.GVK.Version)
	if err != nil {
		return fmt.Errorf("failed to get REST mapping: %w", err)
	}
	gvr = mapping.Resource

	// Sync: Get Source Object -> Sanitize -> Apply to Destination.
	_, err = dynClient.Resource(gvr).Namespace(targetObj.GetNamespace()).Apply(ctx, targetObj.GetName(), targetObj, metav1.ApplyOptions{FieldManager: "krmsyncer", Force: true})

	if err != nil {
		return fmt.Errorf("failed to apply resource: %w", err)
	}

	// Status Sync: Explicitly sync the status subresource if the object has one.
	if _, ok := targetObj.Object["status"]; ok {
		_, err = dynClient.Resource(gvr).Namespace(targetObj.GetNamespace()).Apply(ctx, targetObj.GetName(), targetObj, metav1.ApplyOptions{FieldManager: "krmsyncer", Force: true}, "status")
		if err != nil {
			// We log and return error, but it might fail if status subresource is not enabled.
			// For MVP we assume if status is present, we should sync it.
			return fmt.Errorf("failed to apply status: %w", err)
		}
	}

	return nil
}

func getResourceName(kind string) string {
	return strings.ToLower(kind) + "s"
}
