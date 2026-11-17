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

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTEXT="${KUBECTL_CONTEXT:-kage-adc}"
AX_BIN="${AX_BIN:-${HOME}/bin/ax}"
COUNT="${CLUSTER_COUNT:-250}"
CONCURRENCY="${CONCURRENCY:-16}"
IMAGE="${AX_KUBE_AGENT_IMAGE:?AX_KUBE_AGENT_IMAGE (pinned by @sha256:...) is required}"

echo "==> 1. Applying Gateway/kube-agents-cluster-gw on context ${CONTEXT}..."
"${AX_BIN}" apply -f "${SCRIPT_DIR}/gateway.yaml" --context="${CONTEXT}"

echo "==> 2. Creating ${COUNT} Cluster Agent Tasks (cluster-agent-001 .. cluster-agent-$(printf '%03d' "${COUNT}")) in parallel (concurrency=${CONCURRENCY})..."
START_TS="$(date +%s)"
seq 1 "${COUNT}" | xargs -P "${CONCURRENCY}" -I {} bash -ec '
  idx=$(printf "%03d" "$1")
  "$2" apply --context="$3" -f - >/dev/null <<EOF
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: cluster-agent-${idx}
  atespace: default
spec:
  debug: true
  profile:
    image: $4
  gateway:
    name: kube-agents-cluster-gw
  env:
    - name: CLUSTER_ID
      value: cluster-${idx}
EOF
' _ {} "${AX_BIN}" "${CONTEXT}" "${IMAGE}"
END_TS="$(date +%s)"
echo "    Created ${COUNT} Cluster Agent Tasks + Substrate Actors in $((END_TS - START_TS))s."
