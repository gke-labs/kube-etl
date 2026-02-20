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

package integration

import (
	"context"
	krmv1alpha1 "github.com/gke-labs/kube-etl/syncer/api/v1alpha1"
	"github.com/gke-labs/kube-etl/syncer/controllers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io/ioutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"path/filepath"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"testing"
	"time"
)

func TestKRMSyncerIntegration(t *testing.T) {
	// 1. Setup Environments
	testScheme := scheme.Scheme
	require.NoError(t, krmv1alpha1.AddToScheme(testScheme))

	// Cluster A
	t.Log("Starting Cluster A...")
	cfgA := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "config", "crd"), filepath.Join("..", "integration", "testcrd")},
		ErrorIfCRDPathMissing: true,
	}
	configA, err := cfgA.Start()
	require.NoError(t, err)
	defer cfgA.Stop()

	k8sClientA, err := client.New(configA, client.Options{Scheme: testScheme})
	require.NoError(t, err)

	// Cluster B
	t.Log("Starting Cluster B...")
	cfgB := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "integration", "testcrd")},
		ErrorIfCRDPathMissing: true,
	}
	configB, err := cfgB.Start()
	require.NoError(t, err)
	defer cfgB.Stop()

	k8sClientB, err := client.New(configB, client.Options{Scheme: testScheme})
	require.NoError(t, err)

	// 2. Start Manager for Cluster A
	t.Log("Starting Manager on Cluster A...")
	mgr, err := ctrl.NewManager(configA, ctrl.Options{
		Scheme:  testScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	require.NoError(t, err)

	reconciler := &controllers.KRMSyncerReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Manager: mgr,
	}
	require.NoError(t, reconciler.SetupWithManager(mgr))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		err := mgr.Start(ctx)
		if err != nil {
			t.Errorf("Manager failed: %v", err)
		}
	}()

	// 3. Setup Synchronization (Kubeconfig Secret)
	t.Log("Creating Kubeconfig Secret...")
	destKubeconfig, err := createKubeconfig(configB)
	require.NoError(t, err)

	secret := &corev1.Secret{
		ObjectMeta: ctrl.ObjectMeta{
			Name:      "dest-kubeconfig",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"kubeconfig": destKubeconfig,
		},
	}
	require.NoError(t, k8sClientA.Create(ctx, secret))

	// 4. Run Test Cases
	casesDir := "../integration/cases"
	dirs, err := ioutil.ReadDir(casesDir)
	require.NoError(t, err)

	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		caseName := d.Name()
		t.Run(caseName, func(t *testing.T) {
			runTestCase(t, ctx, k8sClientA, k8sClientB, filepath.Join(casesDir, caseName))
		})
	}
}

func runTestCase(t *testing.T, ctx context.Context, clientA, clientB client.Client, caseDir string) {
	t.Logf("Running test case: %s", filepath.Base(caseDir))

	// Load Syncer
	syncerBytes, err := ioutil.ReadFile(filepath.Join(caseDir, "syncer.yaml"))
	require.NoError(t, err)
	syncer := &krmv1alpha1.KRMSyncer{}
	require.NoError(t, yaml.Unmarshal(syncerBytes, syncer))

	// Clean up Syncer at end of test
	defer func() {
		_ = clientA.Delete(ctx, syncer)
	}()

	// Start the Syncer
	require.NoError(t, clientA.Create(ctx, syncer))

	// Create Resource in A
	createBytes, err := ioutil.ReadFile("../integration/testdata/object.yaml") // Default create
	require.NoError(t, err)
	resource := &unstructured.Unstructured{}
	require.NoError(t, yaml.Unmarshal(createBytes, resource))

	// Capture status from YAML
	initialStatus, hasStatus, err := unstructured.NestedFieldCopy(resource.Object, "status")
	require.NoError(t, err)

	// Clean up Resource at end of test
	defer func() {
		_ = clientA.Delete(ctx, resource)
		_ = clientB.Delete(ctx, resource) // Cleanup on B too just in case
	}()

	t.Log("Creating Resource in Cluster A...")
	require.NoError(t, clientA.Create(ctx, resource))

	// Update Status in A
	if hasStatus {
		statusObj := resource.DeepCopy()
		err := unstructured.SetNestedField(statusObj.Object, initialStatus, "status")
		require.NoError(t, err)
		require.NoError(t, clientA.Status().Update(ctx, statusObj))
	}

	// Sleep to allow resource to sync
	time.Sleep(1 * time.Second)

	// Verify Output
	expectedBytes, err := ioutil.ReadFile(filepath.Join(caseDir, "expected.yaml"))
	// If expected.yaml is empty or missing (and we assume empty implies Not Found for suspend case)
	if err == nil && len(expectedBytes) > 0 {
		expected := &unstructured.Unstructured{}
		require.NoError(t, yaml.Unmarshal(expectedBytes, expected))

		t.Log("Verifying expected resource in Cluster B...")
		actual := &unstructured.Unstructured{}
		actual.SetGroupVersionKind(resource.GroupVersionKind())
		err := clientB.Get(ctx, client.ObjectKeyFromObject(resource), actual)
		require.NoError(t, err)

		// Compare Spec
		expectedSpec, _, _ := unstructured.NestedMap(expected.Object, "spec")
		actualSpec, _, _ := unstructured.NestedMap(actual.Object, "spec")
		assert.Equal(t, expectedSpec, actualSpec, "Spec mismatch")

		// Compare Status
		expectedStatus, _, _ := unstructured.NestedMap(expected.Object, "status")
		actualStatus, _, _ := unstructured.NestedMap(actual.Object, "status")
		assert.Equal(t, expectedStatus, actualStatus, "Status mismatch")

	} else {
		// Expect Not Found (Suspend case)
		t.Log("Verifying resource does NOT exist in Cluster B...")
		actual := &unstructured.Unstructured{}
		actual.SetGroupVersionKind(resource.GroupVersionKind())
		err := clientB.Get(ctx, client.ObjectKeyFromObject(resource), actual)
		assert.True(t, errors.IsNotFound(err), "Resource should not exist in Cluster B")
	}
}

func createKubeconfig(cfg *rest.Config) ([]byte, error) {
	config := clientcmdapi.NewConfig()
	config.Clusters["cluster"] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
		InsecureSkipTLSVerify:    cfg.Insecure,
	}
	config.AuthInfos["user"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
		Token:                 cfg.BearerToken,
		Username:              cfg.Username,
		Password:              cfg.Password,
	}
	config.Contexts["default"] = &clientcmdapi.Context{
		Cluster:  "cluster",
		AuthInfo: "user",
	}
	config.CurrentContext = "default"
	return clientcmd.Write(*config)
}
