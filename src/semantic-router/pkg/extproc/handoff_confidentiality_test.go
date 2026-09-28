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
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/headers"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/llmprotocol"
)

func TestHandoffToolStateRefsNeverReachProviderOrReplay(t *testing.T) {
	const opaqueRef = "opaque-tool-state-ref-never-export"
	encodedCarrier := encodedHandoffForTest(`,"tool_state_refs":["` + opaqueRef + `"]`)
	logCore, observedLogs := observer.New(zapcore.DebugLevel)
	restoreLogger := zap.ReplaceGlobals(zap.New(logCore))
	defer restoreLogger()

	transportRouter := handoffTransportRouter(true)
	transportCtx := &RequestContext{Headers: map[string]string{}}
	_, err := transportRouter.handleRequestHeaders(
		handoffHeaderRequest("POST", "/v1/chat/completions", encodedCarrier),
		transportCtx,
	)
	require.NoError(t, err)

	router, model := routingTestRouterForFormat(llmprotocol.OpenAIChatV1)
	request := testNeutralRequest(model, "ordinary user request")
	ctx := routingTestContext(llmprotocol.OpenAIChatV1, request)
	ctx.Handoff = transportCtx.Handoff
	ctx.HandoffReceipt = handoffReceipt{
		Version: "1", ID: "handoff-test", Status: handoffStatusIgnored, Reason: "concrete_model",
	}

	response, err := router.handleSpecifiedModelRouting(request, model, "", ctx)

	require.NoError(t, err)
	providerMutation := response.GetRequestBody().GetResponse()
	require.NotNil(t, providerMutation)
	assert.Contains(t, providerMutation.GetHeaderMutation().GetRemoveHeaders(), headers.VSRHandoffEnvelope)
	providerWire, err := protojson.Marshal(response)
	require.NoError(t, err)
	assert.NotContains(t, string(providerWire), opaqueRef)

	replayWire, err := json.Marshal(buildReplayRoutingRecord(ctx, model, model, ""))
	require.NoError(t, err)
	assert.NotContains(t, string(replayWire), opaqueRef)
	assert.NotContains(t, string(replayWire), headers.VSRHandoffEnvelope)

	var logText strings.Builder
	for _, entry := range observedLogs.All() {
		fmt.Fprintf(&logText, "%s %v\n", entry.Message, entry.ContextMap())
	}
	assert.NotContains(t, logText.String(), opaqueRef)
	assert.NotContains(t, logText.String(), encodedCarrier)
	assert.NotContains(t, logText.String(), headers.VSRHandoffEnvelope)
}
