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
# configures it to pull resources from the source GKE cluster. It automates
# syncer/steps.txt:
#
#   1. Configure Workload Identity: a Google Service Account (GSA) for the
#      controller with roles/container.viewer, bound to the controller's
#      Kubernetes ServiceAccount.
#   2. Build and push the controller image (skipped with --skip-build).
#   3. Store the source cluster kubeconfig in a Secret on the destination cluster.
#   4. Deploy the KRMSyncer CRD, RBAC and controller to the destination cluster.
#   5. Deploy the KRMSyncer configuration (config/templates/krmsyncer.yaml).
#      --sync-mode=status (default) syncs only status, plus spec.resourceID for
#      the GVKs listed in
#      config/templates/service-generated-resource-id-gvks.yaml;
#      --sync-mode=full syncs spec and status of all KCC resources.
#
# Nothing is created on the source cluster: read access comes from IAM
# (roles/container.viewer), not Kubernetes RBAC. The source kubeconfig uses the
# standard gke-gcloud-auth-plugin exec entry; the controller does not run the
# plugin, it recognizes it and exchanges its Workload Identity credentials for
# a token in process.
#
# The script talks to both clusters through the kubectl contexts created by
# `gcloud container clusters get-credentials` (gke_<project>_<location>_<cluster>).

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

# Secret holding the source kubeconfig, and the KRMSyncer CR rendered from
# config/templates/krmsyncer.yaml that references it.
SOURCE_SECRET_NAME="source-cluster"
SYNC_CONFIG_NAME="kcc-resource-syncer"

# KCC GVKs whose rules must also sync spec.resourceID. With --sync-mode=status,
# deploy_sync_config appends one rule per entry to the rules in
# config/templates/krmsyncer.yaml.
RESOURCE_ID_GVKS_FILE="${TEMPLATES_DIR}/service-generated-resource-id-gvks.yaml"

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
resources from the source GKE cluster. Both clusters need kubectl contexts
created by \`gcloud container clusters get-credentials\`, and the destination
cluster needs Workload Identity enabled.

Flags:
      --source-cluster   Name of the source GKE cluster (required)
      --source-location  Location (region or zone) of the source cluster (required)
      --dest-cluster     Name of the destination GKE cluster (required)
      --dest-location    Location (region or zone) of the destination cluster (required)
      --project          GCP project of both clusters (required)
  -n, --namespace        Namespace for the KRMSyncer CR and source-cluster Secret
                         (default: ${CONTROLLER_NAMESPACE})
  -i, --image            Controller image (default: gcr.io/<project>/krmsyncer/controller:latest)
      --skip-build       Do not build/push the image; deploy --image as is
      --sync-mode        What to sync for KCC resources (default: status):
                           status  status only, plus spec.resourceID for GVKs with
                                   service-generated IDs (service-generated-resource-id-gvks.yaml)
                           full    spec and status
  -h, --help             Show this help

Example:
  $(basename "$0") --source-cluster src --source-location us-west1 \\
    --dest-cluster dst --dest-location us-central1 --project my-project
EOF
}

# --- Flags -------------------------------------------------------------------

SOURCE_CLUSTER=""
SOURCE_LOCATION=""
DEST_CLUSTER=""
DEST_LOCATION=""
PROJECT=""
NAMESPACE="${CONTROLLER_NAMESPACE}"
IMAGE=""
SKIP_BUILD="false"
SYNC_MODE="status"

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
      --dest-cluster)    DEST_CLUSTER="$2" ;;
      --dest-location)   DEST_LOCATION="$2" ;;
      --project)         PROJECT="$2" ;;
      -n|--namespace)    NAMESPACE="$2" ;;
      -i|--image)        IMAGE="$2" ;;
      --sync-mode)       SYNC_MODE="$2" ;;
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

  case "${SYNC_MODE}" in
    status|full) ;;
    *) usage >&2; die "invalid --sync-mode \"${SYNC_MODE}\"; must be status or full" ;;
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

  # Context names created by `gcloud container clusters get-credentials`.
  SOURCE_CONTEXT="gke_${PROJECT}_${SOURCE_LOCATION}_${SOURCE_CLUSTER}"
  DEST_CONTEXT="gke_${PROJECT}_${DEST_LOCATION}_${DEST_CLUSTER}"
  [[ "${SOURCE_CONTEXT}" != "${DEST_CONTEXT}" ]] ||
    die "source and destination are the same cluster (${DEST_CONTEXT})"

  local ctx cluster location
  for ctx in "${SOURCE_CONTEXT}" "${DEST_CONTEXT}"; do
    if ! kubectl config get-contexts "${ctx}" >/dev/null 2>&1; then
      location="${ctx#gke_"${PROJECT}"_}"
      cluster="${location#*_}"
      location="${location%%_*}"
      die "kubectl context \"${ctx}\" not found; run: gcloud container clusters get-credentials ${cluster} --location ${location} --project ${PROJECT}"
    fi
  done

  [[ -n "${IMAGE}" ]] || IMAGE="gcr.io/${PROJECT}/krmsyncer/controller:latest"

  GSA_EMAIL="${GSA_NAME}@${PROJECT}.iam.gserviceaccount.com"
  WI_MEMBER="serviceAccount:${PROJECT}.svc.id.goog[${CONTROLLER_NAMESPACE}/${CONTROLLER_KSA}]"
}

print_summary() {
  echo "=================================================="
  echo "          KRMSyncer Deployment Setup              "
  echo "=================================================="
  log_info "Source Cluster Context : ${SOURCE_CONTEXT}"
  log_info "Dest Cluster Context   : ${DEST_CONTEXT}"
  log_info "GCP Project            : ${PROJECT}"
  log_info "Syncer CR Namespace    : ${NAMESPACE}"
  log_info "Controller Namespace   : ${CONTROLLER_NAMESPACE}"
  log_info "Controller Image       : ${IMAGE}"
  log_info "Sync Mode              : ${SYNC_MODE}"
  log_info "Google Service Account : ${GSA_EMAIL}"
  echo "=================================================="
}

# --- Helpers -----------------------------------------------------------------

kube_src()  { kubectl --context="${SOURCE_CONTEXT}" "$@"; }
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

# print_resource_id_rule prints one KRMSyncer rule that syncs status and
# spec.resourceID, indented to continue the spec.rules list of
# config/templates/krmsyncer.yaml.
# Usage: print_resource_id_rule <file> <group> <version> <kind>
print_resource_id_rule() {
  [[ -n "$2" && -n "$3" && -n "$4" ]] ||
    die "entry in $1 is missing group, version or kind (group=\"$2\" version=\"$3\" kind=\"$4\")"
  printf '  - group: %s\n    version: %s\n    kind: %s\n    syncFields:\n    - status\n    - spec.resourceID\n' \
    "$2" "$3" "$4"
}

# render_resource_id_rules prints a status + spec.resourceID rule for each entry
# of a YAML file holding a list of block-style {group, version, kind} mappings:
#   - group: <group>
#     version: <version>
#     kind: <kind>
# Keys may appear in any order and values may be quoted. '#' comments and blank
# lines are ignored. The file is parsed without a YAML library, so other YAML
# syntax (flow style, anchors, multi-line values) is rejected.
# Usage: render_resource_id_rules <file>
render_resource_id_rules() {
  local file="$1" line group="" version="" kind="" in_entry="false"
  local kv_re='^[[:space:]]*([A-Za-z]+):[[:space:]]*["'"'"']?([^"'"'"'[:space:]]+)["'"'"']?[[:space:]]*$'
  [[ -f "${file}" ]] || die "GVK file ${file} not found"
  while IFS= read -r line || [[ -n "${line}" ]]; do
    line="${line%%#*}"
    [[ "${line}" =~ ^[[:space:]]*$ ]] && continue
    if [[ "${line}" =~ ^-[[:space:]]+(.*)$ ]]; then
      # Start of a new list entry; flush the previous one.
      if [[ "${in_entry}" == "true" ]]; then
        print_resource_id_rule "${file}" "${group}" "${version}" "${kind}"
      fi
      group="" version="" kind="" in_entry="true"
      line="${BASH_REMATCH[1]}"
    elif [[ "${in_entry}" == "false" || ! "${line}" =~ ^[[:space:]] ]]; then
      die "unexpected line \"${line}\" in ${file}; expected a list of group/version/kind entries"
    fi
    [[ "${line}" =~ ${kv_re} ]] ||
      die "unexpected line \"${line}\" in ${file}; expected \"<key>: <value>\""
    case "${BASH_REMATCH[1]}" in
      group)   group="${BASH_REMATCH[2]}" ;;
      version) version="${BASH_REMATCH[2]}" ;;
      kind)    kind="${BASH_REMATCH[2]}" ;;
      *) die "unknown key \"${BASH_REMATCH[1]}\" in ${file}; expected group, version or kind" ;;
    esac
  done <"${file}"
  if [[ "${in_entry}" == "true" ]]; then
    print_resource_id_rule "${file}" "${group}" "${version}" "${kind}"
  fi
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

  # roles/container.viewer grants read-only access to Kubernetes objects in
  # all GKE clusters of the project, so no RBAC objects are needed on the
  # source cluster.
  log_info "Granting roles/container.viewer on project ${PROJECT} to ${GSA_EMAIL}..."
  quiet gcloud projects add-iam-policy-binding "${PROJECT}" \
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

configure_remote_access() {
  log_info "Verifying cluster connectivity..."
  kube_src get --raw /version >/dev/null ||
    die "cannot reach source cluster with context \"${SOURCE_CONTEXT}\""
  kube_dest get --raw /version >/dev/null ||
    die "cannot reach destination cluster with context \"${DEST_CONTEXT}\""
  log_success "Both clusters are reachable."

  # Nothing is created on the source cluster; the controller reads it with the
  # GSA's IAM permissions (roles/container.viewer).
  local server ca_data
  server="$(kubectl config view --minify --flatten --context="${SOURCE_CONTEXT}" \
    -o jsonpath='{.clusters[0].cluster.server}')"
  ca_data="$(kubectl config view --minify --flatten --context="${SOURCE_CONTEXT}" \
    -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
  [[ -n "${server}" ]] || die "could not determine API server URL from context \"${SOURCE_CONTEXT}\""
  [[ -n "${ca_data}" ]] || die "no certificate-authority-data in context \"${SOURCE_CONTEXT}\""
  log_info "Source API server: ${server}"

  # Same shape as `kubectl config view --minify --flatten` for a GKE context.
  # The controller replaces the gke-gcloud-auth-plugin entry with an
  # in-process token exchange using its Workload Identity credentials.
  local kubeconfig
  kubeconfig="$(cat <<EOF
apiVersion: v1
kind: Config
clusters:
- name: source
  cluster:
    server: ${server}
    certificate-authority-data: ${ca_data}
contexts:
- name: source
  context:
    cluster: source
    user: source
current-context: source
users:
- name: source
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: gke-gcloud-auth-plugin
      provideClusterInfo: true
      interactiveMode: Never
EOF
)"

  log_info "Creating Secret ${NAMESPACE}/${SOURCE_SECRET_NAME} on destination cluster..."
  kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kube_dest apply -f -
  kubectl create secret generic "${SOURCE_SECRET_NAME}" -n "${NAMESPACE}" \
    --from-literal=kubeconfig="${kubeconfig}" \
    --dry-run=client -o yaml | kube_dest apply -f - >/dev/null
  log_success "Secret ${NAMESPACE}/${SOURCE_SECRET_NAME} created."
}

deploy_controller() {
  local config_dir="${MODULE_DIR}/config/default"
  log_info "Rendering manifests from ${config_dir}..."
  local manifests
  manifests="$(kubectl kustomize "${config_dir}")"
  [[ "${manifests}" == *"image: ${IMAGE_PLACEHOLDER}"* ]] ||
    die "image \"${IMAGE_PLACEHOLDER}\" not found in rendered manifests; check config/manager/manager.yaml"

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
  log_info "Applying KRMSyncer ${NAMESPACE}/${SYNC_CONFIG_NAME} (sync mode: ${SYNC_MODE})..."
  local sync_fields="[status]"
  [[ "${SYNC_MODE}" == "full" ]] && sync_fields="[spec, status]"

  # Render into a variable first so an invalid GVK entry aborts before applying.
  local manifest
  manifest="$(render_template "${TEMPLATES_DIR}/krmsyncer.yaml" \
    "SYNC_CONFIG_NAME=${SYNC_CONFIG_NAME}" \
    "NAMESPACE=${NAMESPACE}" \
    "SOURCE_SECRET_NAME=${SOURCE_SECRET_NAME}" \
    "SYNC_FIELDS=${sync_fields}")"

  # In status mode, append the spec.resourceID rules. spec.rules is the last
  # field of the template, so they can be appended to the end. They must come
  # after the wildcard status-only rule: the controller applies every matching
  # rule in order with the same SSA field manager, so a later status-only apply
  # would drop spec.resourceID. They are skipped in full mode, where a later
  # [status, spec.resourceID] apply would drop every other spec field.
  if [[ "${SYNC_MODE}" == "status" ]]; then
    manifest+=$'\n'"$(render_resource_id_rules "${RESOURCE_ID_GVKS_FILE}")"
  fi
  printf '%s\n' "${manifest}" | kube_dest apply -f -
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
  log_header "Step 3: Configure Remote Access to Source Cluster"
  configure_remote_access
  log_header "Step 4: Deploy KRMSyncer Controller"
  deploy_controller
  log_header "Step 5: Deploy KRMSyncer Configuration"
  deploy_sync_config

  echo
  log_info "View controller logs with:"
  log_info "  kubectl --context=${DEST_CONTEXT} -n ${CONTROLLER_NAMESPACE} logs deploy/${CONTROLLER_DEPLOYMENT} -f"
}

main "$@"
