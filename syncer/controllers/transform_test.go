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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"path/filepath"
	"testing"
	"time"
	"k8s.io/klog/v2"
)

func TestSyncerTransform(t *testing.T) {
	// Setup Logic
	ctrl.SetLogger(klog.NewKlogr())
	ctx, cancel := context.WithCancel(context.Background())
	
	// Start Source Cluster
	testEnvSource := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
		DownloadBinaryAssets:  true,
	}
	cfgSource, err := testEnvSource.Start()
	require.NoError(t, err)

	// Start Destination Cluster
	testEnvDest := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
		DownloadBinaryAssets:  true,
	}
	cfgDest, err := testEnvDest.Start()
	require.NoError(t, err)

	defer func() {
		cancel()
		time.Sleep(100 * time.Millisecond)
		require.NoError(t, testEnvSource.Stop())
		require.NoError(t, testEnvDest.Stop())
	}()

	require.NoError(t, krmv1alpha1.AddToScheme(scheme.Scheme))
	k8sClientSource, err := client.New(cfgSource, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)
	k8sClientDest, err := client.New(cfgDest, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)

	mgr, err := ctrl.NewManager(cfgSource, ctrl.Options{
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	require.NoError(t, err)

	err = (&KRMSyncerReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Manager: mgr,
	}).SetupWithManager(mgr)
	require.NoError(t, err)

	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Printf("manager failed: %v\n", err)
		}
	}()

	ns := "default"
	secretName := "dest-kubeconfig-transform"
	syncerName := "test-syncer-transform"
	configMapName := "test-cm-transform"

	destKubeconfigContent, err := createKubeconfig(cfgDest)
	require.NoError(t, err)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
		Data:       map[string][]byte{"kubeconfig": destKubeconfigContent},
	}
	require.NoError(t, k8sClientSource.Create(ctx, secret))

	// Create Syncer with Transform
	syncer := &krmv1alpha1.KRMSyncer{
		ObjectMeta: metav1.ObjectMeta{Name: syncerName, Namespace: ns},
		Spec: krmv1alpha1.KRMSyncerSpec{
			Destination: &krmv1alpha1.DestinationConfig{
				ClusterConfig: &krmv1alpha1.ClusterConfig{
					KubeConfigSecretRef: &corev1.SecretReference{Name: secretName, Namespace: ns},
				},
			},
			Rules: []krmv1alpha1.ResourceRule{
				{
					Group: "", Version: "v1", Kind: "ConfigMap",
					Namespaces: []string{ns},
					Transforms: []krmv1alpha1.Transformation{
						{
							Type:      "RemoveField",
							FieldPath: "data.tempKey",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClientSource.Create(ctx, syncer))

	// Create ConfigMap in Source with the field to be removed
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: ns},
		Data:       map[string]string{"keepKey": "keepValue", "tempKey": "removeValue"},
	}
	require.NoError(t, k8sClientSource.Create(ctx, cm))

	// Verify ConfigMap Sync to Dest and Transform
	err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		destCm := &corev1.ConfigMap{}
		err := k8sClientDest.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: ns}, destCm)
		if err != nil {
			return false, nil
		}
		
		// Check if "keepKey" is present
		if destCm.Data["keepKey"] != "keepValue" {
			return false, nil
		}
		
		// Check if "tempKey" is REMOVED
		if _, ok := destCm.Data["tempKey"]; ok {
			// Fail: tempKey should be removed
			return false, nil
		}
		
		return true, nil
	})
	assert.NoError(t, err, "ConfigMap should be synced to dest with tempKey removed")
}
