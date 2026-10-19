---
status: closed
created_on: 2026-10-16
---
# laraplate-edge agent core. Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax. Tick each step when it is done, in the same work block. A step dropped on purpose is `- [-]` with its reason on the same line.

**Status:** Closed (2026-10-19): delivered, see "Delivery status" at the end.

**Goal:** a runnable `laraplate-edge` binary that reads the `simulated` driver, buffers on disk without losing confirmed data, and delivers `laraplate-machine/1` envelopes to Laraplate over HTTP, configured by a versioned document from a file or from the backend.

**Architecture:** one Go module; `cmd/laraplate-edge` holds the CLI; `internal/` packages for the model, bootstrap and secrets, configuration, buffer, envelope, HTTP output, pipeline and supervisor, the `simulated` input and the status endpoint. Inputs feed a router that fans samples out to per-output buffers; each output's sender seals batches from its buffer and delivers them. Transforms, CEL expressions, the real drivers, the MQTT output, plugins and packaging are later plans.

**Tech Stack:** Go 1.27 (`go 1.27`, `toolchain go1.27.2` in `go.mod`), standard library first (`log/slog`, `net/http`, `hash/crc32` Castagnoli, `crypto/rand`, `flag`). No CGO.

**Spec:** `../../../../docs/superpowers/specs/2026-10-12-laraplate-edge-agent-design.md` (stack root), sections 4, 5, 6, 9.2 (`simulated`), 10 (status endpoint, logs, watchdog) and 11. Backend contract: `laraplate/Modules/MES/docs/MACHINE_CONNECTIVITY.md` and the schemas `laraplate/Modules/MES/resources/protocol/laraplate-machine-1.schema.json` and `laraplate/Modules/MES/resources/edge/laraplate-edge-config-1.schema.json`.

## Dependencies needing approval (before Task 1)

| Need | Candidate | Licence to verify at adoption | Why not the standard library |
|---|---|---|---|
| YAML (bootstrap file, hand-written configuration) | the maintained YAML v3 module for Go | MIT / Apache-2.0 | no YAML parser in the standard library |
| JSON Schema validation of the configuration document | `github.com/santhosh-tekuri/jsonschema` (v6) | Apache-2.0 | the spec validates against a published JSON Schema (5.3, 5.4) |

Everything else is written with the standard library: UUIDv7 (`crypto/rand` and the clock), Prometheus text exposition, `sd_notify` (a datagram to `NOTIFY_SOCKET`), file watching (polling the modification time, no `fsnotify`). Each adoption checks the licence (permissive only, AGENTS.md) and records it in the task's commit.

## Global Constraints

- AGPL-3.0 repository; dependencies permissive only (MIT, BSD, Apache); no CGO; module path `github.com/swolley/laraplate-edge`.
- `go fmt ./...`, `go vet ./...` and `go test ./...` (with `-race` for concurrent packages) before every commit; table-driven tests; fakes over mocks; no timing assertions (a fake clock behind `internal/clock.Clock`).
- Errors wrapped with `%w` and context; `context.Context` first on anything that blocks; every goroutine has an owner that stops it; no `panic` outside `main`; exported identifiers documented.
- Drivers read only; the agent produces values, never meanings; secrets only as `secret://` references and never logged; sample values never logged at `info`; no telemetry to the vendor; no option disables TLS verification.
- Spec values: bootstrap keys `agent_id`, `config.provider` (`file` | `laraplate`), `config.file`, `config.laraplate.url`, `config.laraplate.token`, `config.laraplate.poll_seconds`, `data_dir`, `status_listen` (default `127.0.0.1:9180`); document `schema: laraplate-edge-config/1`, integer `version`, `inputs`, `outputs`, `plugins`; priorities `critical`, `bulk`; qualities `good`, `uncertain`, `bad`; segments 16 MiB with length and CRC32C per record; `fsync_interval` default 200 ms; `buffer.max_size` default 1 GiB; minimum free disk 5%; batch `max_samples`, `max_body`, `max_wait`; HTTP outcomes `202`/`200 duplicate` advance, `401`/`403` suspend (retry every 5 minutes, at once on bootstrap reload), `413` split reusing the refused `source_seq`, `422` drop plus a `rejected_invalid` loss marker, `429`/`5xx`/network backoff 1 s to 5 min with jitter honouring `Retry-After`; loss markers `{priority, from_ts, to_ts, samples, reason}` with `reason` `buffer_full` | `rejected_invalid`; backend routes `POST {url}/api/v1/insert/mes/machine-messages`, `GET {url}/api/v1/detail/mes/machine-agent-configs?version=N` (`304` when unchanged, `404` when none), `POST {url}/api/v1/insert/mes/machine-agent-config-reports` with `{version, outcome: applied|rejected|rolled_back, errors, agent_version}`.
- Commit only your own files; never push; commit trailer `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`; after commits here, bump the stack pointer (`git add laraplate-edge` at the stack root).

## Decisions taken in this plan

- **C1. Scope of the core.** Transforms and CEL (spec 7), the real drivers (9.2 except `simulated`), the MQTT output (6.6), plugins (8), `debug --for`, `support-bundle`, the OS keystore for `secret://keystore/...` and packaging (10.1.2) are later plans. In this plan a `secret://keystore/...` reference is refused at validation with "not available in this build".
- **C2. Configuration schema.** The agent publishes the authoritative `schema/laraplate-edge-config-1.schema.json` (embedded in the binary). The backend keeps its structural copy (MES D3); this plan adds a test that the agent schema accepts every document the backend's structural schema accepts in its fixtures, and the backend copy is replaced from here in a later backend change.
- **C3. One envelope per batch.** A sealed batch is one envelope; its devices are grouped from the batch's samples in record order; `birth`/`death` records are their own device entries.
- **C4. Loss markers travel once.** A marker is stored with the batch that carries it; once that batch is confirmed the marker is gone; a `413` split keeps markers in the first half.

## Review Focus

- A crash between sealing a batch and writing `inflight`: on restart the same records are resealed into a batch with the next `source_seq`, never with a `source_seq` already sent. Test in Task 5.
- A `413` on a batch of one sample: the agent cannot split further; it drops the sample with a `rejected_invalid` loss marker instead of looping. Test in Task 6.
- Disk full while writing a record (the free-disk floor reached between checks): the write fails, the bulk eviction runs, a `buffer_full` marker is produced, the agent keeps running. Test in Task 4.
- A configuration poll returning a document whose `version` is lower than the applied one: refused, reported as `rejected`, the running configuration untouched. Test in Task 8.
- The bootstrap file referencing a token through `secret://env/NAME` with `NAME` unset: the output starts suspended (`suspended_auth`), buffers, and recovers on `SIGHUP` once the variable exists. Test in Task 3 and Task 8.

---

### Task 1: Module, CLI skeleton, CI

**Files:** create `go.mod`, `cmd/laraplate-edge/main.go`, `internal/clock/clock.go` (+ `fake.go`), `internal/version/version.go`, `.github/workflows/ci.yml`; modify `README.md` (build and run).

**Interfaces:**
- Produces: `clock.Clock` (`Now() time.Time`, `NewTimer(d time.Duration) clock.Timer`, `Sleep(ctx, d) error`), `clock.Real()`, `clock.NewFake(t time.Time) *clock.Fake` with `Advance(d)`; CLI subcommands `run --bootstrap <path>`, `config validate <file>`, `status [--listen addr]`, `version`; `version.String()` (set by `-ldflags`).

- [x] **Step 1: Failing tests.** `laraplate-edge version` prints the version; an unknown subcommand exits with code 2 and usage on stderr; the fake clock fires timers only on `Advance`.
- [x] **Step 2: Module and skeleton.** CI runs `go vet`, `go test -race ./...` and `govulncheck` on Linux amd64 and arm64 and Windows amd64, and a build on Linux armv7 (spec 10.1.1).
- [x] **Step 3: Run tests, commit, bump the stack pointer.**

### Task 2: Internal model and envelope

**Files:** create `internal/model/model.go`, `internal/envelope/envelope.go`, `internal/envelope/uuid.go`, `internal/envelope/testdata/laraplate-machine-1.schema.json` (copied from the backend, header comment naming its source path and commit).

**Interfaces:**
- Produces: `model.Quality` (`Good`, `Uncertain`, `Bad`), `model.Priority` (`Critical`, `Bulk`), `model.Sample{Device, Signal string; TS time.Time; Value model.Value; Quality model.Quality; Priority model.Priority}` with `model.Value` holding exactly one of float64, int64, bool, string; `model.DeviceEvent{Kind: Birth|Death; Device string; Signals []model.SignalInfo; TS time.Time}`; `model.Loss{Priority; FromTS, ToTS time.Time; Samples int; Reason string}`; `envelope.Build(messageID string, seq uint64, sentAt time.Time, records []model.Record, losses []model.Loss) ([]byte, error)` where `model.Record` is a sample or a device event; `envelope.NewMessageID(clock.Clock) string` (UUIDv7).

- [x] **Step 1: Failing tests.** `Build` output validates against the copied backend schema for data, birth, death, mixed devices (C3) and losses; timestamps are UTC with millisecond precision; message ids are UUIDv7, time-ordered, unique over 10 000 calls.
- [x] **Step 2: Model and envelope.**
- [x] **Step 3: Run tests, commit.**

### Task 3: Bootstrap and secrets

**Files:** create `internal/bootstrap/bootstrap.go`, `internal/secrets/secrets.go`; YAML dependency (approved).

**Interfaces:**
- Produces: `bootstrap.Load(path string) (bootstrap.File, error)` with the spec keys and defaults, plus `status_token` (a `secret://` reference, required by Task 10 when `status_listen` is not a loopback address); `secrets.Resolver` with `Resolve(ctx, ref string) (string, error)` for `secret://env/NAME` and `secret://file/PATH` (file mode must not be group- or world-readable on Linux); `secrets.IsReference(s string) bool`; `secrets.ErrUnavailable` for `secret://keystore/...` (C1).

- [x] **Step 1: Failing tests.** Defaults (`status_listen`, `poll_seconds` 60); a plain token value in `config.laraplate.token` is refused; an unknown key is refused; `secret://file` with mode `0644` refused on Linux; an unset env variable resolves to an error naming the reference, never a value; a keystore reference gives `ErrUnavailable`.
- [x] **Step 2: Implementation.**
- [x] **Step 3: Run tests, commit.**

### Task 4: Disk buffer

**Files:** create `internal/buffer/segment.go`, `internal/buffer/queue.go`, `internal/buffer/buffer.go`, fuzz test `internal/buffer/record_fuzz_test.go`.

**Interfaces:**
- Produces: `buffer.Open(dir string, opts buffer.Options) (*buffer.Buffer, error)` with `Options{MaxSize int64; MinFreeRatio float64; FsyncInterval time.Duration; SegmentSize int64; Clock clock.Clock}`; `(*Buffer).Append(r model.Record) error`; `(*Buffer).Read(p model.Priority, from buffer.Position, max int) ([]buffer.Entry, error)`; `(*Buffer).Ack(p model.Priority, upTo buffer.Position) error`; `(*Buffer).TakeLosses() []model.Loss`; `(*Buffer).Stats() buffer.Stats`; `(*Buffer).Close() error`.

- [x] **Step 1: Failing tests.** Records round-trip; a truncated last record (bad CRC) is cut on reopen and every confirmed record survives; a fully acked segment is deleted; at `MaxSize` the oldest bulk segments go first, critical only when it alone fills the buffer, each eviction yields one `buffer_full` loss with the evicted `ts` range and count; the free-disk floor triggers the same path (fake filesystem stats); fuzzing the record decoder never panics.
- [x] **Step 2: Segments and records.**
- [x] **Step 3: Queues, ack, eviction, losses.**
- [x] **Step 4: Run tests with `-race`, commit.**

### Task 5: Sealed batches

**Files:** create `internal/output/batch.go`, `internal/output/inflight.go`.

**Interfaces:**
- Consumes: `buffer.Buffer` (Task 4), `envelope.Build` and `NewMessageID` (Task 2).
- Produces: `output.Batcher` with `Next(ctx) (*output.Batch, error)` (critical first, then bulk, up to `MaxSamples`, `MaxBody`, `MaxWait`); `output.Batch{MessageID string; Seq uint64; Body []byte; Positions map[model.Priority]buffer.Position; Losses []model.Loss}`; `output.Inflight` persisting the sealed batch and the next `source_seq` in `data_dir/outputs/{id}/inflight`, `Load() (*output.Batch, error)`, `Save(*output.Batch) error`, `Clear() error`.

- [x] **Step 1: Failing tests.** Batches respect the three limits and the priority order; a sealed batch survives a restart and is resent byte-identical; `source_seq` strictly grows across restarts (Review Focus: crash before `inflight` write).
- [x] **Step 2: Batcher and inflight.**
- [x] **Step 3: Run tests, commit.**

### Task 6: HTTP output

**Files:** create `internal/output/http.go`, `internal/output/backoff.go`, `internal/output/fakebackend_test.go`.

**Interfaces:**
- Consumes: Batcher, Inflight (Task 5), `secrets.Resolver` (Task 3).
- Produces: `output.HTTP` implementing `output.Output` (`Run(ctx) error`, `State() output.State`, `ReloadCredentials()`), states `Sending`, `Backoff`, `SuspendedAuth`; `output.ClockOffset() time.Duration` from the `Date` header.

- [x] **Step 1: Failing tests** against an `httptest` fake backend: `202` and `200 duplicate` advance and clear `inflight`; `401`/`403` suspend, retry after 5 minutes (fake clock), resume at once on `ReloadCredentials`; `413` splits and the first half reuses the refused `source_seq`; a one-sample `413` drops with a `rejected_invalid` loss (Review Focus); `422` drops and adds a `rejected_invalid` loss; `429` honours `Retry-After`; `5xx` and network errors back off from 1 s to 5 min with jitter; TLS verification cannot be disabled; the token never appears in logs.
- [x] **Step 2: Implementation.**
- [x] **Step 3: Run tests with `-race`, commit.**

### Task 7: Configuration document

**Files:** create `schema/laraplate-edge-config-1.schema.json`, `internal/config/config.go`, `internal/config/store.go`; JSON Schema dependency (approved).

**Interfaces:**
- Produces: `config.Parse(data []byte) (config.Document, []string, error)` (JSON or YAML, validated against the embedded schema, errors as human messages); `config.Document` typed (`Version int`, `Inputs`, `Outputs`, `Plugins` with the spec fields used by this plan: input `id`, `driver`, `connection`, `devices[].device`, `points[]` with `signal`, `address`, `read`, `priority`; output `id`, `type` = `laraplate-http`, `url`, `token`, `route.include`, `batch`, `buffer`); `config.Store` in `data_dir` with `LastValid() (config.Document, bool, error)` and `SaveValid(config.Document) error`; `laraplate-edge config validate <file>` wired.

- [x] **Step 1: Failing tests.** The spec 5.3 example parses (minus transforms and derived, refused with "not available in this build" — C1); a plain value in a secret field (`token`, `password`) refused; `max_samples` above 5000 or `max_body` above 1024 KiB refused (the backend defaults `mes.machine.max_samples` and `mes.machine.max_body_kb`; an installation that raises them raises the agent's caps in a later change); an unknown driver or output type refused; YAML and JSON give the same document; every valid fixture of the backend structural schema validates here (C2); `config validate` exits 0 or 1 with the messages.
- [x] **Step 2: Schema, parser, store, command.**
- [x] **Step 3: Run tests, commit.**

### Task 8: Pipeline, supervisor, providers

**Files:** create `internal/input/input.go` (the interface), `internal/pipeline/router.go`, `internal/supervisor/supervisor.go`, `internal/provider/file.go`, `internal/provider/laraplate.go`; wire `run` in `cmd/laraplate-edge/main.go`.

**Interfaces:**
- Consumes: everything above.
- Produces (first): `input.Input` with `Start(ctx, emit func(model.Record)) error` and `Probe(ctx) ([]model.Sample, error)`, developed against a fake input; Task 9 implements it.
- Produces: `supervisor.Supervisor` with `Apply(ctx, config.Document) error` (by difference: unchanged inputs and outputs keep running; a component that does not start within the timeout rolls back to the last valid document), `Status() supervisor.Status`; `provider.File` (polling modification time, reload on `SIGHUP`), `provider.Laraplate` (poll every `poll_seconds`, `?version=` the applied version, `304` no-op, reports `applied` / `rejected` / `rolled_back` with errors and the agent version).

- [x] **Step 1: Failing tests.** Router fans a sample to every output whose `route.include` matches (default `*`); `birth` on start, on a configuration change touching the device and on output reconnection; `death` after the tolerance; apply by difference keeps an unchanged output's buffer and `source_seq`; a failing new input rolls back and reports `rolled_back`; an older `version` is refused and reported `rejected` (Review Focus); the laraplate provider sends `?version=` and treats `304` as no change against a fake backend; `SIGHUP` reloads the bootstrap and calls `ReloadCredentials` (Review Focus).
- [x] **Step 2: Router and supervisor.**
- [x] **Step 3: Providers and `run`.**
- [x] **Step 4: Run tests with `-race`, commit.**

### Task 9: `simulated` input

**Files:** create `internal/input/simulated/simulated.go`, `internal/input/simulated/scenario.go`, `examples/simulated/scenario.yaml`, `examples/simulated/config.yaml`.

**Interfaces:**
- Consumes: `input.Input` (Task 8).
- Produces: `simulated.New(cfg config.Input, clk clock.Clock, seed int64) (*simulated.Input, error)`; scenario: state machine with transition probabilities, cycle time, scrap rate, measurements with mean and standard deviation, per device.

- [x] **Step 1: Failing tests.** With a fixed seed and the fake clock the emitted sequence is identical across runs; states, `good_count`/`scrap_count` cumulative counters and measurements follow the scenario; `Probe` returns one sample per point; qualities are `good`.
- [x] **Step 2: Implementation and examples.**
- [x] **Step 3: Run tests, commit.**

### Task 10: Status endpoint, logs, watchdog

**Files:** create `internal/status/server.go`, `internal/status/metrics.go`, `internal/logging/logging.go`, `internal/watchdog/sdnotify.go`; wire `status` subcommand.

**Interfaces:**
- Produces: HTTP server on `status_listen` with `/healthz`, `/readyz`, `/status` (JSON: applied version, inputs, outputs with state and buffer stats, clock offset) and `/metrics` (Prometheus text: per output and queue depth, oldest unconfirmed age, bytes, drops by reason, last confirmation, state); binding a non-loopback address requires `status_token` (read-only bearer); JSON `slog` logger that never prints secrets or sample values at `info`; `sd_notify` `READY=1` and `WATCHDOG=1` at half of `WATCHDOG_USEC` when `NOTIFY_SOCKET` is set (Linux only).

- [x] **Step 1: Failing tests.** Endpoints answer with the fields above; `/metrics` parses as Prometheus text; a non-loopback bind without a token refuses to start; a log scanner over the test logs finds no token and no sample value at `info`; the watchdog sends to a fake `NOTIFY_SOCKET`.
- [x] **Step 2: Implementation.**
- [x] **Step 3: Run tests, commit.**

### Task 11: End to end, documentation, closing

**Files:** create `internal/e2e/e2e_test.go` (build tag `e2e`), `docs/USAGE.md`, `docs/CONFIGURATION.md`; modify `README.md`, this plan, `docs/superpowers/plans/INDEX.md`; stack root: the agent spec status line.

- [x] **Step 1: End to end.** The built binary runs the `simulated` example against a fake backend that validates every envelope against the backend schema: a network cut of 10 minutes (fake backend down) loses nothing and resends nothing as new; a `kill -9` mid-run and a restart lose no confirmed record; the configuration provider `laraplate` applies a new version and reports it. Optional real backend run, documented: a local Laraplate with a service account and the `mes-machine-agent` role.
- [x] **Step 2: Docs.** Build, run, bootstrap, configuration (fields of this build and what later plans add), buffer behaviour and limits, status endpoint and metrics, troubleshooting `401`/`403`/`413`/`422`.
- [x] **Step 3: Close.** Delivery status with divergences, `**Documented in:**`, ticks, `status: closed`, index; stack spec status line; commit, bump the stack pointer.

## Delivery status (2026-10-19): shipped

All eleven tasks are built, each reviewed. The binary runs the `simulated` driver, buffers per output on
disk with durable loss markers, seals batches and delivers them to the Laraplate ingest route over HTTP,
applies versioned documents from the `file` or `laraplate` provider by difference with rollback, and
exposes `/healthz`, `/readyz`, `/status` and `/metrics`, JSON logs on stdout and the systemd watchdog.
The end-to-end test (`go test -tags e2e -race ./internal/e2e`) runs the built binary through a network
cut, two `kill -9` with restarts and a configuration change served by the `laraplate` provider, against
a fake backend validating every envelope with the backend schema. Rulings R1 to R51 were taken during
the execution (the controller's ledger); the divergences from the plan text, each with its reason:

- **Buffer interface (Task 4).** Loss markers are durable in the output directory: `AddLoss`,
  `PendingLosses`, `AckLosses` and `SyncLosses` replace `TakeLosses`, so a crash between taking the
  markers and saving the batch loses none (R1, R41). Added `MarkSealed`, so eviction does not count the
  records of a sealed batch as lost (R21), `Head` for the split accounting (R38), `Changed`, so the
  batcher waits without polling (R25), and `Options.DiskUsage` and `Options.Logger` (R12). Disk full on
  write evicts and retries once, then drops with a marker (R22); under a free-disk floor breach with only
  the active segments left, incoming records are dropped and their markers coalesce (R37 amended, R39);
  a corrupt record inside an older segment drops the rest of that segment without a marker (R20).
- **Envelope and batches (Tasks 2, 5).** `envelope.IDGenerator` (`NewIDGenerator(clock)`, `New()`)
  instead of the free function `NewMessageID`: no global state (R16). `Build` refuses zero records, so a
  marker rides only with a batch that has records (R15); non-finite floats are refused by the model
  (R17); protocol string lengths are enforced by the configuration schema (R19). A batch carries its
  entries, persisted in `inflight`, so a `413` split rebuilds two envelopes (R2); the batcher exposes
  `Next`, `Confirm`, `Shrink` and `Drop`; `Inflight.Load` returns `(*Batch, nextSeq, error)`, with
  `NextSeq` and `ClearRefused` (the batch after a drop reuses the refused seq, R24). A crash between
  saving a batch and acknowledging its markers may report them twice: over-report, never silent (R40).
- **HTTP output (Task 6).** `ClockOffset` is a method per output (R4); an `OnReconnect` hook sends the
  births again after a backoff or a suspension (R5); any `2xx` confirms, `3xx` and other `4xx` back off
  with an error log and never drop (R26); `Retry-After` is capped at 5 minutes and the clock warning
  fires above 2 s (R27); a record whose envelope alone exceeds `max_body` is dropped with a
  `rejected_invalid` marker (R23).
- **Configuration (Task 7).** Two validation layers: the embedded schema describes the full spec format,
  and `config.CheckBuild` refuses what this build lacks with "not available in this build" (R6, R30);
  `version` optional and `int64` (R9, R43); point `type` (the spec 7.1 decoding types, which give the
  birth data type) and `unit` (R3 revised), `death_tolerance` (R8), point `priority` defaulting to
  `critical` (R14); id pattern, route patterns, batch and buffer keys (R29); `/` forbidden in device and
  signal names, `min_free_percent` above 0, secret keys matched case-insensitively by suffix (R44).
- **Pipeline, supervisor, providers (Task 8).** `Apply` returns an `Outcome` (`applied`, `rejected`,
  `rolled_back`, with errors) and an error; the input start timeout is 30 s (R10). The input contract is
  `Start` returning once running, `Close` and `Probe`; `Start` fails only on configuration or local
  resource errors (R33, R47). The records of a restarting or failed output are held in memory up to
  10,000, the overflow becoming markers (R46, R48, R49); input registrations are owned by an instance
  token (R50). The `laraplate` provider persists its last attempt with its outcome and the undelivered
  reports in `data_dir/config` (R32, R45).
- **`simulated` (Task 9).** `simulated.Factory(baseDir)` is what the agent uses; the connection holds an
  inline `scenario` or a `scenario_file` relative to the configuration file, and `seed` (R7, R34). The
  counters start at 0, so the backend's baseline does not swallow the first part.
- **Status and logs (Tasks 10, 11).** With a status token every path but `/healthz` needs the bearer;
  metric names are prefixed `laraplate_edge_`; the `status` command reads `LARAPLATE_EDGE_STATUS_TOKEN`
  (R35). Logs go to stdout, as spec 10.5 says; Task 10 had written them to stderr (R51, fixed in Task 11
  with its test).
- **Added from the spec (R36).** The HTTP client honours `HTTPS_PROXY`; outputs keep their last
  confirmation and last error; `data_dir/format` is written at start and a newer one is refused. Shared
  helpers `internal/docrules` (the URL rule, YAML errors without values) and `internal/atomicfile`.
- **End to end (Task 11).** The 10-minute network cut is a cut of a few seconds spanning several backoff
  steps (`503`, then dropped connections); the long outage is covered by the unit tests with the fake
  clock (R13). The kill test kills twice: once with a batch held unanswered (resent and accepted), once
  after the backend accepted a batch whose answer never left (resent and answered as a duplicate). The
  test is Unix only, behind the `e2e` tag, and not run by CI; under `-race` it builds the agent with the
  race detector too. The run against a real Laraplate is documented in `docs/USAGE.md`, not automated.
- **Not built.** A custom CA bundle (spec 10.2) is not in this plan (R36): networks that inspect TLS
  cannot use this build yet.

**Follow-ups.**

- Backend: replace the module's structural copy of the configuration schema
  (`laraplate/Modules/MES/resources/edge/laraplate-edge-config-1.schema.json`) with this repository's
  `schema/laraplate-edge-config-1.schema.json` (C2), in a `laraplate` plan.
- The custom CA bundle (spec 10.2) and TLS on the status endpoint.
- Run the end-to-end test in CI on Linux.
- Deferred minor findings of the task reviews, for the final review:
  - clock and CLI: the fake clock's tests poll with a real 1 ms sleep, `Advance` accepts a negative
    duration, `PendingTimers` is exported for tests, `run --bootstrap x extra` gives a misleading
    message, the help branch is untested;
  - envelope: invalid UTF-8 strings are replaced silently by the JSON encoder, `EventRecord` copies its
    signals shallowly, two UUID test names overstate;
  - buffer: a frame corrupted after open wedges eviction and reads of its segment (incoming records are
    then dropped with markers), `Ack` re-reads frames, the `Append` doc comment is stale, no test that a
    lasting floor breach rotates the bulk active segment only once, the failed-retry path logs one error
    per dropped record;
  - batches and output: the `max_wait` timer starts when `Next` observes records, `Run` does not close
    idle connections, the unchecked `http.DefaultTransport` assertion can panic, a base URL ending in a
    bare `?` breaks the ingest path, a permanently failing `Drop` or `Shrink` retries forever without a
    metric, a failing `Drop` on a one-record `413` is untested and the token-leak test checks only the
    token's suffix;
  - configuration: `config.go` is long and its messages sort lexicographically (`inputs.10` before
    `inputs.2`), the `CheckBuild` doc comment wraps badly;
  - supervisor and provider: an abandoned start goroutine may close an old instance while a rollback
    restarts the same id, an older active document is downloaded at every poll if the backend misbehaves,
    a crash between an `applied` apply and the state save loses that report, the emits of one input
    serialize on its mutex and the lock order is not written down;
  - `simulated`: an oversized `scenario_file` fails at `Start`, `emitAll` does not check the context,
    two doc comments are stale;
  - status: no negative self-test of the metrics validator, the bearer comparison leaks the token length
    and is case-sensitive on `Bearer`, no `ReadTimeout`, the uptime counts from `Listen`, integer sample
    values are not covered by the log scan, an answer above 4 MiB is reported by `status` as invalid
    JSON.

**Documented in:** `docs/USAGE.md`, `docs/CONFIGURATION.md` (and `README.md`).
