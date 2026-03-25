# KRMSyncer

The KRMSyncer is a Kubernetes-native tool designed for multi-cluster state
synchronization. It facilitates **Active-Passive (Failover)** scenarios where
one cluster acts as the leader (Syncer's `Source`) and another acts as a standby
(Syncer's `Destination`).

## Features

-   **Push & Pull Models:** Support both pushing from local to remote and
    pulling from remote to local clusters.
-   **Dynamic Watching:** Dynamically registers watches for resources specified
    in the configuration.
-   **Field-Level Syncing:** Syncs specific fields (e.g., `spec`, `status`)
    using Server-Side Apply.
-   **Suspension:** Supports pausing sync operations via a `suspend` field.

## Overview

The operator manages the `KRMSyncer` Custom Resource to coordinate resource
replication:

1.  **Pull Mode (Default)**:

    *   Runs on the **Destination (Passive)** cluster.
    *   Watches resources on the **Source (Active)** cluster and pulls them
        locally.
    *   Requires a `Secret` in the Destination cluster containing the Kubeconfig
        of the Source cluster.

2.  **Push Mode**:

    *   Runs on the **Source (Active)** cluster.
    *   Watches local resources and pushes them to the **Destination (Passive)**
        cluster.
    *   Requires a `Secret` in the Source cluster containing the Kubeconfig of
        the Destination cluster.

3.  **Suspended**:

    *   When `suspend: true`, the controller remains idle, pausing all
        synchronization activities.

## Configuration (KRMSyncer CRD)

The `KRMSyncer` resource allows you to define what to sync and where to sync it.

```yaml
apiVersion: syncer.gkelabs.io/v1alpha1
kind: KRMSyncer
metadata:
  name: resource-sync
spec:
  suspend: false
  mode: pull # Can be 'push' or 'pull'. Defaults to 'pull'.
  rules:
    - group: "networking.k8s.io"
      version: "v1"
      kind: "Ingress"
      namespaces: ["prod"] # Optional: Only sync resources in these namespaces
      syncFields: ["spec", "status"] # Optional: Default is ["status"]
    - group: "e2e.gkelabs.io"
      version: "v1alpha1"
      kind: "TestCRD"
      syncFields: ["spec.resourceID", "status"]
  remote:
    clusterConfig:
      kubeConfigSecretRef:
        name: "remote-cluster-kubeconfig"
```

### SyncFields

The `syncFields` list determines which parts of the resource are synchronized.
Currently supported fields are: - `spec`: The entire spec subresource. -
`status`: The entire status subresource. - `spec.resourceID`: A specific field
within the spec.

**Note:** If `syncFields` is not specified, it defaults to `["status"]`. For
resources without a status subresource (like ConfigMaps), you must ensure the
fields you want to sync are supported by the API.

## Run Integration test

```bash
# Run integration tests using envtest
make test-integration
```

## Getting Started

### 1. Prerequisites

-   **Remote Cluster Secret**: A Secret in the same namespace as the `KRMSyncer`
    resource containing the `kubeconfig` key with the target cluster's
    configuration.
-   **RBAC**: The operator needs permissions to read the resources defined in
    the rules (on both clusters depending on mode) and to manage `KRMSyncer`
    resources.

### Build and Deploy

```bash
# Build the manager binary
make build

# Build Docker image
make docker-build
```

Alternatively, you can start the KRMSyncer controller locally.

```bash
go run main.go
```

### Usage Example: Cross-Cluster Sync

1.  **Extract the Remote Kubeconfig**:

    1.  Find the remote cluster context name: `bash kubectl config get-contexts`

    2.  Export the context to file: `bash kubectl config view
        --context=<REMOTE_CONTEXT_NAME> --minify --flatten >
        remote-kubeconfig.yaml * Replace <REMOTE_CONTEXT_NAME> with the name
        found above * --minify: Only includes the information for that specific
        context. * --flatten: Embeds the certificate data directly into the file
        so it doesn't rely on external file paths.`

    3.  Verify the file: `bash kubectl --kubeconfig=remote-kubeconfig.yaml get
        nodes` If this command works, `remote-kubeconfig.yaml` is ready to be
        used.

1.  **Create the Kubeconfig Secret** (on the Local cluster): `bash kubectl
    create secret generic remote-kubeconfig \
    --from-file=kubeconfig=remote-kubeconfig.yaml`

1.  **(Optional) Update the Kubeconfig Secret**

    To sync to a new destination, the best practice is to create a new
    Kubeconfig Secret for the destination cluster. You can update an existing
    Secret as well, there are two ways:

    1.  Delete and recreate

    ```bash
    kubectl delete secret destination-kubeconfig
    kubectl create secret generic destination-kubeconfig --from-file=kubeconfig=new-dest-kubeconfig.yaml
    ```

    1.  Use `kubectl patch`

    ```bash
    kubectl patch secret destination-kubeconfig -p "{\"data\":{\"kubeconfig\":\"$(base64 -w 0 <new-dest-kubeconfig.yaml)\"}}"
    ```

    Note that any changes must be reflected in the KRMSyncer CR. If the Secret
    with the same name now points to a different cluster, you will likely need
    to delete and recreate the CR to ensure the Syncer updates its destination;
    otherwise, it may continue targeting the old destination. So in-place
    Kubeconfig Secret update should be avoided.

1.  **Apply the Syncer Resource** (on the Local cluster): ```yaml

    # test-syncer.yaml

    apiVersion: syncer.gkelabs.io/v1alpha1 kind: KRMSyncer metadata: name:
    ingress-sync spec: rules: - group: "networking.k8s.io" version: "v1" kind:
    "Ingress" namespaces: ["default"] remote: clusterConfig:
    kubeConfigSecretRef: name: "remote-kubeconfig" ` `bash kubectl apply -f
    test-syncer.yaml ```

1.  **Verify the Results**:

    1.  Create a test resource in the Remote cluster: `bash kubectl
        --context=<remote-cluster-context> create configmap test-sync-data
        --from-literal=key=value1`

    2.  Check the Local cluster: Switch your kubectl context to the Local
        cluster and verify the ConfigMap has appeared: `bash kubectl get
        configmap test-sync-data`

    3.  Expected Result:

    4.  The `test-sync-data` ConfigMap created in the Source cluster should
        automatically appear in the Passive cluster within seconds.

    5.  If you update the ConfigMap in the Active cluster, the changes should
        reflect in the Passive cluster.

    6.  If you delete it from the Active cluster, it should be removed from the
        Passive cluster.
