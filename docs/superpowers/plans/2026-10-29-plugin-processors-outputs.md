---
status: closed
created_on: 2026-10-29
---
# Plugin processors and plugin outputs. Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax. Tick each step when it is done, in the same work block.

**Status:** Closed (2026-10-30). See the delivery status at the end.

**Goal:** the agent runs the two remaining plugin kinds: a processor (`processors[]`) observes copies of the records of
named devices and emits samples for derived devices it declares, never touching machine data; a plugin output
(`type: plugin:<id>`) sends the sealed batches of an output's buffer through the plugin's `Send`, with the outcomes
handled as the HTTP responses are.

**Architecture:** the output loop of `internal/output` is split from its HTTP transport behind a `Sender` seam, so the
buffer, sealed batches, split, drop, backoff and suspension are shared by `laraplate-http` and plugin outputs. In
`internal/plugin` an output adapter implements `Sender` over the plugin's `Output` service, and a processor adapter
runs the `Process` stream: it is fed by a non-blocking tap on the inputs' emit path and emits its derived samples as a
virtual input registered with the router, with the agent's lifecycle helper deciding birth and death. The supervisor
starts the plugins that inputs, outputs or processors use, and applies, restarts and rolls back processors and plugin
outputs by difference, as it does for inputs.

**Tech Stack:** Go 1.27; `github.com/getlaraplate/laraedge-sdk` raised from v0.1.0 to v0.1.1 (the `acquire` fix of
laraedge-sdk PR #2; same licences, no new dependency).

**Spec:** `../../../../docs/superpowers/specs/2026-10-22-laraedge-plugins-design.md` (stack root) sections 4
(Processor, Output, limits), 5 (components, processors, offline validation), 6 (health and restart for processors and
outputs, supervisor, observability), 7 (examples, tests), 9 plan 3, as amended by the delivery status of the SDK plan
(`../../../laraedge-sdk/docs/superpowers/plans/2026-10-23-sdk.md`, rulings R9, R10, R18) and of plan 2
(`2026-10-26-plugin-host-inputs.md`, rulings R1-R25 and `plugins_dir`).

## Global Constraints

- AGPL-3.0 repo; module `github.com/getlaraplate/laraedge`; never copy SDK code, import it; permissive dependencies
  only; no CGO; work on branch `develop`; commit only your own files; never push; no `Co-Authored-By` trailer; do not
  commit at the stack root.
- `gofmt -l .` empty, `go vet ./...`, `GOOS=windows go vet ./...`, `go test -race ./...`, `GOARCH=386 go test ./...`
  before every commit; table-driven tests; fakes over mocks; injected clock for timing, no sleeps.
- Spec values exactly: deadlines `Configure` 10 s, `Send` 30 s (a `Send` past its deadline counts as `RETRY_AFTER`);
  messages up to 16 MB; outcomes `ACCEPTED`/`DUPLICATE` confirm, `RETRY_AFTER(d)` backoff with jitter 1 s to 5 min
  honouring `d`, `SPLIT` splits in two reusing the refused `source_seq`, `REJECT_INVALID(reason)` drops the batch with
  a `rejected_invalid` loss marker, `SUSPEND_AUTH` suspends that output, keeps buffering, retries every 5 minutes and
  at reload; a plugin output never buffers.
- Processors (spec P4): observe and derive only; a processor can never change, drop or delay machine data; when it is
  down machine data flows untouched and only its derived signals go `bad` (one sample per signal, zero value of the
  type) and then `death` after `death_tolerance`; a derived device is alive while its processor instance is healthy,
  with no per-sample liveness; anything the plugin sends that is not a declared derived device and signal is refused
  and counted; the agent sends the births of derived devices.
- Secret-key rule (keys equal to or ending in `password`, `token`, `secret`, `api_key`, any case, must be `secret://`
  references) extended to every `config` object; secrets resolved at every `Configure`; messages name paths, never
  values.
- A plugin starts only when an input, a processor or an output uses it; plan 2's host, hashes, `plugins_dir`, restart
  and stop rules apply unchanged.

## Decisions taken in this plan

- **Q1. One output loop, two senders.** `output.HTTP`'s transport-agnostic loop (token step, `Next`, settle, split,
  drop, backoff, suspend, `OnReconnect`, state, last error, last confirmation) moves into `output.Loop`, which calls a
  `Sender`; `laraplate-http` becomes a `Sender` whose result is mapped from its status codes exactly as today. No
  behaviour change for HTTP outputs: their existing tests pass unchanged.
- **Q2. Plugin output and plugin restarts.** After a plugin restart (a new generation from `Host.Watch`) the output
  adapter configures its instance again before the next `Send`; while the plugin is down a `Send` is a retry, so the
  buffer keeps everything. A refused re-`Configure` keeps the output in backoff and logs the paths (H6 of plan 2,
  applied to outputs). `ReloadCredentials` (SIGHUP) re-resolves the secrets of `config` and configures the instance
  again, and wakes a suspended output.
- **Q3. Derived signal types.** A derived signal declares `type` among `int`, `float`, `bool`, `string` (the value
  kinds of the agent's model; birth data type `number`, `number`, `boolean`, `string`), `priority` `critical` or
  `bulk` (default `critical`) and optional `unit`. A derived sample whose value kind is not the declared one is
  refused and counted.
- **Q4. The tap.** Copies of records reach a processor through a bounded queue of 1,000 records per processor
  instance, filled after the input's transforms and never blocking the input: when it is full the copy is dropped and
  counted. Records of observed devices only (samples and device events).
- **Q5. Derived device liveness.** While the `Process` stream is up the adapter marks every derived device as read on
  a 1 s tick of the injected clock; when it is down the tick stops, one `bad` sample per derived signal is emitted and
  the lifecycle declares `death` after `death_tolerance` (default as for inputs).
- **Q6. Plugins used.** `used` plugins are those of inputs (`plugin:<id>` drivers), processors (`plugin`) and outputs
  (`plugin:<id>` types). A plugin restart (H5) restarts every component on it: inputs as today, processors and plugin
  outputs stopped and started again, the output keeping its buffer.
- **Q7. `Release` stays deferred** (SDK ruling R18): a removed processor or output instance keeps its state in the
  plugin until the plugin restarts; documented as a known limit.

## Review Focus

- A plugin output down for minutes (crash loop): nothing is lost, batches stay buffered and are delivered in order once
  it is back; a batch re-sent under its `message_id` answered `DUPLICATE` is confirmed. Test in Task 4 and Task 7.
- A processor that stops reading its stream (hung) or a burst of machine data: inputs never block, copies beyond the
  queue are dropped and counted, machine data reaches the outputs on time. Test in Task 5.
- A processor that emits a device it did not declare, a declared device's undeclared signal, or a wrong value kind:
  refused, counted, never routed. Test in Task 5.
- A reload that changes only one plugin output's `config`: the plugin keeps its pid, only that output instance is
  stopped and started (configured again), its buffer untouched. Test in Task 6.
- `observe` naming a device no input has, or a derived device sharing its name with a real device or another
  processor's: refused offline with the document path. Test in Task 1.

---

### Task 1: SDK bump and configuration

**Files:** modify `go.mod`/`go.sum` (`laraedge-sdk` v0.1.1), `schema/laraplate-edge-config-1.schema.json` (top-level
`processors`; `output.config` free object allowed only for `plugin:<id>` types), `internal/config/config.go`
(`Document.Processors`, `Output.Config`, parse and rules), `internal/config/check.go` (accept `plugin:<id>` outputs and
`processors`; `laraplate-mqtt` stays refused), tests; `.github/workflows/ci.yml` and `cmd/laraplate-edge` conformance
references from `@v0.1.0` to `@v0.1.1`.

**Interfaces:**
- Produces: `config.Processor{ID, Plugin string; Observe []string; Devices []DerivedDevice; DeathTolerance
  time.Duration; Config json.RawMessage; Raw json.RawMessage}`, `config.DerivedDevice{Device string; Signals
  []DerivedSignal}`, `config.DerivedSignal{Signal, Type, Priority, Unit string}`; `config.Output.Config
  json.RawMessage`.

- [x] **Step 1: Failing tests:** schema and `Parse` accept a processor and a plugin output with `config`; refuse
  `config` on `laraplate-http`; refuse, at their dotted paths: an `observe` device no input has, a derived device
  equal to a real device or to another processor's derived device, duplicate derived signals, a bad `type` or
  `priority`, a processor id equal to another processor's, a non-reference value under a secret key inside any
  `config`; `CheckBuild` accepts plugin outputs and processors, still refuses `laraplate-mqtt`; secrets in `config`
  are reported by the existing secret check.
- [x] **Step 2: Implement.**
- [x] **Step 3: Run the checks; commit.**

### Task 2: Offline validation of processors and plugin outputs

**Files:** modify `internal/plugin/validate.go` (`Validate`), `cmd/laraplate-edge/main.go` if needed, tests.

**Interfaces:**
- Consumes: Task 1 types; `manifest.ValidateProcessor`, `manifest.ValidateOutput`.
- Produces: `plugin.Validate` also checks every processor (`plugin` exists and offers `processor`; the section
  `{id, observe, devices, config}` against `Manifest.ValidateProcessor`) and every plugin output (exists and offers
  `output`; `{id, config}` against `Manifest.ValidateOutput`); problems prefixed `processors.<i>` / `outputs.<i>`;
  plan 2's R5 drop rule (secret key or `secret://` value) applied to `config`.

- [x] **Step 1: Failing tests:** an unknown plugin, a plugin without the kind, a `config` the manifest refuses, at
  their paths; a `secret://` value where the schema wants an integer is not reported offline; `config validate`
  reports them; no message contains a value.
- [x] **Step 2: Implement.**
- [x] **Step 3: Run the checks; commit.**

### Task 3: One output loop behind a `Sender`

**Files:** create `internal/output/loop.go`; modify `internal/output/http.go`, `internal/output/batch.go` (export
`(*Batch).Records() []model.Record`, critical first), tests.

**Interfaces:**
- Produces: `output.Sender interface{ Send(ctx context.Context, b *Batch) Outcome; ReloadCredentials() }`,
  `output.Outcome{Kind OutcomeKind; RetryAfter time.Duration; Reason string}` with kinds `Confirmed`, `Retry`,
  `Split`, `Invalid`, `Suspend`; `output.NewLoop(LoopConfig{ID string; Sender Sender; Batcher *Batcher; Clock
  clock.Clock; Logger *slog.Logger; OnReconnect func()}) *Loop` with `Run(ctx) error`, `State() State`,
  `LastError() (string, time.Time)`, `LastConfirmation() time.Time`, `ReloadCredentials()`; `output.HTTP` keeps its
  constructor and `ClockOffset()` and runs on a `Loop`.

- [x] **Step 1: Failing tests:** a fake `Sender` drives each outcome through the loop: confirm advances, retry backs
  off honouring `RetryAfter` within 1 s to 5 min, split shrinks reusing `source_seq`, invalid drops with a
  `rejected_invalid` marker, suspend waits 5 minutes or `ReloadCredentials`; `OnReconnect` on the first confirm after
  backoff or suspension. All existing `internal/output` tests pass unchanged.
- [x] **Step 2: Implement** (move, do not rewrite: the HTTP status mapping becomes `HTTP`'s `Send`).
- [x] **Step 3: Run the checks; commit.**

### Task 4: Plugin output adapter

**Files:** create `internal/plugin/output.go`, tests; extend `internal/plugin/testdata/plugin` with an `Output` kind
(behaviours: answer a given outcome, block past the deadline, count `message_id`s).

**Interfaces:**
- Consumes: Task 3 `Sender`, `Outcome`, `Batch.Records()`; Task 1 `config.Output`; plan 2 `Host` (`Conn`, `Watch`),
  `SecretResolver`.
- Produces: `plugin.NewOutputSender(h *Host, cfg config.Output, resolver SecretResolver, clk clock.Clock, log
  *slog.Logger) (*OutputSender, error)` implementing `output.Sender`, with `Close()` (ends its `Watch`).

- [x] **Step 1: Failing tests** (against the test plugin): the batch reaches the plugin with `message_id`,
  `source_seq`, records and losses; each SDK outcome maps per Global Constraints; a `Send` past 30 s is a retry; the
  plugin down is a retry and the next `Send` after its restart is preceded by `Configure`; a refused `Configure`
  keeps the output retrying and logs the paths; `ReloadCredentials` configures again with re-resolved secrets;
  resolved secret values appear in no log or error.
- [x] **Step 2: Implement.**
- [x] **Step 3: Run the checks; commit.**

### Task 5: Processor adapter

**Files:** create `internal/plugin/processor.go`, tests; extend the test plugin with a `Processor` kind (behaviours:
echo a derived sample per observed sample, emit an undeclared device or signal or a wrong kind, stop reading).

**Interfaces:**
- Consumes: Task 1 `config.Processor`; plan 2 `Host`; `input.NewLifecycle`.
- Produces: `plugin.NewProcessor(h *Host, cfg config.Processor, resolver SecretResolver, clk clock.Clock, log
  *slog.Logger) *Processor` with `Start(ctx, emit func(model.Record)) error` (configures, opens the stream, returns
  without waiting for samples; a `ConfigError` fails it with paths), `Offer(rec model.Record)` (non-blocking, Q4),
  `Close() error`, `Input() config.Input` (the virtual input of the derived devices, for the router's births),
  `Stats() ProcessorStats{Up bool; Refused, Dropped uint64}`.

- [x] **Step 1: Failing tests:** observed samples reach the plugin, derived samples come back and are emitted with
  the plugin's ts and quality; refusals and counts per the Review Focus; `Offer` never blocks with a hung plugin and
  counts drops beyond 1,000; on a plugin crash one `bad` sample per derived signal then `death` after the tolerance,
  and after the restart `Configure` and `Process` again then birth; `Close` ends the stream and nothing is emitted
  afterwards.
- [x] **Step 2: Implement.**
- [x] **Step 3: Run the checks (also `-race -count=5 ./internal/plugin`); commit.**

### Task 6: Supervisor, status and metrics

**Files:** modify `internal/supervisor/supervisor.go` (outputs built by type over `output.Loop`; processors in diff,
build, execute, rollback, Close; the tap in `inputInstance.emit` after transforms; derived devices registered with
the router as virtual inputs; `used` per Q6), `internal/supervisor/status.go`, `internal/status/metrics.go`,
`internal/status/server.go`, tests.

**Interfaces:**
- Produces: `/status` `processors` (id, plugin, state, refused, dropped) and outputs of type `plugin:<id>` (without
  clock offset); metrics `laraplate_edge_processor_up{processor}`,
  `laraplate_edge_processor_refused_total{processor}`, `laraplate_edge_processor_dropped_total{processor}`; output
  series unchanged and filled for plugin outputs.

- [x] **Step 1: Failing tests:** a document with a processor and a plugin output runs end to end inside the
  supervisor (machine data and derived data reach the output); a processor or plugin output failing to start rolls
  the document back; Review Focus reload of one output's `config` keeps the pid; a plugin crash restarts its
  processors and outputs, the buffer keeps everything; removing the last component stops the plugin; status and
  metric families; `Close` stops processors before their plugin, outputs after the inputs as today.
- [x] **Step 2: Implement.**
- [x] **Step 3: Run the checks; commit.**

### Task 7: End to end, documentation, closing

**Files:** add to `internal/e2e/plugin_test.go`; modify `examples/` (a config with `simulated-counter`, `line-state`
and `jsonl`), `docs/CONFIGURATION.md` (`processors`, plugin outputs, secret fields, what this build refuses),
`docs/USAGE.md` (running plugins, status and metrics, troubleshooting), `README.md`, this plan,
`docs/superpowers/plans/INDEX.md`.

- [x] **Step 1: End to end.** The built agent with the SDK's `simulated-counter`, `line-state` and `jsonl` (built
  from the SDK module at v0.1.1): machine and derived samples land in the `jsonl` file; killing `jsonl` loses nothing
  (the file holds every record after the restart, `DUPLICATE` tolerated); the `.suspend` file puts the output in
  `suspended_auth` and removing it resumes; killing `line-state` leaves machine data flowing, derived signals `bad`
  then `death`, and after the restart derived data again.
- [x] **Step 2: Docs.**
- [x] **Step 3: Close.** Delivery status with divergences, `**Documented in:**`, ticks, `status: closed`, index; the
  stack spec status line; commit.

## Delivery status (2026-10-30): shipped

All seven tasks are built on branch `feat/plugin-processors-outputs`, each reviewed, then a whole-branch review and one
fix wave. The agent runs the two remaining plugin kinds on `laraedge-sdk` v0.1.1. Configuration: top-level
`processors[]` (`observe`, derived `devices` with typed signals, `death_tolerance`, `config`) and `outputs[].config`
for `plugin:<id>` types only, with the secret-key rule in every `config` and the refusals of the Review Focus at their
paths; `config validate` checks processors and plugin outputs against their manifests. `output.HTTP`'s delivery loop
moved into `output.Loop` behind a `Sender`, with no behaviour change for `laraplate-http`; a plugin output is an
`OutputSender` on the same loop (outcomes mapped as in the Global Constraints, `Send` past 30 s a retry, re-`Configure`
after a plugin restart, `ReloadCredentials` on SIGHUP). A processor gets copies of its observed devices' records,
births included, through a 1,000-record non-blocking tap after the transforms; it emits only declared derived samples
(refusals counted), and its derived devices live as a virtual input keyed `processor:<id>`, with the agent sending
births and deaths. The supervisor starts the plugins used by inputs, processors and outputs, applies and rolls back
processors and plugin outputs by difference, and reports them in `/status` and in `laraplate_edge_processor_up`,
`laraplate_edge_processor_refused_total` and `laraplate_edge_processor_dropped_total`. The end-to-end test runs the
SDK's `simulated-counter`, `line-state` and `jsonl` through a jsonl kill, a suspension and a line-state kill past the
tolerance, and checks that nothing is lost or repeated. Commits from `3705aac` to `b105a95`, and the closing commit. The
divergences from the plan, each with its reason:

- **Branch.** The work ran on `feat/plugin-processors-outputs`, branched from `develop`, not on `develop`, so the pull
  request follows the repository's branch-and-review flow.
- **The token step stays in HTTP.** `output.Loop` knows no token; HTTP resolves it in an unexported `preparer` hook the
  loop calls before sealing a batch, so an HTTP output without a usable token still suspends before sealing. A plugin
  output seals first and gets `Retry` or `Suspend` from `Send`. `LoopConfig` also has `Rand`, for deterministic jitter.
- **Unresolvable secrets in a plugin output's `config` suspend the output**, as an unresolvable HTTP token does; the
  plugin refusing the configuration after a restart is a retry (Q2). At apply, `OutputSender.Configure` runs
  synchronously, so a refusal or an unresolvable secret rolls the document back; a plugin down or timing out at apply
  only logs, the rule of plan 2's plugin inputs.
- **A batch over 16 MB is split before sending**, checked against the handshake's message limit; a one-record batch over
  it is dropped with a `rejected_invalid` marker.
- **Processor details.** After a restart a derived device is born at the first 1 s tick of the new stream; derived
  samples take the declared priority, not the plugin's; copies queued while the stream is down go to the next stream; a
  copy lost to a failed stream send is counted as dropped; processor counters restart from zero with the instance.
- **Births reach processors.** The router's births of observed devices are offered to the tap when an input registers,
  and a processor that starts after its inputs is offered their current births; such a birth can come after a sample
  sent at the same moment, or twice.
- **Restarts are done by the adapters.** On a plugin crash its processors and plugin outputs configure again and reopen
  their streams on the new generation, as plugin inputs do; the supervisor does not stop and start them.
- **Close order unchanged**: inputs, processors, plugins, outputs. A plugin output's last send during shutdown is a retry
  and its batch stays buffered.
- **`url` and `token` are refused on plugin outputs** (`outputs.<i>.url: is not allowed`), so the documentation's "leave
  them out" is enforced.

Left open, for decisions outside this plan:

- **Derived signals after a short outage.** After a processor outage shorter than `death_tolerance` its derived signals
  stay `bad` until the plugin emits again, and the new stream starts without history; documented as a known limit. A
  follow-up could offer the reopened stream the last sample of each observed signal.
- **Births after a plugin restart.** A processor whose plugin restarts on its own is not offered the births of its
  observed devices again; documented as a known limit.
- **Plugin output backoff after a plugin recovery.** A plugin output in backoff after a crash loop is not woken by the
  plugin's new generation, so delivery can resume up to 5 minutes later than it could; nothing is lost.
- **Socket path length.** With a relative `data_dir` in a deep checkout the plugin socket path can exceed the 107 bytes
  Linux allows (plan 2 host behaviour); documented in the troubleshooting table and `examples/plugin-line`. A shorter
  socket directory is a follow-up.
- **`Release` stays deferred** (Q7): a removed processor or output instance keeps its state in the plugin until the
  plugin restarts; documented as a known limit.

**Documented in:** `docs/CONFIGURATION.md`, `docs/USAGE.md` (and `README.md`, `examples/plugin-line/`).
