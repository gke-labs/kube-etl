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
    *   The remote GKE cluster is referenced directly in `spec.remote.gkeCluster`; the controller authenticates to it with Workload Identity.
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
  mode: pull # Defaults to 'pull'.
  rules:
    - group: ""
      version: "v1"
      kind: "PubsubTopic"
      namespaces: ["default"] # Only sync PubsubTopic in the 'default' namespace
  remote:
    gkeCluster:
      project: my-project
      location: us-central1
      name: source-cluster
      endpoint: Default # Optional. Default | DNS | PrivateIP
```

`spec.remote.gkeCluster.endpoint` selects how the controller reaches the remote cluster:
- `Default` (default): the cluster's default endpoint, the same one `gcloud container clusters get-credentials` uses.
- `DNS`: the cluster's DNS-based endpoint. It must be enabled for external traffic (`gcloud container clusters update --enable-dns-access`); needs no VPC connectivity, and is unaffected by credential rotation.
- `PrivateIP`: the cluster's private endpoint. The controller must be able to reach the cluster's VPC.
## Run Integration test
```bash
# Build the manager binary
cd syncer
make test-integration
```

## Getting Started

### Prerequisites

Before running [`krmsyncer.sh`](krmsyncer.sh), make sure you have the following.

**Local tools**

- `kubectl`, `gcloud`, and `docker`.
- [`gke-gcloud-auth-plugin`](https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl#install_plugin), so your local `kubectl` context can authenticate to the destination cluster.
- Push access to the image registry (e.g. `gcloud auth configure-docker gcr.io`).

**Clusters**

- The destination cluster must have [Workload Identity](https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity) enabled.
- You need permission to create service accounts and grant IAM roles in the project.
- You need permission to grant IAM roles in the source cluster's project, and to describe the source cluster.
- You need a kubeconfig context for the destination cluster, with permission to deploy on it. Create it with:
  ```bash
  gcloud container clusters get-credentials <DEST_CLUSTER_NAME> --location <DEST_CLUSTER_LOCATION> --project <GCP_PROJECT_ID>
  ```
  No kubeconfig for the source cluster is needed.
- The source cluster must expose the endpoint you select with `--source-endpoint`, and the destination cluster must be able to reach it (see [Network access to the source cluster](#network-access-to-the-source-cluster)).
- To use the sample `KRMSyncer` CR, destination cluster needs the Config Connector CRDs installed for all the resources in the source cluster.

### Network access to the source cluster

The controller runs in the destination cluster and calls the source cluster's control plane directly, so the destination cluster's Pods need a network path to the endpoint selected by `--source-endpoint`. Configure this before deploying; otherwise the controller only logs connection timeouts.

| `--source-endpoint` | Network requirements |
|---|---|
| `DNS` | None between the clusters: traffic goes over Google's network and access is controlled by IAM (`container.clusters.connect`, included in `roles/container.viewer`). The DNS endpoint must accept external traffic: `gcloud container clusters update <SOURCE_CLUSTER_NAME> --location <SOURCE_CLUSTER_LOCATION> --enable-dns-access`. |
| `PrivateIP` | The destination cluster must be able to route to the source's private endpoint: same VPC or Shared VPC, or a directly connected VPC (VPC peering, Cloud VPN, Interconnect; peering is not transitive). If the destination is in a different region than the source, the source must have [control plane global access](https://cloud.google.com/kubernetes-engine/docs/how-to/private-clusters#cp-global-access) enabled (`--enable-master-global-access`); without it, cross-region connections time out even on the same VPC. Firewall rules must allow the destination's node and Pod ranges to reach it on TCP 443. If the source enforces authorized networks on the private endpoint (`privateEndpointEnforcementEnabled`), add those ranges to its authorized networks. |
| `Default` | The source's IP endpoint must accept the destination's traffic. If it's the public endpoint, add the destination's egress IPs (e.g. its Cloud NAT addresses) to the source's [authorized networks](https://cloud.google.com/kubernetes-engine/docs/how-to/authorized-networks). If the cluster only exposes its private endpoint, the `PrivateIP` requirements apply. |

> [!NOTE]
> **Config Controller as the source.** Config Controller clusters (`krmapihost-*`) are private clusters that, by default, also have a public endpoint, control plane global access off, and DNS external traffic off. In practice:
> - Use `--source-endpoint Default` (the public endpoint). It works from any region and VPC.
> - Use `--source-endpoint PrivateIP` only if the destination cluster is in the same region and on the same VPC as the source cluster. Cross-region connections to the private endpoint time out.
> - `--source-endpoint DNS` requires enabling DNS access on the managed cluster, which may not be permitted.
>
> If the source was created with `--use-private-endpoint`, it has no public endpoint; see [Private-only Config Controller](#private-only-config-controller).

#### Private-only Config Controller

A Config Controller cluster created with `--use-private-endpoint` is reachable only at its private endpoint, an internal IP in the source's VPC subnet. Its network settings are fixed when it is created, so the destination cluster has to be created to fit them. Use `--source-endpoint PrivateIP`.

First, read the source's settings:
```bash
gcloud container clusters describe <SOURCE_CLUSTER_NAME> --location <SOURCE_CLUSTER_LOCATION> --project <SOURCE_PROJECT_ID> \
  --format="yaml(location, network, privateClusterConfig.privateEndpoint, masterAuthorizedNetworksConfig.cidrBlocks)"
```

Then create the destination cluster so that:

1. **Region:** it is in the source's `location`. The source has control plane global access off, so connections from other regions time out, even over the same VPC, VPC peering, Cloud VPN or Interconnect.
2. **Network:** it is on the source's `network`. For a destination in another project, use [Shared VPC](https://cloud.google.com/vpc/docs/shared-vpc) and pass the host project's network and subnet.
3. **IP ranges:** its node subnet range and Pod range fall inside the source's `masterAuthorizedNetworksConfig.cidrBlocks`. Authorized networks are enforced on the private endpoint, and the control plane sees the Pods' own IPs (GKE does not SNAT Pod traffic to internal ranges). With the default `0.0.0.0/0`, any range works; otherwise pick unused ranges inside the listed blocks. If no free range fits, the destination can't reach the source.
4. **Workload Identity:** it is enabled (`--workload-pool`), which the controller needs to authenticate.

For example:
```bash
gcloud container clusters create <DEST_CLUSTER_NAME> --project <DEST_PROJECT_ID> \
  --location <SOURCE_CLUSTER_LOCATION> \
  --network <SOURCE_NETWORK> --subnetwork <SUBNET_IN_SOURCE_REGION> \
  --enable-ip-alias --cluster-ipv4-cidr <POD_RANGE_INSIDE_AUTHORIZED_BLOCKS> \
  --workload-pool <DEST_PROJECT_ID>.svc.id.goog
```

Also check:
- **Firewall:** egress within a VPC is allowed by default. If your organization adds egress deny rules, allow TCP 443 from the destination's node and Pod ranges to the private endpoint.
- **Google APIs:** the controller still calls `container.googleapis.com` and the Workload Identity token endpoints. If the destination's nodes have no internet egress (private nodes without Cloud NAT), enable [Private Google Access](https://cloud.google.com/vpc/docs/private-google-access) on their subnet:
  ```bash
  gcloud compute networks subnets update <DEST_SUBNET> --region <SOURCE_CLUSTER_LOCATION> --enable-private-ip-google-access
  ```

If the destination must run in a different region, it can't reach a private-only source directly.

#### Inspecting endpoints and testing reachability

Inspect the source cluster's endpoints and access settings with:
```bash
gcloud container clusters describe <SOURCE_CLUSTER_NAME> --location <SOURCE_CLUSTER_LOCATION> --project <SOURCE_PROJECT_ID> \
  --format="yaml(network, endpoint, privateClusterConfig.privateEndpoint, privateClusterConfig.masterGlobalAccessConfig, controlPlaneEndpointsConfig, masterAuthorizedNetworksConfig)"
```

To confirm the destination cluster can reach the endpoint, open a TCP connection to port 443 from inside it (replace the address with the selected endpoint). `open` means there is a network path; a timeout means traffic is dropped (routing, firewall or authorized networks):
```bash
kubectl --context=<DEST_CONTEXT> run nettest --rm -i --restart=Never --image=busybox -- \
  nc -zv -w 10 <SOURCE_ENDPOINT> 443
```

### 1. Deploy KRMSyncer to the Destination Cluster

Use the [`krmsyncer.sh`](krmsyncer.sh) script.

```bash
cd syncer

./krmsyncer.sh \
  --source-cluster <SOURCE_CLUSTER_NAME> \
  --source-location <SOURCE_CLUSTER_LOCATION> \
  --dest-cluster <DEST_CLUSTER_NAME> \
  --dest-location <DEST_CLUSTER_LOCATION> \
  --project <GCP_PROJECT_ID> \
  [--source-project <SOURCE_PROJECT_ID>] \
  [--source-endpoint Default|DNS|PrivateIP] \
  [-n <NAMESPACE>] \
  [-i <IMAGE>] \
  [--skip-build]
```

`--source-project` sets the source cluster's project (default: `--project`).

`--source-endpoint` selects the source cluster's control plane endpoint: `Default`, `DNS` or `PrivateIP` (default: `Default`).

`-n` sets the namespace for the `KRMSyncer` CR (default: `krmsyncer-system`, the same namespace as the controller).

`-i` sets the controller image to deploy. Default to `gcr.io/<project>/krmsyncer/controller:latest`.

`--skip-build` deploys the image as is, without building and pushing it.

This command:
1. Configures Workload Identity. It creates the `krmsyncer@<project>.iam.gserviceaccount.com` Google Service Account (GSA), grants it `roles/container.viewer` on the source cluster's project, and lets the controller's `krmsyncer-system/krmsyncer-controller-manager` Kubernetes ServiceAccount impersonate it. `roles/container.viewer` lets the GSA look up the source cluster, connect to it (including over the DNS endpoint) and read its Kubernetes objects.
2. Builds and pushes the controller image (unless `--skip-build` is set).
3. Checks that the source cluster exists and exposes the selected endpoint.
4. Deploys the RBAC, KRMSyncer CRD and controller into the `krmsyncer-system` namespace of the destination cluster, and annotates the controller's ServiceAccount with the GSA.
5. Applies the `kcc-resource-syncer` `KRMSyncer` CR from [`config/templates/krmsyncer.yaml`](config/templates/krmsyncer.yaml). It references the source cluster in `spec.remote.gkeCluster` and syncs all Config Connector resources from it. The destination cluster needs the matching KCC CRDs pre-installed.

View the controller logs with:
```bash
kubectl --context=gke_<project>_<dest-location>_<dest-cluster> -n krmsyncer-system logs deploy/krmsyncer-controller-manager -f
```

### 2. Verify the Results
1. Create a test resource in the Source cluster:
   ```bash
   kubectl create pubsubtopic test-topic
   ```

1. Check the Destination cluster:
   Switch your kubectl context to the Destination cluster and verify the PubsubTopic has appeared:
   ```bash
   kubectl --context=<dest-cluster-context> get pubsubtopic test-topic
   ```
1.  Expected Result:
- The `test-topic` PubsubTopic created in the Source cluster should automatically appear in the Destination cluster within seconds.
- If you update the PubsubTopic in the Source cluster, the changes should reflect in the Destination cluster.
- If you delete it from the Source cluster, it should be removed from the Destination cluster.
