#!/bin/bash

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

# Configuration
METRICS_URL="http://localhost:8080/metrics"
SYSTEM_NAMESPACE="cnrm-system"
SCRIPT_DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"
GEN_SCRIPT="$SCRIPT_DIR/generate_resources.go"

# Default Values
COUNT=50
KINDS="ArtifactRegistryRepository"
NUM_KINDS=0
USER_TIMEOUT=0
SOURCE_CTX=${SOURCE_CONTEXT:-$(kubectl config current-context)}
DEST_CTX=${DEST_CONTEXT:-$(kubectl config current-context)}
NAMESPACE=${SOURCE_NAMESPACE:-"default"}

usage() {
    echo "Usage: $0 [options]"
    echo ""
    echo "Options:"
    echo "  -c, --count <int>          Total number of resources to generate (default: 50)"
    echo "  -k, --kinds <string>       Comma separated list of resource kinds (default: ArtifactRegistryRepository)"
    echo "  -n, --num-kinds <int>      Pick N random kinds from the pool (default: 0)"
    echo "  -t, --timeout <int>        Benchmark timeout in seconds (default: calculated)"
    echo "  -s, --source-context <str> Kubernetes context for Source cluster"
    echo "  -d, --dest-context <str>   Kubernetes context for Destination cluster"
    echo "  -ns, --namespace <str>     Namespace (default: default)"
    echo "  -h, --help                 Show this help message"
    exit 0
}

# Parse Flags
while [[ $# -gt 0 ]]; do
    case $1 in
        -c|--count) COUNT="$2"; shift 2 ;;
        -k|--kinds) KINDS="$2"; shift 2 ;;
        -n|--num-kinds) NUM_KINDS="$2"; shift 2 ;;
        -t|--timeout) USER_TIMEOUT="$2"; shift 2 ;;
        -s|--source-context) SOURCE_CTX="$2"; shift 2 ;;
        -d|--dest-context) DEST_CTX="$2"; shift 2 ;;
        -ns|--namespace) NAMESPACE="$2"; shift 2 ;;
        -h|--help) usage ;;
        *) echo "Unknown option: $1"; usage ;;
    esac
done

# Function to draw progress bar
draw_progress_bar() {
    local current=$1
    local total=$2
    local percent=$(( (current * 100) / total ))
    local completed=$(( (percent * 40) / 100 ))
    local remaining=$(( 40 - completed ))
    
    printf "\rProgress: ["
    printf "%${completed}s" | tr ' ' '='
    printf ">"
    printf "%${remaining}s" | tr ' ' ' '
    printf "] %d%% (%d/%d)" "$percent" "$current" "$total"
}

echo "--- Configuration ---"
echo "Source Context:   $SOURCE_CTX"
echo "Dest Context:     $DEST_CTX"
echo "Resource Count:   $COUNT"
echo "----------------------"

if [ ! -f "$GEN_SCRIPT" ]; then
    echo "Error: Generator script not found at $GEN_SCRIPT"; exit 1
fi

get_success_count() {
    curl -s --connect-timeout 2 "$METRICS_URL" | \
    grep "^syncer_sync_success_total{" | \
    grep 'operation="apply"' | \
    awk '{sum += $NF} END {print sum + 0}'
}

cleanup() {
    if [ -n "${PF_PID:-}" ]; then
        kill "$PF_PID" > /dev/null 2>&1 || true
    fi
    if [ -n "${RES_FILE:-}" ]; then
        rm -f "$RES_FILE"
    fi
    # Ensure syncer is resumed on exit
    kubectl --context "$DEST_CTX" -n "$SYSTEM_NAMESPACE" patch krmsyncer test-syncer -p '{"spec":{"suspend":false}}' --type=merge > /dev/null 2>&1 || true
}
trap cleanup EXIT

if ! curl -s --connect-timeout 2 "$METRICS_URL" > /dev/null 2>&1; then
    echo "Connecting to Destination Cluster..."
    POD_NAME=$(kubectl --context "$DEST_CTX" get pods -n "$SYSTEM_NAMESPACE" --no-headers 2>/dev/null | \
               grep "^syncer-controller" | grep "Running" | head -n 1 | awk '{print $1}')
    if [ -n "$POD_NAME" ]; then
        kubectl --context "$DEST_CTX" port-forward "pod/$POD_NAME" 8080:8080 -n "$SYSTEM_NAMESPACE" > /dev/null 2>&1 &
        PF_PID=$!
        sleep 2
    fi
fi

echo "Suspending syncer..."
kubectl --context "$DEST_CTX" -n "$SYSTEM_NAMESPACE" patch krmsyncer test-syncer -p '{"spec":{"suspend":true}}' --type=merge > /dev/null

PREFIX="bench-$(date +%s)"
RES_FILE="/tmp/resources-${PREFIX}.yaml"
go run "$GEN_SCRIPT" -count "$COUNT" -kinds "$KINDS" -num-kinds "$NUM_KINDS" -prefix "$PREFIX" | \
sed 's/metadata:/metadata:\n  labels:\n    batch-bench: "true"/' > "$RES_FILE"

TIMEOUT=$(( USER_TIMEOUT > 0 ? USER_TIMEOUT : (COUNT / 2) + 60 ))
START_COUNT=$(get_success_count)

echo "Applying resources..."
kubectl --context "$SOURCE_CTX" apply -f "$RES_FILE" --server-side --force-conflicts > /dev/null

echo "Applying resources status..."
kubectl --context "$SOURCE_CTX" apply -f "$RES_FILE" --server-side --subresource=status --force-conflicts > /dev/null

echo "Resuming syncer and starting timer..."
kubectl --context "$DEST_CTX" -n "$SYSTEM_NAMESPACE" patch krmsyncer test-syncer -p '{"spec":{"suspend":false}}' --type=merge > /dev/null
START_TIME=$(date +%s%3N)
SYNC_START_UNIX=$(date +%s)
EXPECTED_COUNT=$((START_COUNT + COUNT))
TIMED_OUT=false

while true; do
    CURRENT_COUNT=$(get_success_count)
    DIFF=$((CURRENT_COUNT - START_COUNT))
    [ "$DIFF" -lt 0 ] && DIFF=0
    
    draw_progress_bar "$DIFF" "$COUNT"
    
    if [ "$CURRENT_COUNT" -ge "$EXPECTED_COUNT" ]; then
        END_TIME=$(date +%s%3N)
        printf "\nDone!\n"
        break
    fi
    if [ $(( $(date +%s) - SYNC_START_UNIX )) -gt "$TIMEOUT" ]; then
        printf "\nError: Timeout reached.\n"
        TIMED_OUT=true; break
    fi
    sleep 0.5
done

if [ "$TIMED_OUT" = false ]; then
    TOTAL_MS=$((END_TIME - START_TIME))

    echo "-------------------------------------------"
    echo "Batch Latency: ${TOTAL_MS}ms"
fi

echo "Cleaning up..."
ALL_RESOURCES=$(kubectl --context "$SOURCE_CTX" api-resources --verbs=list --namespaced=true -o name 2>/dev/null | paste -sd, -)
if [ -n "$ALL_RESOURCES" ]; then
    kubectl --context "$SOURCE_CTX" delete "$ALL_RESOURCES" -l batch-bench=true -n "$NAMESPACE" --ignore-not-found --wait=false > /dev/null 2>&1 || true
fi

[ "$TIMED_OUT" = true ] && exit 1
exit 0
