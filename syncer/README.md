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
    *   The remote cluster is a GKE cluster referenced directly in the spec; the controller authenticates with its own Google Workload Identity.
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
  remote:
    gkeCluster:
      project: my-project
      location: us-central1
      name: remote-cluster
      endpoint: Default # Optional. Default | DNS | PrivateIP
```
## Run Integration test
```bash
# Build the manager binary
cd syncer
make test-integration
```

## Getting Started

### 1. Prerequisites
- **Remote GKE cluster**: The remote cluster must be a GKE cluster. It is referenced in `spec.remote.gkeCluster` by project, location and name.
- **Control plane endpoint**: `spec.remote.gkeCluster.endpoint` selects how the controller reaches the remote cluster:
  - `Default` (default): the cluster's default endpoint, the same one `gcloud container clusters get-credentials` uses.
  - `DNS`: the cluster's DNS-based endpoint (it must be enabled on the cluster). This works from anywhere with IAM-based access and needs no VPC connectivity.
  - `PrivateIP`: the cluster's private endpoint. The controller must have network connectivity to the cluster's VPC.
- **Google service account for the controller**: The controller authenticates to the GKE API and the remote cluster with Application Default Credentials. On GKE, the `krmsyncer-system/krmsyncer-controller-manager` Kubernetes service account must impersonate a Google service account (GSA) through [Workload Identity Federation for GKE](https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity) (see "Link Kubernetes ServiceAccounts to IAM").
  - `container.clusters.get` on the remote cluster's project (e.g. `roles/container.clusterViewer`), the controller also uses it to look up the cluster endpoint and CA.
  - Kubernetes RBAC on the remote cluster to read (pull mode) or write (push mode) the synced resources, bound to the GSA's email as a `User` subject.
- **RBAC**: The operator needs permissions to read the resources defined in the rules and to manage `Syncer` resources.

> [!WARNING]
> All `KRMSyncer` objects share the controller's Google identity. Anyone who can create a `KRMSyncer` can sync with any cluster that identity can access, so restrict who can create `KRMSyncer` objects.

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

1. **Set up a Google service account for the controller** (in the Local cluster's project):
    ```bash
    gcloud iam service-accounts create krmsyncer --project=<LOCAL_PROJECT>

    # Allow the controller's Kubernetes service account to impersonate the GSA.
    gcloud iam service-accounts add-iam-policy-binding krmsyncer@<LOCAL_PROJECT>.iam.gserviceaccount.com \
      --role=roles/iam.workloadIdentityUser \
      --member="serviceAccount:<LOCAL_PROJECT>.svc.id.goog[krmsyncer-system/krmsyncer-controller-manager]"

    kubectl annotate serviceaccount krmsyncer-controller-manager -n krmsyncer-system \
      iam.gke.io/gcp-service-account=krmsyncer@<LOCAL_PROJECT>.iam.gserviceaccount.com
    ```
    Instead of `kubectl annotate`, you can set the annotation in [`config/rbac/service_account.yaml`](config/rbac/service_account.yaml) before deploying.

1. **Grant the GSA access to the Remote cluster**:
    ```bash
    # Required to authenticate to the remote cluster and to look up its endpoint and CA.
    gcloud projects add-iam-policy-binding <REMOTE_PROJECT> \
      --role=roles/container.clusterViewer \
      --member=serviceAccount:krmsyncer@<LOCAL_PROJECT>.iam.gserviceaccount.com
    ```
    Then grant the GSA Kubernetes RBAC on the Remote cluster for the resources being synced, using its email as a `User` subject.

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
        gkeCluster:
          project: <REMOTE_PROJECT>
          location: <REMOTE_LOCATION>
          name: <REMOTE_CLUSTER>

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
