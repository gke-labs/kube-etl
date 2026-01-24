package export

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestExport(t *testing.T) {
	// 1. Setup envtest
	testEnv := &envtest.Environment{
		DownloadBinaryAssets: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("Failed to start envtest: %v", err)
	}
	defer func() {
		if err := testEnv.Stop(); err != nil {
			t.Logf("Failed to stop envtest: %v", err)
		}
	}()

	// 2. Write kubeconfig to temp file
	kubeconfigDir, err := os.MkdirTemp("", "kube-etl-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(kubeconfigDir)

	kubeconfigPath := filepath.Join(kubeconfigDir, "kubeconfig")

	// Create a minimal kubeconfig structure from the rest.Config
	clusters := make(map[string]*api.Cluster)
	clusters["default-cluster"] = &api.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
	}

	contexts := make(map[string]*api.Context)
	contexts["default-context"] = &api.Context{
		Cluster:  "default-cluster",
		AuthInfo: "default-user",
	}

	authInfos := make(map[string]*api.AuthInfo)
	authInfos["default-user"] = &api.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
	}

	clientConfig := api.Config{
		Kind:           "Config",
		APIVersion:     "v1",
		Clusters:       clusters,
		Contexts:       contexts,
		AuthInfos:      authInfos,
		CurrentContext: "default-context",
	}

	if err := clientcmd.WriteToFile(clientConfig, kubeconfigPath); err != nil {
		t.Fatalf("Failed to write kubeconfig: %v", err)
	}

	// 3. Set KUBECONFIG
	os.Setenv("KUBECONFIG", kubeconfigPath)
	defer os.Unsetenv("KUBECONFIG")

	// 4. Run Export
	outputPath := filepath.Join(kubeconfigDir, "output.zip")
	opt := ExportOptions{
		Output: outputPath,
	}

	if err := RunExport(t.Context(), opt); err != nil {
		t.Fatalf("RunExport failed: %v", err)
	}

	// 5. Verify output
	r, err := zip.OpenReader(outputPath)
	if err != nil {
		t.Fatalf("Failed to open output zip: %v", err)
	}
	defer r.Close()

	foundNamespace := false
	for _, f := range r.File {
		// We expect at least the default namespace or kube-system
		// Path structure: <namespace>/<group>/<kind>/<name>.yaml
		// Namespace objects are cluster scoped, so they will be in _cluster namespace folder.
		if f.Name == "_cluster/core/Namespace/default.yaml" || 
           f.Name == "_cluster/core/Namespace/kube-system.yaml" {
			foundNamespace = true
			break
		}
	}

	if !foundNamespace {
		t.Errorf("Did not find expected namespace in zip file. Files found: %d", len(r.File))
        for _, f := range r.File {
            t.Logf("Found file: %s", f.Name)
        }
	}
}