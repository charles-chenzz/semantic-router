/*
Copyright 2025 vLLM Semantic Router.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package testcases

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vllm-project/semantic-router/e2e/pkg/helpers"
	pkgtestcases "github.com/vllm-project/semantic-router/e2e/pkg/testcases"
	"k8s.io/client-go/kubernetes"
)

const (
	handoffRequestHeader = "x-vsr-handoff-envelope"
	handoffVersionHeader = "x-vsr-handoff-version"
	handoffIDHeader      = "x-vsr-handoff-id"
	handoffStatusHeader  = "x-vsr-handoff-status"
	handoffReasonHeader  = "x-vsr-handoff-reason"
	selectedModelHeader  = "x-vsr-selected-model"
)

type handoffE2EEnvelope struct {
	Version              string   `json:"version"`
	HandoffID            string   `json:"handoff_id"`
	RootInvocationID     string   `json:"root_invocation_id"`
	ParentInvocationID   string   `json:"parent_invocation_id,omitempty"`
	DelegatedRole        string   `json:"delegated_role"`
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`
	RemainingTokens      int      `json:"remaining_tokens"`
	ContextPortability   string   `json:"context_portability"`
	ToolStateRefs        []string `json:"tool_state_refs,omitempty"`
	ExpiresAt            string   `json:"expires_at"`
}

type handoffHTTPResult struct {
	status int
	header http.Header
	body   []byte
}

func init() {
	pkgtestcases.Register("agentgateway-selection-handoff", pkgtestcases.TestCase{
		Description: "Verify trusted Chat Completions handoffs narrow selection, enforce expiry and sticky continuity, and stay out of provider requests",
		Tags:        []string{"agentgateway", "gateway", "routing", "handoff"},
		Fn:          testAgentGatewaySelectionHandoff,
	})
}

func testAgentGatewaySelectionHandoff(
	ctx context.Context,
	client *kubernetes.Clientset,
	opts pkgtestcases.TestCaseOptions,
) error {
	const (
		gatewayNamespace  = "agentgateway-system"
		gatewayService    = "agentgateway-proxy"
		gatewayLocalPort  = "18083"
		providerNamespace = "default"
		providerService   = "vllm-llama3-8b-instruct"
		providerLocalPort = "18084"
	)

	stopGateway, err := helpers.StartPortForward(
		ctx, client, opts.RestConfig, gatewayNamespace, gatewayService,
		gatewayLocalPort+":80", opts.Verbose,
	)
	if err != nil {
		return fmt.Errorf("start agentgateway port-forward: %w", err)
	}
	defer stopGateway()
	stopProvider, err := helpers.StartPortForward(
		ctx, client, opts.RestConfig, providerNamespace, providerService,
		providerLocalPort+":8000", opts.Verbose,
	)
	if err != nil {
		return fmt.Errorf("start mock provider port-forward: %w", err)
	}
	defer stopProvider()
	time.Sleep(2 * time.Second)

	httpClient := &http.Client{Timeout: 30 * time.Second}
	gatewayURL := "http://localhost:" + gatewayLocalPort + "/v1/chat/completions"
	providerDebugURL := "http://localhost:" + providerLocalPort + "/debug/last-request"
	now := time.Now().UTC()
	const (
		sessionID = "handoff-agentgateway-session"
	)

	first, firstModel, err := runCapableAgentGatewayHandoff(
		ctx, httpClient, gatewayURL, providerDebugURL, sessionID, now,
	)
	if err != nil {
		return err
	}

	impossibleResult, err := runImpossibleCapabilityHandoff(ctx, httpClient, gatewayURL, now)
	if err != nil {
		return err
	}

	expired := newHandoffE2EEnvelope("handoff-expired", "portable", now.Add(-time.Minute))
	expiredResult, err := sendAgentGatewayHandoffRequest(
		ctx, httpClient, gatewayURL, "handoff-expired-session", "handoff-expired-provider", expired,
	)
	if err != nil {
		return err
	}
	if err := requireHandoffResult(expiredResult, 422, "", "expired", "expired"); err != nil {
		return fmt.Errorf("expired handoff: %w", err)
	}

	sticky := newHandoffE2EEnvelope("handoff-sticky", "sticky", time.Now().UTC().Add(10*time.Minute))
	sticky.RequiredCapabilities = []string{"tools"}
	stickyResult, err := sendAgentGatewayHandoffRequest(
		ctx, httpClient, gatewayURL, sessionID, "handoff-sticky-provider", sticky,
	)
	if err != nil {
		return err
	}
	if err := requireHandoffResult(stickyResult, http.StatusOK, "handoff-sticky", "accepted", "constraints_applied"); err != nil {
		return fmt.Errorf("sticky handoff: %w", err)
	}
	if stickyModel := stickyResult.header.Get(selectedModelHeader); stickyModel != firstModel {
		return fmt.Errorf("sticky handoff selected %q, want previous model %q", stickyModel, firstModel)
	}

	if opts.SetDetails != nil {
		opts.SetDetails(map[string]interface{}{
			"selected_model":        firstModel,
			"accepted_status":       first.status,
			"impossible_status":     impossibleResult.status,
			"expired_status":        expiredResult.status,
			"sticky_selected_model": stickyResult.header.Get(selectedModelHeader),
		})
	}
	return nil
}

func runCapableAgentGatewayHandoff(
	ctx context.Context,
	client *http.Client,
	gatewayURL string,
	providerDebugURL string,
	sessionID string,
	now time.Time,
) (handoffHTTPResult, string, error) {
	const (
		providerSession = "handoff-provider-observation"
		opaqueToolRef   = "opaque-ref-must-not-reach-model"
	)
	envelope := newHandoffE2EEnvelope("handoff-capable", "portable", now.Add(10*time.Minute))
	envelope.RequiredCapabilities = []string{"tools"}
	envelope.ToolStateRefs = []string{opaqueToolRef}
	result, err := sendAgentGatewayHandoffRequest(
		ctx, client, gatewayURL, sessionID, providerSession, envelope,
	)
	if err != nil {
		return handoffHTTPResult{}, "", err
	}
	if err := requireHandoffResult(result, http.StatusOK, "handoff-capable", "accepted", "constraints_applied"); err != nil {
		return handoffHTTPResult{}, "", fmt.Errorf("capable handoff: %w", err)
	}
	model := result.header.Get(selectedModelHeader)
	if model == "" {
		return handoffHTTPResult{}, "", fmt.Errorf("capable handoff omitted %s", selectedModelHeader)
	}
	if err := assertProviderHandoffConfidentiality(
		ctx, client, providerDebugURL, providerSession, opaqueToolRef,
	); err != nil {
		return handoffHTTPResult{}, "", err
	}
	return result, model, nil
}

func runImpossibleCapabilityHandoff(
	ctx context.Context,
	client *http.Client,
	gatewayURL string,
	now time.Time,
) (handoffHTTPResult, error) {
	envelope := newHandoffE2EEnvelope("handoff-impossible", "portable", now.Add(10*time.Minute))
	envelope.RequiredCapabilities = []string{"unavailable-capability"}
	result, err := sendAgentGatewayHandoffRequest(
		ctx, client, gatewayURL, "handoff-impossible-session", "handoff-impossible-provider", envelope,
	)
	if err != nil {
		return handoffHTTPResult{}, err
	}
	if err := requireHandoffResult(result, 422, "handoff-impossible", "rejected", "capability_unsatisfied"); err != nil {
		return handoffHTTPResult{}, fmt.Errorf("impossible capability handoff: %w", err)
	}
	return result, nil
}

func newHandoffE2EEnvelope(id, portability string, expiresAt time.Time) handoffE2EEnvelope {
	return handoffE2EEnvelope{
		Version:            "1",
		HandoffID:          id,
		RootInvocationID:   "root-agentgateway-test",
		DelegatedRole:      "researcher",
		RemainingTokens:    100_000,
		ContextPortability: portability,
		ExpiresAt:          expiresAt.UTC().Format(time.RFC3339),
	}
}

func sendAgentGatewayHandoffRequest(
	ctx context.Context,
	client *http.Client,
	url string,
	sessionID string,
	providerSession string,
	envelope handoffE2EEnvelope,
) (handoffHTTPResult, error) {
	payload := `{"model":"auto","messages":[{"role":"user","content":"What is the derivative of f(x) = x^3?"}],"max_tokens":64,"temperature":0}`
	envelopeJSON, err := json.Marshal(envelope)
	if err != nil {
		return handoffHTTPResult{}, fmt.Errorf("marshal handoff envelope: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		return handoffHTTPResult{}, fmt.Errorf("create handoff request: %w", err)
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("x-session-id", sessionID)
	request.Header.Set("x-vsr-test-session-id", providerSession)
	request.Header.Set(handoffRequestHeader, base64.RawURLEncoding.EncodeToString(envelopeJSON))

	response, err := client.Do(request)
	if err != nil {
		return handoffHTTPResult{}, fmt.Errorf("send handoff request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return handoffHTTPResult{}, fmt.Errorf("read handoff response: %w", err)
	}
	return handoffHTTPResult{status: response.StatusCode, header: response.Header.Clone(), body: body}, nil
}

func requireHandoffResult(result handoffHTTPResult, status int, id, handoffStatus, reason string) error {
	if result.status != status {
		return fmt.Errorf("HTTP status %d, want %d: %s", result.status, status, result.body)
	}
	if got := result.header.Get(handoffVersionHeader); got != "1" && id != "" {
		return fmt.Errorf("%s=%q, want 1", handoffVersionHeader, got)
	}
	if got := result.header.Get(handoffIDHeader); got != id {
		return fmt.Errorf("%s=%q, want %q", handoffIDHeader, got, id)
	}
	if got := result.header.Get(handoffStatusHeader); got != handoffStatus {
		return fmt.Errorf("%s=%q, want %q", handoffStatusHeader, got, handoffStatus)
	}
	if got := result.header.Get(handoffReasonHeader); got != reason {
		return fmt.Errorf("%s=%q, want %q", handoffReasonHeader, got, reason)
	}
	return nil
}

func assertProviderHandoffConfidentiality(
	ctx context.Context,
	client *http.Client,
	url string,
	providerSession string,
	opaqueToolRef string,
) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create provider observation request: %w", err)
	}
	request.Header.Set("x-vsr-test-session-id", providerSession)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("fetch provider observation: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read provider observation: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("provider observation status %d: %s", response.StatusCode, body)
	}
	var observed struct {
		Body    map[string]interface{} `json:"body"`
		Headers map[string]string      `json:"headers"`
	}
	if err := json.Unmarshal(body, &observed); err != nil {
		return fmt.Errorf("decode provider observation: %w", err)
	}
	for key := range observed.Headers {
		if strings.EqualFold(key, handoffRequestHeader) {
			return fmt.Errorf("provider received forbidden handoff carrier header")
		}
	}
	observedJSON, err := json.Marshal(observed)
	if err != nil {
		return fmt.Errorf("marshal provider observation: %w", err)
	}
	if strings.Contains(string(observedJSON), opaqueToolRef) {
		return fmt.Errorf("provider received opaque tool state reference")
	}
	return nil
}
