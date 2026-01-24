package syncer

import (
	"context"
	"fmt"
	krmv1alpha1 "github.com/gke-labs/kube-etl/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/rest"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"testing"
	"time"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestSyncerSync(t *testing.T) {
	// Setup Logic
	ctrl.SetLogger(klog.NewKlogr())
	ctx, cancel := context.WithCancel(context.Background())
	// Start Source Cluster
	testEnvSource := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfgSource, err := testEnvSource.Start()
	require.NoError(t, err)

	// Start Destination Cluster
	testEnvDest := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "config", "crd")},
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
	configMapName := "test-cm"

	// Generate kubeconfig from envtest Dest config
	destKubeconfigContent, err := createKubeconfig(cfgDest)
	require.NoError(t, err)

	// Create Secret in Source with Dest Kubeconfig
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
		Data:       map[string][]byte{"kubeconfig": destKubeconfigContent},
	}
	require.NoError(t, k8sClientSource.Create(ctx, secret))

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
			Rules: []krmv1alpha1.ResourceRule{
				{
					Group: "", Version: "v1", Kind: "ConfigMap",
					Namespaces: []string{ns},
				},
			},
		},
	}
	require.NoError(t, k8sClientSource.Create(ctx, syncer))

	// Create ConfigMap in Source
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: ns},
		Data:       map[string]string{"key": "initial"},
	}
	require.NoError(t, k8sClientSource.Create(ctx, cm))

	// Verify ConfigMap Sync to Dest
	err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		destCm := &corev1.ConfigMap{}
		err := k8sClientDest.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: ns}, destCm)
		if err != nil {
			return false, nil
		}
		return destCm.Data["key"] == "initial", nil
	})
	assert.NoError(t, err, "ConfigMap should be synced to dest")

	// Update ConfigMap in Source
	require.NoError(t, k8sClientSource.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: ns}, cm))
	cm.Data["key"] = "updated"
	require.NoError(t, k8sClientSource.Update(ctx, cm))

	// Verify ConfigMap Update in Dest
	err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		destCm := &corev1.ConfigMap{}
		err := k8sClientDest.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: ns}, destCm)
		if err != nil {
			return false, nil
		}
		return destCm.Data["key"] == "updated", nil
	})
	assert.NoError(t, err, "ConfigMap update should be synced to dest")

	// Delete ConfigMap in Source
	require.NoError(t, k8sClientSource.Delete(ctx, cm))

	// Verify Deletion in Dest
	err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		destCm := &corev1.ConfigMap{}
		err := k8sClientDest.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: ns}, destCm)
		return client.IgnoreNotFound(err) == nil && err != nil, nil
	})
	assert.NoError(t, err, "ConfigMap should be deleted from dest")
}

func createKubeconfig(cfg *rest.Config) ([]byte, error) {
	clusterName := "default-cluster"
	userName := "default-user"
	contextName := "default-context"

	clusters := make(map[string]*clientcmdapi.Cluster)
	clusters[clusterName] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
		InsecureSkipTLSVerify:    cfg.Insecure,
	}

	authInfos := make(map[string]*clientcmdapi.AuthInfo)
	authInfos[userName] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
		Token:                 cfg.BearerToken,
		Username:              cfg.Username,
		Password:              cfg.Password,
	}

	contexts := make(map[string]*clientcmdapi.Context)
	contexts[contextName] = &clientcmdapi.Context{
		Cluster:  clusterName,
		AuthInfo: userName,
	}

	config := clientcmdapi.Config{
		Kind:           "Config",
		APIVersion:     "v1",
		Clusters:       clusters,
		AuthInfos:      authInfos,
		Contexts:       contexts,
		CurrentContext: contextName,
	}

	return clientcmd.Write(config)
}
