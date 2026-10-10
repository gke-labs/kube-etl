#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# krmsyncer.sh installs KRMSyncer into the destination GKE cluster and
# configures it to pull resources from the source GKE cluster:
#
#   1. Configure Workload Identity: a Google Service Account (GSA) for the
#      controller, impersonated by the controller's Kubernetes ServiceAccount,
#      with roles/container.viewer on the source cluster's project.
#   2. Build and push the controller image (skipped with --skip-build).
#   3. Check that the source cluster exists and that the selected control
#      plane endpoint (Default, DNS or PrivateIP) is available.
#   4. Deploy the KRMSyncer CRD, RBAC and controller to the destination cluster.
#   5. Deploy the KRMSyncer configuration (config/templates/krmsyncer.yaml),
#      which references the source cluster in spec.remote.gkeCluster.
#
# No credentials are stored in either cluster and nothing is created on the
# source cluster. The controller looks up the source cluster's endpoint and CA
# through the GKE API and authenticates as the GSA. roles/container.viewer
# grants read access to Kubernetes objects in the source project's clusters
# through IAM, so no RBAC objects are needed on the source cluster.
#
# The script deploys to the destination cluster through the kubectl context
# created by `gcloud container clusters get-credentials`
# (gke_<project>_<location>_<cluster>).

set -o errexit
set -o nounset
set -o pipefail

# --- Constants ---------------------------------------------------------------

MODULE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATES_DIR="${MODULE_DIR}/config/templates"

CONTROLLER_NAMESPACE="krmsyncer-system"
CONTROLLER_DEPLOYMENT="krmsyncer-controller-manager"
KRMSYNCER_CRD="krmsyncers.syncer.gkelabs.io"
IMAGE_PLACEHOLDER="controller:latest"

# KRMSyncer CR rendered from config/templates/krmsyncer.yaml.
SYNC_CONFIG_NAME="kcc-resource-syncer"

# Kubernetes ServiceAccount of the controller (config/rbac/service_account.yaml),
# and the Google Service Account it impersonates via Workload Identity.
CONTROLLER_KSA="krmsyncer-controller-manager"
GSA_NAME="krmsyncer"

# --- Logging -----------------------------------------------------------------

log_info()    { echo -e "\033[0;34m[INFO]\033[0m $*"; }
log_success() { echo -e "\033[0;32m[OK]\033[0m $*"; }
log_header()  { echo -e "\n\033[1;36m=== $* ===\033[0m"; }
die()         { echo -e "\033[0;31m[ERROR]\033[0m $*" >&2; exit 1; }

usage() {
  cat <<EOF
Usage: $(basename "$0") --source-cluster <NAME> --source-location <LOCATION> \\
         --dest-cluster <NAME> --dest-location <LOCATION> --project <PROJECT> [flags]

Deploys KRMSyncer to the destination GKE cluster and configures it to pull
resources from the source GKE cluster. The destination cluster needs a kubectl
context created by \`gcloud container clusters get-credentials\` and Workload
Identity enabled.

Flags:
      --source-cluster   Name of the source GKE cluster (required)
      --source-location  Location (region or zone) of the source cluster (required)
      --source-project   GCP project of the source cluster (default: --project)
      --source-endpoint  Control plane endpoint used to reach the source cluster:
                         Default, DNS or PrivateIP (default: Default)
      --dest-cluster     Name of the destination GKE cluster (required)
      --dest-location    Location (region or zone) of the destination cluster (required)
      --project          GCP project of the destination cluster and the controller's
                         Google Service Account (required)
  -n, --namespace        Namespace for the KRMSyncer CR (default: ${CONTROLLER_NAMESPACE})
  -i, --image            Controller image (default: gcr.io/<project>/krmsyncer/controller:latest)
      --skip-build       Do not build/push the image; deploy --image as is
  -h, --help             Show this help

Example:
  $(basename "$0") --source-cluster src --source-location us-west1 \\
    --dest-cluster dst --dest-location us-central1 --project my-project \\
    --source-endpoint DNS
EOF
}

# --- Flags -------------------------------------------------------------------

SOURCE_CLUSTER=""
SOURCE_LOCATION=""
SOURCE_PROJECT=""
SOURCE_ENDPOINT="Default"
DEST_CLUSTER=""
DEST_LOCATION=""
PROJECT=""
NAMESPACE="${CONTROLLER_NAMESPACE}"
IMAGE=""
SKIP_BUILD="false"

parse_flags() {
  while [[ $# -gt 0 ]]; do
    local flag="$1"
    case "${flag}" in
      -h|--help) usage; exit 0 ;;
      --skip-build) SKIP_BUILD="true"; shift; continue ;;
      --*=*)
        # Support --flag=value by splitting it into --flag value.
        set -- "${flag%%=*}" "${flag#*=}" "${@:2}"
        continue
        ;;
    esac
    [[ $# -ge 2 ]] || die "flag ${flag} requires a value"
    case "${flag}" in
      --source-cluster)  SOURCE_CLUSTER="$2" ;;
      --source-location) SOURCE_LOCATION="$2" ;;
      --source-project)  SOURCE_PROJECT="$2" ;;
      --source-endpoint) SOURCE_ENDPOINT="$2" ;;
      --dest-cluster)    DEST_CLUSTER="$2" ;;
      --dest-location)   DEST_LOCATION="$2" ;;
      --project)         PROJECT="$2" ;;
      -n|--namespace)    NAMESPACE="$2" ;;
      -i|--image)        IMAGE="$2" ;;
      *) usage >&2; die "unknown flag: ${flag}" ;;
    esac
    shift 2
  done

  local missing=()
  [[ -n "${SOURCE_CLUSTER}" ]]  || missing+=("--source-cluster")
  [[ -n "${SOURCE_LOCATION}" ]] || missing+=("--source-location")
  [[ -n "${DEST_CLUSTER}" ]]    || missing+=("--dest-cluster")
  [[ -n "${DEST_LOCATION}" ]]   || missing+=("--dest-location")
  [[ -n "${PROJECT}" ]]         || missing+=("--project")
  if [[ ${#missing[@]} -gt 0 ]]; then
    usage >&2
    die "required flag(s) not set: ${missing[*]}"
  fi

  case "${SOURCE_ENDPOINT}" in
    Default|DNS|PrivateIP) ;;
    *) die "--source-endpoint must be Default, DNS or PrivateIP (got \"${SOURCE_ENDPOINT}\")" ;;
  esac
}

check_deps() {
  local deps=(kubectl gcloud)
  [[ "${SKIP_BUILD}" == "true" ]] || deps+=(docker)
  local dep
  for dep in "${deps[@]}"; do
    command -v "${dep}" >/dev/null 2>&1 || die "dependency \"${dep}\" is required but not found"
  done
}

complete() {
  [[ -f "${MODULE_DIR}/Dockerfile" && -d "${MODULE_DIR}/config/default" ]] ||
    die "${MODULE_DIR} is not the syncer directory (missing Dockerfile or config/default)"

  [[ -n "${SOURCE_PROJECT}" ]] || SOURCE_PROJECT="${PROJECT}"
  [[ "${SOURCE_PROJECT}/${SOURCE_LOCATION}/${SOURCE_CLUSTER}" != "${PROJECT}/${DEST_LOCATION}/${DEST_CLUSTER}" ]] ||
    die "source and destination are the same cluster (${PROJECT}/${DEST_LOCATION}/${DEST_CLUSTER})"

  # Context name created by `gcloud container clusters get-credentials`.
  DEST_CONTEXT="gke_${PROJECT}_${DEST_LOCATION}_${DEST_CLUSTER}"
  kubectl config get-contexts "${DEST_CONTEXT}" >/dev/null 2>&1 ||
    die "kubectl context \"${DEST_CONTEXT}\" not found; run: gcloud container clusters get-credentials ${DEST_CLUSTER} --location ${DEST_LOCATION} --project ${PROJECT}"

  [[ -n "${IMAGE}" ]] || IMAGE="gcr.io/${PROJECT}/krmsyncer/controller:latest"

  GSA_EMAIL="${GSA_NAME}@${PROJECT}.iam.gserviceaccount.com"
  WI_MEMBER="serviceAccount:${PROJECT}.svc.id.goog[${CONTROLLER_NAMESPACE}/${CONTROLLER_KSA}]"
}

print_summary() {
  echo "=================================================="
  echo "          KRMSyncer Deployment Setup              "
  echo "=================================================="
  log_info "Source Cluster         : projects/${SOURCE_PROJECT}/locations/${SOURCE_LOCATION}/clusters/${SOURCE_CLUSTER}"
  log_info "Source Endpoint        : ${SOURCE_ENDPOINT}"
  log_info "Dest Cluster Context   : ${DEST_CONTEXT}"
  log_info "GCP Project            : ${PROJECT}"
  log_info "Syncer CR Namespace    : ${NAMESPACE}"
  log_info "Controller Namespace   : ${CONTROLLER_NAMESPACE}"
  log_info "Controller Image       : ${IMAGE}"
  log_info "Google Service Account : ${GSA_EMAIL}"
  echo "=================================================="
}

# --- Helpers -----------------------------------------------------------------

kube_dest() { kubectl --context="${DEST_CONTEXT}" "$@"; }

# render_template prints a template with ${KEY} placeholders replaced.
# Usage: render_template <file> KEY=value...
render_template() {
  local content
  content="$(<"$1")"
  shift
  local kv
  for kv in "$@"; do
    content="${content//"\${${kv%%=*}}"/"${kv#*=}"}"
  done
  printf '%s\n' "${content}"
}

# quiet runs a command, printing its output only if it fails.
quiet() {
  local out
  if ! out="$("$@" 2>&1)"; then
    printf '%s\n' "${out}" >&2
    return 1
  fi
}

# --- Steps -------------------------------------------------------------------

configure_workload_identity() {
  log_info "Checking Workload Identity on destination cluster ${DEST_CLUSTER}..."
  local pool
  pool="$(gcloud container clusters describe "${DEST_CLUSTER}" \
    --location "${DEST_LOCATION}" --project "${PROJECT}" \
    --format='value(workloadIdentityConfig.workloadPool)')"
  [[ -n "${pool}" ]] || die "Workload Identity is not enabled on ${DEST_CLUSTER}; enable it with:
  gcloud container clusters update ${DEST_CLUSTER} --location ${DEST_LOCATION} --project ${PROJECT} --workload-pool=${PROJECT}.svc.id.goog
(existing node pools also need --workload-metadata=GKE_METADATA)"
  log_success "Workload Identity enabled (pool: ${pool})."

  if gcloud iam service-accounts describe "${GSA_EMAIL}" --project "${PROJECT}" >/dev/null 2>&1; then
    log_info "Google Service Account ${GSA_EMAIL} already exists."
  else
    log_info "Creating Google Service Account ${GSA_EMAIL}..."
    gcloud iam service-accounts create "${GSA_NAME}" --project "${PROJECT}" \
      --display-name "KRMSyncer controller"
  fi

  # roles/container.viewer includes container.clusters.get (look up the source
  # cluster's endpoint and CA, and authenticate to it), container.clusters.connect
  # (needed for the DNS endpoint) and read access to Kubernetes objects in the
  # project's clusters, so no RBAC objects are needed on the source cluster.
  log_info "Granting roles/container.viewer on project ${SOURCE_PROJECT} to ${GSA_EMAIL}..."
  quiet gcloud projects add-iam-policy-binding "${SOURCE_PROJECT}" \
    --member "serviceAccount:${GSA_EMAIL}" --role roles/container.viewer \
    --condition=None --quiet

  log_info "Allowing ${WI_MEMBER} to impersonate ${GSA_EMAIL}..."
  quiet gcloud iam service-accounts add-iam-policy-binding "${GSA_EMAIL}" --project "${PROJECT}" \
    --member "${WI_MEMBER}" --role roles/iam.workloadIdentityUser \
    --condition=None --quiet
  log_success "Workload Identity configured."
}

build_and_push_image() {
  if [[ "${SKIP_BUILD}" == "true" ]]; then
    log_info "Skipping build; deploying ${IMAGE} as is."
    return
  fi

  local go_version
  go_version="$(awk '$1 == "go" && NF == 2 { print $2 }' "${MODULE_DIR}/go.mod")"
  [[ -n "${go_version}" ]] || die "no go directive found in ${MODULE_DIR}/go.mod"

  log_info "Building controller image ${IMAGE} (Go ${go_version})..."
  docker build --platform linux/amd64 \
    --build-arg "GO_VERSION=${go_version}" -t "${IMAGE}" "${MODULE_DIR}"
  log_info "Pushing controller image ${IMAGE}..."
  docker push "${IMAGE}" ||
    die "failed to push ${IMAGE} (you may need to run: gcloud auth configure-docker ${IMAGE%%/*})"
  log_success "Controller image pushed."
}

# print_reachability_hint prints a command that tests, from inside the
# destination cluster, whether the source endpoint at $1 is reachable. IP
# endpoints depend on VPC routing, firewall rules and authorized networks,
# which the script can't verify from outside the cluster.
print_reachability_hint() {
  log_info "IP endpoints need a network path from the destination cluster (VPC routing, firewall,"
  log_info "authorized networks, and control plane global access if the clusters are in different regions)."
  log_info "To verify it, run:"
  log_info "  kubectl --context=${DEST_CONTEXT} run nettest --rm -i --restart=Never --image=busybox -- nc -zv -w 10 ${1} 443"
  log_info "\"open\" means there is a network path; a timeout means traffic is dropped (routing, firewall or authorized networks)."
}

# check_source_cluster verifies that the source cluster exists and exposes the
# selected endpoint, so misconfiguration fails here instead of in the controller.
check_source_cluster() {
  log_info "Looking up source cluster ${SOURCE_CLUSTER} in ${SOURCE_PROJECT}/${SOURCE_LOCATION}..."
  local described
  described="$(gcloud container clusters describe "${SOURCE_CLUSTER}" \
    --location "${SOURCE_LOCATION}" --project "${SOURCE_PROJECT}" \
    --format='csv[no-heading](endpoint,privateClusterConfig.privateEndpoint,controlPlaneEndpointsConfig.ipEndpointsConfig.privateEndpoint,controlPlaneEndpointsConfig.dnsEndpointConfig.endpoint,controlPlaneEndpointsConfig.dnsEndpointConfig.allowExternalTraffic)')" ||
    die "cannot describe source cluster ${SOURCE_CLUSTER} in ${SOURCE_PROJECT}/${SOURCE_LOCATION}"

  local endpoint private_legacy private_ip dns_endpoint dns_external
  # CSV keeps empty fields in place (whitespace-separated output would collapse them).
  IFS=',' read -r endpoint private_legacy private_ip dns_endpoint dns_external <<<"${described}"
  [[ -n "${private_ip}" ]] || private_ip="${private_legacy}"

  local update="gcloud container clusters update ${SOURCE_CLUSTER} --location ${SOURCE_LOCATION} --project ${SOURCE_PROJECT}"
  case "${SOURCE_ENDPOINT}" in
    Default)
      [[ -n "${endpoint}" ]] || die "source cluster has no IP endpoint (IP access may be disabled); use --source-endpoint DNS"
      log_info "Source endpoint (Default): ${endpoint}"
      print_reachability_hint "${endpoint}"
      ;;
    PrivateIP)
      [[ -n "${private_ip}" ]] || die "source cluster has no private endpoint; use --source-endpoint DNS or Default"
      log_info "Source endpoint (PrivateIP): ${private_ip}. The controller must be able to reach the source cluster's VPC."
      print_reachability_hint "${private_ip}"
      ;;
    DNS)
      [[ -n "${dns_endpoint}" && "${dns_external,,}" == "true" ]] ||
        die "source cluster's DNS endpoint is not enabled for external traffic; enable it with:
  ${update} --enable-dns-access
or use --source-endpoint Default or PrivateIP"
      log_info "Source endpoint (DNS): ${dns_endpoint}"
      ;;
  esac
  log_success "Source cluster found."
}

deploy_controller() {
  local config_dir="${MODULE_DIR}/config/default"
  log_info "Rendering manifests from ${config_dir}..."
  local manifests
  manifests="$(kubectl kustomize "${config_dir}")"
  [[ "${manifests}" == *"image: ${IMAGE_PLACEHOLDER}"* ]] ||
    die "image \"${IMAGE_PLACEHOLDER}\" not found in rendered manifests; check config/manager/manager.yaml"

  log_info "Verifying destination cluster connectivity..."
  kube_dest get --raw /version >/dev/null ||
    die "cannot reach destination cluster with context \"${DEST_CONTEXT}\""

  log_info "Applying CRD, RBAC and controller Deployment..."
  printf '%s\n' "${manifests//"image: ${IMAGE_PLACEHOLDER}"/"image: ${IMAGE}"}" | kube_dest apply -f -

  log_info "Annotating ServiceAccount for Workload Identity..."
  kube_dest -n "${CONTROLLER_NAMESPACE}" annotate serviceaccount "${CONTROLLER_KSA}" \
    "iam.gke.io/gcp-service-account=${GSA_EMAIL}" --overwrite

  log_info "Waiting for KRMSyncer CRD to be established..."
  kube_dest wait --for condition=established --timeout=60s "crd/${KRMSYNCER_CRD}"

  # Restart so pods pick up the ServiceAccount annotation and a newly pushed image.
  log_info "Restarting controller..."
  kube_dest -n "${CONTROLLER_NAMESPACE}" rollout restart deployment "${CONTROLLER_DEPLOYMENT}"
  kube_dest -n "${CONTROLLER_NAMESPACE}" rollout status deployment "${CONTROLLER_DEPLOYMENT}" --timeout=180s
  log_success "KRMSyncer controller is running."
}

deploy_sync_config() {
  log_info "Applying KRMSyncer ${NAMESPACE}/${SYNC_CONFIG_NAME}..."
  kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kube_dest apply -f - >/dev/null
  render_template "${TEMPLATES_DIR}/krmsyncer.yaml" \
    "SYNC_CONFIG_NAME=${SYNC_CONFIG_NAME}" \
    "NAMESPACE=${NAMESPACE}" \
    "SOURCE_PROJECT=${SOURCE_PROJECT}" \
    "SOURCE_LOCATION=${SOURCE_LOCATION}" \
    "SOURCE_CLUSTER=${SOURCE_CLUSTER}" \
    "SOURCE_ENDPOINT=${SOURCE_ENDPOINT}" | kube_dest apply -f -
  log_success "KRMSyncer configuration applied."
}

# --- Main --------------------------------------------------------------------

main() {
  parse_flags "$@"
  check_deps
  complete
  print_summary

  # Workload Identity runs before the image build: new IAM bindings can take a
  # few minutes to propagate, and the build gives them time.
  log_header "Step 1: Configure Workload Identity"
  configure_workload_identity
  log_header "Step 2: Build and Push Controller Image"
  build_and_push_image
  log_header "Step 3: Check Source Cluster"
  check_source_cluster
  log_header "Step 4: Deploy KRMSyncer Controller"
  deploy_controller
  log_header "Step 5: Deploy KRMSyncer Configuration"
  deploy_sync_config

  echo
  log_info "View controller logs with:"
  log_info "  kubectl --context=${DEST_CONTEXT} -n ${CONTROLLER_NAMESPACE} logs deploy/${CONTROLLER_DEPLOYMENT} -f"
  log_info "If the logs show the source cluster can't be reached right after setup (e.g. new IAM"
  log_info "bindings still propagating), restart the controller once they take effect:"
  log_info "  kubectl --context=${DEST_CONTEXT} -n ${CONTROLLER_NAMESPACE} rollout restart deploy/${CONTROLLER_DEPLOYMENT}"
}

main "$@"
