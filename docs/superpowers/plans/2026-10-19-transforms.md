---
status: open
created_on: 2026-10-19
---
# Level-1 transforms. Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax. Tick each step when it is done, in the same work block.

**Status:** In progress (subagent-driven execution, branch develop).

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

- [ ] **Step 1: Failing tests** (fake clock where time matters). `deadband: 0.5`: 10.0, 10.3 (dropped), 10.6 (sent), quality change at 10.6 (sent); `deadband: "5%"`: after 100, 104 dropped, 106 sent; after a sent 0, 0.001 sent (P5). `heartbeat: 10s`: one sample then nothing → the same value resent at +10s with the clock time; a new sample resets the deadline. `min_interval: 1s`: three samples in one window → only the last, at the window end, with its own ts. `debounce: 2s`: 1, 0 (+0.5s), 1 (+1s) → nothing until 1 stays for 2s → 1 with the ts of its change. `derivative`: 100 at t, 110 at t+5s → 2.0; first sample → nothing; 50 after 110 (reset) → nothing. `edge_counter: rising`: false, true, false, true → 1, 2; `both` → 1, 2, 3; starts from the store's value; `Save` called on every increment.
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Run** `go test -race ./internal/transform`; commit.

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

- [ ] **Step 1: Failing tests.** A chain applied through `Wrap`; a runtime error → `bad` sample with the zero value of the output kind and `Errors()` incremented; heartbeat fires through the goroutine on `clk.Advance`; Review Focus: `Close` while a deadline is due → nothing emitted after `Close` returns; `edge_counter` survives `Close` and a new `NewProcessor` on the same dir; a truncated state file → renamed `.corrupt-<unix>`, ERROR logged with the input id, counter from 0 (P3); a document changing one point's chain → other points keep their counter, the changed step starts at 0 (P2). Run with `-race`.
- [ ] **Step 2: Implement.** One goroutine waits on the earliest deadline of all chains (clock timer, re-armed after each `Apply`); emits under the same lock order as the supervisor's input instance (the processor is inside the instance mutex; it never calls back into the supervisor).
- [ ] **Step 3: Run** `go test -race ./internal/transform`; commit.

### Task 4: Configuration, birth data type, document check

**Files:** modify `schema/laraplate-edge-config-1.schema.json` (one `$defs` entry per transform with exact parameters), `internal/config/check.go` (stop refusing transforms of points; keep refusing derived signals and their transforms, and plugins), `internal/input/input.go` (`Signals` takes the data type from the chain), create `internal/transform/check.go`; tests.

**Interfaces:**
- Produces: `transform.CheckDocument(doc config.Document) []string` (one message per bad chain, naming input, device, signal, step position and transform); `transform.OutputDataType(p config.Point) string` (birth data type of the chain's output kind; the point's own data type when the chain is empty or invalid); `input.Signals` uses it.

- [ ] **Step 1: Failing tests.** Schema: every spec section 4 parameter shape accepted; `round: 11`, `bit: 64`, `deadband: "5"`, `valid_range: {}`, `regex_extract: {}`, `edge_counter: "up"`, `map: {}` refused with fixed messages; a plain value under a secret-like key still refused (regression). `CheckBuild` accepts point transforms and still refuses `derived` and plugins. `CheckDocument` refuses `bit` after `float32`. `Signals`: a point `int16` with `bit: 0` births `boolean`; `float32` with `round: 1` births `number`; `string` with `regex_extract {as: integer}` births `number`. The C2 backend fixtures still pass layer 1 (existing test).
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Run** `go test -race ./internal/config ./internal/transform ./internal/input ./internal/pipeline`; commit.

### Task 5: Supervisor wiring and metric

**Files:** modify `internal/supervisor/supervisor.go` (build a processor per input instance; `CheckDocument` before applying), `internal/supervisor/status.go` (`InputStatus.TransformErrors uint64`), `internal/status/metrics.go` (`laraplate_edge_transform_errors_total{input}` counter), `internal/status/server.go` (`/status` field `transform_errors`), tests.

**Interfaces:**
- Consumes: Task 3 `NewProcessor`, `Wrap`, `Start`, `Close`, `Errors`; Task 4 `CheckDocument`.

- [ ] **Step 1: Failing tests.** A document with a chain applied end to end through the supervisor with the simulated input (`scale` on a measurement reaches the fake output scaled); a document whose chain does not type-check is `rejected` with the message, the running configuration untouched; restarting a changed input closes its processor (no emit after) and keeps `edge_counter` state for unchanged steps; the metric and the `/status` field appear with the error count.
- [ ] **Step 2: Implement.** The processor is created with the input instance, started before `Start`, closed after the input's `Close`; `data_dir` comes from the supervisor's options.
- [ ] **Step 3: Run** `go test -race ./internal/supervisor ./internal/status ./cmd/...`; commit.

### Task 6: `probe` and `config test`

**Files:** create `cmd/laraplate-edge/probe.go`, `cmd/laraplate-edge/configtest.go`, `internal/transform/samplefile.go` (JSON Lines read/write, P7); modify `cmd/laraplate-edge/main.go` (subcommands; `config validate` also runs `CheckDocument`); tests.

**Interfaces:**
- Produces: `transform.WriteSamples(w io.Writer, s []model.Sample) error`, `transform.ReadSamples(r io.Reader, kindOf func(device, signal string) (model.ValueKind, bool)) ([]model.Sample, error)` (an unknown device/signal is an error naming the line number, never the value).

- [ ] **Step 1: Failing tests.** `probe` against the simulated example (`examples/simulated/config.yaml` via `--config`) prints one line per point and exit 0; `--record` writes valid JSON Lines; an unknown input id exits 1 with a message. `config test` with a document whose chain is `scale` + `deadband` and a recorded file prints the expected samples deterministically (two runs equal); time-driven steps flush at the end; a malformed line exits 1 naming the line. `config validate` reports a chain type error. Exit codes 0/1/2 as the other subcommands.
- [ ] **Step 2: Implement** (P7, P8).
- [ ] **Step 3: Run** `go test -race ./cmd/... ./internal/transform`; commit.

### Task 7: End to end, documentation, closing

**Files:** modify `internal/e2e/e2e_test.go` (a run with transforms), `examples/simulated/config.yaml` (a commented transform example), `docs/CONFIGURATION.md` (transforms section: catalogue table, kinds, birth data type, runtime errors, state file; remove "transforms not available in this build"), `docs/USAGE.md` (`probe`, `config test`, the new metric, the state file in the data_dir layout), this plan, `docs/superpowers/plans/INDEX.md`, `docs/superpowers/specs/INDEX.md`.

- [ ] **Step 1: End to end.** The built binary with the simulated example plus `map` on `state` (to an integer code), `scale` and `deadband` on a measurement: the fake backend receives the mapped and scaled values, every envelope validates against the backend schema, births carry the chain's data types.
- [ ] **Step 2: Docs.**
- [ ] **Step 3: Close.** Delivery status with divergences, `**Documented in:**`, ticks, `status: closed`, both indexes; commit.
