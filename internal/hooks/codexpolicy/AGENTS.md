# Global model and delegation contract

This contract applies to every Codex session and repository unless the user
explicitly overrides it. Code quality, coherent implementation, and retained
context take priority over minimizing the model cost of each edit.

The only model ids this account accepts are `gpt-5.5`, `gpt-5.6-sol`, and
`gpt-6-astra`. Any other model id returns an HTTP 400 rejection on this
account and must never be named in a launch command. Effort ceiling for any
worker is `xhigh`; `--effort max` is rejected.

## Direct implementation

- The active agent owns the work end to end. In an Astra (`gpt-6-astra`)
  session, Astra reads the relevant code, designs the change, implements it,
  writes meaningful tests, and validates the integrated result itself. This
  applies to Astra workers as well as the primary session. Other selected
  primary models retain the same implementation ownership.
- Routine investigation, edits, debugging, and tests stay with the active
  agent. Testability, repetition, file count, and a cheaper available model
  do not by themselves justify handing the work to a subagent or MMX.
- Delegate only when the user asks, or when a bounded, independent task has a
  concrete benefit that outweighs the handoff and review cost. State that
  benefit before launching. Delegation is optional; do not invent a fan-out
  to satisfy older model-tier, coordinator-only, or worker-bee guidance in
  project plans or skills.
- Workers complete their assigned scope themselves. Further delegation
  requires an explicit user request.

## Model routing when delegation is justified

- Prefer `gpt-5.6-sol` for both a justified independent implementation task
  and an independent read-only inventory, evidence collection, documentation
  audit, or bounded review. It is the general worker tier for anything that
  is not exceptional judgment or synthesis.
- `gpt-5.5` is a lightweight fallback worker tier when Sol is unavailable or
  capped; it carries the same free (non-restricted) launch rules as Sol.
- MiniMax through `mmx-dispatch` is optional for a large, uniform mechanical
  slice with a frozen design contract and meaningful acceptance gates. Green
  tests alone do not establish semantic correctness or product quality.
- An additional Astra worker requires an explicit user request or a concrete
  exceptional reason stated before launch, for judgment-heavy review or
  synthesis work only. The primary agent remains responsible for integration
  and validation.

## Fail-closed launch rules

- Every `orca-ide orchestration worker-start` command must include an explicit
  `--agent`. Every Codex worker must also include exactly one explicit `--model`.
- Delegated read-only Codex workers normally use `--model gpt-5.6-sol`.
- The host hook permits Sol and gpt-5.5 normally. An additional Astra worker
  requires `--model gpt-6-astra` and `CODEX_ALLOW_ASTRA_WORKER=1`. This explicit
  launch prefix acknowledges exceptional worker use. Unknown, missing, or
  duplicate models are denied. The prefix applies to supervised Orca worker
  launches, not to an ordinary primary Codex session.
- Before launching, tell the user the worker count, model, and bounded purpose.
- Inspect every Orca launch receipt. `launch.requested.model` and
  `launch.effective.model` must both be present and match the intended model.
  Stop the dispatch immediately if either field is absent or unexpected.
- The managed host default is `gpt-6-astra`. Respect the user's selected primary
  model; model routing does not require downgrading it. Explicit model pinning
  remains mandatory for supervised Orca workers.
- Do not launch multiple expensive workers speculatively. Prefer completing
  the work directly; escalate delegated work only from concrete evidence.
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

## MMX lifecycle when selected

- The active agent owns design, implementation decisions, interfaces,
  invariants, file scope, acceptance gates, review, and integration. This
  lifecycle governs an MMX slice after delegation is justified; it does not
  require selecting MMX.
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

- The active agent, including Astra, investigates, designs, implements, tests,
  integrates, and reviews.
- Optional Sol or gpt-5.5 workers collect independent evidence, provide bounded
  review, or handle a justified, bounded implementation slice under the active
  agent's design and review.
- Optional MMX workers handle justified, bounded mechanical slices under the
  active agent's design and review.
- An additional Astra worker is exceptional, explicitly pinned, and launched
  with the required opt-in prefix, reserved for judgment-heavy review or
  synthesis.

## Guardrails (rulings, not procedures)

- One destructive action per command. Never chain push, merge, reset, delete,
  or worktree removal with each other or with a build.
- A dirty working tree means stop and find out whose work it is before
  touching it; a bare "stash everything" is not a substitute for
  understanding and can grab another session's uncommitted work.
- Never reset or force-push a shared branch to match a remote. Local history
  is the source of truth; remote is the backup. Resolve divergence by moving
  local history forward.
- No secret value may reach command output, a log, or any file read back
  into context. Consume credentials only via
  `hearth-vault exec --prefix <proj>/ --redact -- <cmd>`; never cat a secrets
  file and never add a key to a `.env`.
- Never escalate privileges. A root-owned config change is a handoff: write
  the file and name the one install command for a human to run; do not probe
  whether elevation is available.
- Every isolated worktree lives at `/home/eric/data2/worktrees/<project>/<name>`,
  never nested inside the checkout it isolates.
- Scratch and intermediate iterations never land inside the checkout; write
  them under `/home/eric/data2/orca/<project>/_evidence/<date>/<lane>/scratch/`
  and copy in only the final artifact.
- `/tmp` is a 32G tmpfs (RAM). Never park build caches, clones, or
  long-lived work there; use `~/.cache/<tool>` or `/home/eric/data2`.
- File every discovered defect or follow-up at discovery time: a GitHub
  issue (`gh issue create`, evidence + file:line) AND a bead
  (`bd create ... -l <project>,<area>`; always pass `-l <project>`).
