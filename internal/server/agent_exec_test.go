// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	envguest "github.com/agent-substrate/env/guest"
	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/internal/substrate/substratetest"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRouteClusterAgentPrompt(t *testing.T) {
	tests := []struct {
		name        string
		defaultTask string
		prompt      string
		wantTask    string
		wantPrompt  string
	}{
		{
			name:        "no cluster agent prefix",
			defaultTask: "cluster-agent-001",
			prompt:      "Check failing pods in kube-system",
			wantTask:    "cluster-agent-001",
			wantPrompt:  "Check failing pods in kube-system",
		},
		{
			name:        "space separated prefix",
			defaultTask: "router-task",
			prompt:      "@cluster-agent-001 Run kubectl get pods -n ax-system",
			wantTask:    "cluster-agent-001",
			wantPrompt:  "Run kubectl get pods -n ax-system",
		},
		{
			name:        "colon separated multiline prefix",
			defaultTask: "router-task",
			prompt:      "  @cluster-agent-125: Check nodes\nand list events",
			wantTask:    "cluster-agent-125",
			wantPrompt:  "Check nodes\nand list events",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTask, gotPrompt := server.RouteClusterAgentPrompt(tt.defaultTask, tt.prompt)
			if gotTask != tt.wantTask || gotPrompt != tt.wantPrompt {
				t.Fatalf("RouteClusterAgentPrompt(%q, %q) = (%q, %q), want (%q, %q)",
					tt.defaultTask, tt.prompt, gotTask, gotPrompt, tt.wantTask, tt.wantPrompt)
			}
		})
	}
}

func startFakeHermesGuest(t *testing.T, scriptBody string) (addr string, lastTargetActor func() string) {
	t.Helper()

	binDir := t.TempDir()
	hermesPath := filepath.Join(binDir, "hermes")
	if err := os.WriteFile(hermesPath, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write fake hermes: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var (
		mu          sync.Mutex
		targetActor string
	)

	cfg := envguest.DefaultConfig()
	cfg.Workspace = t.TempDir()
	cfg.LogDir = t.TempDir()
	guestSrv, cleanup, err := envguest.NewServer(cfg)
	if err != nil {
		t.Fatalf("new guest server: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	h2Srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if v := r.Header.Get("Ate-Target-Actor"); v != "" {
				mu.Lock()
				targetActor = v
				mu.Unlock()
			}
			guestSrv.ServeHTTP(w, r)
		}),
	}
	h2Srv.Protocols = new(http.Protocols)
	h2Srv.Protocols.SetHTTP1(true)
	h2Srv.Protocols.SetUnencryptedHTTP2(true)
	go func() { _ = h2Srv.Serve(lis) }()
	t.Cleanup(func() {
		_ = h2Srv.Close()
		guestSrv.Stop()
		cleanup()
		_ = lis.Close()
	})

	return lis.Addr().String(), func() string {
		mu.Lock()
		defer mu.Unlock()
		return targetActor
	}
}

func startReadyProbeServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestHandleTaskChatCompletions_RoutesAutoCreatesAndSuspends(t *testing.T) {
	guestAddr, lastTargetActor := startFakeHermesGuest(t, `#!/bin/sh
set -eu
# Verify expected flags: chat -Q --yolo -q <prompt>
if [ "$1" != "chat" ] || [ "$2" != "-Q" ] || [ "$3" != "--yolo" ] || [ "$4" != "-q" ]; then
  echo "unexpected args: $*" >&2
  exit 2
fi
if [ "${HERMES_DOCKER_EXEC_AS_ROOT:-}" != "1" ]; then
  echo "missing HERMES_DOCKER_EXEC_AS_ROOT" >&2
  exit 3
fi
printf '\033[32mCluster reply for: %s\033[0m\n' "$5"
printf '\nsession_id: sess-test-123\n' >&2
`)
	t.Setenv("ATENET_ROUTER_ADDR", "http://"+guestAddr)

	st := memory.NewStore()
	sub := substratetest.NewServer(t)
	sub.SetWorkerIP(startReadyProbeServer(t))
	srv := server.NewServer(st, newTestReconciler(sub))
	ctx := context.Background()

	// Create Gateway and cluster-agent-seed so lazy creation resolves shared AteActorTemplate.
	if _, err := srv.CreateGateway(ctx, &v1alpha1.CreateGatewayRequest{
		Gateway: &v1alpha1.Gateway{
			Metadata: &v1alpha1.ObjectMeta{Atespace: "default", Name: "kube-agents-cluster-gw"},
			Spec:     &v1alpha1.GatewaySpec{},
		},
	}); err != nil {
		t.Fatalf("CreateGateway: %v", err)
	}
	seed, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Atespace: "default", Name: "cluster-agent-seed"},
			Spec: &v1alpha1.TaskSpec{
				Debug:   true,
				Profile: &v1alpha1.Profile{Image: "example.com/kube-agent:test"},
				Gateway: &v1alpha1.GatewayRef{Name: "kube-agents-cluster-gw"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask(seed): %v", err)
	}

	reqBody, _ := json.Marshal(map[string]any{
		"model": "gemini-4-pro",
		"messages": []map[string]any{
			{"role": "system", "content": "You are an SRE assistant."},
			{"role": "user", "content": "@cluster-agent-125: List pods in ax-system"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1alpha1/tasks/default/cluster-agent-001/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hermes-Session-Id", "sid-abc")
	req.Header.Set("X-Hermes-Session-Key", "gchat:thread-42")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Hermes-Session-Id"); got != "sid-abc" {
		t.Errorf("X-Hermes-Session-Id = %q, want %q", got, "sid-abc")
	}
	if got := rec.Header().Get("X-Hermes-Session-Key"); got != "gchat:thread-42" {
		t.Errorf("X-Hermes-Session-Key = %q, want %q", got, "gchat:thread-42")
	}

	var resp struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.ID != "chatcmpl-ax" || resp.Object != "chat.completion" || resp.Model != "gemini-4-pro" {
		t.Errorf("unexpected response metadata: %+v", resp)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "Cluster reply for: List pods in ax-system" {
		t.Fatalf("choices = %+v, want stripped prompt response without ANSI codes", resp.Choices)
	}
	if got := lastTargetActor(); got != "default/cluster-agent-125" {
		t.Errorf("ate-target-actor = %q, want %q", got, "default/cluster-agent-125")
	}

	// Verify cluster-agent-125 shares seed's template and auto-suspended back to SUSPENDED.
	routedTask, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: "default", Name: "cluster-agent-125"})
	if err != nil {
		t.Fatalf("GetTask(cluster-agent-125): %v", err)
	}
	if routedTask.GetStatus().GetAteActorTemplate() != seed.GetStatus().GetAteActorTemplate() {
		t.Errorf("ateActorTemplate = %q, want %q", routedTask.GetStatus().GetAteActorTemplate(), seed.GetStatus().GetAteActorTemplate())
	}
	if routedTask.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
		t.Errorf("post-turn state = %v, want TASK_STATE_SUSPENDED", routedTask.GetStatus().GetState())
	}
	if got := srv.ActiveTurnCount("default", "cluster-agent-125"); got != 0 {
		t.Errorf("ActiveTurnCount = %d, want 0", got)
	}
}

func TestHandleTaskExec_RetriesTransientResumeError(t *testing.T) {
	guestAddr, _ := startFakeHermesGuest(t, `#!/bin/sh
printf 'ok: %s\n' "$5"
`)
	t.Setenv("ATENET_ROUTER_ADDR", "http://"+guestAddr)

	st := memory.NewStore()
	sub := substratetest.NewServer(t)
	sub.SetWorkerIP(startReadyProbeServer(t))
	srv := server.NewServer(st, newTestReconciler(sub))
	ctx := context.Background()

	if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Atespace: "default", Name: "cluster-agent-001"},
			Spec: &v1alpha1.TaskSpec{
				Debug:   true,
				Profile: &v1alpha1.Profile{Image: "example.com/kube-agent:test"},
			},
		},
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Inject transient ResourceExhausted briefly, clearing it asynchronously while resume retries.
	sub.SetErrors(substratetest.Errors{
		ResumeActor: status.Error(codes.ResourceExhausted, "no worker has room for the actor"),
	})
	go func() {
		sub.SetErrors(substratetest.Errors{})
	}()

	reqBody, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "Check health"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1alpha1/tasks/default/cluster-agent-001:exec", bytes.NewReader(reqBody))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ok: Check health") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestHandleTaskChatCompletions_RefcountedConcurrentTurns(t *testing.T) {
	gateFile := filepath.Join(t.TempDir(), "release")
	startedDir := t.TempDir()
	guestAddr, _ := startFakeHermesGuest(t, `#!/bin/sh
set -eu
touch "`+startedDir+`/turn-$$"
while [ ! -f "`+gateFile+`" ]; do
  sleep 0.02
done
printf 'done: %s\n' "$5"
`)
	t.Setenv("ATENET_ROUTER_ADDR", "http://"+guestAddr)

	st := memory.NewStore()
	sub := substratetest.NewServer(t)
	sub.SetWorkerIP(startReadyProbeServer(t))
	srv := server.NewServer(st, newTestReconciler(sub))
	ctx := context.Background()

	if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Atespace: "default", Name: "cluster-agent-002"},
			Spec: &v1alpha1.TaskSpec{
				Debug:   true,
				Profile: &v1alpha1.Profile{Image: "example.com/kube-agent:test"},
			},
		},
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]any{
				"messages": []map[string]any{
					{"role": "user", "content": "@cluster-agent-002 check status"},
				},
			})
			req := httptest.NewRequest(http.MethodPost, "/v1alpha1/tasks/default/cluster-agent-002/v1/chat/completions", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			codes[idx] = rec.Code
		}(i)
	}

	// Wait until both turns are active inside the guest, verify ActiveTurnCount == 2 and WORKING.
	for {
		entries, _ := os.ReadDir(startedDir)
		if len(entries) == 2 {
			break
		}
	}
	if got := srv.ActiveTurnCount("default", "cluster-agent-002"); got != 2 {
		t.Fatalf("ActiveTurnCount during concurrent turns = %d, want 2", got)
	}
	liveTask, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: "default", Name: "cluster-agent-002"})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if liveTask.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_WORKING {
		t.Fatalf("state during concurrent turns = %v, want TASK_STATE_WORKING", liveTask.GetStatus().GetState())
	}

	// Release both turns and verify task suspends cleanly once activeTurns drops to 0.
	if err := os.WriteFile(gateFile, []byte("go"), 0o644); err != nil {
		t.Fatalf("write gate file: %v", err)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("turn %d HTTP status = %d, want 200", i, c)
		}
	}
	finalTask, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: "default", Name: "cluster-agent-002"})
	if err != nil {
		t.Fatalf("GetTask final: %v", err)
	}
	if finalTask.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
		t.Fatalf("final state = %v, want TASK_STATE_SUSPENDED", finalTask.GetStatus().GetState())
	}
}
