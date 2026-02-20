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
	krmv1alpha1 "github.com/gke-labs/kube-etl/syncer/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestKRMSyncerReconciler_Reconcile(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(krmv1alpha1.AddToScheme(scheme))

	ctx := t.Context()

	t.Run("Suspended syncer updates status", func(t *testing.T) {
		syncer := &krmv1alpha1.KRMSyncer{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-syncer",
				Namespace: "default",
			},
			Spec: krmv1alpha1.KRMSyncerSpec{
				Suspend: true,
			},
		}

		cl := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(syncer).WithStatusSubresource(syncer).Build()
		r := &KRMSyncerReconciler{
			Client: cl,
			Scheme: scheme,
		}

		req := ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-syncer",
				Namespace: "default",
			},
		}

		_, err := r.Reconcile(ctx, req)
		assert.NoError(t, err)

		updatedSyncer := &krmv1alpha1.KRMSyncer{}
		err = cl.Get(ctx, req.NamespacedName, updatedSyncer)
		assert.NoError(t, err)

		cond := meta.FindStatusCondition(updatedSyncer.Status.Conditions, "Suspended")
		assert.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, "SuspendedBySpec", cond.Reason)
	})

	t.Run("Active syncer updates status", func(t *testing.T) {
		syncer := &krmv1alpha1.KRMSyncer{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "active-syncer",
				Namespace: "default",
			},
			Spec: krmv1alpha1.KRMSyncerSpec{
				Suspend: false,
			},
		}

		cl := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(syncer).WithStatusSubresource(syncer).Build()
		r := &KRMSyncerReconciler{
			Client:      cl,
			Scheme:      scheme,
			WatchedGVKs: make(map[schema.GroupVersionKind]bool),
		}

		req := ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      "active-syncer",
				Namespace: "default",
			},
		}

		// Reconcile might fail because r.Manager is nil when startWatcher is called
		// but let's see how it behaves.
		_, _ = r.Reconcile(ctx, req)

		updatedSyncer := &krmv1alpha1.KRMSyncer{}
		err := cl.Get(ctx, req.NamespacedName, updatedSyncer)
		assert.NoError(t, err)

		cond := meta.FindStatusCondition(updatedSyncer.Status.Conditions, "Active")
		assert.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
	})
}

func TestFilterFields(t *testing.T) {
	dr := &DynamicResourceReconciler{
		Client: nil,
		GVK:    krmv1alpha1.GroupVersion.WithKind("KRMSyncer"),
	}

	src := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "syncer.gkelabs.io/v1alpha1",
			"kind":       "KRMSyncer",
			"metadata": map[string]interface{}{
				"name":      "test",
				"namespace": "default",
				"labels": map[string]interface{}{
					"l1": "v1",
				},
			},
			"spec": map[string]interface{}{
				"resourceID": "id1",
				"resource": map[string]interface{}{
					"ID": "id2",
				},
				"other": "val",
			},
			"status": map[string]interface{}{
				"phase": "Active",
			},
		},
	}

	// Test case 1: Sync spec.resourceID and status
	fields := []string{"spec.resourceID", "status"}
	dest, err := dr.filterFields(src, fields)
	assert.NoError(t, err)

	val, found, _ := unstructured.NestedString(dest.Object, "spec", "resourceID")
	assert.True(t, found)
	assert.Equal(t, "id1", val)

	_, found, _ = unstructured.NestedMap(dest.Object, "spec", "resource")
	assert.False(t, found)

	_, found, _ = unstructured.NestedString(dest.Object, "spec", "other")
	assert.False(t, found)

	status, found, _ := unstructured.NestedMap(dest.Object, "status")
	assert.True(t, found)
	assert.Equal(t, "Active", status["phase"])

	assert.Equal(t, "test", dest.GetName())
	assert.Equal(t, "v1", dest.GetLabels()["l1"])

	// Test case 2: Sync full "spec" with nested fields of various types
	srcFull := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name": "full-spec-test",
			},
			"spec": map[string]interface{}{
				"primitive": "string-val",
				"integer":   int64(42),
				"boolean":   true,
				"complex": map[string]interface{}{
					"field1": "val1",
					"field2": int64(2),
					"nested": map[string]interface{}{
						"deep": "deeper",
					},
				},
				"list": []interface{}{
					map[string]interface{}{"item": int64(1)},
					"simple-item",
				},
			},
		},
	}
	fieldsFull := []string{"spec"}
	destFull, err := dr.filterFields(srcFull, fieldsFull)
	assert.NoError(t, err)

	spec, found, _ := unstructured.NestedMap(destFull.Object, "spec")
	assert.True(t, found)
	assert.Equal(t, "string-val", spec["primitive"])
	assert.Equal(t, int64(42), spec["integer"])
	assert.Equal(t, true, spec["boolean"])

	complexVal, found, _ := unstructured.NestedMap(destFull.Object, "spec", "complex")
	assert.True(t, found)
	assert.Equal(t, "val1", complexVal["field1"])

	deepVal, found, _ := unstructured.NestedString(destFull.Object, "spec", "complex", "nested", "deep")
	assert.True(t, found)
	assert.Equal(t, "deeper", deepVal)

	listVal, found, _ := unstructured.NestedSlice(destFull.Object, "spec", "list")
	assert.True(t, found)
	assert.Len(t, listVal, 2)
}

func TestDynamicResourceReconciler_Reconcile(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(krmv1alpha1.AddToScheme(scheme))

	ctx := t.Context()
	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	t.Run("Syncs resource to remote cluster", func(t *testing.T) {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(gvk)
		cm.SetName("test-cm")
		cm.SetNamespace("default")
		cm.Object["data"] = map[string]interface{}{"key": "value"}

		syncer := &krmv1alpha1.KRMSyncer{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-syncer",
				Namespace: "default",
			},
			Spec: krmv1alpha1.KRMSyncerSpec{
				Rules: []krmv1alpha1.ResourceRule{
					{
						Group:      "",
						Version:    "v1",
						Kind:       "ConfigMap",
						SyncFields: []string{"data"},
					},
				},
			},
		}

		localCl := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(cm, syncer).Build()
		remoteCl := fake.NewClientBuilder().WithScheme(scheme).Build()

		dr := &DynamicResourceReconciler{
			Client: localCl,
			GVK:    gvk,
			GetRemoteClient: func(ctx context.Context, krmsyncer *krmv1alpha1.KRMSyncer) (client.Client, error) {
				return remoteCl, nil
			},
		}

		req := ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-cm",
				Namespace: "default",
			},
		}

		_, err := dr.Reconcile(ctx, req)
		assert.NoError(t, err)

		// Verify on remote
		remoteCM := &unstructured.Unstructured{}
		remoteCM.SetGroupVersionKind(gvk)
		err = remoteCl.Get(ctx, req.NamespacedName, remoteCM)
		assert.NoError(t, err)
		assert.Equal(t, "value", remoteCM.Object["data"].(map[string]interface{})["key"])
	})

	t.Run("Deletes resource on remote cluster", func(t *testing.T) {
		syncer := &krmv1alpha1.KRMSyncer{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-syncer",
				Namespace: "default",
			},
			Spec: krmv1alpha1.KRMSyncerSpec{
				Rules: []krmv1alpha1.ResourceRule{
					{
						Group:   "",
						Version: "v1",
						Kind:    "ConfigMap",
					},
				},
			},
		}

		remoteCM := &unstructured.Unstructured{}
		remoteCM.SetGroupVersionKind(gvk)
		remoteCM.SetName("test-cm")
		remoteCM.SetNamespace("default")

		localCl := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(syncer).Build()
		remoteCl := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(remoteCM).Build()

		dr := &DynamicResourceReconciler{
			Client: localCl,
			GVK:    gvk,
			GetRemoteClient: func(ctx context.Context, krmsyncer *krmv1alpha1.KRMSyncer) (client.Client, error) {
				return remoteCl, nil
			},
		}

		req := ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-cm",
				Namespace: "default",
			},
		}

		// Reconcile when resource is missing from local (deletion event)
		_, err := dr.Reconcile(ctx, req)
		assert.NoError(t, err)

		// Verify deletion on remote
		err = remoteCl.Get(ctx, req.NamespacedName, remoteCM)
		assert.True(t, errors.IsNotFound(err))
	})
}
