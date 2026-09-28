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
	"errors"
	"fmt"
	"strings"

	ext_proc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/config"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/handoff"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/selection"
)

type handoffRoutingError struct {
	statusCode int
	reason     string
}

func (e *handoffRoutingError) Error() string {
	return "selection handoff rejected: " + e.reason
}

func (r *OpenAIRouter) admitHandoffRequest(ctx *RequestContext) error {
	if !handoffConstraintsActive(ctx) {
		return nil
	}
	if err := handoff.Validate(ctx.Handoff, r.currentHandoffTime()); err != nil {
		code := handoff.CodeOf(err)
		if code == handoff.CodeExpired {
			return rejectHandoffRouting(ctx, 422, handoffStatusExpired, string(code))
		}
		return rejectHandoffRouting(ctx, 400, handoffStatusRejected, string(code))
	}
	if ctx.Handoff.RemainingTokens <= ctx.VSRContextTokenCount {
		return rejectHandoffRouting(ctx, 422, handoffStatusRejected, "remaining_tokens_exhausted")
	}
	return nil
}

func (r *OpenAIRouter) handoffEligibleModelRefs(
	refs []config.ModelRef,
	ctx *RequestContext,
) ([]config.ModelRef, error) {
	eligible, routingErr := r.narrowHandoffModelRefs(refs, ctx)
	if routingErr == nil {
		return eligible, nil
	}
	return nil, rejectHandoffRouting(
		ctx,
		routingErr.statusCode,
		handoffStatusRejected,
		routingErr.reason,
	)
}

func (r *OpenAIRouter) narrowHandoffModelRefs(
	refs []config.ModelRef,
	ctx *RequestContext,
) ([]config.ModelRef, *handoffRoutingError) {
	if !handoffConstraintsActive(ctx) {
		return refs, nil
	}
	if ctx.Handoff.ContextPortability == "sticky" {
		previousModel := strings.TrimSpace(ctx.PreviousModel)
		if previousModel == "" {
			return nil, &handoffRoutingError{statusCode: 409, reason: "sticky_model_missing"}
		}
		for _, ref := range refs {
			if ref.Model != previousModel && ref.LoRAName != previousModel {
				continue
			}
			if !r.modelHasHandoffCapabilities(ref.Model, ctx.Handoff.RequiredCapabilities) {
				break
			}
			return []config.ModelRef{ref}, nil
		}
		return nil, &handoffRoutingError{statusCode: 409, reason: "sticky_model_ineligible"}
	}

	if len(ctx.Handoff.RequiredCapabilities) == 0 {
		return refs, nil
	}
	if r.handoffConflictsWithToolLoopLock(refs, ctx) {
		return nil, &handoffRoutingError{statusCode: 409, reason: "tool_loop_lock_conflict"}
	}
	eligible := make([]config.ModelRef, 0, len(refs))
	for _, ref := range refs {
		if r.modelHasHandoffCapabilities(ref.Model, ctx.Handoff.RequiredCapabilities) {
			eligible = append(eligible, ref)
		}
	}
	if len(eligible) == 0 {
		return nil, &handoffRoutingError{statusCode: 422, reason: "capability_unsatisfied"}
	}
	return eligible, nil
}

// Check the existing protection identity against the pre-handoff candidates.
// Removing its locked model must fail closed, not look like a policy-driven
// candidate-boundary release to Router Learning. Observe/bypass add no lock.
func (r *OpenAIRouter) handoffConflictsWithToolLoopLock(refs []config.ModelRef, ctx *RequestContext) bool {
	if r == nil || r.Config == nil || !r.Config.RouterLearning.Enabled ||
		!r.Config.RouterLearning.Protection.EffectiveEnabled() ||
		protectionMode(ctx) != config.DecisionAdaptationModeApply ||
		!conversationFactsIndicateActiveToolLoop(ctx.VSRConversationFacts) {
		return false
	}
	identity, ok := r.protectionIdentity(ctx, r.Config.RouterLearning.Protection)
	if !ok {
		return false
	}
	protected := r.protectionSelectionContext(&selection.SelectionContext{CandidateModels: refs}, ctx, identity)
	current := currentLearningModel(protected)
	if current == "" {
		return false
	}
	ref := modelRefForName(refs, current)
	return ref != nil && !r.modelHasHandoffCapabilities(ref.Model, ctx.Handoff.RequiredCapabilities)
}

func (r *OpenAIRouter) modelHasHandoffCapabilities(model string, required []string) bool {
	if r == nil || r.Config == nil || len(required) == 0 {
		return len(required) == 0
	}
	params, ok := r.Config.ModelConfig[model]
	if !ok || len(params.Capabilities) == 0 {
		return false
	}
	available := make(map[string]struct{}, len(params.Capabilities))
	for _, capability := range params.Capabilities {
		available[capability] = struct{}{}
	}
	for _, capability := range required {
		if _, ok := available[capability]; !ok {
			return false
		}
	}
	return true
}

func (r *OpenAIRouter) applyHandoffDefaultConstraints(
	model string,
	ctx *RequestContext,
) error {
	if !handoffConstraintsActive(ctx) {
		return nil
	}
	if err := r.admitHandoffRequest(ctx); err != nil {
		return err
	}
	if r.modelNameExceedsContextWindow(model, ctx.VSRContextTokenCount) {
		return rejectHandoffRouting(ctx, 422, handoffStatusRejected, "default_model_context_ineligible")
	}
	eligible, err := r.handoffEligibleModelRefs([]config.ModelRef{{Model: model}}, ctx)
	if err != nil {
		return err
	}
	if len(eligible) != 1 {
		return rejectHandoffRouting(ctx, 422, handoffStatusRejected, "default_model_ineligible")
	}
	acceptHandoff(ctx)
	return nil
}

func handoffConstraintsActive(ctx *RequestContext) bool {
	if ctx == nil || ctx.Handoff == nil {
		return false
	}
	switch ctx.HandoffReceipt.Status {
	case handoffStatusIgnored, handoffStatusRejected, handoffStatusExpired:
		return false
	default:
		return true
	}
}

func acceptHandoff(ctx *RequestContext) {
	if !handoffConstraintsActive(ctx) {
		return
	}
	setHandoffReceipt(
		ctx,
		ctx.Handoff.Version,
		ctx.Handoff.HandoffID,
		handoffStatusAccepted,
		"constraints_applied",
	)
}

func ignoreHandoff(ctx *RequestContext, reason string) {
	if ctx == nil || ctx.Handoff == nil || ctx.HandoffReceipt.Status != "" {
		return
	}
	setHandoffReceipt(
		ctx,
		ctx.Handoff.Version,
		ctx.Handoff.HandoffID,
		handoffStatusIgnored,
		reason,
	)
}

func rejectHandoffRouting(
	ctx *RequestContext,
	statusCode int,
	status string,
	reason string,
) error {
	version, id := "", ""
	if ctx != nil && ctx.Handoff != nil {
		version = ctx.Handoff.Version
		id = ctx.Handoff.HandoffID
	}
	setHandoffReceipt(ctx, version, id, status, reason)
	return &handoffRoutingError{statusCode: statusCode, reason: reason}
}

func rejectActiveHandoff(ctx *RequestContext, statusCode int, status, reason string) {
	if handoffConstraintsActive(ctx) {
		_ = rejectHandoffRouting(ctx, statusCode, status, reason)
	}
}

func handoffRoutingErrorResponse(
	err error,
	router *OpenAIRouter,
) *ext_proc.ProcessingResponse {
	var routingErr *handoffRoutingError
	if !errors.As(err, &routingErr) {
		return nil
	}
	return router.createErrorResponse(routingErr.statusCode, fmt.Sprintf("selection handoff rejected: %s", routingErr.reason))
}
