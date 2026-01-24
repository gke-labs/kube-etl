package syncer

import (
	"context"
	"fmt"
	krmv1alpha1 "github.com/gke-labs/kube-etl/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	"path/filepath"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"testing"
	"time"
)

func TestSyncerPrune(t *testing.T) {
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
	syncerName := "test-syncer-prune"
	orphanCmName := "orphan-cm"

	// Generate kubeconfig from envtest Dest config
	destKubeconfigContent, err := createKubeconfig(cfgDest)
	require.NoError(t, err)

	// Create Secret in Source with Dest Kubeconfig
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
		Data:       map[string][]byte{"kubeconfig": destKubeconfigContent},
	}
	require.NoError(t, k8sClientSource.Create(ctx, secret))

	// Create Orphan ConfigMap in Dest (Simulate resource deleted while syncer was down)
	orphanCm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: orphanCmName, Namespace: ns},
		Data:       map[string]string{"key": "should-be-deleted"},
	}
	require.NoError(t, k8sClientDest.Create(ctx, orphanCm))

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

	// Verify Orphan ConfigMap Deletion in Dest
	err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		destCm := &corev1.ConfigMap{}
		err := k8sClientDest.Get(ctx, types.NamespacedName{Name: orphanCmName, Namespace: ns}, destCm)
		if client.IgnoreNotFound(err) == nil && err != nil {
			return true, nil // Deleted
		}
		return false, nil
	})
	assert.NoError(t, err, "Orphan ConfigMap should be deleted from dest")
}
