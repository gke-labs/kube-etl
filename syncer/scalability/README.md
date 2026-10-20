# KRMSyncer Scalability Evaluation

This directory contains tools for evaluating the performance and scalability of the `krmsyncer` controller.

## Tools

### 1. `generate_resources.go`
A utility script that generates high volumes of KRM resources (specifically Google Config Connector / KCC resources) with valid `status.conditions`.
*   **Dynamic Timestamps**: Automatically sets `lastTransitionTime` to the current time.
*   **Diverse Types**: Supports ~50+ different KCC resource kinds.

Example usage:
```bash
# Generate 100 ArtifactRegistryRepository resources:
go run generate_resources.go -count 100 -kinds ArtifactRegistryRepository > ar_50.yaml
# Generate 100 total resources using a mix of 10 random kinds
go run generate_resources.go -count 100 -num-kinds 10 > mix_10_kinds_100.yaml
```

### 2. `benchmark_sync.sh`
The primary automation script for running end-to-end performance benchmarks. It handles resource generation, multi-cluster synchronization, real-time progress tracking, and performance reporting.

## Benchmark Usage

### Multi-Cluster Synchronization Example
Measure how long it takes to sync resources from a Source cluster to a Destination cluster:
```bash
./scalability/benchmark_sync.sh \
  --count 500 \
  --num-kinds 10 \
  --source-context "gke_prod_us-central1_cluster-a" \
  --dest-context "gke_prod_us-west1_cluster-b" \
  --timeout 600
```

### Available Flags
| Flag | Description | Default |
| :--- | :--- | :--- |
| `-c, --count` | Total number of resources to sync. | `50` |
| `-k, --kinds` | Comma-separated list of specific resource kinds. | `ArtifactRegistryRepository` |
| `-n, --num-kinds` | Pick N random kinds from the available pool. | `0` (Disabled) |
| `-t, --timeout` | Max time (seconds) to wait for sync completion. | `(count / 2) + 60` |
| `-s, --source-context` | Kubernetes context for the Source cluster. | Current Context |
| `-d, --dest-context` | Kubernetes context for the Destination cluster. | Current Context |
| `-ns, --namespace` | Namespace. | `default` |

## How it Works

1. **Suspension**: The script first suspends the `test-syncer` object in the destination cluster. This ensures that the controller does not begin syncing while resources are still being applied to the source cluster.
2. **Resource Preparation**: Uses `generate_resources.go` to create a YAML manifest with unique names and fresh timestamps.
3. **Setup Monitoring**:
   * Automatically sets up a `kubectl port-forward` to the syncer pod if needed.
   * Captures the initial `syncer_sync_success_total` Prometheus counter.
4. **Synchronization & Timing**:
   * Resumes the `test-syncer` by patching `suspend: false`.
   * Starts the timer.
   * Polls the `syncer_sync_success_total` Prometheus counter for progress.
5. **Batch Latency**: Total time from when the syncer was resumed until 100% of resources are synchronized.
6. **Cleanup**:
   * Automatically deletes all benchmarked resources from the Source cluster using the `batch-bench=true` label.
   * Ensures the `test-syncer` is left in a resumed state.
