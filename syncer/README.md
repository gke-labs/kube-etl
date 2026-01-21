# KRMSyncer

The `KRMSyncer` controller enables unidirectional synchronization of Kubernetes resources (KRM) from a local (source) cluster to a remote (destination) cluster.

## Features

- **Dynamic Watching:** Dynamically registers watches for resources specified in the configuration.
- **Resource Syncing:** Syncs standard resources (e.g., ConfigMaps, Secrets) and CRDs.
- **Status Syncing:** Optionally syncs the status subresource.
- **Suspension:** Supports pausing sync operations via a `suspend` field.
- **Namespace Mapping:** Supports syncing to a specific destination namespace.

## Configuration

```yaml
apiVersion: syncer.gkelabs.io/v1alpha1
kind: KRMSyncer
metadata:
  name: example-syncer
  namespace: default
spec:
  destination:
    kubeConfigSecretRef:
      name: dest-kubeconfig
      namespace: default
    namespace: dest-ns # Optional: Sync to this namespace
  rules:
  - group: ""
    version: "v1"
    kind: "ConfigMap"
    namespaces:
    - "default"
```
