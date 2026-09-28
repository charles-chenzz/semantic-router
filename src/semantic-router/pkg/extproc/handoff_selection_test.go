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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/classification"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/config"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/decision"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/handoff"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/sessiontelemetry"
)

var handoffTestNow = time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)

func handoffTestContext(capabilities []string, portability string) *RequestContext {
	return &RequestContext{
		Handoff: &handoff.Envelope{
			Version:              handoff.Version,
			HandoffID:            "handoff-test",
			RootInvocationID:     "root-test",
			DelegatedRole:        "researcher",
			RequiredCapabilities: capabilities,
			RemainingTokens:      4_096,
			ContextPortability:   portability,
			ExpiresAt:            handoffTestNow.Add(10 * time.Minute),
		},
		HandoffReceipt: handoffReceipt{
			Version: handoff.Version,
			ID:      "handoff-test",
		},
	}
}

func handoffSelectionRouter(models map[string]config.ModelParams) *OpenAIRouter {
	return &OpenAIRouter{
		Config: &config.RouterConfig{
			RouterOptions: config.RouterOptions{Handoff: config.HandoffConfig{Enabled: true}},
			BackendModels: config.BackendModels{ModelConfig: models},
		},
		handoffNow: func() time.Time { return handoffTestNow },
	}
}

func TestHandoffCapabilitiesAreAllOfExactAndNonWidening(t *testing.T) {
	router := handoffSelectionRouter(map[string]config.ModelParams{
		"missing-metadata": {},
		"vision-only":      {Capabilities: []string{"vision"}},
		"capable":          {Capabilities: []string{"tools", "vision", "json"}},
	})
	ctx := handoffTestContext([]string{"vision", "tools"}, "portable")
	refs := []config.ModelRef{
		{Model: "missing-metadata"},
		{Model: "vision-only"},
		{Model: "capable"},
	}

	eligible, err := router.handoffEligibleModelRefs(refs, ctx)

	require.NoError(t, err)
	assertModelRefs(t, eligible, []string{"capable"})
	assertModelRefs(t, refs, []string{"missing-metadata", "vision-only", "capable"})
}

func TestHandoffCapabilityFilterPreservesAuthorizedCandidateOrder(t *testing.T) {
	router := handoffSelectionRouter(map[string]config.ModelParams{
		"first":  {Capabilities: []string{"tools"}},
		"second": {Capabilities: []string{"tools", "vision"}},
		"third":  {Capabilities: []string{"tools"}},
	})
	ctx := handoffTestContext([]string{"tools"}, "portable")

	eligible, err := router.handoffEligibleModelRefs([]config.ModelRef{
		{Model: "first"}, {Model: "second"}, {Model: "third"},
	}, ctx)

	require.NoError(t, err)
	assertModelRefs(t, eligible, []string{"first", "second", "third"})
}

func TestHandoffImpossibleCapabilityReturnsUnprocessableEntity(t *testing.T) {
	router := handoffSelectionRouter(map[string]config.ModelParams{
		"text": {Capabilities: []string{"text"}},
	})
	ctx := handoffTestContext([]string{"vision"}, "portable")

	_, err := router.handoffEligibleModelRefs([]config.ModelRef{{Model: "text"}}, ctx)

	var routingErr *handoffRoutingError
	require.ErrorAs(t, err, &routingErr)
	assert.Equal(t, 422, routingErr.statusCode)
	assert.Equal(t, "capability_unsatisfied", routingErr.reason)
	assert.Equal(t, handoffStatusRejected, ctx.HandoffReceipt.Status)
}

func TestHandoffFilterRunsBeforeMinimumCandidates(t *testing.T) {
	decisionConfig := &config.Decision{
		Name:      "capability-panel",
		ModelRefs: []config.ModelRef{{Model: "capable"}, {Model: "incapable"}},
		Algorithm: &config.AlgorithmConfig{
			Type:              config.DecisionAlgorithmStatic,
			MinimumCandidates: 2,
		},
	}
	router := handoffSelectionRouter(map[string]config.ModelParams{
		"capable":   {Capabilities: []string{"tools"}},
		"incapable": {Capabilities: []string{"text"}},
	})
	ctx := handoffTestContext([]string{"tools"}, "portable")

	_, _, err := router.selectDecisionRuntimeModel(
		&decision.DecisionResult{Decision: decisionConfig},
		decisionConfig.Name,
		"request",
		"",
		1,
		ctx,
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, errNoContextEligibleDecisionModel)
	assert.Contains(t, err.Error(), "requires at least 2 eligible candidates")
	assertModelRefs(t, ctx.VSREligibleModelRefs, []string{"capable"})
	assert.Equal(t, handoffStatusRejected, ctx.HandoffReceipt.Status)
}

func TestHandoffDefaultModelMustSatisfyCapabilities(t *testing.T) {
	router := handoffSelectionRouter(map[string]config.ModelParams{
		"fallback": {Capabilities: []string{"text"}},
	})
	router.Config.DefaultModel = "fallback"
	ctx := handoffTestContext([]string{"vision"}, "portable")

	_, _, err := router.selectDecisionRuntimeModel(
		&decision.DecisionResult{Decision: &config.Decision{Name: "empty"}},
		"empty",
		"request",
		"",
		1,
		ctx,
	)

	var routingErr *handoffRoutingError
	require.ErrorAs(t, err, &routingErr)
	assert.Equal(t, "capability_unsatisfied", routingErr.reason)
}

func TestHandoffIsAcceptedOnlyAfterNormalSelectionConstraintsRun(t *testing.T) {
	decisionConfig := &config.Decision{
		Name:      "normal",
		ModelRefs: []config.ModelRef{{Model: "capable"}, {Model: "incapable"}},
		Algorithm: &config.AlgorithmConfig{Type: config.DecisionAlgorithmStatic},
	}
	router := handoffSelectionRouter(map[string]config.ModelParams{
		"capable":   {Capabilities: []string{"tools"}},
		"incapable": {Capabilities: []string{"text"}},
	})
	ctx := handoffTestContext([]string{"tools"}, "portable")

	selected, _, err := router.selectDecisionRuntimeModel(
		&decision.DecisionResult{Decision: decisionConfig},
		decisionConfig.Name,
		"request",
		"",
		1,
		ctx,
	)

	require.NoError(t, err)
	assert.Equal(t, "capable", selected)
	assert.Equal(t, handoffStatusAccepted, ctx.HandoffReceipt.Status)
	assert.Equal(t, "constraints_applied", ctx.HandoffReceipt.Reason)
}

func TestHandoffUnsupportedRoutingPathsAreNeverAccepted(t *testing.T) {
	t.Run("empty semantic input", func(t *testing.T) {
		router := newEntrypointTestRouter(t)
		ctx := handoffTestContext(nil, "portable")
		ctx.TraceContext = context.Background()
		ctx.Headers = map[string]string{}
		router.resolveEntrypointForRequest(config.DefaultVSRAutoModelName, ctx)

		_, _, _, selected, err := router.performDecisionEvaluation(
			config.DefaultVSRAutoModelName,
			signalConversationHistory{nonUserMessages: []string{""}},
			ctx,
		)

		require.NoError(t, err)
		assert.Empty(t, selected)
		assert.Equal(t, handoffStatusIgnored, ctx.HandoffReceipt.Status)
		assert.Equal(t, "unsupported_routing_path", ctx.HandoffReceipt.Reason)
	})

	t.Run("route action", func(t *testing.T) {
		router := routeActionRouter(map[string]int{"safe-model": 0})
		ctx := handoffTestContext(nil, "portable")
		_, _, _, selected, err := router.finalizeDecisionEvaluation(
			&decision.DecisionResult{Decision: guardDecision(routeAction("safe-model")), Confidence: 1},
			"auto",
			"request",
			ctx,
		)
		require.NoError(t, err)
		assert.Equal(t, "safe-model", selected)
		assert.Equal(t, handoffStatusIgnored, ctx.HandoffReceipt.Status)
		assert.Equal(t, "route_action", ctx.HandoffReceipt.Reason)
	})

	t.Run("concrete model", func(t *testing.T) {
		router := routeActionRouter(map[string]int{"safe-model": 0})
		ctx := handoffTestContext(nil, "portable")
		_, _, _, selected, err := router.finalizeDecisionEvaluation(
			&decision.DecisionResult{Decision: guardDecision(nil), Confidence: 1},
			"safe-model",
			"request",
			ctx,
		)
		require.NoError(t, err)
		assert.Empty(t, selected)
		assert.Equal(t, handoffStatusIgnored, ctx.HandoffReceipt.Status)
		assert.Equal(t, "concrete_model", ctx.HandoffReceipt.Reason)
	})

	t.Run("fast response", func(t *testing.T) {
		payload, err := config.NewStructuredPayload(config.FastResponsePluginConfig{Message: "policy response"})
		require.NoError(t, err)
		decisionConfig := &config.Decision{
			Name: "fast",
			Plugins: []config.DecisionPlugin{{
				Type: config.DecisionPluginFastResponse, Configuration: payload,
			}},
		}
		router := handoffSelectionRouter(map[string]config.ModelParams{"fallback": {}})
		router.Config.DefaultModel = "fallback"
		ctx := handoffTestContext(nil, "portable")
		selected, _, err := router.selectDecisionRuntimeModel(
			&decision.DecisionResult{Decision: decisionConfig}, decisionConfig.Name, "request", "", 1, ctx,
		)
		require.NoError(t, err)
		assert.Empty(t, selected)
		assert.Equal(t, handoffStatusIgnored, ctx.HandoffReceipt.Status)
		assert.Equal(t, "fast_response", ctx.HandoffReceipt.Reason)
	})

	t.Run("looper decision even when looper is disabled", func(t *testing.T) {
		decisionConfig := &config.Decision{
			Name:      "workflow",
			ModelRefs: []config.ModelRef{{Model: "capable"}},
			Algorithm: &config.AlgorithmConfig{Type: config.DecisionAlgorithmReMoM},
		}
		router := handoffSelectionRouter(map[string]config.ModelParams{
			"capable": {Capabilities: []string{"tools"}},
		})
		ctx := handoffTestContext([]string{"tools"}, "portable")
		selected, _, err := router.selectDecisionRuntimeModel(
			&decision.DecisionResult{Decision: decisionConfig}, decisionConfig.Name, "request", "", 1, ctx,
		)
		require.NoError(t, err)
		assert.Equal(t, "capable", selected)
		assert.Equal(t, handoffStatusIgnored, ctx.HandoffReceipt.Status)
		assert.Equal(t, "looper", ctx.HandoffReceipt.Reason)
	})
}

func TestRouterLearningExpansionReappliesHandoffEligibility(t *testing.T) {
	router := handoffSelectionRouter(map[string]config.ModelParams{
		"authorized-capable":    {Capabilities: []string{"tools"}},
		"expanded-incapable":    {Capabilities: []string{"vision"}},
		"expanded-unregistered": {Capabilities: []string{"tools"}},
	})
	delete(router.Config.ModelConfig, "expanded-unregistered")
	ctx := handoffTestContext([]string{"tools"}, "portable")

	eligible := router.eligibleLearningModelRefs([]config.ModelRef{
		{Model: "authorized-capable"},
		{Model: "expanded-incapable"},
		{Model: "expanded-unregistered"},
	}, ctx)

	assertModelRefs(t, eligible, []string{"authorized-capable"})
	assert.Empty(t, ctx.HandoffReceipt.Status)

	ctx = handoffTestContext([]string{"tools"}, "portable")
	eligible = router.eligibleLearningModelRefs([]config.ModelRef{{Model: "expanded-incapable"}}, ctx)
	assert.Empty(t, eligible)
	assert.Empty(t, ctx.HandoffReceipt.Status, "an unusable learning expansion must not reject an eligible base selection")
}

func TestHandoffRemainingTokensIsConservativeContextFloor(t *testing.T) {
	router := handoffSelectionRouter(nil)

	pass := handoffTestContext(nil, "portable")
	pass.Handoff.RemainingTokens = 101
	pass.VSRContextTokenCount = 100
	require.NoError(t, router.admitHandoffRequest(pass))

	for _, contextTokens := range []int{100, 101} {
		ctx := handoffTestContext(nil, "portable")
		ctx.Handoff.RemainingTokens = 100
		ctx.VSRContextTokenCount = contextTokens
		err := router.admitHandoffRequest(ctx)
		var routingErr *handoffRoutingError
		require.ErrorAs(t, err, &routingErr)
		assert.Equal(t, "remaining_tokens_exhausted", routingErr.reason)
		assert.Equal(t, 422, routingErr.statusCode)
	}
}

func TestHandoffExpiryIsRecheckedAtSelectionUseTime(t *testing.T) {
	router := handoffSelectionRouter(nil)
	ctx := handoffTestContext(nil, "portable")
	ctx.Handoff.ExpiresAt = handoffTestNow.Add(-time.Nanosecond)

	err := router.admitHandoffRequest(ctx)

	var routingErr *handoffRoutingError
	require.ErrorAs(t, err, &routingErr)
	assert.Equal(t, handoffStatusExpired, ctx.HandoffReceipt.Status)
	assert.Equal(t, string(handoff.CodeExpired), routingErr.reason)
}

func TestStickyHandoffRequiresKnownEligiblePreviousModel(t *testing.T) {
	router := handoffSelectionRouter(map[string]config.ModelParams{
		"previous": {Capabilities: []string{"tools"}},
		"other":    {Capabilities: []string{"tools"}},
	})

	t.Run("success", func(t *testing.T) {
		ctx := handoffTestContext([]string{"tools"}, "sticky")
		ctx.PreviousModel = "previous"
		eligible, err := router.handoffEligibleModelRefs(
			[]config.ModelRef{{Model: "other"}, {Model: "previous"}}, ctx,
		)
		require.NoError(t, err)
		assertModelRefs(t, eligible, []string{"previous"})
	})

	t.Run("missing previous model", func(t *testing.T) {
		ctx := handoffTestContext([]string{"tools"}, "sticky")
		_, err := router.handoffEligibleModelRefs([]config.ModelRef{{Model: "previous"}}, ctx)
		var routingErr *handoffRoutingError
		require.ErrorAs(t, err, &routingErr)
		assert.Equal(t, 409, routingErr.statusCode)
		assert.Equal(t, "sticky_model_missing", routingErr.reason)
	})

	t.Run("previous model is not eligible", func(t *testing.T) {
		ctx := handoffTestContext([]string{"tools"}, "sticky")
		ctx.PreviousModel = "previous"
		_, err := router.handoffEligibleModelRefs([]config.ModelRef{{Model: "other"}}, ctx)
		var routingErr *handoffRoutingError
		require.ErrorAs(t, err, &routingErr)
		assert.Equal(t, 409, routingErr.statusCode)
		assert.Equal(t, "sticky_model_ineligible", routingErr.reason)
	})

	t.Run("previous model lacks a required capability", func(t *testing.T) {
		ctx := handoffTestContext([]string{"vision"}, "sticky")
		ctx.PreviousModel = "previous"
		_, err := router.handoffEligibleModelRefs([]config.ModelRef{{Model: "previous"}}, ctx)
		var routingErr *handoffRoutingError
		require.ErrorAs(t, err, &routingErr)
		assert.Equal(t, 409, routingErr.statusCode)
		assert.Equal(t, "sticky_model_ineligible", routingErr.reason)
	})
}

func TestPortableHandoffDoesNotUnlockExistingSessionLocks(t *testing.T) {
	router := handoffSelectionRouter(map[string]config.ModelParams{"previous": {}})
	ctx := handoffTestContext(nil, "portable")
	ctx.PreviousModel = "previous"
	ctx.PreviousResponseID = "resp-provider-state"
	ctx.VSRConversationFacts = classification.ConversationFacts{LastMessageToolResult: true}

	session := router.buildAgenticSessionContext(ctx, []config.ModelRef{{Model: "previous"}}, "session", "user")

	require.NotNil(t, session)
	assert.True(t, session.ActiveToolLoop)
	assert.True(t, session.HasNonPortableContext)
	assert.Equal(t, "previous_response_id", session.NonPortableContextReason)
	assert.Equal(t, "previous", session.PreviousModel)
}

func TestHandoffRoutingErrorResponsePreservesSafeReceipt(t *testing.T) {
	router := handoffSelectionRouter(nil)
	ctx := handoffTestContext(nil, "portable")
	err := rejectHandoffRouting(ctx, 422, handoffStatusRejected, "capability_unsatisfied")

	response := handoffRoutingErrorResponse(err, router)
	appendHandoffReceiptToImmediateResponse(response, ctx)

	assert.Equal(t, 422, int(response.GetImmediateResponse().GetStatus().GetCode()))
	assert.Equal(t, handoffStatusRejected, immediateHeaderValue(response, "x-vsr-handoff-status"))
	assert.Equal(t, "capability_unsatisfied", immediateHeaderValue(response, "x-vsr-handoff-reason"))
	assert.NotContains(t, string(response.GetImmediateResponse().GetBody()), "handoff-test")
}

func TestPortableHandoffPreservesToolLoopSelectionLock(t *testing.T) {
	for _, test := range []struct {
		name       string
		capability string
		mode       string
		wantModel  string
		conflict   bool
	}{
		{name: "no envelope baseline", wantModel: "frontier"},
		{name: "conflicting capability", capability: "tools", conflict: true},
		{name: "compatible capability", capability: "chat", wantModel: "frontier"},
		{name: "observe does not create a lock", capability: "tools", mode: config.DecisionAdaptationModeObserve, wantModel: "cheap"},
		{name: "bypass does not create a lock", capability: "tools", mode: config.DecisionAdaptationModeBypass, wantModel: "cheap"},
	} {
		t.Run(test.name, func(t *testing.T) {
			sessiontelemetry.ResetRouterSessionMemoryForTesting()
			t.Cleanup(sessiontelemetry.ResetRouterSessionMemoryForTesting)
			sessiontelemetry.RecordSessionDecision(sessiontelemetry.SessionDecisionParams{
				SessionID: "handoff-session/conversation", SelectedModel: "frontier",
				DecisionName: "same", TurnIndex: 2, ActiveToolLoop: true, Timestamp: time.Now(),
			})
			router := &OpenAIRouter{
				Config:     routerLearningProtectionOnlyTestConfig(config.RouterLearningScopeConversation),
				handoffNow: func() time.Time { return handoffTestNow },
			}
			router.Config.ModelConfig["cheap"] = config.ModelParams{Capabilities: []string{"chat", "tools"}}
			router.Config.ModelConfig["frontier"] = config.ModelParams{Capabilities: []string{"chat"}}
			ctx := routerLearningRequestContext("handoff-session", "conversation")
			ctx.VSRConversationFacts = classification.ConversationFacts{LastMessageToolResult: true}
			decisionConfig := &config.Decision{
				Name: "same", ModelRefs: []config.ModelRef{{Model: "cheap"}, {Model: "frontier"}},
				Algorithm: &config.AlgorithmConfig{Type: config.DecisionAlgorithmStatic},
			}
			if test.mode != "" {
				decisionConfig.Adaptations.Protection = &config.DecisionLearningProtectionConfig{Mode: test.mode}
			}
			ctx.VSRSelectedDecision = decisionConfig
			if test.capability != "" {
				handoffCtx := handoffTestContext([]string{test.capability}, "portable")
				ctx.Handoff, ctx.HandoffReceipt = handoffCtx.Handoff, handoffCtx.HandoffReceipt
			}
			selected, _, err := router.selectDecisionRuntimeModel(
				&decision.DecisionResult{Decision: decisionConfig}, "same", "tool followup", "", 1, ctx,
			)
			if test.conflict {
				var routingErr *handoffRoutingError
				require.ErrorAs(t, err, &routingErr)
				assert.Empty(t, selected)
				assert.Equal(t, 409, routingErr.statusCode)
				assert.Equal(t, "tool_loop_lock_conflict", ctx.HandoffReceipt.Reason)
				response := router.decisionEvaluationErrorResponse(ctx, "auto", err)
				appendHandoffReceiptToImmediateResponse(response, ctx)
				assert.Equal(t, 409, int(response.GetImmediateResponse().GetStatus().GetCode()))
				assert.Equal(t, handoffStatusRejected, immediateHeaderValue(response, "x-vsr-handoff-status"))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.wantModel, selected)
			if test.capability == "" {
				assert.Empty(t, ctx.HandoffReceipt.Status)
			} else {
				assert.Equal(t, handoffStatusAccepted, ctx.HandoffReceipt.Status)
			}
		})
	}
}
