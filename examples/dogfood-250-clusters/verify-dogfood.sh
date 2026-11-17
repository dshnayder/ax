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
    META_AUTH_CODE=$(curl -s -o /dev/null -w "%{http_code}" -H "X-AX-Metadata-Token: $(cat /ax/metadata-token)" http://127.0.0.1:8099/metadata/v1alpha1/ax/task)
    test "${META_AUTH_CODE}" = "200"
    echo "[Container 1: agent] OK: no kubectl binary in agent container; /ax/metadata-token -> HTTP ${META_AUTH_CODE}"
    ssh -T -p 2222 -i /etc/sandbox-ssh/id_ed25519 agent@127.0.0.1 "sh -eu" << "SANDBOX_EOF"
      if [ -e /root/.hermes ] || command -v hermes >/dev/null 2>&1 || [ -e /ax/metadata-token ]; then
        echo "FAIL: sandbox container must not see /root/.hermes, hermes binary, or /ax/metadata-token!" >&2
        exit 1
      fi
      echo "[Container 2: sandbox over 127.0.0.1:2222] uid=$(id -un) ($(id -u)) | OK: /root/.hermes, hermes, and /ax/metadata-token isolated"
      META_CODE=$(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:8099/metadata/v1alpha1/ax/task)
      echo "[Container 2: sandbox] Loopback metadata 127.0.0.1:8099 HTTP status: ${META_CODE}"
      test "${META_CODE}" = "403"
      GRPC_CODE=$(curl -s -o /dev/null -w "%{http_code}" --http2-prior-knowledge -H "Content-Type: application/grpc" http://127.0.0.1:80/ateenv.v1alpha.ProcessService/StartProcess)
      echo "[Container 2: sandbox] Loopback guest gRPC 127.0.0.1:80 HTTP status: ${GRPC_CODE}"
      test "${GRPC_CODE}" = "403"
      if curl -m 2 -s http://169.254.169.254 >/dev/null 2>&1; then
        echo "FAIL: 169.254.169.254 should be unreachable from sandbox!" >&2
        exit 1
      fi
      echo "[Container 2: sandbox] OK: 169.254.169.254 GKE metadata blocked"
      if env | grep -Eq "^(AX_TASK_YAML|AX_WORKSPACE_YAML)=" || tr "\0" "\n" < /proc/1/environ | grep -Eq "^(AX_TASK_YAML|AX_WORKSPACE_YAML)="; then
        echo "FAIL: AX_TASK_YAML or AX_WORKSPACE_YAML leaked into sandbox env!" >&2
        exit 1
      fi
      echo "[Container 2: sandbox] OK: AX_TASK_YAML and AX_WORKSPACE_YAML absent from env and /proc/1/environ"
      echo "[Container 2: sandbox] kubectl=$(kubectl version --client -o yaml | awk "/gitVersion:/ {print \$2}") | leeway=$(leeway version 2>&1 | head -n1)"
      kubectl get nodes --no-headers | wc -l | xargs -I{} echo "[Container 2: sandbox] Visible cluster nodes via k8s-api-proxy: {}"
      PW=$(kubectl get secret ax-postgres -n ax-system -o jsonpath="{.data.password}")
      echo "[Container 2: sandbox] Redacted Secret data.password=${PW}"
      test "${PW}" = "UkVEQUNURUQ="
SANDBOX_EOF
  '
done

echo "==> 4. Running live Hermes Cluster Agent turn inside cluster-agent-001..."
"${AX_BIN}" ssh cluster-agent-001 --context="${CONTEXT}" -- env HERMES_DOCKER_EXEC_AS_ROOT=1 \
  hermes chat --yolo -q "Run kubectl get pods -n ax-system and report the status of each pod in one concise sentence."

echo "==> 5. Suspending ${SAMPLES[*]} back to zero compute (TASK_STATE_SUSPENDED)..."
for agent in "${SAMPLES[@]}"; do
  "${AX_BIN}" suspend task "${agent}" --context="${CONTEXT}"
done
