---
status: closed
created_on: 2026-10-19
---
# Level-1 transforms. Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax. Tick each step when it is done, in the same work block.

**Status:** Closed (2026-10-20): delivered on branch develop, see "Delivery status" at the end.

**Goal:** the agent applies the whole level-1 transform catalogue to every point, type-checked when a document is applied, with `probe` and `config test` for technicians.

**Architecture:** a new package `internal/transform` compiles each point's chain against the point's decoding kind; a processor per input instance wraps the supervisor's `emit`, applies the chains, owns one goroutine for the time-driven transforms and persists `edge_counter`. Births take their data type from the chain's output kind. `CheckBuild` stops refusing transforms.

**Tech Stack:** Go 1.27, standard library only (`regexp` is RE2). No new dependency.

**Spec:** `../specs/2026-10-19-transforms-modbus-opcua-design.md` (sections 2 D1, D5; 3; 4; 7 commands), parent `../../../../docs/superpowers/specs/2026-10-12-laraplate-edge-agent-design.md` 7.1–7.3.

## Global Constraints

- AGPL-3.0 repo; no new dependency; no CGO; module `github.com/swolley/laraplate-edge`.
- `go fmt ./...`, `gofmt -l .` empty, `go vet ./...`, `go test -race ./...`, `GOARCH=386 go test ./...` before every commit; table-driven tests; fakes over mocks; fake clock (`internal/clock`), no sleeps or timing assertions.
- Errors wrapped with `%w`; `context.Context` first on anything that blocks; every goroutine has an owner that stops it; no panic outside `main`; exported identifiers documented.
- No configuration value, secret or sample value in any error, message or log at info or above (core rule; see `internal/docrules`).
- Value kinds are those of `model.Value`: `KindBool`, `KindInt`, `KindFloat`, `KindString`. A point's input kind comes from its decoding `type`: `bool` → bool; `int16`…`uint64` → int; `float32`, `float64` → float; `string` → string.
- Catalogue, parameters, kinds and behaviour exactly as spec section 4. Runtime error of a step: a `bad` sample of the step's output kind with the zero value, counted in `laraplate_edge_transform_errors_total{input}`; a `bad` input sample passes through uncomputed.
- `edge_counter` persists to `data_dir/inputs/{input_id}/transform-state.json`, written atomically (`internal/atomicfile`) on every increment.
- Commit only your own files; never push; trailer `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Do not commit at the stack root (the user updates the stack pointer).

## Decisions taken in this plan

- **P1. Type checking runs at apply and in `config validate`.** `config` cannot import `transform` (cycle), so `transform.CheckDocument(doc config.Document) []string` is called by the supervisor before applying (messages → outcome `rejected`) and by `config validate` after `Parse` and `CheckBuild`.
- **P2. State identity.** A step's state is keyed by device, signal, position and the step's canonical JSON, so a changed chain starts that step clean (an `edge_counter` whose step changed restarts from 0; the backend treats a lower cumulative value as a reset).
- **P3. Corrupt state file.** A `transform-state.json` that cannot be read is renamed to `transform-state.json.corrupt-<unix time>`, logged at ERROR with the input id, and the counters start from 0. `Start` does not fail.
- **P4. Text keys of `map`.** Booleans `true`/`false`; integers in decimal; floats with `strconv.FormatFloat(f, 'g', -1, 64)`; strings as they are.
- **P5. `deadband` percent with a last sent value of 0:** any change passes.
- **P6. Non-finite results.** A step whose float result is NaN or ±Inf (`scale` overflow, `derivative` over a zero interval is already excluded) is a runtime error.
- **P7. `probe` and `config test` sample files.** JSON Lines, one object per sample: `{"device", "signal", "ts", "value", "quality"}`; `ts` RFC 3339 UTC with milliseconds; the value's kind on reading is the point's input kind. `probe` records raw driver samples (before transforms); `config test` replays them through the chains with a virtual clock set to each sample's `ts`, flushes pending time-driven steps at the end, and writes the resulting samples to stdout in the same format.
- **P8. `probe` input.** `laraplate-edge probe <input> [--bootstrap path] [--config file] [--record file]`: the document comes from `--config`, else the last valid document in the bootstrap's `data_dir`; it builds the input through the driver registry, calls `Probe`, prints one line per point (device, signal, value, kind, quality) plus the total response time, and never starts the input or an output.

## Review Focus

- A `heartbeat` or `min_interval` deadline firing while the input is closing: nothing is emitted after `Close` returns. Test in Task 3.
- `transform-state.json` truncated by a power loss: renamed aside, counters from 0, input running (P3). Test in Task 3.
- `scale` on a large integer producing ±Inf: a `bad` sample and the error counter, never a refused record or a stall (P6). Test in Task 1.
- `deadband: "5%"` after a sent value of 0 (P5). Test in Task 2.
- A document changing only one point's chain: the other points keep their state; the changed step starts clean (P2). Test in Task 3.

---

### Task 1: Chain compiler and stateless transforms

**Files:** create `internal/transform/transform.go` (kinds, `Step`, `Chain`, `Compile`, `KindOf`), `internal/transform/stateless.go` (`scale`, `offset`, `round`, `bit`, `bcd`, `map`, `valid_range`, `trim`, `regex_extract`), tests next to them.

**Interfaces:**
- Produces:
  - `transform.KindOf(pointType string) model.ValueKind`.
  - `transform.Step` interface: `OutKind() model.ValueKind`; `Apply(s model.Sample) (out []model.Sample, err error)` (zero or one sample for stateless steps); `Deadline() (time.Time, bool)` and `Fire(now time.Time) []model.Sample` for time-driven steps (stateless steps return no deadline).
  - `transform.Compile(steps []config.Transform, in model.ValueKind, newState StateFactory) (*Chain, error)`; the error text names the step position and the transform, never a parameter value.
  - `(*Chain).OutKind() model.ValueKind`, `(*Chain).Apply(s model.Sample) ([]model.Sample, []error)`, `(*Chain).Deadline() (time.Time, bool)`, `(*Chain).Fire(now time.Time) ([]model.Sample, []error)`; a sample produced by step k flows through steps k+1…n.
  - `transform.Store` interface: `Load() (int64, bool)`, `Save(int64) error` (persisted step state; Task 2's `edge_counter` uses it, Task 3 implements it).
  - `transform.StateFactory` is `func(position int, canonical json.RawMessage) Store`; a nil factory means no persistence (unit tests, `config test`).

- [x] **Step 1: Failing tests.** Type check: `bit` after `float32` refused with "step 1 (bit): accepts integer, got float"; `scale` then `bit` refused; `map` with mixed value kinds refused. One table per transform with spec section 4 values: `scale: 0.1` on int 123 → float 12.3; `round: 1` on 12.345 → 12.3 and on -12.35 → -12.4; `bit: 3` on 8 → true; `bcd` on 0x1234 → 1234 and on 0x1A → runtime error; `map` with `{table: {"1": "run"}}` on 1 → "run", on 2 → runtime error, with `default: "idle"` → "idle"; `valid_range: {max: 100}` on 150 → quality bad, value 150; `trim`; `regex_extract: {pattern: "W=([0-9.]+)", as: "float"}` on "W=12.5g" → 12.5, no match → runtime error. A `bad` input sample passes through unchanged. Review Focus: `scale: 1e308` on 1e308 → runtime error (P6).
- [x] **Step 2: Implement** `transform.go` and `stateless.go`. Parameter parsing rejects unknown keys and wrong types with fixed messages (no echo).
- [x] **Step 3: Run** `go test -race ./internal/transform`; commit.

### Task 2: Stateful and time-driven transforms

**Files:** create `internal/transform/stateful.go` (`deadband`, `heartbeat`, `min_interval`, `debounce`, `derivative`, `edge_counter`), tests.

**Interfaces:**
- Consumes: Task 1's `Step`, `Compile`, `StateFactory`.
- Produces: the steps registered in the catalogue so `Compile` builds them; `edge_counter` keeps its count in memory when the factory is nil.

- [x] **Step 1: Failing tests** (fake clock where time matters). `deadband: 0.5`: 10.0, 10.3 (dropped), 10.6 (sent), quality change at 10.6 (sent); `deadband: "5%"`: after 100, 104 dropped, 106 sent; after a sent 0, 0.001 sent (P5). `heartbeat: 10s`: one sample then nothing → the same value resent at +10s with the clock time; a new sample resets the deadline. `min_interval: 1s`: three samples in one window → only the last, at the window end, with its own ts. `debounce: 2s`: 1, 0 (+0.5s), 1 (+1s) → nothing until 1 stays for 2s → 1 with the ts of its change. `derivative`: 100 at t, 110 at t+5s → 2.0; first sample → nothing; 50 after 110 (reset) → nothing. `edge_counter: rising`: false, true, false, true → 1, 2; `both` → 1, 2, 3; starts from the store's value; `Save` called on every increment.
- [x] **Step 2: Implement.**
- [x] **Step 3: Run** `go test -race ./internal/transform`; commit.

### Task 3: Processor

**Files:** create `internal/transform/processor.go`, `internal/transform/statefile.go`, tests.

**Interfaces:**
- Consumes: Tasks 1–2.
- Produces:
  - `transform.NewProcessor(cfg config.Input, dataDir string, clk clock.Clock, logger *slog.Logger) (*Processor, error)`: compiles every point's chain (an error here is a programming error after `CheckDocument`, still returned).
  - `(*Processor).Wrap(emit func(model.Record)) func(model.Record)`; device events pass through; samples of points without a chain pass through.
  - `(*Processor).Start(ctx context.Context)` starts the deadline goroutine; `(*Processor).Close()` stops it and waits; after `Close` returns, the processor emits nothing.
  - `(*Processor).Errors() uint64` (runtime errors, for the metric).
  - The state file: `data_dir/inputs/{input_id}/transform-state.json`, `{"version": 1, "counters": {"<device>/<signal>/<position>/<sha256 of canonical step>": <int64>}}`.

- [x] **Step 1: Failing tests.** A chain applied through `Wrap`; a runtime error → `bad` sample with the zero value of the output kind and `Errors()` incremented; heartbeat fires through the goroutine on `clk.Advance`; Review Focus: `Close` while a deadline is due → nothing emitted after `Close` returns; `edge_counter` survives `Close` and a new `NewProcessor` on the same dir; a truncated state file → renamed `.corrupt-<unix>`, ERROR logged with the input id, counter from 0 (P3); a document changing one point's chain → other points keep their counter, the changed step starts at 0 (P2). Run with `-race`.
- [x] **Step 2: Implement.** One goroutine waits on the earliest deadline of all chains (clock timer, re-armed after each `Apply`); emits under the same lock order as the supervisor's input instance (the processor is inside the instance mutex; it never calls back into the supervisor).
- [x] **Step 3: Run** `go test -race ./internal/transform`; commit.

### Task 4: Configuration, birth data type, document check

**Files:** modify `schema/laraplate-edge-config-1.schema.json` (one `$defs` entry per transform with exact parameters), `internal/config/check.go` (stop refusing transforms of points; keep refusing derived signals and their transforms, and plugins), `internal/input/input.go` (`Signals` takes the data type from the chain), create `internal/transform/check.go`; tests.

**Interfaces:**
- Produces: `transform.CheckDocument(doc config.Document) []string` (one message per bad chain, naming input, device, signal, step position and transform); `transform.OutputDataType(p config.Point) string` (birth data type of the chain's output kind; the point's own data type when the chain is empty or invalid); `input.Signals` uses it.

- [x] **Step 1: Failing tests.** Schema: every spec section 4 parameter shape accepted; `round: 11`, `bit: 64`, `deadband: "5"`, `valid_range: {}`, `regex_extract: {}`, `edge_counter: "up"`, `map: {}` refused with fixed messages; a plain value under a secret-like key still refused (regression). `CheckBuild` accepts point transforms and still refuses `derived` and plugins. `CheckDocument` refuses `bit` after `float32`. `Signals`: a point `int16` with `bit: 0` births `boolean`; `float32` with `round: 1` births `number`; `string` with `regex_extract {as: integer}` births `number`. The C2 backend fixtures still pass layer 1 (existing test).
- [x] **Step 2: Implement.**
- [x] **Step 3: Run** `go test -race ./internal/config ./internal/transform ./internal/input ./internal/pipeline`; commit.

### Task 5: Supervisor wiring and metric

**Files:** modify `internal/supervisor/supervisor.go` (build a processor per input instance; `CheckDocument` before applying), `internal/supervisor/status.go` (`InputStatus.TransformErrors uint64`), `internal/status/metrics.go` (`laraplate_edge_transform_errors_total{input}` counter), `internal/status/server.go` (`/status` field `transform_errors`), tests.

**Interfaces:**
- Consumes: Task 3 `NewProcessor`, `Wrap`, `Start`, `Close`, `Errors`; Task 4 `CheckDocument`.

- [x] **Step 1: Failing tests.** A document with a chain applied end to end through the supervisor with the simulated input (`scale` on a measurement reaches the fake output scaled); a document whose chain does not type-check is `rejected` with the message, the running configuration untouched; restarting a changed input closes its processor (no emit after) and keeps `edge_counter` state for unchanged steps; the metric and the `/status` field appear with the error count.
- [x] **Step 2: Implement.** The processor is created with the input instance, started before `Start`, closed after the input's `Close`; `data_dir` comes from the supervisor's options.
- [x] **Step 3: Run** `go test -race ./internal/supervisor ./internal/status ./cmd/...`; commit.

### Task 6: `probe` and `config test`

**Files:** create `cmd/laraplate-edge/probe.go`, `cmd/laraplate-edge/configtest.go`, `internal/transform/samplefile.go` (JSON Lines read/write, P7); modify `cmd/laraplate-edge/main.go` (subcommands; `config validate` also runs `CheckDocument`); tests.

**Interfaces:**
- Produces: `transform.WriteSamples(w io.Writer, s []model.Sample) error`, `transform.ReadSamples(r io.Reader, kindOf func(device, signal string) (model.ValueKind, bool)) ([]model.Sample, error)` (an unknown device/signal is an error naming the line number, never the value).

- [x] **Step 1: Failing tests.** `probe` against the simulated example (`examples/simulated/config.yaml` via `--config`) prints one line per point and exit 0; `--record` writes valid JSON Lines; an unknown input id exits 1 with a message. `config test` with a document whose chain is `scale` + `deadband` and a recorded file prints the expected samples deterministically (two runs equal); time-driven steps flush at the end; a malformed line exits 1 naming the line. `config validate` reports a chain type error. Exit codes 0/1/2 as the other subcommands.
- [x] **Step 2: Implement** (P7, P8).
- [x] **Step 3: Run** `go test -race ./cmd/... ./internal/transform`; commit.

### Task 7: End to end, documentation, closing

**Files:** modify `internal/e2e/e2e_test.go` (a run with transforms), `examples/simulated/config.yaml` (a commented transform example), `docs/CONFIGURATION.md` (transforms section: catalogue table, kinds, birth data type, runtime errors, state file; remove "transforms not available in this build"), `docs/USAGE.md` (`probe`, `config test`, the new metric, the state file in the data_dir layout), this plan, `docs/superpowers/plans/INDEX.md`, `docs/superpowers/specs/INDEX.md`.

- [x] **Step 1: End to end.** The built binary with the simulated example plus `map` on `state` (to an integer code), `scale` and `deadband` on a measurement: the fake backend receives the mapped and scaled values, every envelope validates against the backend schema, births carry the chain's data types.
- [x] **Step 2: Docs.**
- [x] **Step 3: Close.** Delivery status with divergences, `**Documented in:**`, ticks, `status: closed`, both indexes; commit.

## Delivery status (2026-10-20): shipped

All seven tasks are built on branch `develop`, each reviewed, after a Task 0 the plan did not have. The
agent applies the whole level-1 catalogue of spec section 4 to the points of every input: a processor
per input instance wraps the input's emit, applies the chains, serves the time-driven steps with one
goroutine on the injected clock and persists the `edge_counter` counts in
`data_dir/inputs/{id}/transform-state.json`. Chains are type-checked by `config validate` and before
every apply (a refused chain rejects the document), births announce the data type of the chain's output,
runtime errors send the sample `bad` and are counted in `laraplate_edge_transform_errors_total{input}` and
`/status` `transform_errors`, and `probe` and `config test` let a technician try a chain offline. The
end-to-end test runs the built binary with `map` on `state` and `scale` plus `deadband` on `press_force`
and checks every transformed sample against a companion point without a chain on the same address.
Commits from `7ecb149` to `fe5b7bb`, and the closing commit. The divergences from the plan text, each with its
reason (the rulings are in "Controller rulings" below):

- **Task 0 added (R1).** The three CI failures of master were fixed first: `govulncheck` v1.9.0 instead
  of v1.1.4, whose `x/tools` panicked on Go 1.27 syntax; `atomicfile.SyncDir` of a missing directory is
  an error on Windows too; the flaky supervisor test counts a resent unconfirmed batch once (the resend
  during an output restart is the legitimate at-least-once behaviour, not a defect).
- **Module path (R0).** `github.com/getlaraplate/laraplate-edge`, not the `swolley` path of the Global
  Constraints.
- **Chain exit (R5, Task 1).** Every sample leaving a chain has the chain's output kind: a `bad` sample of
  another kind leaves with the zero value of that kind (its original value is lost), a good integer
  leaving a float chain becomes a float. The plan said only that a bad sample passes unchanged.
- **`map` numbers (R6, Task 1).** Integers and floats in one table unify to float; the plan's "mixed
  value kinds refused" now means booleans, numbers and strings mixed. The spec section 4 note was added.
  `round` decides ties on the decimal digits as written (`1.005` to `1.01`), and a float `-0` result is
  normalised to `0`.
- **`min_interval` (R7, Task 2).** The first sample of a quiet period passes at once; then at most one
  sample per interval, the latest of the window at its end. The plan's Task 2 test ("three samples in one
  window: only the last") and the spec's wording describe the window without the immediate first sample.
- **`edge_counter` (R8, Task 2).** The first sample after a start emits the current count as a baseline
  (`false, true, false, true` with `rising` gives 0, 1, 2, where the plan's Task 2 test says 1, 2).
- **`heartbeat` (R9, Task 2).** A `bad` sample that left the step is the last value and is resent `bad`;
  the wait counts from the later of the last sample's ts and the last Fire time, so a sample released
  late by a hold-back step is not resent at once.
- **`debounce` and `min_interval` with `bad` samples (R10, Task 2).** A `bad` sample cancels the value
  `debounce` waits on and makes it forget its last sent value (so the value coming back is sent again);
  in `min_interval` it passes at once and drops the held sample, and a good sample after it, in the still
  open window, waits for the window's end.
- **Step hooks (Task 2).** Two unexported optional interfaces the plan's `Step` interface does not name:
  `badObserver` (the steps that must see a `bad` sample passing uncomputed) and `clockObserver` (the
  steps that must know the time of a `Fire` before samples of earlier steps reach them). `deadband`
  compares with a relative tolerance of 1e-9, so a change of exactly the threshold passes.
- **Lock order (R2, Task 3).** Task 3 step 2 says the processor is inside the instance mutex; it is
  before it: processor, input instance, router, buffer. The processor holds its lock while it calls the
  downstream emit.
- **Pruning (R11, Task 3).** `NewProcessor` drops the counts no current chain requests and `Start` writes
  the pruned file, so a reverted step restarts at 0 instead of resuming an old, higher count. A negative
  count makes the file corrupt (P3). `NewProcessor` with an empty `dataDir` keeps no state (unit tests;
  the supervisor always passes its data directory). Runtime errors are logged with a rate limit per
  device, signal and step, in episodes ended by a quiet minute.
- **Supervisor (Task 5).** The error count of an input survives its restarts (closed processors' counts
  are kept); an abandoned start closes its processor at once, so two live processors never share a state
  file.
- **`config test` (R4, R12, Task 6).** Samples are applied in file order; a sample earlier than the
  virtual clock is applied at the clock's time. Between samples every deadline up to the next sample's
  ts fires in time order (a heartbeat repeats across a gap); after the last sample deadlines fire up to a
  horizon of the later of the last ts and the clock plus the longest time-driven duration of the
  document, replacing P7's "flushes pending time-driven steps at the end". A replay above 1,000,000
  firings exits 1. The runtime error count goes to stderr with exit 0. `probe` takes `--config` over
  `--bootstrap` when both are given and prints tab-separated lines.
- **Task 7.** The end-to-end test adds companion points without a chain (`state_text`,
  `press_force_kn`) to check each transformed value exactly; the simulated example carries the chain
  commented (the running demo keeps sending the raw values), checked by hand with `config validate`,
  `probe --record` and `config test`, and its unit test now also runs `CheckDocument`; `README.md` was
  updated besides the planned files.

Deferred minors, left as they are (from the reviews):

- fake backend: deduplicates by `message_id` only, answers `202` to a resend (the real one `200
  duplicate`); an assertion right after a birth count cannot see a later duplicate (Task 0);
- `valid_range` compares integers as floats, exact to 2^53 (documented as a known limit); parameter
  parsing accepts `3.0` as an integer; the `Chain.Apply` doc comment and a test comment predate R5; `-0`
  is still reachable through `regex_extract` with `as: float` and a `map` float literal (Task 1);
- `heartbeat` keeps the latest clock time seen only forward: a backward step of the agent's clock
  delays it by the step (documented as a known limit) (Task 2);
- a fake-clock window between `Now` and `NewTimer` (test only); the "Close while due" test takes one of
  two correct paths; a dead `downstream == nil` branch in `fire`; a failed write of the pruned file
  leaves stale counts on disk until the next count (logged, and in the troubleshooting table) (Task 3);
- `OutputDataType` compiles the chain on every call (router events); names that do not have the key
  shape, such as `press-07`, are shown as `*` (documented: the path locates the chain); no
  `CheckDocument` assertion on the spec 5.3 fixture; no exponent or out-of-range cases in
  `TestSchemaAndCompileAgree`; `map` table keys of the key shape appear in schema error paths
  (documented) (Task 4);
- an unreachable `NewProcessor` failure would leak the built input and show unfiltered names; a timeout
  subtest calls `t.Fatal` off the test goroutine; the metric's HELP wording; the series of a removed
  input disappears (documented) (Task 5);
- `ReadSamples` matches keys case-insensitively and has no total size cap; `parseArgs` re-reads flags
  after `--` (Task 6);
- the schema's `min_interval` description and spec section 4 still word `min_interval`, `heartbeat` and
  `edge_counter` as before R7 to R9; the documentation describes the delivered behaviour.

Open, awaiting the user: `golang.org/x/text` v0.14.0 (indirect) has two vulnerabilities the code does not
call; upgrading it changes `go.mod` and needs approval (Task 0).

**Documented in:** `docs/CONFIGURATION.md`, `docs/USAGE.md` (and `README.md`).

## Controller rulings (subagent-driven execution, 2026-10-20)

Every ruling the controller took while executing this plan, copied verbatim from its execution ledger,
with what each costs if wrong. The Delivery status above cites them by number.

- R0 Ruling: module path is github.com/getlaraplate/laraplate-edge (user commit 302db2a); the plan's constraint naming swolley is outdated — cost: none.
- R1 Ruling: fix the three CI causes first, as Task 0 on develop, before the plan's Task 1 — red CI is the user's pain point and each later task's CI evidence depends on it — cost: one extra small task.
- R2 Ruling: lock order is processor -> input instance -> router -> buffer. The processor holds its own lock while it calls the downstream emit, so samples of one signal keep their order across the input's goroutines and the deadline goroutine (parent 6.2); nothing downstream calls back into the processor. Task 3 step 2's "inside the instance mutex" is wrong and is corrected in the dispatch — cost: emits of one input serialize on the processor lock (they already serialize on the instance mutex).
- R3 Ruling: package transform imports config, model, clock, atomicfile only; input imports transform (for OutputDataType) — never the reverse — cost: none.
- R4 Ruling: config test processes samples in file order; a sample whose ts is earlier than the virtual clock does not move the clock back (it is applied at the current virtual time); documented — cost: time-driven steps on unordered files behave as if late.
- R5 Ruling: chain-exit invariant — every sample leaving Chain.Apply/Fire has Value.Kind == Chain.OutKind(): a bad sample of another kind gets the zero value of OutKind (other fields kept); a good integer leaving a float chain is converted to float — consumers (Task 3, encoder, births) can rely on it — cost: a bad sample's original value is lost when its kind differs.
- R6 Ruling: map values (and default) of the numeric family unify: any float -> float kind with integers promoted; all integers -> integer kind; spec section 4 gets the note "integers and floats unify to float" in Task 7 — cost: none.
- R7 Ruling: min_interval passes the first sample of a quiet period at once and emits the latest held sample at the window end (plan's "only the last" test adjusted) — lower latency, still at most one emission per interval after the first — cost: plan wording diverges, record in delivery status.
- R8 Ruling: edge_counter emits its current count as a baseline on the first sample after start, then the new count on each counted edge — backend baseline semantics (first cumulative sample = delta 0) — cost: one extra sample per start.
- R9 Ruling: heartbeat observes bad samples: a bad sample leaving the step becomes `last` and re-arms the deadline (ts + D), so heartbeat resends what actually went out (bad stays bad) — spec "resends the last value" and context rule 3 — cost: none.
- R10 Ruling: a bad sample passing debounce cancels the pending value (it was not seen unchanged for D); in min_interval a bad sample passes at once and drops the held older sample — a bad source never looks good after the fact — cost: a good value held just before a bad one is not sent.
- R11 Ruling: NewProcessor prunes in memory every counter key not requested by the current document's chains and persists the pruned map at Start — pruning only errs toward a reset, which the backend handles — cost: a step removed and re-added starts from 0.
- R12 Ruling (amends task-6-context item 3): between samples, fire every deadline <= the next sample's ts in time order, advancing the virtual clock to each (heartbeats repeat across gaps); at the end, fire deadlines in time order up to last sample ts + the longest duration parameter of the document's time-driven steps, then stop (terminates with heartbeats; flushes debounce/min_interval) — cost: heartbeat output after the last sample is bounded by that horizon.
