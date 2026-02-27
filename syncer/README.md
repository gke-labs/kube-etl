# KRMSyncer

The KRMSyncer is a Kubernetes-native tool designed for multi-cluster state synchronization. It facilitates **Active-Passive (Failover)** scenarios where one cluster acts as the leader (Syncer's `Source`) and another acts as a standby (Syncer's `Destination`).

## Features

- **Push & Pull Models:** Support both pushing from local to remote and pulling from remote to local clusters.
- **Dynamic Watching:** Dynamically registers watches for resources specified in the configuration.
- **Resource Syncing:** Syncs standard resources (e.g., ConfigMaps, Secrets) and CRDs.
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

### 1. Prerequisites
- **Remote Cluster Secret**: A Secret in the same namespace as the `KRMSyncer` resource containing the `kubeconfig` key with the target cluster's configuration.
- **RBAC**: The operator needs permissions to read the resources defined in the rules and to manage `Syncer` resources.

### 2. Build and Deploy

```bash
# Build the manager binary
cd syncer
make build

# Build Docker image
docker build -t syncer-operator:latest .
```

Alternatively, you can start the KRMSyncer controller locally.
```bash
go run main.go
```

### 3. Usage Example: Cross-Cluster Sync

1. **Extract the Remote Kubeconfig**:
    1. Find the remote cluster context name:
       ```bash
       kubectl config get-contexts
       ```

    2. Export the context to file:
       ```bash
       kubectl config view --context=<REMOTE_CONTEXT_NAME> --minify --flatten > remote-kubeconfig.yaml
       * Replace <REMOTE_CONTEXT_NAME> with the name found above
       * --minify: Only includes the information for that specific context.
       * --flatten: Embeds the certificate data directly into the file so it doesn't rely on external file paths.
       ```

    3. Verify the file:
       ```bash
       kubectl --kubeconfig=remote-kubeconfig.yaml get nodes
       ```
       If this command works, `remote-kubeconfig.yaml` is ready to be used.

1. **Create the Kubeconfig Secret** (on the Local cluster):
    ```bash
    kubectl create secret generic remote-kubeconfig \
      --from-file=kubeconfig=remote-kubeconfig.yaml
    ```

1. **Apply the Syncer Resource** (on the Local cluster):
    ```yaml
    # test-syncer.yaml
    apiVersion: syncer.gkelabs.io/v1alpha1
    kind: KRMSyncer
    metadata:
      name: configmap-sync
    spec:
      suspend: false
      mode: push
      rules:
        - group: ""
          version: "v1"
          kind: "ConfigMap"
          namespaces: ["default"] # Only sync ConfigMaps in the 'default' namespace
      remote:
        clusterConfig:
          kubeConfigSecretRef:
            name: "remote-kubeconfig"

    ```
    ```bash
    kubectl apply -f test-syncer.yaml
    ```
1. **Verify the Results**:
    1. Create a test resource in the Local cluster:
       ```bash
       kubectl create configmap test-sync-data --from-literal=key=value1
       ```

    1. Check the Remote cluster:
       Switch your kubectl context to the Remote cluster and verify the ConfigMap has appeared:
       ```bash
       kubectl --context=<remote-cluster-context> get configmap test-sync-data
       ```
    1.  Expected Result:
    - The `test-sync-data` ConfigMap created in the Source cluster should automatically appear in the Passive cluster within seconds.
    - If you update the ConfigMap in the Active cluster, the changes should reflect in the Passive cluster.
    - If you delete it from the Active cluster, it should be removed from the Passive cluster.
