# Global model and delegation contract

This contract applies to every Codex session and repository unless the user
explicitly overrides it. Cost selection is part of correctness.

## Model routing

- Use `gpt-5.6-luna` extensively for read-only work: repository and runtime
  inventory, searches, status checks, log and artifact inspection, documentation
  audits, phase reconciliation, evidence collection, and bounded review.
- Use MiniMax through `mmx-dispatch` for bulk or mechanical implementation:
  repetitive multi-file edits, boilerplate, code generation, test scaffolding,
  and other slices frozen behind an explicit design contract.
- Use Terra only when a bounded task needs more implementation judgment than
  Luna and is not a good mechanical MMX slice.
- Reserve Sol for the primary coordinator's architecture, high-risk design,
  adversarial integration review, and genuinely frontier reasoning. A Sol
  subagent or parallel worker is permitted when the user explicitly requests it
  or the coordinator states a concrete exceptional reason before launch.
- Subagents inherit this routing rule and must not spawn Sol descendants by
  default.

## Fail-closed launch rules

- Every `orca-ide orchestration worker-start` command must include an explicit
  `--agent`. Every Codex worker must also include exactly one explicit `--model`.
- Read-only Codex workers use `--model gpt-5.6-luna`.
- The host hook permits Luna and Terra normally. A Sol worker requires both
  `--model gpt-5.6-sol` and the explicit launch prefix
  `CODEX_ALLOW_SOL_WORKER=1`; unknown, missing, or duplicate models are denied.
- Before launching, tell the user the worker count, model, and bounded purpose.
- Inspect every Orca launch receipt. `launch.requested.model` and
  `launch.effective.model` must both be present and match the intended model.
  Stop the dispatch immediately if either field is absent or unexpected.
- The global Codex default is Luna as a safety net. Explicit model pinning is
  still mandatory for supervised Orca workers.
- Do not launch multiple expensive workers speculatively. Start with Luna or
  MMX and escalate only from concrete evidence.

## MMX worker-bee cadence

- The primary coordinator owns design, interfaces, invariants, file scope,
  acceptance gates, review, and integration.
- One MMX brief is one bounded mechanical slice. Run `mmx-dispatch lint` before
  `run`, then use `status` plus `asks`, `harvest`, trusted diff review,
  cherry-pick, and `reap`.
- Never weaken gates to obtain a pass. Scope violations, gate failures, no-ops,
  and timeouts mean the brief or slice must be corrected.
- MMX does not replace required runtime, visual, packaged, production, or human
  evidence.

## Responsibility split

- Luna discovers and audits.
- MMX types bounded mechanical changes.
- Terra handles bounded judgment-heavy implementation when justified.
- Sol designs, integrates, and reviews in the primary session or a deliberately
  opted-in exceptional worker.
