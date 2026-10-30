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
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestObjectSyncTracker(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "tracker.test", Version: "v1", Kind: "Widget"}
	tracker := newObjectSyncTracker(gvk)
	a := types.NamespacedName{Namespace: "ns", Name: "a"}
	b := types.NamespacedName{Namespace: "ns", Name: "b"}
	c := types.NamespacedName{Namespace: "ns", Name: "c"}
	d := types.NamespacedName{Namespace: "ns", Name: "d"}

	type counts struct{ pending, synced, failed float64 }
	get := func() counts {
		return counts{
			pending: testutil.ToFloat64(PendingObjects.WithLabelValues(gvk.Group, gvk.Version, gvk.Kind)),
			synced:  testutil.ToFloat64(SyncedObjects.WithLabelValues(gvk.Group, gvk.Version, gvk.Kind)),
			failed:  testutil.ToFloat64(FailedObjects.WithLabelValues(gvk.Group, gvk.Version, gvk.Kind)),
		}
	}

	// Objects processed before seeding are not reported as pending: a is already
	// failing and d was deleted, so the listing used for seeding is stale for it.
	tracker.markFailed(a)
	tracker.forget(d)
	tracker.seedPending([]types.NamespacedName{a, b, c, d})
	assert.Equal(t, counts{pending: 2, failed: 1}, get())

	// Seeding happens only once.
	tracker.seedPending([]types.NamespacedName{{Namespace: "ns", Name: "e"}})
	assert.Equal(t, counts{pending: 2, failed: 1}, get())

	tracker.markSynced(b)
	tracker.markFailed(c)
	assert.Equal(t, counts{synced: 1, failed: 2}, get())

	// Repeated outcomes count once.
	tracker.markFailed(c)
	assert.Equal(t, counts{synced: 1, failed: 2}, get())

	tracker.markSynced(a)
	tracker.markSynced(c)
	assert.Equal(t, counts{synced: 3}, get())

	tracker.forget(a)
	tracker.forget(b)
	// Forgetting an untracked object is a no-op.
	tracker.forget(b)
	assert.Equal(t, counts{synced: 1}, get())

	// A nil tracker ignores updates.
	var nilTracker *objectSyncTracker
	nilTracker.markSynced(a)
	nilTracker.markFailed(a)
	nilTracker.forget(a)
	nilTracker.seedPending([]types.NamespacedName{a})
}
