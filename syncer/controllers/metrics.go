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
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// FailedObjects is the number of objects whose latest sync attempt failed,
	// partitioned by GVK. An object is counted from its first failed attempt until it
	// syncs successfully, however many times it is retried in between.
	FailedObjects = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "syncer_failed_objects",
			Help: "Number of objects whose latest sync attempt failed, partitioned by GVK.",
		},
		[]string{"group", "version", "kind"},
	)

	// SetupFailed is the number of KRMSyncers whose latest setup attempt (API discovery,
	// connecting to the remote cluster, starting watches) failed. With a single
	// KRMSyncer this is 0 or 1.
	SetupFailed = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "syncer_setup_failed",
			Help: "Number of KRMSyncers whose latest setup attempt failed.",
		},
	)

	// SyncedObjects is the number of objects targeted by a KRMSyncer whose latest
	// sync attempt succeeded, partitioned by GVK. An object stops being counted when
	// it fails, is deleted from the source, or is no longer targeted by any KRMSyncer.
	SyncedObjects = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "syncer_synced_objects",
			Help: "Number of objects whose latest sync attempt succeeded, partitioned by GVK.",
		},
		[]string{"group", "version", "kind"},
	)

	// PendingObjects is the number of objects that existed in the source when their
	// GVK watch started and have not been processed yet, partitioned by GVK. It drops
	// to 0 once the initial sync of every watched object has been attempted.
	PendingObjects = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "syncer_pending_objects",
			Help: "Number of source objects found when the watch started that have not been processed yet, partitioned by GVK.",
		},
		[]string{"group", "version", "kind"},
	)
)

func init() {
	metrics.Registry.MustRegister(FailedObjects, SetupFailed, SyncedObjects, PendingObjects)
}

func failedObjectsGauge(gvk schema.GroupVersionKind) prometheus.Gauge {
	return FailedObjects.WithLabelValues(gvk.Group, gvk.Version, gvk.Kind)
}

// failureTracker tracks which objects are currently failing and keeps a gauge equal
// to their number: an object is added on a failed attempt and removed on success, so
// repeated failures of the same object count once.
//
// A nil *failureTracker ignores all updates.
type failureTracker struct {
	gauge prometheus.Gauge

	mu     sync.Mutex
	failed map[types.NamespacedName]struct{}
}

func newFailureTracker(gauge prometheus.Gauge) *failureTracker {
	return &failureTracker{gauge: gauge, failed: make(map[types.NamespacedName]struct{})}
}

// markFailed records that the latest attempt for key failed.
func (t *failureTracker) markFailed(key types.NamespacedName) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.failed[key]; !ok {
		t.failed[key] = struct{}{}
		t.gauge.Inc()
	}
}

// markSucceeded records that the latest attempt for key succeeded, or that key no
// longer needs syncing (e.g. it was deleted).
func (t *failureTracker) markSucceeded(key types.NamespacedName) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.failed[key]; ok {
		delete(t.failed, key)
		t.gauge.Dec()
	}
}

// objectState is the sync state of an object tracked by objectSyncTracker.
type objectState int

const (
	// statePending means the object was in the source when the watch started and
	// has not been processed yet.
	statePending objectState = iota
	// stateSynced means the latest sync attempt of the object succeeded.
	stateSynced
	// stateFailed means the latest sync attempt of the object failed.
	stateFailed
)

// objectSyncTracker tracks the sync state of the objects of one GVK watch and keeps
// the syncer_{pending,synced,failed}_objects gauges equal to the number of objects in
// each state. Objects that are deleted from the source or not targeted by any
// KRMSyncer are not tracked.
//
// Several watches (e.g. pulling the same GVK from different clusters) may share the
// same gauges; each adds its own objects to them.
//
// A nil *objectSyncTracker ignores all updates.
type objectSyncTracker struct {
	gauges map[objectState]prometheus.Gauge

	mu     sync.Mutex
	states map[types.NamespacedName]objectState
	// seeded is set once the objects found when the watch started have been added
	// as pending. Until then, forgotten records objects that were processed and
	// dropped, so that seeding does not add them back from a stale listing.
	seeded    bool
	forgotten map[types.NamespacedName]struct{}
}

func newObjectSyncTracker(gvk schema.GroupVersionKind) *objectSyncTracker {
	return &objectSyncTracker{
		gauges: map[objectState]prometheus.Gauge{
			statePending: PendingObjects.WithLabelValues(gvk.Group, gvk.Version, gvk.Kind),
			stateSynced:  SyncedObjects.WithLabelValues(gvk.Group, gvk.Version, gvk.Kind),
			stateFailed:  failedObjectsGauge(gvk),
		},
		states:    make(map[types.NamespacedName]objectState),
		forgotten: make(map[types.NamespacedName]struct{}),
	}
}

// setLocked moves key to state. t.mu must be held.
func (t *objectSyncTracker) setLocked(key types.NamespacedName, state objectState) {
	if old, ok := t.states[key]; ok {
		if old == state {
			return
		}
		t.gauges[old].Dec()
	}
	t.states[key] = state
	t.gauges[state].Inc()
}

// seedPending adds the objects found in the source when the watch started as
// pending, except those that have already been processed. It is called at most once.
func (t *objectSyncTracker) seedPending(keys []types.NamespacedName) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seeded {
		return
	}
	for _, key := range keys {
		if _, ok := t.states[key]; ok {
			continue
		}
		if _, ok := t.forgotten[key]; ok {
			continue
		}
		t.setLocked(key, statePending)
	}
	t.seeded = true
	t.forgotten = nil
}

// markSynced records that the latest sync attempt for key succeeded.
func (t *objectSyncTracker) markSynced(key types.NamespacedName) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.setLocked(key, stateSynced)
}

// markFailed records that the latest sync attempt for key failed.
func (t *objectSyncTracker) markFailed(key types.NamespacedName) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.setLocked(key, stateFailed)
}

// forget stops tracking key because it no longer needs syncing: it was deleted
// from the source, or no KRMSyncer targets it.
func (t *objectSyncTracker) forget(key types.NamespacedName) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.seeded {
		t.forgotten[key] = struct{}{}
	}
	if old, ok := t.states[key]; ok {
		delete(t.states, key)
		t.gauges[old].Dec()
	}
}
