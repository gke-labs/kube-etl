# KRMSyncer

The KRMSyncer is a Kubernetes-native tool designed for multi-cluster KRM resources synchronization. 

## Features

- **Push & Pull Models:** Support both pushing from local to remote and pulling from remote to local clusters.
- **Dynamic Watching:** Dynamically registers watches for resources specified in the configuration.
- **Resource Syncing:** Syncs standard resources (e.g., ConfigMaps, Secrets).
- **Status Syncing:** Optionally syncs the status subresource.
- **Suspension:** Supports pausing sync operations via a `suspend` field.
- **Namespace Mapping:** Supports syncing to a specific destination namespace.

## Overview

The operator manages the `KRMSyncer` Custom Resource to coordinate resource replication:

1.  **Reconciling (Active cluster)**:
    *   Watches specific Kubernetes resources defined in rules.
    *   Continuously syncs their state directly to the destination.
    *   Requires a `Secret` containing the Kubeconfig of the remote cluster.
    *   Default mode is `pull`.

2.  **Suspended (Passive cluster)**:
    *   Acts as the receiver.
    *   The controller in this mode remains idle regarding synchronization, waiting for updates from the other cluster.

## Configuration (KRMSyncer CRD)

The `KRMSyncer` resource allows you to define what to sync and where to sync it.

```yaml
apiVersion: syncer.gkelabs.io/v1alpha1
kind: KRMSyncer
metadata:
  name: resource-sync
spec:
  suspend: false
  mode: pull # New field! Can be 'push' or 'pull'. Defaults to 'pull'.
  rules:
    - group: ""
      version: "v1"
      kind: "ConfigMap"
      namespaces: ["default"] # Only sync ConfigMaps in the 'default' namespace
    - group: "networking.k8s.io"
      version: "v1"
      kind: "Ingress"
  remote: # Renamed field!
    clusterConfig:
      kubeConfigSecretRef:
        name: "remote-cluster-kubeconfig"
```
## Run Integration test
```bash
# Build the manager binary
cd syncer
make test-integration
```

## Getting Started

### 1. Start KRMSyncer Locally

Use the automated `syncer start` command:

```bash
./syncer start \
  --source-cluster <SOURCE_CLUSTER_NAME> \
  --source-location <SOURCE_CLUSTER_LOCATION> \
  --project <GCP_PROJECT_ID> \
  [--dest-cluster <DEST_CLUSTER_NAME>] \
  [--dest-location <DEST_CLUSTER_LOCATION>] \
  [-n <NAMESPACE>]
```

### 2. Usage Example: Cross-Cluster Sync

1. Create a test resource in the Source cluster:
   ```bash
   kubectl --context=<source-cluster-context> create configmap test-sync-data --from-literal=key=value1
   ```

1. Verify the test resource has been synced to the Destination cluster:
   ```bash
   kubectl get configmap test-sync-data
   ```
   
1.  Expected Result:
- The `test-sync-data` ConfigMap created in the Source cluster should automatically appear in the Destination cluster within seconds.
- If you update the ConfigMap in the Source cluster, the changes should reflect in the Destination cluster.
- If you delete it from the Source cluster, it should be removed from the Destination cluster.
