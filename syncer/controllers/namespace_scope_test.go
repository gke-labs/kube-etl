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
	stderrors "errors"
	"fmt"
	"testing"
	"time"

	krmv1alpha1 "github.com/gke-labs/kube-etl/syncer/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestRuleSelects(t *testing.T) {
	rule := func(namespaces ...string) krmv1alpha1.ResourceRule {
		return krmv1alpha1.ResourceRule{Version: "v1", Kind: "ConfigMap", Namespaces: namespaces}
	}
	for _, tc := range []struct {
		name      string
		rule      krmv1alpha1.ResourceRule
		namespace string // "" for a cluster-scoped object
		want      bool
	}{
		{"a listed namespace", rule("tenant", "other"), "other", true},
		{"an unlisted namespace", rule("other"), "tenant", false},
		{"no namespaces: nothing", rule(), "tenant", false},
		{"all namespaces: any namespace", rule(AllNamespaces), "other", true},
		{"all namespaces: not cluster-scoped objects", rule(AllNamespaces), "", false},
		{"listed: not cluster-scoped objects", rule("tenant"), "", false},
		{"empty name: not cluster-scoped objects", rule(""), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ruleSelects(tc.rule, tc.namespace))
		})
	}
}

func TestCheckNamespaces(t *testing.T) {
	syncer := func(namespace string, ruleNamespaces ...[]string) *krmv1alpha1.KRMSyncer {
		s := &krmv1alpha1.KRMSyncer{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: namespace}}
		for _, ns := range ruleNamespaces {
			s.Spec.Rules = append(s.Spec.Rules, krmv1alpha1.ResourceRule{Version: "v1", Kind: "ConfigMap", Namespaces: ns})
		}
		return s
	}
	for _, tc := range []struct {
		name       string
		syncer     *krmv1alpha1.KRMSyncer
		trusted    []string
		wantReason string // empty means allowed
		wantErr    string // substring of the error
	}{
		{name: "untrusted, its own namespace", syncer: syncer("tenant", []string{"tenant"})},
		{name: "no namespaces", syncer: syncer("tenant", nil),
			wantReason: reasonInvalidNamespaces, wantErr: `spec.rules[0].namespaces is required: list the namespaces to sync, such as "tenant", or "*"`},
		{name: "no namespaces in a later rule", syncer: syncer("tenant", []string{"tenant"}, []string{}),
			wantReason: reasonInvalidNamespaces, wantErr: "spec.rules[1].namespaces is required"},
		{name: "empty name", syncer: syncer("tenant", []string{"tenant", ""}),
			wantReason: reasonInvalidNamespaces, wantErr: "spec.rules[0].namespaces must not contain empty names"},
		{name: "* combined with a namespace", syncer: syncer("admin", []string{AllNamespaces, "kube-system"}), trusted: []string{"admin"},
			wantReason: reasonInvalidNamespaces, wantErr: `spec.rules[0].namespaces: "*" cannot be combined with other namespaces`},
		{name: "invalid even when trusted", syncer: syncer("admin", nil), trusted: []string{AllNamespaces},
			wantReason: reasonInvalidNamespaces, wantErr: "is required"},
		{name: "untrusted, another namespace", syncer: syncer("tenant", []string{"tenant", "kube-system"}), trusted: []string{"admin"},
			wantReason: reasonNamespaceNotAllowed, wantErr: `outside the KRMSyncer's namespace "tenant" ("kube-system")`},
		{name: "untrusted, all namespaces", syncer: syncer("tenant", []string{AllNamespaces}),
			wantReason: reasonNamespaceNotAllowed, wantErr: `("*")`},
		{name: "untrusted, each outside namespace listed once", syncer: syncer("tenant", []string{"a"}, []string{"tenant"}, []string{"a", "b"}),
			wantReason: reasonNamespaceNotAllowed, wantErr: `("a", "b")`},
		{name: "trusted, another namespace", syncer: syncer("admin", []string{"kube-system"}), trusted: []string{"admin"}},
		{name: "trusted, all namespaces", syncer: syncer("admin", []string{AllNamespaces}), trusted: []string{"other", "admin"}},
		{name: "every namespace trusted", syncer: syncer("tenant", []string{AllNamespaces}), trusted: []string{AllNamespaces}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, err := checkNamespaces(tc.syncer, tc.trusted)
			if tc.wantReason == "" {
				assert.NoError(t, err)
				assert.Empty(t, reason)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tc.wantReason, reason)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestExpandRuleSkipsClusterScopedKinds checks that globs never select
// cluster-scoped kinds, whose objects are never synced.
func TestExpandRuleSkipsClusterScopedKinds(t *testing.T) {
	const group = "pubsub.cnrm.cloud.google.com"
	resourceLists := []*metav1.APIResourceList{{
		GroupVersion: group + "/v1beta1",
		APIResources: []metav1.APIResource{
			{Name: "pubsubtopics", Kind: "PubSubTopic", Namespaced: true},
			{Name: "pubsubclusterpolicies", Kind: "PubSubClusterPolicy", Namespaced: false},
		},
	}}
	r := &KRMSyncerReconciler{}
	rule := krmv1alpha1.ResourceRule{Group: "*.cnrm.cloud.google.com", Version: "*", Kind: "*", Namespaces: []string{AllNamespaces}}
	gvks, err := r.expandRule(t.Context(), nil, rule, krmv1alpha1.ModePull, resourceLists)
	require.NoError(t, err)
	assert.Equal(t, []schema.GroupVersionKind{{Group: group, Version: "v1beta1", Kind: "PubSubTopic"}}, gvks)
}

// TestDynamicResourceReconcilerNamespaceScope checks the scope where objects are
// synced, independently of whether KRMSyncerReconciler started a watch: in pull
// mode a remote cluster decides which objects show up, and only the namespaces a
// valid KRMSyncer may list are written.
func TestDynamicResourceReconcilerNamespaceScope(t *testing.T) {
	gvk := corev1.SchemeGroupVersion.WithKind("Service")
	remote := testRemote("source")
	connKey, err := testRemoteProvider.Key("tenant", remote)
	require.NoError(t, err)

	for _, tc := range []struct {
		name           string
		ruleNamespaces []string
		objNamespace   string
		trusted        []string
		wantApplied    bool
	}{
		{name: "own namespace", ruleNamespaces: []string{"tenant"}, objNamespace: "tenant", wantApplied: true},
		{name: "own namespace, object elsewhere", ruleNamespaces: []string{"tenant"}, objNamespace: "kube-system", wantApplied: false},
		{name: "no namespaces (stored before the CRD required them)", objNamespace: "tenant", wantApplied: false},
		{name: "trusted, * combined with a namespace", ruleNamespaces: []string{AllNamespaces, "tenant"}, objNamespace: "kube-system", trusted: []string{"tenant"}, wantApplied: false},
		{name: "untrusted, lists another namespace", ruleNamespaces: []string{"kube-system"}, objNamespace: "kube-system", wantApplied: false},
		{name: "untrusted, all namespaces", ruleNamespaces: []string{AllNamespaces}, objNamespace: "kube-system", wantApplied: false},
		{name: "untrusted rule elsewhere blocks its own namespace too", ruleNamespaces: []string{"tenant", "kube-system"}, objNamespace: "tenant", wantApplied: false},
		{name: "trusted, lists another namespace", ruleNamespaces: []string{"kube-system"}, objNamespace: "kube-system", trusted: []string{"tenant"}, wantApplied: true},
		{name: "trusted, all namespaces", ruleNamespaces: []string{AllNamespaces}, objNamespace: "kube-system", trusted: []string{"tenant"}, wantApplied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := types.NamespacedName{Namespace: tc.objNamespace, Name: "svc"}
			source := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(&corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
				Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
			}).Build()
			syncer := &krmv1alpha1.KRMSyncer{
				ObjectMeta: metav1.ObjectMeta{Name: "syncer", Namespace: "tenant"},
				Spec: krmv1alpha1.KRMSyncerSpec{
					Mode:   krmv1alpha1.ModePull,
					Remote: remote,
					Rules: []krmv1alpha1.ResourceRule{
						{Version: "v1", Kind: "Service", Namespaces: tc.ruleNamespaces, SyncFields: []string{"spec"}},
					},
				},
			}
			applied := false
			dest := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(syncer).WithInterceptorFuncs(interceptor.Funcs{
				// The fake client does not support server-side apply.
				Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
					applied = applied || client.ObjectKeyFromObject(obj) == key
					return nil
				},
			}).Build()

			r := &DynamicResourceReconciler{
				LocalClient:          dest,
				RemoteClient:         source,
				RemoteConfigProvider: testRemoteProvider,
				GVK:                  gvk,
				Mode:                 krmv1alpha1.ModePull,
				Remote:               RemoteGVK{Cluster: connKey, GVK: gvk},
				trustedNamespaces:    tc.trusted,
			}
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
			require.NoError(t, err)
			assert.Equal(t, tc.wantApplied, applied)
		})
	}
}

// TestDynamicResourceReconcilerSkipsClusterScopedObjects checks that
// cluster-scoped objects are never synced, even for a trusted KRMSyncer with
// "*" and a watch on their kind.
func TestDynamicResourceReconcilerSkipsClusterScopedObjects(t *testing.T) {
	gvk := rbacv1.SchemeGroupVersion.WithKind("ClusterRole")
	remote := testRemote("source")
	connKey, err := testRemoteProvider.Key("admin", remote)
	require.NoError(t, err)

	source := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(&rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-role"},
	}).Build()
	syncer := &krmv1alpha1.KRMSyncer{
		ObjectMeta: metav1.ObjectMeta{Name: "syncer", Namespace: "admin"},
		Spec: krmv1alpha1.KRMSyncerSpec{
			Mode:   krmv1alpha1.ModePull,
			Remote: remote,
			Rules: []krmv1alpha1.ResourceRule{
				{Group: rbacv1.GroupName, Version: "v1", Kind: "ClusterRole", Namespaces: []string{AllNamespaces}, SyncFields: []string{"spec"}},
			},
		},
	}
	applied := false
	dest := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(syncer).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			applied = true
			return nil
		},
	}).Build()

	r := &DynamicResourceReconciler{
		LocalClient:          dest,
		RemoteClient:         source,
		RemoteConfigProvider: testRemoteProvider,
		GVK:                  gvk,
		Mode:                 krmv1alpha1.ModePull,
		Remote:               RemoteGVK{Cluster: connKey, GVK: gvk},
		trustedNamespaces:    []string{"admin"},
	}
	_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "cluster-role"}})
	require.NoError(t, err)
	assert.False(t, applied, "cluster-scoped objects must never be synced")
}

// TestKRMSyncerReconcilerRejectsStoredRuleWithoutNamespaces checks a KRMSyncer
// stored before the CRD required namespaces, which the API server no longer
// accepts (see TestKRMSyncerNamespaceScope): it is rejected without retries.
func TestKRMSyncerReconcilerRejectsStoredRuleWithoutNamespaces(t *testing.T) {
	s := &krmv1alpha1.KRMSyncer{
		ObjectMeta: metav1.ObjectMeta{Name: "stored", Namespace: "tenant"},
		Spec: krmv1alpha1.KRMSyncerSpec{
			Mode:   krmv1alpha1.ModePush,
			Remote: testRemote(destClusterName),
			Rules:  []krmv1alpha1.ResourceRule{{Version: "v1", Kind: "Service"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(s).WithStatusSubresource(s).Build()
	// The Service watch already exists, so a KRMSyncer that got past the check
	// would be set up successfully, without a manager.
	r := &KRMSyncerReconciler{Client: c, WatchedGVKs: map[schema.GroupVersionKind]bool{corev1.SchemeGroupVersion.WithKind("Service"): true}}

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(s)})
	require.Error(t, err)
	assert.True(t, stderrors.Is(err, reconcile.TerminalError(nil)), "the rejection should not be retried: %v", err)

	var got krmv1alpha1.KRMSyncer
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(s), &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, "InvalidRule")
	require.NotNil(t, cond, "conditions: %v", got.Status.Conditions)
	assert.Equal(t, reasonInvalidNamespaces, cond.Reason)
	assert.Contains(t, cond.Message, "spec.rules[0].namespaces is required")
	assert.True(t, meta.IsStatusConditionFalse(got.Status.Conditions, "Active"), "a rejected KRMSyncer should not be Active")
}

// TestKRMSyncerNamespaceScope runs KRMSyncers through the test manager, which
// trusts only trustedTestNamespace.
func TestKRMSyncerNamespaceScope(t *testing.T) {
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000)
	tenantNS := "scope-tenant-" + suffix
	otherNS := "scope-other-" + suffix
	scopeNamespace(t, tenantNS)
	scopeNamespace(t, otherNS)
	scopeNamespace(t, trustedTestNamespace)

	t.Run("API server requires namespaces", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			namespaces []string
			wantErr    string
		}{
			{"missing", nil, "Required value"},
			{"empty list", []string{}, "should have at least 1 items"},
			{"empty name", []string{""}, "should be at least 1 chars long"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := &krmv1alpha1.KRMSyncer{
					ObjectMeta: metav1.ObjectMeta{Name: "invalid-namespaces", Namespace: tenantNS},
					Spec: krmv1alpha1.KRMSyncerSpec{
						Mode:   krmv1alpha1.ModePush,
						Remote: testRemote(destClusterName),
						Rules:  []krmv1alpha1.ResourceRule{{Version: "v1", Kind: "Service", Namespaces: tc.namespaces}},
					},
				}
				err := k8sClientSource.Create(t.Context(), s)
				if err == nil {
					_ = k8sClientSource.Delete(context.Background(), s)
				}
				require.True(t, errors.IsInvalid(err), "expected an Invalid error, got %v", err)
				assert.Contains(t, err.Error(), "spec.rules[0].namespaces")
				assert.Contains(t, err.Error(), tc.wantErr)
			})
		}
	})

	t.Run("untrusted KRMSyncer listing its own namespace syncs only it", func(t *testing.T) {
		scopeSyncer(t, tenantNS, "own-namespace",
			krmv1alpha1.ResourceRule{Version: "v1", Kind: "Service", Namespaces: []string{tenantNS}, SyncFields: []string{"spec"}})
		scopeService(t, tenantNS, "own")
		scopeService(t, otherNS, "elsewhere")
		scopeRequireOnDest(t, types.NamespacedName{Namespace: tenantNS, Name: "own"}, &corev1.Service{})
		scopeRequireNeverOnDest(t, types.NamespacedName{Namespace: otherNS, Name: "elsewhere"}, &corev1.Service{})
	})

	t.Run("untrusted KRMSyncer listing another namespace is rejected", func(t *testing.T) {
		s := scopeSyncer(t, tenantNS, "cross-namespace",
			krmv1alpha1.ResourceRule{Version: "v1", Kind: "Service", Namespaces: []string{otherNS}, SyncFields: []string{"spec"}})
		got := scopeRequireInvalidRule(t, s, reasonNamespaceNotAllowed)
		assert.True(t, meta.IsStatusConditionFalse(got.Status.Conditions, "Active"), "rejected KRMSyncer should not be Active")
		assert.Contains(t, meta.FindStatusCondition(got.Status.Conditions, "InvalidRule").Message, fmt.Sprintf("%q", otherNS))

		scopeService(t, otherNS, "rejected")
		scopeRequireNeverOnDest(t, types.NamespacedName{Namespace: otherNS, Name: "rejected"}, &corev1.Service{})
	})

	t.Run("trusted KRMSyncer syncs every namespace with *", func(t *testing.T) {
		// LimitRanges are used by no other test, so syncing them everywhere has no side effects.
		scopeSyncer(t, trustedTestNamespace, "all-namespaces-"+suffix,
			krmv1alpha1.ResourceRule{Version: "v1", Kind: "LimitRange", Namespaces: []string{AllNamespaces}, SyncFields: []string{"spec"}})
		lr := &corev1.LimitRange{
			ObjectMeta: metav1.ObjectMeta{Name: "limits", Namespace: otherNS},
			Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
				Type: corev1.LimitTypeContainer,
				Max:  corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			}}},
		}
		require.NoError(t, k8sClientSource.Create(t.Context(), lr))
		t.Cleanup(func() { _ = k8sClientSource.Delete(context.Background(), lr) })
		scopeRequireOnDest(t, client.ObjectKeyFromObject(lr), &corev1.LimitRange{})
	})

	t.Run("rule naming a cluster-scoped kind is rejected, other rules keep syncing", func(t *testing.T) {
		// ResourceQuotas and ClusterRoles are used by no other test.
		s := scopeSyncer(t, trustedTestNamespace, "cluster-scoped-"+suffix,
			krmv1alpha1.ResourceRule{Group: rbacv1.GroupName, Version: "v1", Kind: "ClusterRole", Namespaces: []string{AllNamespaces}, SyncFields: []string{"spec"}},
			krmv1alpha1.ResourceRule{Version: "v1", Kind: "ResourceQuota", Namespaces: []string{otherNS}, SyncFields: []string{"spec"}})
		got := scopeRequireInvalidRule(t, s, reasonClusterScopedKind)
		assert.Contains(t, meta.FindStatusCondition(got.Status.Conditions, "InvalidRule").Message, "rbac.authorization.k8s.io/v1 ClusterRole")

		quota := &corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: otherNS},
			Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("10")}},
		}
		require.NoError(t, k8sClientSource.Create(t.Context(), quota))
		t.Cleanup(func() { _ = k8sClientSource.Delete(context.Background(), quota) })
		scopeRequireOnDest(t, client.ObjectKeyFromObject(quota), &corev1.ResourceQuota{})

		role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "scope-cluster-role-" + suffix}}
		require.NoError(t, k8sClientSource.Create(t.Context(), role))
		t.Cleanup(func() { _ = k8sClientSource.Delete(context.Background(), role) })
		scopeRequireNeverOnDest(t, client.ObjectKeyFromObject(role), &rbacv1.ClusterRole{})
	})
}

func scopeNamespace(t *testing.T, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sClientSource.Create(t.Context(), ns); err != nil && !errors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
}

// scopeSyncer creates a push-mode KRMSyncer to the dest cluster and waits until
// it has been reconciled once, so objects created afterwards are not missed.
func scopeSyncer(t *testing.T, namespace, name string, rules ...krmv1alpha1.ResourceRule) *krmv1alpha1.KRMSyncer {
	t.Helper()
	s := &krmv1alpha1.KRMSyncer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: krmv1alpha1.KRMSyncerSpec{
			Mode:   krmv1alpha1.ModePush,
			Remote: testRemote(destClusterName),
			Rules:  rules,
		},
	}
	require.NoError(t, k8sClientSource.Create(t.Context(), s))
	t.Cleanup(func() { _ = k8sClientSource.Delete(context.Background(), s) })
	require.NoError(t, wait.PollUntilContextTimeout(t.Context(), 100*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		var got krmv1alpha1.KRMSyncer
		if err := k8sClientSource.Get(ctx, client.ObjectKeyFromObject(s), &got); err != nil {
			return false, nil
		}
		return meta.FindStatusCondition(got.Status.Conditions, "Active") != nil, nil
	}), "KRMSyncer %s/%s was never reconciled", namespace, name)
	return s
}

// scopeRequireInvalidRule waits until s has an InvalidRule condition with
// reason, and returns s as last read.
func scopeRequireInvalidRule(t *testing.T, s *krmv1alpha1.KRMSyncer, reason string) *krmv1alpha1.KRMSyncer {
	t.Helper()
	var got krmv1alpha1.KRMSyncer
	require.NoError(t, wait.PollUntilContextTimeout(t.Context(), 100*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := k8sClientSource.Get(ctx, client.ObjectKeyFromObject(s), &got); err != nil {
			return false, nil
		}
		c := meta.FindStatusCondition(got.Status.Conditions, "InvalidRule")
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == reason, nil
	}), "expected InvalidRule/%s, got conditions %v", reason, got.Status.Conditions)
	return &got
}

func scopeService(t *testing.T, namespace, name string) {
	t.Helper()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
	}
	require.NoError(t, k8sClientSource.Create(t.Context(), svc))
	t.Cleanup(func() { _ = k8sClientSource.Delete(context.Background(), svc) })
}

func scopeRequireOnDest(t *testing.T, key types.NamespacedName, obj client.Object) {
	t.Helper()
	require.NoError(t, wait.PollUntilContextTimeout(t.Context(), 100*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		return k8sClientDest.Get(ctx, key, obj) == nil, nil
	}), "%s should be synced to the destination", key)
}

// scopeRequireNeverOnDest checks that key does not show up on the destination
// within a few seconds, longer than a sync takes in these tests.
func scopeRequireNeverOnDest(t *testing.T, key types.NamespacedName, obj client.Object) {
	t.Helper()
	err := wait.PollUntilContextTimeout(t.Context(), 200*time.Millisecond, 3*time.Second, true, func(ctx context.Context) (bool, error) {
		return k8sClientDest.Get(ctx, key, obj) == nil, nil
	})
	require.Error(t, err, "%s must not be synced to the destination", key)
}
