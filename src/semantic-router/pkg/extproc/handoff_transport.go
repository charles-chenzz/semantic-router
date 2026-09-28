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
	"strings"
	"time"

	ext_proc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/handoff"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/headers"
)

const maxHandoffHeaderBytes = 6 * 1024

const (
	handoffStatusAccepted = "accepted"
	handoffStatusRejected = "rejected"
	handoffStatusExpired  = "expired"
	handoffStatusIgnored  = "ignored"
)

type handoffReceipt struct {
	Version string
	ID      string
	Status  string
	Reason  string
}

func (r *OpenAIRouter) captureHandoffEnvelope(
	request *ext_proc.ProcessingRequest_RequestHeaders,
	method string,
	path string,
	ctx *RequestContext,
) *ext_proc.ProcessingResponse {
	values := handoffHeaderValues(request)
	removeHeaderValueCI(ctx, headers.VSRHandoffEnvelope)
	if len(values) == 0 {
		return nil
	}
	if !r.handoffEnabled() {
		setHandoffReceipt(ctx, "", "", handoffStatusIgnored, "feature_disabled")
		return nil
	}
	if !supportedHandoffEndpoint(method, path) {
		setHandoffReceipt(ctx, "", "", handoffStatusIgnored, "unsupported_protocol")
		return nil
	}
	if ctx.SkipProcessing {
		setHandoffReceipt(ctx, "", "", handoffStatusIgnored, "skip_processing")
		return nil
	}
	if len(values) != 1 {
		return r.rejectHandoff(ctx, 400, handoffStatusRejected, "multiple_headers")
	}
	encoded := values[0]
	if len(encoded) > maxHandoffHeaderBytes {
		return r.rejectHandoff(ctx, 413, handoffStatusRejected, "encoded_payload_too_large")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return r.rejectHandoff(ctx, 400, handoffStatusRejected, "malformed_encoding")
	}
	envelope, err := handoff.Parse(decoded, r.currentHandoffTime())
	if err != nil {
		code := handoff.CodeOf(err)
		switch code {
		case handoff.CodePayloadTooLarge:
			return r.rejectHandoff(ctx, 413, handoffStatusRejected, string(code))
		case handoff.CodeExpired:
			return r.rejectHandoff(ctx, 422, handoffStatusExpired, string(code))
		default:
			return r.rejectHandoff(ctx, 400, handoffStatusRejected, string(code))
		}
	}
	ctx.Handoff = envelope
	setHandoffReceipt(ctx, envelope.Version, envelope.HandoffID, "", "")
	return nil
}

func handoffHeaderValues(request *ext_proc.ProcessingRequest_RequestHeaders) []string {
	if request == nil || request.RequestHeaders == nil || request.RequestHeaders.Headers == nil {
		return nil
	}
	var values []string
	for _, header := range request.RequestHeaders.Headers.Headers {
		if strings.EqualFold(header.Key, headers.VSRHandoffEnvelope) {
			values = append(values, extractHeaderValue(header))
		}
	}
	return values
}

func supportedHandoffEndpoint(method, path string) bool {
	path, _, _ = strings.Cut(path, "?")
	return method == "POST" && path == "/v1/chat/completions"
}

func (r *OpenAIRouter) handoffEnabled() bool {
	return r != nil && r.Config != nil && r.Config.Handoff.IsEnabled()
}

func (r *OpenAIRouter) currentHandoffTime() time.Time {
	if r != nil && r.handoffNow != nil {
		return r.handoffNow()
	}
	return time.Now()
}

func (r *OpenAIRouter) rejectHandoff(
	ctx *RequestContext,
	statusCode int,
	receiptStatus string,
	reason string,
) *ext_proc.ProcessingResponse {
	setHandoffReceipt(ctx, "", "", receiptStatus, reason)
	return r.createErrorResponse(statusCode, "invalid selection handoff envelope")
}

func setHandoffReceipt(ctx *RequestContext, version, id, status, reason string) {
	if ctx == nil {
		return
	}
	ctx.HandoffReceipt = handoffReceipt{
		Version: version,
		ID:      id,
		Status:  status,
		Reason:  reason,
	}
}

func appendHandoffReceiptToImmediateResponse(response *ext_proc.ProcessingResponse, ctx *RequestContext) {
	if response != nil && response.GetImmediateResponse() != nil && handoffConstraintsActive(ctx) {
		ignoreHandoff(ctx, "request_rejected_before_selection")
	}
	if ctx == nil || ctx.HandoffReceipt.Status == "" {
		return
	}
	appendImmediateResponseHeader(response, headers.VSRHandoffVersion, ctx.HandoffReceipt.Version)
	appendImmediateResponseHeader(response, headers.VSRHandoffID, ctx.HandoffReceipt.ID)
	appendImmediateResponseHeader(response, headers.VSRHandoffStatus, ctx.HandoffReceipt.Status)
	appendImmediateResponseHeader(response, headers.VSRHandoffReason, ctx.HandoffReceipt.Reason)
}
