package controllers

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	krmv1alpha1 "github.com/gke-labs/kube-etl/api/v1alpha1"
)

var _ = Describe("KRMSyncer Controller", func() {
	const timeout = time.Second * 10
	const interval = time.Millisecond * 250

	Context("When creating a KRMSyncer", func() {
		It("Should start watching and sync resources", func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// 1. Setup Manager
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{
				Scheme:  scheme,
				Metrics: metricsserver.Options{BindAddress: "0"}, // Disable metrics
			})
			Expect(err).ToNot(HaveOccurred())

			reconciler := &KRMSyncerReconciler{
				Client: mgr.GetClient(),
				Scheme: mgr.GetScheme(),
				Cache:  mgr.GetCache(),
			}
			err = reconciler.SetupWithManager(mgr)
			Expect(err).ToNot(HaveOccurred())

			go func() {
				defer GinkgoRecover()
				err = mgr.Start(ctx)
				Expect(err).ToNot(HaveOccurred())
			}()

			// 2. Create KubeConfig Secret
			kubeConfigBytes, err := createKubeConfig(cfg)
			Expect(err).ToNot(HaveOccurred())

			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dest-kubeconfig",
					Namespace: "default",
				},
				Data: map[string][]byte{
					"kubeconfig": kubeConfigBytes,
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			// 3. Create KRMSyncer
			krmSyncer := &krmv1alpha1.KRMSyncer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-syncer",
					Namespace: "default",
				},
				Spec: krmv1alpha1.KRMSyncerSpec{
					Destination: krmv1alpha1.DestinationConfig{
						KubeConfigSecretRef: &corev1.SecretReference{
							Name:      "dest-kubeconfig",
							Namespace: "default",
						},
					},
					Rules: []krmv1alpha1.ResourceRule{
						{
							Group:   "",
							Version: "v1",
							Kind:    "ConfigMap",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, krmSyncer)).To(Succeed())

			// 4. Create ConfigMap and Verify Sync
			// Since we sync to the same cluster, we check if the object is updated with FieldManager="krmsyncer"

			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-cm-",
					Namespace:    "default",
				},
				Data: map[string]string{"foo": "bar"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			Eventually(func() bool {
				updatedCm := &corev1.ConfigMap{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace}, updatedCm)
				if err != nil {
					return false
				}

				// Check ManagedFields
				for _, entry := range updatedCm.ManagedFields {
					if entry.Manager == "krmsyncer" {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// 5. Update KRMSyncer to add Secret Rule
			Eventually(func() error {
				var currentSyncer krmv1alpha1.KRMSyncer
				err := k8sClient.Get(ctx, types.NamespacedName{Name: krmSyncer.Name, Namespace: krmSyncer.Namespace}, &currentSyncer)
				if err != nil {
					return err
				}
				currentSyncer.Spec.Rules = append(currentSyncer.Spec.Rules, krmv1alpha1.ResourceRule{
					Group:   "",
					Version: "v1",
					Kind:    "Secret",
				})
				return k8sClient.Update(ctx, &currentSyncer)
			}, timeout, interval).Should(Succeed())

			// 6. Create Secret and Verify Sync
			secretSource := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-secret-",
					Namespace:    "default",
				},
				Data: map[string][]byte{"foo": []byte("bar")},
			}
			Expect(k8sClient.Create(ctx, secretSource)).To(Succeed())

			Eventually(func() bool {
				updatedSecret := &corev1.Secret{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: secretSource.Name, Namespace: secretSource.Namespace}, updatedSecret)
				if err != nil {
					return false
				}
				for _, entry := range updatedSecret.ManagedFields {
					if entry.Manager == "krmsyncer" {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// 7. Test Suspend Status
			Eventually(func() error {
				var currentSyncer krmv1alpha1.KRMSyncer
				err := k8sClient.Get(ctx, types.NamespacedName{Name: krmSyncer.Name, Namespace: krmSyncer.Namespace}, &currentSyncer)
				if err != nil {
					return err
				}
				currentSyncer.Spec.Suspend = true
				return k8sClient.Update(ctx, &currentSyncer)
			}, timeout, interval).Should(Succeed())

			// Wait for cache to sync
			time.Sleep(2 * time.Second)

			// Verify Status is Suspended
			Eventually(func() bool {
				var checkSyncer krmv1alpha1.KRMSyncer
				err := k8sClient.Get(ctx, types.NamespacedName{Name: krmSyncer.Name, Namespace: krmSyncer.Namespace}, &checkSyncer)
				if err != nil {
					return false
				}

				for _, cond := range checkSyncer.Status.Conditions {
					if cond.Type == "Suspended" && cond.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

		})
	})
})

func createKubeConfig(cfg *rest.Config) ([]byte, error) { // Note: cfg is *rest.Config
	clusters := make(map[string]*clientcmdapi.Cluster)
	clusters["default-cluster"] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
		InsecureSkipTLSVerify:    cfg.Insecure,
	}

	contexts := make(map[string]*clientcmdapi.Context)
	contexts["default-context"] = &clientcmdapi.Context{
		Cluster:  "default-cluster",
		AuthInfo: "default-user",
	}

	authinfos := make(map[string]*clientcmdapi.AuthInfo)
	authinfos["default-user"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
		Token:                 cfg.BearerToken,
		Username:              cfg.Username,
		Password:              cfg.Password,
	}

	clientConfig := clientcmdapi.Config{
		Kind:           "Config",
		APIVersion:     "v1",
		Clusters:       clusters,
		Contexts:       contexts,
		CurrentContext: "default-context",
		AuthInfos:      authinfos,
	}

	return clientcmd.Write(clientConfig)
}
