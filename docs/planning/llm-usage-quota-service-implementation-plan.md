# LLM Usage, Token Analytics, and Quota Control Service — Implementation Plan

## 1. Objective

Build a local service that provides centralized observability and policy control for LLM usage across Hermes and other consumers.

The service should:

- Collect and normalize LLM request usage data.
- Track token consumption and related request metadata.
- Track provider subscription/API quota state where observable.
- Estimate usage where authoritative provider data is unavailable.
- Compute burn rate and projected quota exhaustion.
- Expose normalized usage and quota data through a local API.
- Support admission-control decisions for Hermes.
- Provide analytics suitable for a local dashboard/widget.
- Preserve enough request lineage to analyze agent trees and expensive workflows.
- Avoid storing prompt/response content by default.

This plan intentionally leaves engineering choices such as language, framework, persistence engine, deployment model, UI framework, and provider integration mechanism to the implementer.

---

## 2. Non-Goals for Initial Implementation

The first implementation does not need to:

- Proxy all LLM traffic.
- Replace provider SDKs.
- Provide billing-grade accounting.
- Guarantee exact token counts where providers do not expose them.
- Store or index full prompts/responses.
- Implement a multi-user hosted service.
- Provide distributed high availability.
- Automatically optimize prompts or agent behavior.
- Perform autonomous model routing beyond returning policy recommendations.
- Support every LLM provider at launch.

These can be added after the core accounting and policy model is stable.

---

## 3. High-Level Components

The service should be decomposed into independently replaceable components.

### 3.1 Usage Event Ingestion

Receives accounting events from Hermes or other LLM consumers.

Responsibilities:

- Accept request metadata.
- Accept provider-reported token usage.
- Accept locally estimated token usage.
- Associate events with a trace, task, agent, model, provider, and account.
- Validate event structure.
- Persist accepted events.
- Deduplicate retransmitted events where possible.

### 3.2 Provider Usage Collectors

Provider-specific integrations that obtain subscription or billing usage information.

Possible source categories include:

- Official provider usage APIs.
- API response metadata or headers.
- Local application state.
- Provider account endpoints.
- Browser/session-derived data.
- Hermes-maintained local counters.
- Manual or externally supplied quota observations.

Each collector should return a common normalized representation regardless of source.

### 3.3 Token Accounting

Tracks token use for each request.

Token accounting should distinguish at least:

- Input tokens.
- Output tokens.
- Cached input tokens, where available.
- Reasoning/internal tokens, where exposed.
- Total provider-reported tokens.
- Locally calculated tokens.
- Estimated tokens.

Every count should carry provenance indicating whether it is:

- Authoritative/provider-reported.
- Locally tokenized.
- Estimated.

### 3.4 Context Composition Accounting

Optionally track which parts of a request consumed input context.

Suggested categories:

- System prompt.
- Agent instructions.
- Persistent memory.
- Conversation history.
- Retrieved documents.
- Tool output.
- User input.
- Generated intermediate context.
- Other/custom categories.

The service should not require prompt text in order to track these categories. Hermes can report token or character counts for each component.

### 3.5 Quota Normalization

Converts provider-specific quota semantics into a common model.

The normalized model should support:

- Percentage used.
- Percentage remaining.
- Absolute amount used, when known.
- Absolute amount remaining, when known.
- Quota unit.
- Window start.
- Window reset/end.
- Rolling versus fixed window.
- Last observed timestamp.
- Data source.
- Confidence or quality indicator.
- Whether the value is authoritative or estimated.

### 3.6 Policy / Admission Control

Answers questions such as:

- May this workload run?
- Should Hermes conserve usage?
- Should an expensive model be avoided?
- Should parallelism be reduced?
- Should background work be denied?
- Should interactive usage be reserved?

The policy layer should operate on normalized telemetry rather than provider-specific semantics.

### 3.7 Analytics

Computes aggregates such as:

- Tokens by provider.
- Tokens by model.
- Tokens by agent.
- Tokens by task.
- Tokens by trace.
- Tokens by context category.
- Request count.
- Retry count.
- Failure count.
- Latency.
- Cost, where calculable.
- Quota burn rate.
- Projected exhaustion.
- Cache utilization.
- Context utilization.

### 3.8 Presentation / Consumers

Potential consumers include:

- Hermes.
- Local web dashboard.
- Desktop widget.
- CLI.
- Prometheus-compatible metrics consumer.
- External scripts.

These should consume the API rather than directly reading the service's persistence layer.

---

## 4. Core Data Model

Exact database schema is an engineering decision. The logical entities below should exist regardless of implementation.

### 4.1 Provider

Represents an LLM service provider.

Suggested fields:

- `provider_id`
- `name`
- `metadata`
- `created_at`
- `updated_at`

### 4.2 Account

Represents a distinct subscription or API account.

Suggested fields:

- `account_id`
- `provider_id`
- `display_name`
- `account_type`
- `metadata`
- `enabled`

Do not require secrets to be stored directly with the account record.

### 4.3 Model

Represents a provider model or deployment.

Suggested fields:

- `model_id`
- `provider_id`
- `provider_model_name`
- `display_name`
- `context_window`, when known
- `metadata`

### 4.4 Request

Represents one logical model invocation.

Suggested fields:

- `request_id`
- `trace_id`
- `parent_request_id`
- `task_id`
- `agent_id`
- `provider_id`
- `account_id`
- `model_id`
- `started_at`
- `completed_at`
- `status`
- `latency_ms`
- `retry_of`
- `metadata`

### 4.5 Usage Event

Represents token/accounting information for a request.

Suggested fields:

- `usage_event_id`
- `request_id`
- `input_tokens`
- `output_tokens`
- `cached_input_tokens`
- `reasoning_tokens`
- `total_tokens`
- `source`
- `confidence`
- `observed_at`

Multiple usage events may exist for the same request if estimates are later reconciled against authoritative provider data.

### 4.6 Context Component

Represents one contribution to input context.

Suggested fields:

- `request_id`
- `category`
- `name`
- `tokens`
- `characters`
- `bytes`
- `source`
- `metadata`

### 4.7 Quota Sample

Represents a point-in-time observation of provider quota state.

Suggested fields:

- `quota_sample_id`
- `provider_id`
- `account_id`
- `quota_type`
- `used`
- `remaining`
- `used_pct`
- `remaining_pct`
- `unit`
- `window_start`
- `window_end`
- `reset_at`
- `source`
- `confidence`
- `observed_at`

### 4.8 Cost Record

Optional normalized cost accounting.

Suggested fields:

- `request_id`
- `currency`
- `input_cost`
- `output_cost`
- `cache_cost`
- `total_cost`
- `pricing_source`
- `is_estimate`

### 4.9 Policy State

Represents the current normalized policy state for an account/provider.

Suggested fields:

- `provider_id`
- `account_id`
- `state`
- `reason`
- `entered_at`
- `last_evaluated_at`
- `metadata`

---

## 5. Trace and Request Lineage

Hermes should assign a trace identifier to each top-level user or automation task.

Child agent activity should retain:

- The same `trace_id`.
- A unique `request_id`.
- A `parent_request_id` where applicable.
- Agent/workflow identifiers.
- Task/work-unit identifiers.

Example:

```text
Trace: task-123
└── Supervisor request
    ├── Research agent A
    │   ├── LLM request
    │   └── LLM synthesis
    ├── Research agent B
    │   └── LLM request
    └── Final synthesis
```

This allows the analytics layer to calculate total resource consumption for a complete task rather than only individual calls.

---

## 6. Event Ingestion API

Exact route naming and transport are engineering decisions. The service should support the following logical operations.

### 6.1 Record Request Start

Input should include:

- Request ID.
- Trace ID.
- Parent request ID, if applicable.
- Provider.
- Account.
- Model.
- Agent.
- Task.
- Timestamp.
- Optional metadata.

### 6.2 Record Request Completion

Input should include:

- Request ID.
- Status.
- Completion timestamp.
- Latency.
- Error/retry metadata.
- Token usage.
- Cost data where available.

### 6.3 Record Context Breakdown

Input should include:

- Request ID.
- Context category.
- Token count or measurable proxy.
- Optional logical component name.

### 6.4 Record Quota Observation

Used by collectors or manual integrations.

Input should include:

- Provider/account.
- Quota type.
- Value.
- Unit.
- Window/reset metadata.
- Source.
- Confidence.

### 6.5 Idempotency

All write endpoints should support safe retries.

Engineering decision:

- Determine whether idempotency is implemented using request IDs, explicit idempotency keys, event sequence numbers, or another mechanism.

---

## 7. Read / Analytics API

The API should support normalized queries such as:

### 7.1 Current Provider Status

Return:

- Current quota state.
- Remaining capacity.
- Reset time.
- Current policy state.
- Burn rate.
- Projected exhaustion.
- Data freshness.
- Confidence.

### 7.2 Usage Summary

Filters should support some combination of:

- Time range.
- Provider.
- Account.
- Model.
- Agent.
- Task.
- Trace.

Grouping should support:

- Provider.
- Model.
- Agent.
- Task.
- Trace.
- Time bucket.

### 7.3 Request Detail

Return one request and associated:

- Usage.
- Cost.
- Context composition.
- Parent/child relationships.
- Retry relationships.

### 7.4 Trace Detail

Return the full request tree for a task/trace.

### 7.5 Context Analytics

Return aggregated context use by category.

Example questions the endpoint should enable:

- How much input context is retrieval?
- How much context is persistent memory?
- Which agent has the largest system prompt?
- How much tool output is being repeatedly sent?

### 7.6 Quota History

Return historical samples for:

- Usage charts.
- Burn-rate calculation.
- Reset detection.
- Collector quality debugging.

---

## 8. Token Counting Strategy

Token counting should support multiple sources simultaneously.

### 8.1 Provider-Reported Usage

Where available, provider usage should be retained as authoritative.

Requirements:

- Preserve the raw reported categories.
- Normalize into common categories where possible.
- Do not discard provider-specific fields that may be useful later.

### 8.2 Local Tokenization

Hermes or the service may count tokens before transmission where a compatible tokenizer is available.

Engineering decisions:

- Where tokenization executes.
- Which tokenizer implementations are supported.
- How tokenizer/model versions are mapped.
- Whether local counts are synchronous or asynchronous.

### 8.3 Estimated Usage

Where exact tokenization is unavailable, support estimated counts.

Potential inputs:

- Character count.
- Byte count.
- Historical provider-reported ratios.
- Model/provider-specific heuristics.

Requirements:

- Mark estimated values as estimated.
- Preserve the estimation method/version.
- Permit later reconciliation.

### 8.4 Reconciliation

If a request initially has a local estimate and later receives provider-reported usage:

- Preserve both observations.
- Mark the provider observation authoritative.
- Use the authoritative value for normal aggregates unless explicitly querying estimates.
- Retain estimation error for calibration analytics.

---

## 9. Quota Collection Strategy

Each provider collector should implement a common logical interface.

Suggested outputs:

```text
provider
account
quota_type
used
remaining
used_pct
remaining_pct
unit
window_start
window_end/reset_at
source
confidence
observed_at
```

Collector behavior should include:

- Polling or event-driven updates.
- Failure handling.
- Last-known-good state.
- Freshness tracking.
- Explicit stale state.
- Rate limiting.
- Authentication/secrets isolation.

Engineering decisions:

- Provider-by-provider acquisition mechanism.
- Polling interval.
- Whether collectors run in-process or separately.
- Credential storage.
- Handling of unsupported/private endpoints.
- Whether UI-derived quota collection is permitted.

---

## 10. Burn Rate and Forecasting

Quota state alone is insufficient for intelligent backoff.

The service should compute burn rate over configurable time windows.

Useful calculations include:

- Usage change per hour.
- Usage change per day.
- Short-term burn rate.
- Long-term burn rate.
- Time until quota reset.
- Projected usage at reset.
- Projected exhaustion time.

A basic forecast may be sufficient initially.

Example:

```text
remaining:             22%
reset in:              30h
6h burn rate:          1.5%/h
24h burn rate:         0.8%/h
projected exhaustion:  14.7h
```

Engineering decisions:

- Forecast model.
- Time windows.
- Treatment of resets.
- Treatment of irregular provider accounting.
- Weighting of recent versus historical usage.

---

## 11. Admission Control Contract

Hermes should be able to ask the service for a decision before starting expensive work.

Logical request inputs:

- Provider.
- Account.
- Model.
- Workload type.
- Priority.
- Estimated token requirement.
- Expected number of child calls.
- Whether workload is interactive/background.
- Optional deadline.
- Optional alternative models/providers.

Logical response:

- Allow/deny/defer recommendation.
- Current policy state.
- Reason.
- Optional constraints.

Possible returned constraints:

- Maximum estimated tokens.
- Maximum parallel agents.
- Avoid specific model class.
- Prefer alternate provider.
- Disable speculative branches.
- Reduce retrieval budget.
- Shorten output budget.
- Preserve reserved capacity.

The service should return policy decisions; Hermes remains responsible for executing them.

---

## 12. Policy Model

Policy must be configurable rather than hard-coded.

Potential inputs:

- Remaining quota.
- Time until reset.
- Burn rate.
- Forecast exhaustion time.
- Workload priority.
- Interactive reserve.
- Background-work reserve.
- Provider availability.
- Cost.
- Token requirement.
- Data freshness/confidence.

Potential states:

```text
normal
conserve
restricted
critical
unavailable
unknown
```

The exact state names are an engineering decision.

### 12.1 Hysteresis

Policy transitions should avoid oscillating near thresholds.

Example concept:

```text
Enter conserve when condition A is met.
Remain in conserve until recovery condition B is met.
```

The specific thresholds should be configurable.

### 12.2 Reservations

Support quota reservations such as:

- Interactive reserve.
- Emergency reserve.
- Scheduled-job allocation.
- Per-agent allocation.
- Per-project allocation.

Initial implementation may use simple percentage or absolute reservations.

---

## 13. Dashboard / Widget Requirements

The service should expose enough information for a local dashboard without embedding UI logic into the accounting core.

Useful views:

### Overview

- Provider status.
- Remaining quota.
- Reset time.
- Current burn rate.
- Forecast exhaustion.
- Policy state.

### Usage

- Tokens over time.
- Requests over time.
- Input/output split.
- Cached token usage.
- Cost over time.

### Model Breakdown

- Requests by model.
- Tokens by model.
- Cost by model.
- Latency by model.

### Agent Breakdown

- Tokens by agent.
- Calls by agent.
- Average tokens per task.
- Error/retry rate.

### Trace Explorer

- Request tree.
- Token use per child.
- Total trace cost.
- Total trace latency.

### Context Breakdown

- System prompt.
- Memory.
- History.
- Retrieval.
- Tool results.
- User content.

### Quota History

- Raw samples.
- Reset events.
- Collector status.
- Estimated versus authoritative state.

UI technology and layout remain engineering decisions.

---

## 14. Metrics Export

Optionally expose machine-consumable metrics.

Candidate metrics:

```text
llm_requests_total
llm_request_errors_total
llm_input_tokens_total
llm_output_tokens_total
llm_cached_tokens_total
llm_reasoning_tokens_total
llm_request_latency_seconds
llm_estimated_cost_total
llm_quota_remaining_ratio
llm_quota_used_ratio
llm_quota_burn_rate
llm_projected_exhaustion_seconds
llm_context_tokens
```

Recommended dimensions:

- Provider.
- Account.
- Model.
- Agent.
- Status.

Avoid unbounded/high-cardinality dimensions such as raw trace IDs in metrics systems not designed for them.

---

## 15. Privacy and Security Requirements

### 15.1 Default Data Minimization

Do not persist prompt or response bodies by default.

Persist:

- Token counts.
- Request metadata.
- Trace relationships.
- Timing.
- Provider/model identifiers.
- Context-category counts.
- Optional hashes.
- Cost/quota observations.

### 15.2 Optional Content Capture

If full content capture is later implemented:

- Make it explicit and opt-in.
- Provide retention controls.
- Support redaction.
- Separate sensitive content from ordinary telemetry if practical.
- Make capture status visible.

### 15.3 Secrets

Provider credentials should not be exposed through analytics endpoints.

Engineering decisions:

- Secret storage mechanism.
- Process boundaries.
- Credential rotation.
- Local API authentication.
- Network binding behavior.

### 15.4 Local API Exposure

The default deployment should assume local-only use unless deliberately configured otherwise.

Requirements if exposed beyond localhost:

- Authentication.
- Authorization.
- TLS or trusted secure transport.
- Clear separation between read and write capabilities.

---

## 16. Failure Modes

The system should behave predictably when telemetry is incomplete.

### Collector Failure

Expected behavior:

- Retain last-known-good sample.
- Mark sample stale.
- Surface collector error.
- Lower confidence.
- Do not silently report stale data as current.

### Tokenizer Failure

Expected behavior:

- Record provider usage if available.
- Otherwise mark token count unavailable or estimated.
- Do not fail the LLM request solely because analytics failed unless explicitly configured.

### Persistence Failure

Engineering decision:

- Whether Hermes requests continue without telemetry.
- Whether events are buffered locally.
- Whether admission control fails open or fails closed.

### Policy Service Unavailable

Hermes should have a defined fallback behavior.

Possible behaviors to choose from:

- Continue normally.
- Enter conserve mode.
- Deny background work.
- Use last-known policy.

This should be deliberate and configurable.

---

## 17. Configuration

The service should support configuration for:

- Enabled providers.
- Accounts.
- Collectors.
- Collector refresh intervals.
- Tokenizer mappings.
- Pricing data.
- Policy thresholds.
- Quota reservations.
- Retention periods.
- API listener.
- Authentication.
- Metrics export.
- Logging level.

Engineering decision:

- Configuration file format.
- Environment-variable overrides.
- Runtime configuration API.
- Hot reload behavior.

---

## 18. Logging

Operational logs should include:

- Collector success/failure.
- Event ingestion errors.
- Persistence errors.
- Policy transitions.
- Quota resets detected.
- Reconciliation events.
- Configuration errors.

Avoid logging:

- Provider secrets.
- Prompt text by default.
- Response text by default.
- Sensitive request headers.

---

## 19. Retention and Aggregation

Raw request telemetry will accumulate over time.

Plan for:

- Configurable raw-event retention.
- Long-term aggregate retention.
- Optional compaction/rollups.
- Optional export/archive.

Possible aggregate buckets:

- Hourly.
- Daily.
- Weekly.

Engineering decisions:

- Retention period.
- Rollup granularity.
- Whether old raw events are deleted or archived.
- Whether cost/token totals remain indefinitely.

---

## 20. Implementation Phases

## Phase 0 — Interface and Data Contract

Define before implementation:

- Common provider identifiers.
- Account identifiers.
- Model identifiers.
- Request/trace ID format.
- Usage event schema.
- Quota sample schema.
- Admission-control schema.
- Error model.
- Versioning strategy.

Deliverable:

- Versioned API/data schema document.
- Example payloads.
- Validation rules.

Exit criteria:

- Hermes can be instrumented against the contract without depending on implementation details.

---

## Phase 1 — Basic Request Accounting

Implement:

- Request ingestion.
- Request completion.
- Provider-reported token accounting.
- Trace/task/agent metadata.
- Basic persistence.
- Basic query endpoints.

Initial analytics:

- Request count.
- Input tokens.
- Output tokens.
- Tokens by model.
- Tokens by agent.
- Tokens by trace.

Exit criteria:

- Hermes requests are recorded reliably.
- Usage can be queried for a selected time range.
- Agent trees can be reconstructed from trace metadata.

---

## Phase 2 — Context Composition Analytics

Instrument Hermes to report context components.

Implement:

- Context component ingestion.
- Context aggregation.
- Per-request context breakdown.
- Per-agent/task aggregates.

Exit criteria:

- A request can be explained in terms of where its input tokens came from.
- Large context contributors can be identified without storing prompt bodies.

---

## Phase 3 — Local Tokenization and Estimation

Implement:

- Token-counting abstraction.
- Model/tokenizer mapping.
- Estimated token accounting.
- Usage provenance.
- Reconciliation against provider counts.

Exit criteria:

- Requests without authoritative counts still receive usable accounting.
- Estimated and authoritative values remain distinguishable.
- Estimation accuracy can be measured.

---

## Phase 4 — Provider Quota Collection

Implement the first provider collector.

Then:

- Normalize quota data.
- Persist samples.
- Expose current status.
- Expose sample history.
- Detect stale samples.
- Detect resets where possible.

Exit criteria:

- The service can answer "how much quota remains?" for at least one provider/account.
- Source and confidence are visible.

---

## Phase 5 — Burn Rate and Forecasting

Implement:

- Burn-rate calculations.
- Reset-aware calculations.
- Exhaustion forecast.
- Historical trend API.

Exit criteria:

- The service can estimate whether current usage will exhaust quota before reset.
- Forecast inputs and freshness are inspectable.

---

## Phase 6 — Admission Control

Implement:

- Policy evaluation.
- Policy states.
- Configurable thresholds.
- Hysteresis.
- Interactive/background distinction.
- Quota reservation.
- Admission API.

Instrument Hermes to consume the decision.

Exit criteria:

- Hermes can alter behavior based on normalized service recommendations.
- Policies can be changed without modifying Hermes code.

---

## Phase 7 — Additional Provider Collectors

For each provider:

1. Identify obtainable usage/quota signals.
2. Define source quality.
3. Implement collector.
4. Normalize output.
5. Test reset/window behavior.
6. Document known limitations.

Exit criteria:

- All commonly used Hermes providers have either authoritative or clearly labeled estimated quota state.

---

## Phase 8 — Dashboard / Widget

Build UI against public API only.

Initial screen:

- Provider/account.
- Remaining quota.
- Reset time.
- Burn rate.
- Projected exhaustion.
- Current policy state.
- Today's token consumption.
- Top models/agents.

Then add:

- Trace explorer.
- Context breakdown.
- Historical charts.
- Collector health.

Exit criteria:

- Operational state can be understood without querying the API manually.

---

## Phase 9 — Operational Hardening

Add:

- Backups.
- Data migration/versioning.
- Retention.
- Health checks.
- Metrics.
- Structured logs.
- Graceful shutdown.
- Event buffering if desired.
- Load testing.
- Failure-injection tests.

Exit criteria:

- Loss of analytics components does not unexpectedly disrupt Hermes.
- Upgrade and rollback procedures are defined.

---

## 21. Testing Plan

### Unit Tests

Cover:

- Normalization.
- Token reconciliation.
- Burn-rate calculations.
- Forecast calculations.
- Policy transitions.
- Hysteresis.
- Reservation logic.
- Stale-data behavior.
- Reset detection.

### Contract Tests

Validate:

- Hermes event payloads.
- Collector output schema.
- Admission-control payloads.
- Backward compatibility.

### Integration Tests

Simulate:

- Normal request flow.
- Retry flow.
- Agent-tree flow.
- Collector outage.
- Provider quota reset.
- Missing token usage.
- Conflicting local/provider token counts.
- Stale quota data.
- Persistence restart.

### Load Tests

Test expected worst-case:

- Highly parallel Hermes agent activity.
- Large numbers of short requests.
- Long-running traces.
- Frequent quota sampling.

### Security Tests

Verify:

- Secrets do not appear in API output.
- Secrets do not appear in logs.
- Content is not stored when disabled.
- Remote access is denied unless configured.

---

## 22. Acceptance Criteria for Initial Useful Release

The first release can be considered useful when all of the following are true:

- Hermes records every model request with a trace ID.
- Provider/model/agent/task are queryable dimensions.
- Input and output tokens are stored when available.
- Unknown/estimated usage is explicitly labeled.
- Per-trace total usage can be calculated.
- Context composition can be reported for instrumented calls.
- At least one provider has quota-state collection.
- Current quota state is exposed through an API.
- Burn rate can be calculated.
- Projected exhaustion can be calculated.
- Hermes can query an admission-control endpoint.
- Policy thresholds are configurable.
- Prompt/response content is not stored by default.
- Collector and telemetry failures are observable.

---

## 23. Engineering Decisions to Make

The following should remain explicit implementation decisions rather than being embedded implicitly in the plan.

### Runtime / Deployment

- Implementation language.
- Web/API framework.
- Single process versus multiple processes.
- Native daemon versus container.
- Service manager/integration.
- Supported operating systems.

### Persistence

- Storage engine.
- Schema strategy.
- Migration system.
- Retention design.
- Backup strategy.

### API

- REST, RPC, or alternate transport.
- Authentication.
- Versioning.
- Serialization format.
- Streaming/event support.

### Hermes Integration

- Direct event reporting versus transparent proxy.
- Synchronous versus asynchronous telemetry submission.
- Failure behavior if accounting service is unavailable.
- Where tokenization occurs.

### Tokenizers

- Supported tokenizer implementations.
- Model-to-tokenizer mapping.
- Handling unknown models.
- Estimation algorithms.

### Provider Collection

For each provider:

- Supported quota source.
- Authentication method.
- Polling frequency.
- Whether unsupported/private endpoints are acceptable.
- Whether browser/session extraction is acceptable.
- Confidence classification.

### Policy

- State names.
- Thresholds.
- Hysteresis values.
- Reservations.
- Priority classes.
- Fallback behavior.
- Whether policy can recommend provider/model changes.

### UI

- Web versus desktop.
- Framework.
- Refresh interval.
- Charting.
- Widget behavior.
- Notifications.

### Observability

- Metrics format.
- Log format.
- Trace integration.
- External monitoring integration.

---

## 24. Suggested Build Order

Without prescribing implementation technology, the dependency order should be:

```text
Data contract
    ↓
Request + trace accounting
    ↓
Basic usage queries
    ↓
Context composition
    ↓
Token estimation/reconciliation
    ↓
Quota collectors
    ↓
Burn-rate forecasting
    ↓
Policy engine
    ↓
Hermes admission-control integration
    ↓
Dashboard/widget
    ↓
Operational hardening
```

This order makes the system useful early while avoiding coupling Hermes to unfinished provider-specific quota logic.

---

## 25. Key Design Principle

Provider-specific behavior should terminate at the collection/normalization boundary.

Hermes should not need to know:

- How a provider reports quota.
- Whether a quota value came from an API, UI, header, or estimate.
- How a provider defines its reset window.
- Which tokenizer a model uses.
- How quota forecasting is calculated.

Hermes should consume stable concepts such as:

```text
current usage
remaining capacity
data freshness
confidence
burn rate
projected exhaustion
policy state
admission recommendation
```

That keeps provider quirks inside the usage service and allows both Hermes and the UI to operate against a stable, provider-neutral interface.
