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
	"testing"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	ext_proc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/config"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/headers"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/llmprotocol"
)

func TestHandoffReceiptHeadersExposeOnlyReceiptIdentityAndOutcome(t *testing.T) {
	for _, status := range []string{
		handoffStatusAccepted,
		handoffStatusRejected,
		handoffStatusExpired,
		handoffStatusIgnored,
	} {
		t.Run(status, func(t *testing.T) {
			ctx := handoffTestContext(nil, "portable")
			ctx.HandoffReceipt.Status = status
			ctx.HandoffReceipt.Reason = "safe_reason"

			response := (&OpenAIRouter{}).createErrorResponse(422, "safe error")
			appendHandoffReceiptToImmediateResponse(response, ctx)

			assert.Equal(t, "1", immediateHeaderValue(response, headers.VSRHandoffVersion))
			assert.Equal(t, "handoff-test", immediateHeaderValue(response, headers.VSRHandoffID))
			assert.Equal(t, status, immediateHeaderValue(response, headers.VSRHandoffStatus))
			assert.Equal(t, "safe_reason", immediateHeaderValue(response, headers.VSRHandoffReason))
			for _, confidential := range []string{
				ctx.Handoff.RootInvocationID,
				ctx.Handoff.DelegatedRole,
			} {
				assert.NotContains(t, serializedImmediateHeaders(response), confidential)
			}
		})
	}
}

func TestHandoffReceiptCoversUpstreamStreamingCacheAndSkipResponses(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		cacheHit    bool
		skip        bool
	}{
		{name: "buffered upstream", contentType: "application/json"},
		{name: "streaming upstream", contentType: "text/event-stream"},
		{name: "cache response headers", contentType: "application/json", cacheHit: true},
		{name: "skip processing response headers", contentType: "application/json", skip: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := handoffTestContext(nil, "portable")
			ctx.HandoffReceipt.Status = handoffStatusAccepted
			ctx.HandoffReceipt.Reason = "constraints_applied"
			ctx.VSRCacheHit = test.cacheHit
			ctx.SkipProcessing = test.skip
			response, err := (&OpenAIRouter{}).handleResponseHeaders(
				responseHeadersForHandoffTest("200", test.contentType), ctx,
			)

			require.NoError(t, err)
			mutation := response.GetResponseHeaders().GetResponse().GetHeaderMutation()
			require.NotNil(t, mutation)
			assert.Equal(t, handoffStatusAccepted, headerValueForTest(mutation, headers.VSRHandoffStatus))
			assert.Equal(t, "constraints_applied", headerValueForTest(mutation, headers.VSRHandoffReason))
			assert.Equal(t, test.contentType == "text/event-stream", ctx.IsStreamingResponse)
		})
	}
}

func TestHandoffReceiptCoversImmediateErrorFastAndCacheResponses(t *testing.T) {
	t.Run("immediate error", func(t *testing.T) {
		ctx := handoffTestContext(nil, "portable")
		ctx.HandoffReceipt.Status = handoffStatusRejected
		ctx.HandoffReceipt.Reason = "capability_unsatisfied"
		response := (&OpenAIRouter{}).createErrorResponse(422, "selection handoff rejected")
		appendHandoffReceiptToImmediateResponse(response, ctx)
		assert.Equal(t, handoffStatusRejected, immediateHeaderValue(response, headers.VSRHandoffStatus))
	})

	t.Run("fast response", func(t *testing.T) {
		payload, err := config.NewStructuredPayload(config.FastResponsePluginConfig{Message: "policy response"})
		require.NoError(t, err)
		ctx := immediateResponseContext(t, llmprotocol.OpenAIChatV1, llmprotocol.OpenAIChatV1, false)
		ctx.Handoff = handoffTestContext(nil, "portable").Handoff
		ctx.HandoffReceipt = handoffReceipt{Version: "1", ID: "handoff-test", Status: handoffStatusIgnored, Reason: "fast_response"}
		ctx.VSRSelectedDecision = &config.Decision{
			Name: "fast",
			Plugins: []config.DecisionPlugin{{
				Type: config.DecisionPluginFastResponse, Configuration: payload,
			}},
		}
		response := (&OpenAIRouter{}).handleFastResponse(ctx, "fast")
		appendHandoffReceiptToImmediateResponse(response, ctx)
		assert.Equal(t, handoffStatusIgnored, immediateHeaderValue(response, headers.VSRHandoffStatus))
		assert.Equal(t, "fast_response", immediateHeaderValue(response, headers.VSRHandoffReason))
	})

	t.Run("cache response", func(t *testing.T) {
		ctx := exactCacheHitContext(nil)
		ctx.SourceFormat = llmprotocol.OpenAIChatV1
		ctx.TargetFormat = llmprotocol.OpenAIChatV1
		ctx.Handoff = handoffTestContext(nil, "portable").Handoff
		ctx.HandoffReceipt = handoffReceipt{Version: "1", ID: "handoff-test", Status: handoffStatusAccepted, Reason: "constraints_applied"}
		response := exactCacheHitResponse(t, &OpenAIRouter{}, ctx)
		appendHandoffReceiptToImmediateResponse(response, ctx)
		assert.Equal(t, handoffStatusAccepted, immediateHeaderValue(response, headers.VSRHandoffStatus))
	})

	t.Run("request rejected before selection", func(t *testing.T) {
		ctx := handoffTestContext(nil, "portable")
		response := (&OpenAIRouter{}).createErrorResponse(400, "invalid request body")
		appendHandoffReceiptToImmediateResponse(response, ctx)
		assert.Equal(t, handoffStatusIgnored, immediateHeaderValue(response, headers.VSRHandoffStatus))
		assert.Equal(t, "request_rejected_before_selection", immediateHeaderValue(response, headers.VSRHandoffReason))
	})
}

func TestMalformedHandoffReceiptIsAttachedByRequestHeaderProcessor(t *testing.T) {
	router := handoffTransportRouter(true)
	ctx := &RequestContext{Headers: map[string]string{}, SourceFormat: llmprotocol.OpenAIChatV1}
	stream := NewMockStream(nil)

	err := router.processRequestHeaders(
		stream,
		handoffHeaderRequest("POST", "/v1/chat/completions", "not+base64"),
		ctx,
	)

	require.NoError(t, err)
	require.Len(t, stream.Responses, 1)
	assert.Equal(t, handoffStatusRejected, immediateHeaderValue(stream.Responses[0], headers.VSRHandoffStatus))
	assert.Equal(t, "malformed_encoding", immediateHeaderValue(stream.Responses[0], headers.VSRHandoffReason))
}

func TestHandoffFullDuplexPassthroughIsIgnored(t *testing.T) {
	router := &OpenAIRouter{Config: &config.RouterConfig{}}
	ctx := handoffTestContext(nil, "portable")
	ctx.FullDuplexRequestBody = true

	requestBodyResponse, err := router.handleRequestBodyDispatch(&ext_proc.ProcessingRequest_RequestBody{
		RequestBody: &ext_proc.HttpBody{Body: []byte(`{"model":"auto"}`), EndOfStream: true},
	}, ctx)

	require.NoError(t, err)
	require.NotNil(t, requestBodyResponse.GetRequestBody().GetResponse().GetBodyMutation().GetStreamedResponse())
	assert.Equal(t, handoffStatusIgnored, ctx.HandoffReceipt.Status)
	assert.Equal(t, "full_duplex_passthrough", ctx.HandoffReceipt.Reason)

	responseHeaders, err := router.handleResponseHeaders(
		responseHeadersForHandoffTest("200", "text/event-stream"), ctx,
	)
	require.NoError(t, err)
	mutation := responseHeaders.GetResponseHeaders().GetResponse().GetHeaderMutation()
	assert.Equal(t, handoffStatusIgnored, headerValueForTest(mutation, headers.VSRHandoffStatus))
	assert.Equal(t, "full_duplex_passthrough", headerValueForTest(mutation, headers.VSRHandoffReason))
}

func responseHeadersForHandoffTest(status, contentType string) *ext_proc.ProcessingRequest_ResponseHeaders {
	return &ext_proc.ProcessingRequest_ResponseHeaders{
		ResponseHeaders: &ext_proc.HttpHeaders{Headers: &core.HeaderMap{Headers: []*core.HeaderValue{
			{Key: ":status", Value: status},
			{Key: "content-type", Value: contentType},
		}}},
	}
}

func serializedImmediateHeaders(response *ext_proc.ProcessingResponse) string {
	if response == nil || response.GetImmediateResponse() == nil || response.GetImmediateResponse().GetHeaders() == nil {
		return ""
	}
	var serialized string
	for _, option := range response.GetImmediateResponse().GetHeaders().GetSetHeaders() {
		serialized += option.GetHeader().GetKey() + ":" + extractHeaderValue(option.GetHeader()) + "\n"
	}
	return serialized
}
