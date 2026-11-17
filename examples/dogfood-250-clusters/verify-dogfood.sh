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
  "cluster-agent-125"
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

echo "==> 3. Verifying 2-container isolation (agent vs sandbox over 127.0.0.1:2222) and k8s-api-proxy inside ${SAMPLES[*]}..."
for agent in "${SAMPLES[@]}"; do
  echo "--- Testing inside ${agent} ---"
  "${AX_BIN}" ssh "${agent}" --context="${CONTEXT}" -- env HERMES_DOCKER_EXEC_AS_ROOT=1 sh -ec '
    echo "[Container 1: agent] hermes=$(hermes --version 2>&1 | head -n1) | skills=$(ls /root/.hermes/skills | wc -l)"
    if command -v kubectl >/dev/null 2>&1; then
      echo "FAIL: kubectl should not exist in agent container!" >&2
      exit 1
    fi
    echo "[Container 1: agent] OK: no kubectl binary in agent container"
    ssh -p 2222 -i /etc/sandbox-ssh/id_ed25519 agent@127.0.0.1 "
      set -eu
      if [ -e /root/.hermes ] || command -v hermes >/dev/null 2>&1; then
        echo \"FAIL: sandbox container must not see /root/.hermes or hermes binary!\" >&2
        exit 1
      fi
      echo \"[Container 2: sandbox over 127.0.0.1:2222] uid=\$(id -un) (\$(id -u)) | OK: /root/.hermes and hermes are isolated from sandbox\"
      echo \"[Container 2: sandbox] kubectl=\$(kubectl version --client -o yaml | awk '\''/gitVersion:/ {print \$2}'\'') | leeway=\$(leeway version 2>&1 | head -n1)\"
      kubectl get nodes --no-headers | wc -l | xargs -I{} echo \"[Container 2: sandbox] Visible cluster nodes via k8s-api-proxy: {}\"
      PW=\$(kubectl get secret ax-postgres -n ax-system -o jsonpath=\"{.data.password}\")
      echo \"[Container 2: sandbox] Redacted Secret data.password=\${PW}\"
      test \"\${PW}\" = \"UkVEQUNURUQ=\"
    "
  '
done

echo "==> 4. Running live Hermes Cluster Agent turn inside cluster-agent-001..."
"${AX_BIN}" ssh cluster-agent-001 --context="${CONTEXT}" -- env HERMES_DOCKER_EXEC_AS_ROOT=1 \
  hermes chat --yolo -q "Run kubectl get pods -n ax-system and report the status of each pod in one concise sentence."

echo "==> 5. Suspending ${SAMPLES[*]} back to zero compute (TASK_STATE_SUSPENDED)..."
for agent in "${SAMPLES[@]}"; do
  "${AX_BIN}" suspend task "${agent}" --context="${CONTEXT}"
done
