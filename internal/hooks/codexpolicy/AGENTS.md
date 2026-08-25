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
- Use one fresh, explicitly pinned Codex worker per bounded Orca Task. Do not
  reuse a Codex terminal with `worker-start --terminal`: Orca forbids combining
  terminal reuse with `--model`, while this host requires an explicit model for
  every Codex worker. A follow-up therefore gets a fresh terminal in the same
  isolated worktree, with explicit `--agent codex --model ...`; reuse is allowed
  only after Orca can attest and receipt the inherited model without weakening
  this policy.
- Process the accepted `worker_done` Delivery before cleanup, then call
  `worker-release` for that exact Dispatch and acknowledge the Delivery. Never
  infer completion from TUI idle, a commit, terminal prose, or a vanished tab.

## MMX worker-bee cadence

- The primary coordinator owns design, interfaces, invariants, file scope,
  acceptance gates, review, and integration.
- One MMX brief is one bounded mechanical slice. When Orca orchestration is
  available, MMX workers must be first-class supervised Orca workers: create a
  Run Task, compile the canonical brief from a validated contract, and launch
  with `orca-ide orchestration worker-start --agent mmx` in an isolated
  worktree. Inspect the receipt, require the requested/effective agent to be
  `mmx`, and keep task, Dispatch, terminal, transcript, and cleanup visible via
  `task-list`, `worker-list`, `worker-read`, and `worker-release`.
- Do not launch an invisible headless MMX job alongside an unrelated Orca card.
  If Orca-native MMX is unavailable, use the headless fallback explicitly:
  `mmx-dispatch lint`, `run`, `status` plus `asks`, `harvest`, trusted diff
  review, cherry-pick, and `reap`; report that the worker is not represented as
  a normal live Orca subagent. A future external-runner adapter may replace
  this fallback only when its receipts preserve the same lifecycle identities
  and safety gates.
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
