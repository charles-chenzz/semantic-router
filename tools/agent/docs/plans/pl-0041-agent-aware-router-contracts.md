# PL-0041: Agent-Aware Router Contracts (Epic #2994)

## Goal

Land maintainer-reviewed, bounded agent-aware contracts in separate implementation
slices. Epic #2994 lets external agent runtimes supply facts and handoff envelopes
so the Router selects logical models safely without owning agent orchestration,
invocation, or composition.

## Scope

- Maintain the ownership proposal and phase tracking while implementation slices
  are active.
- Align phased tracks with sub-issues #3379 (selection facts) and #3380 (handoff
  envelope).
- Current slice: a partial #3380 selection-handoff V1 contract, strict validation,
  policy-safe eligibility narrowing, bounded receipts, and AgentGateway Chat E2E.
- Coordinate envelope overlap with #2546, session continuity with #2973, and
  keep model-only MoM work in #3037.

## Non-Goals

- Adding `providers.agents`, `agentCards`, `targetRefs`, or mixed model/agent
  candidate pools.
- Router-native agent invocation, discovery, or multi-agent composition.
- Completing deferred #3380 idempotency, cancellation, summaries, Replay
  provenance, or additional protocol transports in the current partial slice.
- Extending Router Flow worker pools beyond model-only `modelRefs`.

## Exit Criteria

- [x] Proposal merged at `website/docs/proposals/agent-based-routing.md`.
- [ ] Maintainers agree on ownership boundary and contract field baselines.
- [ ] GitHub sub-issues under #2994 aligned to Phases 1–5 (or explicitly deferred).
- [ ] Open questions in the proposal resolved or explicitly deferred.

## Task List

- [x] `PROP-01` Draft agent-aware Router contracts proposal with ownership boundary.
- [x] `PROP-02` Add proposal to index and website sidebar.
- [x] `PROP-03` Add execution plan PL-0041.
- [ ] `PROP-04` Maintainer review and contract agreement on open questions.
- [ ] `PROP-05` Align Phase 1–5 tracks with #3379, #3380, and collaboration coverage.
- [ ] `PHASE-1` Selection-facts schema, validation, and fail/degrade policy (blocked on PROP-04).
- [ ] `PHASE-2` Signal projection, eligibility narrowing, Replay provenance (#3379).
- [ ] `PHASE-3` Handoff envelope, receipts, idempotency, model-switch E2E (#3380).
  - [ ] `PHASE-3A` Review and land the partial V1 selection-handoff slice: bounded
    contract, strict validation, policy-safe narrowing, receipts, and AgentGateway
    Chat E2E.
  - [ ] `PHASE-3B` Complete the deferred idempotency, cancellation, summary,
    Replay, protocol, and compatibility work required to close #3380.
- [ ] `PHASE-4` One external collaboration surface lifecycle and observability.
- [ ] `PHASE-5` Evaluation graduation and unsupported-integration fallback.

## Next Action

Review the partial #3380 V1 selection-handoff slice against the documented trust,
policy-narrowing, confidentiality, receipt, and supported-boundary contracts.

## Operating Rules

- One phase per implementation PR; no multi-phase mega-PRs.
- Decisions remain model-free with `modelRefs`; entrypoints own `model_names`.
- Facts may narrow eligibility; they must never widen configured candidates.
- Handoff envelopes are validated, content-minimized, and non-executable.
- Update routing-surface catalog, CONFORMANCE.md, and public docs when a phase
  ships user-visible behavior.

## Related Docs

- [Agent-Aware Router Contracts proposal](../../../../website/docs/proposals/agent-based-routing.md)
- [Epic #2994](https://github.com/vllm-project/semantic-router/issues/2994)
- [Feature #3379](https://github.com/vllm-project/semantic-router/issues/3379)
- [Feature #3380](https://github.com/vllm-project/semantic-router/issues/3380)
- [Unified Config Contract v0.3 proposal](../../../../website/docs/proposals/unified-config-contract-v0-3.md)
- [Router Flow Workflows proposal](../../../../website/docs/proposals/router-flow-workflows.md)
