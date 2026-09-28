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

package extproc

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	ext_proc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/config"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/headers"
)

func encodedHandoffForTest(overrides string) string {
	payload := fmt.Sprintf(`{
		"version":"1",
		"handoff_id":"handoff-test",
		"root_invocation_id":"root-test",
		"delegated_role":"researcher",
		"required_capabilities":["tools"],
		"remaining_tokens":4096,
		"context_portability":"portable",
		"expires_at":"2026-09-04T12:10:00Z"%s
	}`, overrides)
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

func handoffHeaderRequest(method, path string, values ...string) *ext_proc.ProcessingRequest_RequestHeaders {
	requestHeaders := []*core.HeaderValue{
		{Key: ":method", Value: method},
		{Key: ":path", Value: path},
	}
	for _, value := range values {
		requestHeaders = append(requestHeaders, &core.HeaderValue{
			Key:   headers.VSRHandoffEnvelope,
			Value: value,
		})
	}
	return &ext_proc.ProcessingRequest_RequestHeaders{
		RequestHeaders: &ext_proc.HttpHeaders{
			Headers: &core.HeaderMap{Headers: requestHeaders},
		},
	}
}

func handoffTransportRouter(enabled bool) *OpenAIRouter {
	return &OpenAIRouter{
		Config: &config.RouterConfig{RouterOptions: config.RouterOptions{
			Handoff: config.HandoffConfig{Enabled: enabled},
		}},
		handoffNow: func() time.Time { return handoffTestNow },
	}
}

func TestHandoffDisabledIsStrippedAndIgnored(t *testing.T) {
	router := handoffTransportRouter(false)
	ctx := &RequestContext{Headers: map[string]string{}}

	response, err := router.handleRequestHeaders(
		handoffHeaderRequest("POST", "/v1/chat/completions", encodedHandoffForTest("")),
		ctx,
	)

	require.NoError(t, err)
	require.NotNil(t, response.GetRequestHeaders())
	assert.Nil(t, ctx.Handoff)
	assert.Equal(t, handoffStatusIgnored, ctx.HandoffReceipt.Status)
	assert.Equal(t, "feature_disabled", ctx.HandoffReceipt.Reason)
	assertHandoffHeaderNotCaptured(t, ctx)
	assert.Contains(t, response.GetRequestHeaders().GetResponse().GetHeaderMutation().GetRemoveHeaders(), headers.VSRHandoffEnvelope)
}

func TestHandoffEnabledParsesWithoutExposingRawHeaderToClassifiers(t *testing.T) {
	router := handoffTransportRouter(true)
	ctx := &RequestContext{Headers: map[string]string{}}
	request := handoffHeaderRequest("POST", "/v1/chat/completions", encodedHandoffForTest(
		`,"parent_invocation_id":"parent-test","tool_state_refs":["opaque-ref"]`,
	))
	request.RequestHeaders.Headers.Headers[2].Key = "X-VSR-Handoff-Envelope"

	response, err := router.handleRequestHeaders(request, ctx)

	require.NoError(t, err)
	require.NotNil(t, response.GetRequestHeaders())
	require.NotNil(t, ctx.Handoff)
	assert.Equal(t, "handoff-test", ctx.Handoff.HandoffID)
	assert.Equal(t, []string{"opaque-ref"}, ctx.Handoff.ToolStateRefs)
	assert.Empty(t, ctx.HandoffReceipt.Status, "acceptance remains pending until selection")
	assertHandoffHeaderNotCaptured(t, ctx)
	assert.Contains(t, response.GetRequestHeaders().GetResponse().GetHeaderMutation().GetRemoveHeaders(), headers.VSRHandoffEnvelope)
}

func TestHandoffTransportRejectsMalformedOversizeAndDuplicates(t *testing.T) {
	tests := []struct {
		name       string
		values     []string
		wantStatus int
		wantReason string
	}{
		{name: "malformed base64", values: []string{"not+base64"}, wantStatus: 400, wantReason: "malformed_encoding"},
		{name: "encoded header exact boundary", values: []string{strings.Repeat("A", maxHandoffHeaderBytes)}, wantStatus: 413, wantReason: "payload_too_large"},
		{name: "encoded header too large", values: []string{strings.Repeat("a", maxHandoffHeaderBytes+1)}, wantStatus: 413, wantReason: "encoded_payload_too_large"},
		{name: "decoded payload too large", values: []string{base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(" ", 4*1024+1)))}, wantStatus: 413, wantReason: "payload_too_large"},
		{name: "duplicate headers", values: []string{encodedHandoffForTest(""), encodedHandoffForTest("")}, wantStatus: 400, wantReason: "multiple_headers"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := handoffTransportRouter(true)
			ctx := &RequestContext{Headers: map[string]string{}}

			response, err := router.handleRequestHeaders(
				handoffHeaderRequest("POST", "/v1/chat/completions", test.values...), ctx,
			)

			require.NoError(t, err)
			require.NotNil(t, response.GetImmediateResponse())
			assert.Equal(t, test.wantStatus, int(response.GetImmediateResponse().GetStatus().GetCode()))
			assert.Equal(t, handoffStatusRejected, ctx.HandoffReceipt.Status)
			assert.Equal(t, test.wantReason, ctx.HandoffReceipt.Reason)
			assertHandoffHeaderNotCaptured(t, ctx)
		})
	}
}

func TestHandoffUnsupportedAndSkipProcessingStillStripCarrier(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		skip       bool
		wantReason string
	}{
		{name: "responses unsupported", method: "POST", path: "/v1/responses", wantReason: "unsupported_protocol"},
		{name: "query string preserves chat endpoint", method: "POST", path: "/v1/chat/completions?trace=true", wantReason: ""},
		{name: "skip processing", method: "POST", path: "/v1/chat/completions", skip: true, wantReason: "skip_processing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := handoffTransportRouter(true)
			ctx := &RequestContext{Headers: map[string]string{}, SkipProcessing: test.skip}
			request := handoffHeaderRequest(test.method, test.path, encodedHandoffForTest(""))

			response, err := router.handleRequestHeaders(request, ctx)

			require.NoError(t, err)
			if requestHeaders := response.GetRequestHeaders(); requestHeaders != nil {
				assert.Contains(t, requestHeaders.GetResponse().GetHeaderMutation().GetRemoveHeaders(), headers.VSRHandoffEnvelope)
			} else {
				require.NotNil(t, response.GetImmediateResponse(), "baseline endpoint handling must remain intact")
			}
			assertHandoffHeaderNotCaptured(t, ctx)
			if test.wantReason == "" {
				require.NotNil(t, ctx.Handoff)
				assert.Empty(t, ctx.HandoffReceipt.Status)
				return
			}
			assert.Nil(t, ctx.Handoff)
			assert.Equal(t, handoffStatusIgnored, ctx.HandoffReceipt.Status)
			assert.Equal(t, test.wantReason, ctx.HandoffReceipt.Reason)
		})
	}
}

func TestHandoffNoEnvelopePreservesOriginalBehaviorAndNoReceipt(t *testing.T) {
	router := handoffTransportRouter(true)
	ctx := &RequestContext{Headers: map[string]string{}}

	response, err := router.handleRequestHeaders(handoffHeaderRequest("POST", "/v1/chat/completions"), ctx)

	require.NoError(t, err)
	require.NotNil(t, response.GetRequestHeaders())
	assert.Nil(t, ctx.Handoff)
	assert.Equal(t, handoffReceipt{}, ctx.HandoffReceipt)
}

func assertHandoffHeaderNotCaptured(t *testing.T, ctx *RequestContext) {
	t.Helper()
	for key := range ctx.Headers {
		assert.False(t, strings.EqualFold(key, headers.VSRHandoffEnvelope), "raw handoff header leaked into request metadata")
	}
}
