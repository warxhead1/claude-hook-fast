# Coordinator inbox: lane decisions reach the right session without polling

Owner ask (2026-10-04): "How can we augment claude hook fast to ensure these are taken care
of by the appropriate session? ... And not just adding yet another message. Think out of the
box here."

## The failure, measured

On 2026-10-04 an Orca run of 11 lanes (battlebots strands) accumulated about 140 lane messages
the coordinator never read. Orca's `check` is a FIFO batch that re-serves the same batch until
it is acked with `--ack <deliveryId>`. The coordinator never acked, so questions,
GPU-window asks and a `worker_done` sat for an hour while lanes blocked. The owner had to
paste a lane's REPORT by hand. The stopgap was a hand-armed Monitor polling `inbox --json`
behind a cursor file. It had to be re-armed after every event and every 30-minute expiry,
and it missed messages that landed between re-arms.

A sample of 20 inbox messages had this mix: 12 `status`, 5 `heartbeat`, 1 `question`,
1 `escalation`, 1 `worker_done`. Only about 15% need a decision. The rest are telemetry.

## Principle

The coordinator should never poll, and never see telemetry. A decision should arrive as a
wake-up, already carrying what is needed to decide it. Questions that already have a
standing ruling should be answered without waking anyone.

## Mechanism (verified against https://code.claude.com/docs/en/hooks, 2026-10-04)

- A command hook with `"async": true, "asyncRewake": true` runs in the background. "When
  you set `asyncRewake: true`, the hook ... wakes Claude on exit code 2. The hook's stderr,
  or stdout if stderr is empty, is shown to Claude as a system reminder." It wakes Claude
  "even after the session is idle". The events that allow async include PostToolUse and
  Notification. Stop and UserPromptSubmit do not.
- `timeout` is not enforced for async command hooks.
- Orca already has a server-side blocking wait:
  `orca-ide orchestration check --run <id> --types <t,...> --wait --timeout-ms <n> --json`.
  It also has `--ack <delivery_id>` and `--peek`.
- Hook settings are snapshotted at session start. New wiring only reaches sessions started
  after the edit.

## Components

### A. `coord-wake`: asyncRewake waiter (identity, read = ack, telemetry kept out)

This is wired as async+asyncRewake on `PostToolUse` (matcher `*`) and on `Notification`
(matcher `idle_prompt`).

1. **Identity fast path.** If `ORCA_TERMINAL_HANDLE` is unset, exit 0 immediately (<1 ms).
   Otherwise resolve the bound run with `orca-ide orchestration run-current --json`, cached
   per session in the state dir with a short TTL. Act only if
   `run.coordinator_handle == ORCA_TERMINAL_HANDLE`. Lanes and unrelated sessions never act.
2. **Singleton.** Take a nonblocking flock on `<state>/<run_id>/wake.lock`. If it is held,
   exit 0. There is at most one waiter per run, however many tool calls fire the hook.
3. **Ack what was already delivered.** If `<state>/<run_id>/pending-ack` holds a
   delivery id from a previous wake, pass it as `--ack`. Delivery is at-least-once: a
   batch is acked only after it has woken Claude, so a crash re-delivers and never loses
   messages.
4. **Wait.** Loop on `check --types question,escalation,worker_done --wait`, using
   `--timeout-ms` slices of about 5 minutes. Between slices, exit 0 if the parent Claude
   process is gone (PPID check), so no orphan outlives its session. The loop has no overall
   deadline, because Claude does not enforce one on async hooks.
5. **Telemetry goes to a ledger, never into context.** On each slice, drain `status` and
   `heartbeat` into `<state>/<run_id>/lanes.json`: one record per lane handle with the last
   status body, the last heartbeat time and the task title. Ack those batches immediately.
   `claude-hook-fast coord-status [--json]` prints the ledger on demand: which lanes are
   alive, which have gone silent, and each lane's last status line.
6. **Wake.** For each actionable message, print a compact block to stderr: lane name (task
   title), type, message id, subject, body (truncated to about 1500 chars), and the exact
   reply command (`orca-ide orchestration reply --id <msg> --body "..."`). For
   `worker_done`, attach the landing verdict (component D). Write the delivery id to
   `pending-ack`, then exit 2.

Kill switch: `COORD_WAKE_DISABLE=1`. Every failure path exits 0, so this hook can never brick
a session.

### B. `coord-stop-gate`: a turn cannot end on an unanswered lane question

This is a synchronous Stop hook. When this session is a coordinator (same identity check),
it lists the `question` and `escalation` messages woken in this session (from `<state>`)
that have no coordinator reply in their thread (`inbox --json`: a message whose
`thread_id` matches, from the coordinator handle). If any exist, it blocks once per
distinct set of ids, with the reason listing each lane, message id and one-line subject.
It fails open on any error and does not block again for the same set.

### C. Standing orders: rulings that answer themselves

`<repo>/.orca/standing-orders.toml` (or `<state>/<run_id>/standing-orders.toml`) holds
entries of the form `{ id, match = "<regex over subject+body>", types = ["question"],
reply = "...", auto = true|false }`.

- `auto = true`: `coord-wake` replies itself (`orchestration reply`), records the reply in
  the ledger, and does NOT wake Claude. This covers "may I have a GPU window", which is
  answered "use bbslot", and similar repeat asks. On 2026-10-04 the repeat questions reduced
  to 3 rulings.
- `auto = false`: the wake block carries "Standing order <id> suggests: <reply>", so the
  coordinator confirms it in one command.

### D. Landing verdict on `worker_done`

Before waking for a `worker_done`, `coord-wake` resolves the dispatch
(`worker-show --dispatch <id> --json`) and its branch head, then computes:

- `git diff --stat <integration>...<head>`: the file count and the top directories.
- Files outside the task's declared FILE SCOPE, taken from lines in the task spec
  (`task-list --json`).
- `git merge-tree --write-tree <integration> <head>`: clean, or the list of conflicting
  paths.
- Whether test files were added or changed.
- Whether the branch is pushed (`git ls-remote`).

The integration branch is set in the run's `standing-orders.toml` as
`integration_branch = "..."`. If unset, the verdict omits the merge check and says so.

## Build order

1. A, with the ledger and `coord-status` (closes the "never read" and telemetry-in-context
   failures).
2. B.
3. C and D.

## Acceptance (each phase)

- `go test ./tools/claude-hooks/...` is green, with table tests over captured
  `orca-ide ... --json` fixtures. Fake `orca-ide` with a stub binary on PATH in the tests.
- **A live check of the wake path is mandatory for A.** In a tmux pane, start a throwaway
  `claude` session with `--settings <file>` that wires `coord-wake` against a stub
  `orca-ide`. The stub blocks, then emits one `question`. Confirm that the idle session wakes
  and shows the block, using `tmux capture-pane`. Run one stub-free probe against the live
  Orca run (read-only `--peek`) to confirm the JSON shapes match the fixtures.
</content>
</invoke>
