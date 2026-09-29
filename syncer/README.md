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

### 1. Deploy KRMSyncer to the Destination Cluster

Use the `krmsyncer sync` command. It's a Go CLI in [`cmd/krmsyncer`](cmd/krmsyncer).

```bash
# Build the manager binary
cd syncer
make build

go run ./cmd/krmsyncer sync \
  --source-cluster <SOURCE_CLUSTER_NAME> \
  --source-location <SOURCE_CLUSTER_LOCATION> \
  --project <GCP_PROJECT_ID> \
  [--dest-cluster <DEST_CLUSTER_NAME>] \
  [--dest-location <DEST_CLUSTER_LOCATION>] \
  [-n <NAMESPACE>]
```

Run the CLI from the `krmsyncer` directory. It builds the image from the `Dockerfile` there and renders manifests from `config/`, and it exits with an error if run anywhere else.

`-n` sets the namespace for the `KRMSyncer` CR and the `source-cluster` Secret (default: `krmsyncer-system`, the same namespace as the controller).

This command:
1. Configures Workload Identity. It creates the `krmsyncer@<project>.iam.gserviceaccount.com` GSA, grants it `roles/container.viewer`, and binds it to the `krmsyncer-system/krmsyncer-controller-manager` KSA. This runs first so the new IAM bindings have time to propagate during the image build.
2. Builds and pushes a fresh `gcr.io/<project>/krmsyncer/controller:latest`, then deletes all older images in that repository (and the stale local copy).
3. Creates the `source-cluster` kubeconfig Secret. The kubeconfig authenticates with `gke-gcloud-auth-plugin --use_application_default_credentials`, so it contains no user credentials.
4. Deploys the CRD, RBAC, and controller into the `krmsyncer-system` namespace of the destination cluster.
5. Applies a sample `KRMSyncer` CR.

Prerequisites: `kubectl`, `gcloud`, and `docker`. The destination cluster must have Workload Identity enabled, and you need a kubeconfig context for the source cluster (`gcloud container clusters get-credentials`).

> [!NOTE]
> New IAM bindings can take a few minutes to take effect. On the first run, or after you change the controller's KSA name or namespace, the controller logs may show errors like these for a short time:
>
> ```
> Permission 'iam.serviceAccounts.getAccessToken' denied ...
> getting credentials: exec: executable gke-gcloud-auth-plugin failed with exit code 1
> ```
>
> The controller retries automatically and starts syncing once the bindings take effect. You don't need to do anything. If the errors keep appearing after about 10 minutes, check the `roles/iam.workloadIdentityUser` binding on the GSA and the `iam.gke.io/gcp-service-account` annotation on the KSA.

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
