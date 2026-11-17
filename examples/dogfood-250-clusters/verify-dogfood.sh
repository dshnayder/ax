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

CONTEXT="${KUBECTL_CONTEXT:-kage-adc}"
AX_BIN="${AX_BIN:-${HOME}/bin/ax}"
SAMPLES=(
  "cluster-agent-001"
  "cluster-agent-050"
  "cluster-agent-100"
  "cluster-agent-150"
  "cluster-agent-200"
  "cluster-agent-250"
)

echo "==> 1. Verifying 250 Cluster Agent Tasks (sharing 1 ActorTemplate) and 3 Worker Pods..."
kubectl --context="${CONTEXT}" -n ate-system get pods -l ate.dev/worker-pool=gvisor-pool -o wide
kubectl --context="${CONTEXT}" -n ax-system exec statefulset/ax-postgres -- \
  psql -U ax -d ax -c "SELECT count(*) AS total_cluster_agents, count(DISTINCT data->'status'->>'ateActorTemplate') AS distinct_templates, min(data->'status'->>'ateActorTemplate') AS shared_template FROM tasks WHERE name LIKE 'cluster-agent-%' AND name != 'cluster-agent-seed';"

echo "==> 2. Resuming ${#SAMPLES[@]} Cluster Agents concurrently (${SAMPLES[*]}) across the 3 Workers..."
for agent in "${SAMPLES[@]}"; do
  "${AX_BIN}" resume task "${agent}" --context="${CONTEXT}" &
done
wait

for agent in "${SAMPLES[@]}"; do
  echo "--- ${agent} status ---"
  "${AX_BIN}" describe task "${agent}" --context="${CONTEXT}" | grep -E 'Name:|State:|Actor:|Template:|WorkspaceReady|Ready'
done

echo "==> 3. Verifying kube-agents CLIs, skills, and k8s-api-proxy inside ${SAMPLES[*]}..."
for agent in "${SAMPLES[@]}"; do
  echo "--- Testing inside ${agent} ---"
  "${AX_BIN}" ssh "${agent}" --context="${CONTEXT}" -- env HERMES_DOCKER_EXEC_AS_ROOT=1 sh -ec '
    echo "CLUSTER_ID=${CLUSTER_ID} | hermes=$(hermes --version 2>&1 | head -n1) | kubectl=$(kubectl version --client -o yaml | awk "/gitVersion:/ {print \$2}") | leeway=$(leeway version 2>&1 | head -n1)"
    echo "Skills installed: $(ls /root/.hermes/skills | wc -l) skills"
    kubectl get nodes --no-headers | wc -l | xargs -I{} echo "Visible cluster nodes via k8s-api-proxy: {}"
    PW=$(kubectl get secret ax-postgres -n ax-system -o jsonpath="{.data.password}")
    echo "Redacted Secret data.password=${PW}"
    test "${PW}" = "UkVEQUNURUQ="
  '
done

echo "==> 4. Running live Hermes Cluster Agent turn inside cluster-agent-001..."
"${AX_BIN}" ssh cluster-agent-001 --context="${CONTEXT}" -- env HERMES_DOCKER_EXEC_AS_ROOT=1 \
  hermes chat --yolo -q "Run kubectl get pods -n ax-system and report the status of each pod in one concise sentence."

echo "==> 5. Suspending ${SAMPLES[*]} back to zero compute (TASK_STATE_SUSPENDED)..."
for agent in "${SAMPLES[@]}"; do
  "${AX_BIN}" suspend task "${agent}" --context="${CONTEXT}"
done
