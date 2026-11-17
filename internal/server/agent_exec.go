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

package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/ax/internal/guest"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultAtenetRouterAddr  = "atenet-router.ate-system.svc.cluster.local:443"
	atenetRouterServerName   = "atenet-router.ate-system.svc"
	podIdentityBundlePath    = "/run/podidentity.podcert.ate.dev/credential-bundle.pem"
	serviceDNSTrustPath      = "/run/servicedns-ca/trust-bundle.pem"
	defaultClusterGateway    = "kube-agents-cluster-gw"
	taskReadyTimeout         = 30 * time.Second
	taskReadyPollInterval    = 250 * time.Millisecond
	autoSuspendTimeout       = 30 * time.Second
	hermesSessionIDHeader    = "X-Hermes-Session-Id"
	hermesSessionKeyHeader   = "X-Hermes-Session-Key"
	maxChatCompletionBodyLen = 4 << 20 // 4 MiB
)

var (
	clusterAgentPrefixRe = regexp.MustCompile(`(?s)^\s*@(cluster-agent-[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)(?:[:\s]+(.*))?$`)
	ansiEscapeRe         = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
)

type chatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Prompt   string        `json:"prompt,omitempty"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type chatContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type chatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Model   string                 `json:"model"`
	Choices []chatCompletionChoice `json:"choices"`
}

type chatCompletionChoice struct {
	Index        int               `json:"index"`
	Message      chatChoiceMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type chatChoiceMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// routeTaskHTTP matches and dispatches task execution HTTP endpoints.
func (s *Server) routeTaskHTTP(w http.ResponseWriter, r *http.Request) bool {
	const prefix = "/v1alpha1/tasks/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)

	const chatSuffix = "/v1/chat/completions"
	if strings.HasSuffix(rest, chatSuffix) {
		atespace, name, ok := splitTaskRef(strings.TrimSuffix(rest, chatSuffix))
		if !ok {
			return false
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return true
		}
		s.handleTaskChatCompletions(w, r, atespace, name)
		return true
	}

	const execSuffix = ":exec"
	if strings.HasSuffix(rest, execSuffix) {
		atespace, name, ok := splitTaskRef(strings.TrimSuffix(rest, execSuffix))
		if !ok {
			return false
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return true
		}
		s.handleTaskExec(w, r, atespace, name)
		return true
	}

	return false
}

func splitTaskRef(raw string) (atespace, name string, ok bool) {
	parts := strings.Split(raw, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	ns, err := url.PathUnescape(parts[0])
	if err != nil || ns == "" {
		return "", "", false
	}
	taskName, err := url.PathUnescape(parts[1])
	if err != nil || taskName == "" {
		return "", "", false
	}
	return ns, taskName, true
}

// RouteClusterAgentPrompt resolves @cluster-agent-NNN prefixes to the target task
// and strips the prefix from the prompt.
func RouteClusterAgentPrompt(defaultTask, prompt string) (targetTask, cleanPrompt string) {
	m := clusterAgentPrefixRe.FindStringSubmatch(prompt)
	if len(m) != 3 {
		return defaultTask, prompt
	}
	return m[1], strings.TrimSpace(m[2])
}

func (s *Server) handleTaskChatCompletions(w http.ResponseWriter, r *http.Request, atespace, name string) {
	s.serveTaskTurn(w, r, atespace, name)
}

func (s *Server) handleTaskExec(w http.ResponseWriter, r *http.Request, atespace, name string) {
	s.serveTaskTurn(w, r, atespace, name)
}

func (s *Server) serveTaskTurn(w http.ResponseWriter, r *http.Request, atespace, name string) {
	var req chatCompletionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChatCompletionBodyLen))
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON request: %v", err))
		return
	}

	prompt := strings.TrimSpace(req.Prompt)
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			prompt = strings.TrimSpace(extractMessageContent(req.Messages[i].Content))
			break
		}
	}
	if prompt == "" {
		writeJSONError(w, http.StatusBadRequest, "request requires a non-empty user message")
		return
	}

	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	targetTask, strippedPrompt := RouteClusterAgentPrompt(name, prompt)
	if strippedPrompt == "" {
		writeJSONError(w, http.StatusBadRequest, "prompt after @cluster-agent prefix cannot be empty")
		return
	}

	reqModel := strings.TrimSpace(req.Model)
	if reqModel == "" {
		reqModel = "model-default"
	}
	sessionID := r.Header.Get(hermesSessionIDHeader)
	sessionKey := r.Header.Get(hermesSessionKeyHeader)

	answer, err := s.executeHermesTurn(r.Context(), atespace, targetTask, strippedPrompt, sessionID, sessionKey)
	if err != nil {
		slog.Error("task turn execution failed", "atespace", atespace, "task", targetTask, "error", err)
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}

	if sessionID != "" {
		w.Header().Set(hermesSessionIDHeader, sessionID)
	}
	if sessionKey != "" {
		w.Header().Set(hermesSessionKeyHeader, sessionKey)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(chatCompletionResponse{
		ID:     "chatcmpl-ax",
		Object: "chat.completion",
		Model:  reqModel,
		Choices: []chatCompletionChoice{{
			Index: 0,
			Message: chatChoiceMessage{
				Role:    "assistant",
				Content: answer,
			},
			FinishReason: "stop",
		}},
	})
}

func (s *Server) executeHermesTurn(ctx context.Context, atespace, targetTask, prompt, sessionID, sessionKey string) (string, error) {
	key := atespace + "/" + targetTask
	lifecycleMu := s.taskLifecycleMutex(key)

	s.turnsMu.Lock()
	s.activeTurns[key]++
	s.turnsMu.Unlock()

	defer func() {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()

		s.turnsMu.Lock()
		s.activeTurns[key]--
		remaining := s.activeTurns[key]
		if remaining <= 0 {
			delete(s.activeTurns, key)
		}
		s.turnsMu.Unlock()

		if remaining == 0 {
			suspendCtx, cancel := context.WithTimeout(context.Background(), autoSuspendTimeout)
			defer cancel()
			if _, err := s.SuspendTask(suspendCtx, &v1alpha1.SuspendTaskRequest{
				Atespace: atespace,
				Name:     targetTask,
			}); err != nil {
				slog.Warn("auto-suspend failed after turn completed", "atespace", atespace, "task", targetTask, "error", err)
			}
		}
	}()

	lifecycleMu.Lock()
	task, err := s.ensureAndResumeTask(ctx, atespace, targetTask)
	lifecycleMu.Unlock()
	if err != nil {
		return "", err
	}

	actorRef := task.GetStatus().GetActor()
	if !strings.Contains(actorRef, "/") {
		actorRef = atespace + "/" + actorRef
	}

	guestClient, err := dialActorGuest(ctx, actorRef)
	if err != nil {
		return "", fmt.Errorf("dialing actor %s: %w", actorRef, err)
	}
	defer func() { _ = guestClient.Close() }()

	env := map[string]string{
		"HERMES_DOCKER_EXEC_AS_ROOT": "1",
		"HOME":                       "/root",
		"HERMES_HOME":                "/root/.hermes",
	}
	if sessionID != "" {
		env["HERMES_SESSION_ID"] = sessionID
	}
	if sessionKey != "" {
		env["HERMES_SESSION_KEY"] = sessionKey
	}

	var stdout, stderr bytes.Buffer
	exitCode, err := guestClient.Exec(ctx, guest.ExecOptions{
		Command: []string{"hermes", "chat", "-Q", "--yolo", "-q", prompt},
		Env:     env,
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	if err != nil {
		return "", fmt.Errorf("executing hermes chat in %s: %w", actorRef, err)
	}

	cleanedOut := cleanHermesOutput(stdout.String())
	if exitCode != 0 && cleanedOut == "" {
		cleanedErr := cleanHermesOutput(stderr.String())
		return "", fmt.Errorf("hermes chat exited with code %d: %s", exitCode, cleanedErr)
	}
	if cleanedOut == "" {
		cleanedOut = cleanHermesOutput(stderr.String())
	}
	return cleanedOut, nil
}

func (s *Server) taskLifecycleMutex(key string) *sync.Mutex {
	s.turnsMu.Lock()
	defer s.turnsMu.Unlock()
	mu, ok := s.lifecycleLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		s.lifecycleLocks[key] = mu
	}
	return mu
}

// ActiveTurnCount returns the current in-flight turn count for atespace/name.
func (s *Server) ActiveTurnCount(atespace, name string) int {
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	s.turnsMu.Lock()
	defer s.turnsMu.Unlock()
	return s.activeTurns[atespace+"/"+name]
}

func (s *Server) ensureAndResumeTask(ctx context.Context, atespace, targetTask string) (*v1alpha1.Task, error) {
	task, err := s.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: atespace, Name: targetTask})
	if status.Code(err) == codes.NotFound {
		sharedTmpl, tmplErr := s.resolveSharedActorTemplate(ctx, atespace)
		if tmplErr != nil {
			return nil, tmplErr
		}
		task, err = s.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
			Task: &v1alpha1.Task{
				Metadata: &v1alpha1.ObjectMeta{
					Atespace: atespace,
					Name:     targetTask,
				},
				Spec: &v1alpha1.TaskSpec{
					Debug: true,
					Profile: &v1alpha1.Profile{
						AteActorTemplate: sharedTmpl,
					},
					Gateway: &v1alpha1.GatewayRef{
						Name: defaultClusterGateway,
					},
				},
			},
		})
	}
	if err != nil {
		return nil, fmt.Errorf("getting or creating task %s/%s: %w", atespace, targetTask, err)
	}

	pollCtx, cancel := context.WithTimeout(ctx, taskReadyTimeout)
	defer cancel()

	if task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_WORKING {
		task, err = s.resumeTaskWithRetry(pollCtx, atespace, targetTask)
		if err != nil {
			return nil, err
		}
	}

	if isTaskWorkingAndReady(task) {
		return task, nil
	}

	ticker := time.NewTicker(taskReadyPollInterval)
	defer ticker.Stop()

	ticks := 0
	for {
		select {
		case <-pollCtx.Done():
			return nil, fmt.Errorf("timed out waiting for task %s/%s to become working and ready (state=%s): %w",
				atespace, targetTask, task.GetStatus().GetState().String(), pollCtx.Err())
		case <-ticker.C:
			ticks++
			cur, getErr := s.GetTask(pollCtx, &v1alpha1.GetTaskRequest{Atespace: atespace, Name: targetTask})
			if getErr == nil {
				task = cur
				if isTaskWorkingAndReady(task) {
					return task, nil
				}
				// Re-run ResumeTask every second when the actor is running while WorkspaceReady is still False.
				if ticks%4 == 0 && task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_WORKING ||
					(ticks%4 == 0 && !conditionTrue(task, v1alpha1.ConditionWorkspaceReady)) {
					if resumed, resErr := s.ResumeTask(pollCtx, &v1alpha1.ResumeTaskRequest{Atespace: atespace, Name: targetTask}); resErr == nil {
						task = resumed
						if isTaskWorkingAndReady(task) {
							return task, nil
						}
					}
				}
			}
		}
	}
}

func (s *Server) resolveSharedActorTemplate(ctx context.Context, atespace string) (string, error) {
	if tmpl := strings.TrimSpace(os.Getenv("AX_SHARED_ACTOR_TEMPLATE")); tmpl != "" {
		return tmpl, nil
	}
	for _, candidate := range []string{"cluster-agent-seed", "cluster-agent-001"} {
		seed, err := s.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: atespace, Name: candidate})
		if err == nil {
			if tmpl := strings.TrimSpace(seed.GetStatus().GetAteActorTemplate()); tmpl != "" {
				return tmpl, nil
			}
		}
	}
	return "", fmt.Errorf("shared actor template not found in AX_SHARED_ACTOR_TEMPLATE, cluster-agent-seed, or cluster-agent-001")
}

func (s *Server) resumeTaskWithRetry(ctx context.Context, atespace, targetTask string) (*v1alpha1.Task, error) {
	backoff := 200 * time.Millisecond
	for {
		task, err := s.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{
			Atespace: atespace,
			Name:     targetTask,
		})
		if err == nil {
			return task, nil
		}
		if !isRetryableResumeError(err) {
			return nil, fmt.Errorf("resuming task %s/%s: %w", atespace, targetTask, err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("resuming task %s/%s after retries: %w", atespace, targetTask, err)
		case <-time.After(backoff):
			if backoff < 2*time.Second {
				backoff *= 2
			}
		}
	}
}

func isRetryableResumeError(err error) bool {
	if err == nil {
		return false
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.ResourceExhausted, codes.Aborted:
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "ResourceExhausted") || strings.Contains(msg, "Unavailable")
}

func isTaskWorkingAndReady(task *v1alpha1.Task) bool {
	return task.GetStatus().GetState() == v1alpha1.TaskState_TASK_STATE_WORKING &&
		task.GetStatus().GetActor() != "" &&
		conditionTrue(task, v1alpha1.ConditionReady)
}

func dialActorGuest(_ context.Context, targetActor string) (*guest.Client, error) {
	routerAddr := strings.TrimSpace(os.Getenv("ATENET_ROUTER_ADDR"))
	if routerAddr == "" {
		routerAddr = defaultAtenetRouterAddr
	}
	if strings.HasPrefix(routerAddr, "http://") {
		return guest.DialTarget(routerAddr, targetActor)
	}

	_, certStatErr := os.Stat(podIdentityBundlePath)
	_, caStatErr := os.Stat(serviceDNSTrustPath)
	if certStatErr != nil || caStatErr != nil {
		return guest.DialTarget(routerAddr, targetActor)
	}

	cert, err := tls.LoadX509KeyPair(podIdentityBundlePath, podIdentityBundlePath)
	if err != nil {
		return nil, fmt.Errorf("loading podidentity client certificate %s: %w", podIdentityBundlePath, err)
	}
	caPEM, err := os.ReadFile(serviceDNSTrustPath)
	if err != nil {
		return nil, fmt.Errorf("reading servicedns trust bundle %s: %w", serviceDNSTrustPath, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("servicedns trust bundle %s contains no valid certificates", serviceDNSTrustPath)
	}

	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      roots,
		Certificates: []tls.Certificate{cert},
		ServerName:   atenetRouterServerName,
	}
	return guest.DialTargetTLS(routerAddr, targetActor, tlsCfg)
}

func extractMessageContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []chatContentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "" || p.Type == "text" {
				if b.Len() > 0 && p.Text != "" {
					b.WriteByte('\n')
				}
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

func cleanHermesOutput(out string) string {
	cleaned := ansiEscapeRe.ReplaceAllString(out, "")
	lines := strings.Split(cleaned, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "session_id:") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.TrimSpace(strings.Join(filtered, "\n"))
}

func writeJSONError(w http.ResponseWriter, statusCode int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "ax_task_exec_error",
		},
	})
}
