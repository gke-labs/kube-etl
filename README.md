# kube-etl

ETL for kubernetes objects: extract objects via watch, polling or audit logs; transform and filter out sensitive information; load to object storage, file systems, databases or other clusters

## Features

### KRMSyncer

The `KRMSyncer` controller enables unidirectional synchronization of Kubernetes resources (KRM) from a local (source) cluster to a remote (destination) cluster.

**Capabilities:**
- **Dynamic Watching:** Dynamically registers watches for resources specified in the configuration.
- **Resource Syncing:** Syncs standard resources (e.g., ConfigMaps, Secrets) and CRDs.
- **Status Syncing:** Optionally syncs the status subresource.
- **Suspension:** Supports pausing sync operations via a `suspend` field.

**Example Configuration:**

```yaml
apiVersion: krm.gke.io/v1alpha1
kind: KRMSyncer
metadata:
  name: example-syncer
  namespace: default
spec:
  destination:
    kubeConfigSecretRef:
      name: dest-kubeconfig
      namespace: default
  rules:
  - group: ""
    version: "v1"
    kind: "ConfigMap"
    namespaces:
    - "default"
  - group: "apps"
    version: "v1"
    kind: "Deployment"
```

## Contributing

This project is licensed under the [Apache 2.0 License](LICENSE).

We welcome contributions! Please see [docs/contributing.md](docs/contributing.md) for more information.

We follow [Google's Open Source Community Guidelines](https://opensource.google.com/conduct/).

## Disclaimer

This is not an officially supported Google product.

This project is not eligible for the Google Open Source Software Vulnerability Rewards Program.