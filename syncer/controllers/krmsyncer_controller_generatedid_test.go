package controllers

import (
	"context"
	"fmt"
	krmv1alpha1 "github.com/gke-labs/kube-etl/syncer/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"testing"
	"time"

	"k8s.io/klog/v2"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestSyncerSyncResourceWithGeneratedID(t *testing.T) {
	// Setup Logic
	ctrl.SetLogger(klog.NewKlogr())
	ctx, cancel := context.WithCancel(context.Background())
	// Start Source Cluster
	testEnvSource := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "config", "crd"),
			filepath.Join("..", "test", "crd"),
		},
		ErrorIfCRDPathMissing: true,
	}
	cfgSource, err := testEnvSource.Start()
	require.NoError(t, err)

	// Start Destination Cluster
	testEnvDest := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "config", "crd"),
			filepath.Join("..", "test", "crd"),
		},
		ErrorIfCRDPathMissing: true,
	}
	cfgDest, err := testEnvDest.Start()
	require.NoError(t, err)

	defer func() {
		cancel()
		// give the manager a moment to stop
		time.Sleep(100 * time.Millisecond)
		require.NoError(t, testEnvSource.Stop())
		require.NoError(t, testEnvDest.Stop())
	}()

	// Register Scheme
	require.NoError(t, krmv1alpha1.AddToScheme(scheme.Scheme))

	// Create Clients
	k8sClientSource, err := client.New(cfgSource, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)
	k8sClientDest, err := client.New(cfgDest, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)

	// Start Manager in Source Cluster
	mgr, err := ctrl.NewManager(cfgSource, ctrl.Options{
		// Scheme: scheme.Scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
	})
	require.NoError(t, err, "failed to create manager")

	err = (&KRMSyncerReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Manager: mgr,
	}).SetupWithManager(mgr)
	require.NoError(t, err, "failed to setup controller")

	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Printf("manager failed: %v\n", err)
		}
	}()

	// Test Logic
	ns := "default"
	secretName := "dest-kubeconfig"
	syncerName := "test-syncer"

	tests := []struct {
		name         string
		gvk          schema.GroupVersionKind
		resourceName string
		resourceID   string
		status       map[string]interface{}
	}{
		{
			name: "ResourceManagedByDirectController",
			gvk: schema.GroupVersionKind{
				Group:   "e2e.gkelabs.io",
				Version: "v1alpha1",
				Kind:    "ResourceWithServiceGeneratedID",
			},
			resourceName: "test-resource-direct",
			resourceID:   "12345",
			status: map[string]interface{}{
				"externalRef": "projects/my-project/resources/12345",
			},
		},
		{
			name: "ResourceManagedByLegacyController",
			gvk: schema.GroupVersionKind{
				Group:   "e2e.gkelabs.io",
				Version: "v1alpha1",
				Kind:    "ResourceWithServiceGeneratedID",
			},
			resourceName: "test-resource-legacy",
			resourceID:   "67890",
		},
	}

	// Generate kubeconfig from envtest Dest config
	destKubeconfigContent, err := createKubeconfig(cfgDest)
	require.NoError(t, err)

	// Create Secret in Source with Dest Kubeconfig
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
		Data:       map[string][]byte{"kubeconfig": destKubeconfigContent},
	}
	require.NoError(t, k8sClientSource.Create(ctx, secret))

	// Build Rules from tests
	var rules []krmv1alpha1.ResourceRule
	for _, tc := range tests {
		rules = append(rules, krmv1alpha1.ResourceRule{
			Group:      tc.gvk.Group,
			Version:    tc.gvk.Version,
			Kind:       tc.gvk.Kind,
			Namespaces: []string{ns},
		})
	}

	// Create Syncer
	syncer := &krmv1alpha1.KRMSyncer{
		ObjectMeta: metav1.ObjectMeta{Name: syncerName, Namespace: ns},
		Spec: krmv1alpha1.KRMSyncerSpec{
			Suspend: false,
			Destination: &krmv1alpha1.DestinationConfig{
				ClusterConfig: &krmv1alpha1.ClusterConfig{
					KubeConfigSecretRef: &corev1.SecretReference{Name: secretName, Namespace: ns},
				},
			},
			Rules: rules,
		},
	}
	require.NoError(t, k8sClientSource.Create(ctx, syncer))

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create Resource in Source
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(tc.gvk)
			u.SetName(tc.resourceName)
			u.SetNamespace(ns)
			require.NoError(t, k8sClientSource.Create(ctx, u))

			if tc.status == nil {
				require.NoError(t, k8sClientSource.Get(ctx, types.NamespacedName{Name: tc.resourceName, Namespace: ns}, u))
				u.Object["spec"] = map[string]interface{}{
					"resourceID": tc.resourceID,
				}
				require.NoError(t, k8sClientSource.Update(ctx, u))
			} else {
				// Update Status
				// We need to get the object back to have the latest ResourceVersion before updating status
				require.NoError(t, k8sClientSource.Get(ctx, types.NamespacedName{Name: tc.resourceName, Namespace: ns}, u))
				u.Object["status"] = tc.status
				require.NoError(t, k8sClientSource.Status().Update(ctx, u))
			}

			// Verify Sync to Dest and check spec.resourceID
			err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
				destU := &unstructured.Unstructured{}
				destU.SetGroupVersionKind(tc.gvk)
				err := k8sClientDest.Get(ctx, types.NamespacedName{Name: tc.resourceName, Namespace: ns}, destU)
				if err != nil {
					return false, nil
				}

				val, _, err := unstructured.NestedString(destU.Object, "spec", "resourceID")
				if err != nil {
					return false, err
				}

				return val == tc.resourceID, nil
			})
			assert.NoError(t, err, "%s should be synced to dest with correct resourceID", tc.name)
		})
	}
}
