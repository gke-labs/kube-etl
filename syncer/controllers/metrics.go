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
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	OperationApply  = "apply"
	OperationStatus = "status"
	OperationDelete = "delete"
)

var (
	// SyncFailuresTotal counts the total number of failed syncs, partitioned by GVK and operation.
	SyncFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "syncer_sync_failures_total",
			Help: "Total number of failed syncs, partitioned by GVK and operation.",
		},
		[]string{"group", "version", "kind", "operation"},
	)

	// SyncSuccessTotal counts the total number of successful syncs, partitioned by GVK and operation.
	SyncSuccessTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "syncer_sync_success_total",
			Help: "Total number of successful syncs, partitioned by GVK and operation.",
		},
		[]string{"group", "version", "kind", "operation"},
	)

	// SyncLatency records the duration of sync operations in seconds, partitioned by GVK and operation.
	SyncLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "syncer_sync_latency_seconds",
			Help:    "Total time to sync a resource in seconds, partitioned by GVK and operation.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"group", "version", "kind", "operation"},
	)
)

func init() {
	metrics.Registry.MustRegister(SyncFailuresTotal, SyncSuccessTotal, SyncLatency)
}

func recordSyncFailure(gvk schema.GroupVersionKind, operation string) {
	SyncFailuresTotal.WithLabelValues(gvk.Group, gvk.Version, gvk.Kind, operation).Inc()
}

func recordSyncSuccess(gvk schema.GroupVersionKind, operation string) {
	SyncSuccessTotal.WithLabelValues(gvk.Group, gvk.Version, gvk.Kind, operation).Inc()
}

func recordSyncLatency(gvk schema.GroupVersionKind, operation string, durationSeconds float64) {
	SyncLatency.WithLabelValues(gvk.Group, gvk.Version, gvk.Kind, operation).Observe(durationSeconds)
}
