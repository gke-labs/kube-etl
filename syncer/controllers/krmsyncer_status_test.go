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
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	krmv1alpha1 "github.com/gke-labs/kube-etl/syncer/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
)

// TestReadyCondition reconciles KRMSyncers against a fake API server and checks
// the Ready condition, and that status is only written when it changes.
func TestReadyCondition(t *testing.T) {
	ctx := t.Context()
	var statusWrites atomic.Int32
	newReconciler := func(objs ...client.Object) (*KRMSyncerReconciler, client.Client) {
		c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(objs...).
			WithStatusSubresource(&krmv1alpha1.KRMSyncer{}).
			WithInterceptorFuncs(interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					statusWrites.Add(1)
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					statusWrites.Add(1)
					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			}).Build()
		return &KRMSyncerReconciler{
			Client:               c,
			Scheme:               scheme.Scheme,
			RemoteConfigProvider: testRemoteProvider,
			WatchedGVKs:          make(map[schema.GroupVersionKind]bool),
			WatchedRemoteGVKs:    make(map[RemoteGVK]bool),
			RemoteClusters:       make(map[string]cluster.Cluster),
			setupFailures:        newSetupFailureTracker(prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_setup_failed"})),
		}, c
	}
	reconcileReady := func(t *testing.T, r *KRMSyncerReconciler, c client.Client, key types.NamespacedName) (*krmv1alpha1.KRMSyncer, *metav1.Condition, error) {
		t.Helper()
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		s := &krmv1alpha1.KRMSyncer{}
		require.NoError(t, c.Get(ctx, key, s))
		ready := meta.FindStatusCondition(s.Status.Conditions, krmv1alpha1.ConditionReady)
		require.NotNil(t, ready, "Ready should be set")
		return s, ready, err
	}

	t.Run("syncing, then suspended, written only on change", func(t *testing.T) {
		statusWrites.Store(0)
		syncer := &krmv1alpha1.KRMSyncer{
			ObjectMeta: metav1.ObjectMeta{Name: "ready", Namespace: "default", Generation: 3},
			// Push mode with no rules: setup has nothing to do.
			Spec: krmv1alpha1.KRMSyncerSpec{Mode: krmv1alpha1.ModePush, Remote: testRemote(destClusterName)},
			// Conditions written by earlier versions are replaced by Ready.
			Status: krmv1alpha1.KRMSyncerStatus{Conditions: []metav1.Condition{
				{Type: "Active", Status: metav1.ConditionTrue, Reason: "Active", LastTransitionTime: metav1.Now()},
			}},
		}
		r, c := newReconciler(syncer)
		key := client.ObjectKeyFromObject(syncer)

		s, ready, err := reconcileReady(t, r, c, key)
		require.NoError(t, err)
		assert.Equal(t, metav1.ConditionTrue, ready.Status)
		assert.Equal(t, krmv1alpha1.ReasonSyncing, ready.Reason)
		assert.Equal(t, s.Generation, ready.ObservedGeneration)
		assert.Equal(t, s.Generation, s.Status.ObservedGeneration)
		assert.Nil(t, meta.FindStatusCondition(s.Status.Conditions, "Active"), "legacy conditions should be removed")
		assert.Equal(t, int32(1), statusWrites.Load())

		// Nothing changed: no status write.
		_, _, err = reconcileReady(t, r, c, key)
		require.NoError(t, err)
		_, _, err = reconcileReady(t, r, c, key)
		require.NoError(t, err)
		assert.Equal(t, int32(1), statusWrites.Load(), "status should not be written when it did not change")

		// Suspend.
		s.Spec.Suspend = true
		require.NoError(t, c.Update(ctx, s))
		_, ready, err = reconcileReady(t, r, c, key)
		require.NoError(t, err)
		assert.Equal(t, metav1.ConditionTrue, ready.Status, "a suspended KRMSyncer is Ready")
		assert.Equal(t, krmv1alpha1.ReasonSuspended, ready.Reason)
		assert.Equal(t, int32(2), statusWrites.Load())
		_, _, err = reconcileReady(t, r, c, key)
		require.NoError(t, err)
		assert.Equal(t, int32(2), statusWrites.Load(), "status should not be written when it did not change")
	})

	t.Run("setup failed", func(t *testing.T) {
		syncer := &krmv1alpha1.KRMSyncer{
			ObjectMeta: metav1.ObjectMeta{Name: "setup-failed", Namespace: "default"},
			Spec: krmv1alpha1.KRMSyncerSpec{
				Mode:   krmv1alpha1.ModePull,
				Remote: testRemote("does-not-exist"),
				Rules:  []krmv1alpha1.ResourceRule{{Version: "v1", Kind: "ConfigMap"}},
			},
		}
		r, c := newReconciler(syncer)
		_, ready, err := reconcileReady(t, r, c, client.ObjectKeyFromObject(syncer))
		require.Error(t, err, "a failed setup should be retried")
		assert.Equal(t, metav1.ConditionFalse, ready.Status)
		assert.Equal(t, krmv1alpha1.ReasonSetupFailed, ready.Reason)
		assert.Contains(t, ready.Message, `unknown test cluster "does-not-exist"`)
	})

	t.Run("invalid rule", func(t *testing.T) {
		syncer := &krmv1alpha1.KRMSyncer{
			ObjectMeta: metav1.ObjectMeta{Name: "invalid-rule", Namespace: "default"},
			Spec: krmv1alpha1.KRMSyncerSpec{
				// Globs run discovery against the remote (envtest) in Pull mode.
				Mode:   krmv1alpha1.ModePull,
				Remote: testRemote(destClusterName),
				Rules:  []krmv1alpha1.ResourceRule{{Group: "*", Version: "v1", Kind: "ConfigMap"}},
			},
		}
		r, c := newReconciler(syncer)
		_, ready, err := reconcileReady(t, r, c, client.ObjectKeyFromObject(syncer))
		require.Error(t, err)
		assert.Equal(t, krmv1alpha1.ReasonSetupFailed, ready.Reason)
		assert.Contains(t, ready.Message, "invalid rule")
	})

	t.Run("remote unreachable takes precedence over setup failure", func(t *testing.T) {
		remote := testRemote("does-not-exist")
		syncer := &krmv1alpha1.KRMSyncer{
			ObjectMeta: metav1.ObjectMeta{Name: "unreachable", Namespace: "default"},
			Spec: krmv1alpha1.KRMSyncerSpec{
				Mode:   krmv1alpha1.ModePull,
				Remote: remote,
				Rules:  []krmv1alpha1.ResourceRule{{Version: "v1", Kind: "ConfigMap"}},
			},
		}
		r, c := newReconciler(syncer)
		connKey, err := testRemoteProvider.Key(syncer.Namespace, remote)
		require.NoError(t, err)
		r.remotes = newRemoteHealthTracker(nil)
		r.remotes.get(connKey).markUnreachable(stderrors.New("Unauthorized"))

		_, ready, err := reconcileReady(t, r, c, client.ObjectKeyFromObject(syncer))
		require.Error(t, err)
		assert.Equal(t, metav1.ConditionFalse, ready.Status)
		assert.Equal(t, krmv1alpha1.ReasonRemoteUnreachable, ready.Reason)
		assert.Equal(t, "remote cluster projects/test-project/locations/us-central1/clusters/does-not-exist: Unauthorized", ready.Message)
	})

	t.Run("sync failed", func(t *testing.T) {
		syncer := &krmv1alpha1.KRMSyncer{
			ObjectMeta: metav1.ObjectMeta{Name: "sync-failed", Namespace: "default"},
			Spec:       krmv1alpha1.KRMSyncerSpec{Mode: krmv1alpha1.ModePush, Remote: testRemote(destClusterName)},
		}
		r, c := newReconciler(syncer)
		r.syncFailures = newSyncFailureTracker(nil, nil)
		key := client.ObjectKeyFromObject(syncer)
		svc := func(name string) objectRef {
			return objectRef{GVK: schema.GroupVersionKind{Version: "v1", Kind: "Service"}, Key: types.NamespacedName{Namespace: "ns", Name: name}}
		}
		const missing, forbidden = "destination object missing", "apply failed (Forbidden)"
		r.syncFailures.markFailed(key, svc("a"), missing)
		r.syncFailures.markFailed(key, svc("b"), forbidden)
		r.syncFailures.markFailed(key, svc("c"), missing)
		// Failures of other KRMSyncers are not counted.
		r.syncFailures.markFailed(types.NamespacedName{Namespace: "default", Name: "other"}, svc("e"), "delete failed")

		// Failures are grouped by error category, most frequent first.
		s, ready, err := reconcileReady(t, r, c, key)
		require.NoError(t, err, "failed objects are retried by their own controllers")
		assert.Equal(t, int32(3), s.Status.FailedObjects)
		assert.Equal(t, metav1.ConditionFalse, ready.Status)
		assert.Equal(t, krmv1alpha1.ReasonSyncFailed, ready.Reason)
		assert.Equal(t, "2 destination object missing, 1 apply failed (Forbidden). See the controller logs for details.", ready.Message)

		// An object failing with another error moves to that category.
		r.syncFailures.markFailed(key, svc("a"), forbidden)
		_, ready, err = reconcileReady(t, r, c, key)
		require.NoError(t, err)
		assert.Equal(t, "2 apply failed (Forbidden), 1 destination object missing. See the controller logs for details.", ready.Message)

		// At most maxFailureGroupsInMessage categories are listed.
		for i, name := range []string{"d", "e", "f", "g", "h"} {
			r.syncFailures.markFailed(key, svc(name), fmt.Sprintf("category %d", i))
		}
		_, ready, err = reconcileReady(t, r, c, key)
		require.NoError(t, err)
		assert.Equal(t, "2 apply failed (Forbidden), 1 category 0, 1 category 1, 1 category 2, 1 category 3, and 2 more. See the controller logs for details.", ready.Message)

		for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
			r.syncFailures.markSucceeded(key, svc(name))
		}
		s, ready, err = reconcileReady(t, r, c, key)
		require.NoError(t, err)
		assert.Zero(t, s.Status.FailedObjects)
		assert.Equal(t, krmv1alpha1.ReasonSyncing, ready.Reason)

		// Suspending forgets the failures.
		r.syncFailures.markFailed(key, svc("a"), missing)
		s.Spec.Suspend = true
		require.NoError(t, c.Update(ctx, s))
		s, ready, err = reconcileReady(t, r, c, key)
		require.NoError(t, err)
		assert.Zero(t, s.Status.FailedObjects)
		assert.Equal(t, krmv1alpha1.ReasonSuspended, ready.Reason)
	})
}

// flakyRoundTripper passes requests through, or fails all of them with a
// given HTTP status.
type flakyRoundTripper struct {
	base   http.RoundTripper
	status atomic.Int32 // 0 passes requests through

	mu     sync.Mutex
	bodies map[*trackedBody]struct{}
}

func (rt *flakyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if code := int(rt.status.Load()); code != 0 {
		text := http.StatusText(code)
		body := fmt.Sprintf(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":%q,"reason":%q,"code":%d}`, text, strings.ReplaceAll(text, " ", ""), code)
		return &http.Response{
			Status:     fmt.Sprintf("%d %s", code, text),
			StatusCode: code,
			Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header:  http.Header{"Content-Type": {"application/json"}},
			Body:    io.NopCloser(strings.NewReader(body)),
			Request: req,
		}, nil
	}
	resp, err := rt.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	b := &trackedBody{ReadCloser: resp.Body, rt: rt}
	rt.mu.Lock()
	if rt.bodies == nil {
		rt.bodies = make(map[*trackedBody]struct{})
	}
	rt.bodies[b] = struct{}{}
	rt.mu.Unlock()
	resp.Body = b
	return resp, nil
}

// failWith makes all requests fail with the HTTP status code (0 to recover).
// Open responses (e.g. watches) are closed, so clients send new requests.
func (rt *flakyRoundTripper) failWith(code int) {
	rt.status.Store(int32(code))
	rt.mu.Lock()
	bodies := rt.bodies
	rt.bodies = nil
	rt.mu.Unlock()
	for b := range bodies {
		_ = b.ReadCloser.Close()
	}
}

type trackedBody struct {
	io.ReadCloser
	rt *flakyRoundTripper
}

func (b *trackedBody) Close() error {
	b.rt.mu.Lock()
	delete(b.rt.bodies, b)
	b.rt.mu.Unlock()
	return b.ReadCloser.Close()
}

// TestReadyRemoteUnreachableAfterSetup verifies that a remote failing after a
// successful setup is reported in Ready (RemoteUnreachable), and that Ready
// recovers once the remote is reachable again.
func TestReadyRemoteUnreachableAfterSetup(t *testing.T) {
	ctx := t.Context()

	// Use a new remote cluster on each run: the informers of a remote that
	// already failed retry with a longer backoff, which would slow down
	// recovery on repeated runs (-count).
	clusterName := "dest-flaky-" + utilrand.String(5)
	flaky := &flakyRoundTripper{}
	cfg := rest.CopyConfig(cfgDest)
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		flaky.base = rt
		return flaky
	})
	testRemoteProvider.add(clusterName, cfg)

	syncer := &krmv1alpha1.KRMSyncer{
		ObjectMeta: metav1.ObjectMeta{Name: "syncer-" + clusterName, Namespace: "default"},
		Spec: krmv1alpha1.KRMSyncerSpec{
			Mode:   krmv1alpha1.ModePull,
			Remote: testRemote(clusterName),
			Rules: []krmv1alpha1.ResourceRule{
				{Version: "v1", Kind: "ConfigMap", Namespaces: []string{"flaky-remote-ns"}},
			},
		},
	}
	require.NoError(t, k8sClientSource.Create(ctx, syncer))
	defer func() { _ = k8sClientSource.Delete(ctx, syncer) }()

	waitForReady := func(status metav1.ConditionStatus, reason, message string) {
		t.Helper()
		var last *metav1.Condition
		err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 60*time.Second, true, func(ctx context.Context) (bool, error) {
			s := &krmv1alpha1.KRMSyncer{}
			if err := k8sClientSource.Get(ctx, client.ObjectKeyFromObject(syncer), s); err != nil {
				return false, nil
			}
			last = meta.FindStatusCondition(s.Status.Conditions, krmv1alpha1.ConditionReady)
			return last != nil && last.Status == status && last.Reason == reason &&
				(message == "" || last.Message == message) &&
				s.Status.ObservedGeneration == s.Generation, nil
		})
		require.NoError(t, err, "want Ready=%s/%s, last: %+v", status, reason, last)
	}

	waitForReady(metav1.ConditionTrue, krmv1alpha1.ReasonSyncing, "")

	// The remote starts rejecting our credentials.
	flaky.failWith(http.StatusUnauthorized)
	waitForReady(metav1.ConditionFalse, krmv1alpha1.ReasonRemoteUnreachable,
		"remote cluster projects/test-project/locations/us-central1/clusters/"+clusterName+": Unauthorized")

	// The remote recovers.
	flaky.failWith(0)
	waitForReady(metav1.ConditionTrue, krmv1alpha1.ReasonSyncing, "")
}
