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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
)

func getRemoteConfig(ctx context.Context, c client.Client, ns string, remoteConfig *krmv1alpha1.RemoteConfig) (*rest.Config, error) {
	if remoteConfig == nil || remoteConfig.ClusterConfig == nil || remoteConfig.ClusterConfig.KubeConfigSecretRef == nil {
		return nil, fmt.Errorf("KubeConfigSecretRef not specified")
	}

	secret := &corev1.Secret{}
	key := client.ObjectKey{
		Name:      remoteConfig.ClusterConfig.KubeConfigSecretRef.Name,
		Namespace: ns, // Secret must be in the same namespace as Syncer
	}
	if err := c.Get(ctx, key, secret); err != nil {
		return nil, err
	}

	kubeconfig, ok := secret.Data["kubeconfig"]
	if !ok {
		return nil, fmt.Errorf("secret %s does not contain 'kubeconfig' key", key.Name)
	}

	return clientcmd.RESTConfigFromKubeConfig(kubeconfig)
}

func getRemoteClient(ctx context.Context, c client.Client, ns string, remoteConfig *krmv1alpha1.RemoteConfig) (client.Client, error) {
	restConfig, err := getRemoteConfig(ctx, c, ns, remoteConfig)
	if err != nil {
		return nil, err
	}

	return client.New(restConfig, client.Options{})
}

func filterFields(src *unstructured.Unstructured, fields []string) (*unstructured.Unstructured, error) {
	dest := &unstructured.Unstructured{
		Object: make(map[string]interface{}),
	}
	dest.SetGroupVersionKind(src.GroupVersionKind())
	dest.SetName(src.GetName())
	dest.SetNamespace(src.GetNamespace())
	dest.SetLabels(src.GetLabels())
	dest.SetAnnotations(src.GetAnnotations())

	for _, field := range fields {
		parts := strings.Split(field, ".")
		val, found, err := unstructured.NestedFieldCopy(src.Object, parts...)
		if err != nil {
			return nil, fmt.Errorf("failed to get field %s: %v", field, err)
		}
		if found {
			if err := unstructured.SetNestedField(dest.Object, val, parts...); err != nil {
				return nil, fmt.Errorf("failed to set field %s: %v", field, err)
			}
		}
	}
	return dest, nil
}

func applyToCluster(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	patchOpts := []client.PatchOption{
		client.ForceOwnership,
		client.FieldOwner("krm-syncer"),
	}

	status, found, err := unstructured.NestedFieldCopy(obj.Object, "status")
	if err != nil {
		return fmt.Errorf("failed to copy status: %v", err)
	}

	// Use Server-Side Apply (SSA) to create or update the resource.
	if err := c.Patch(ctx, obj, client.Apply, patchOpts...); err != nil {
		return fmt.Errorf("failed to apply object: %v", err)
	}

	// If status is present, we also need to patch the status subresource.
	// For many resources, status is a subresource and is ignored by the main patch.
	if found {
		statusObj := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": obj.GetAPIVersion(),
				"kind":       obj.GetKind(),
				"metadata": map[string]interface{}{
					"name":      obj.GetName(),
					"namespace": obj.GetNamespace(),
				},
				"status": status,
			},
		}
		subResourcePatchOpts := []client.SubResourcePatchOption{
			client.ForceOwnership,
			client.FieldOwner("krm-syncer"),
		}
		if err := c.Status().Patch(ctx, statusObj, client.Apply, subResourcePatchOpts...); err != nil {
			return fmt.Errorf("failed to apply status subresource: %v", err)
		}
	}
	return nil
}

func isNamespaceMatched(namespace string, allowedNamespaces []string) bool {
	if len(allowedNamespaces) == 0 {
		return true
	}
	for _, ns := range allowedNamespaces {
		if ns == namespace {
			return true
		}
	}
	return false
}
