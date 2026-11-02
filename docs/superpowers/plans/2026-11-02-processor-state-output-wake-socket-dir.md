---
status: open
created_on: 2026-11-02
---
# Processor state on stream open, plugin output wake, short socket directory. Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax. Tick each step when it is done, in the same work block.

**Status:** Open.

**Goal:** close the four limits plan 3 left open: a processor's stream opens with the current state of its
observed devices (derived signals no longer stay `bad` after a short outage, births reach a plugin that restarted on
its own); a plugin output in backoff tries again as soon as its plugin is back; plugin sockets fit the Unix path limit
in a deep `data_dir`. Also resolve every minor finding plan 3's reviews deferred: each is fixed in the task that
touches its code, or in Task 4 (code) and Task 5 (tests) when no other task does. The table in "Plan 3 deferred
minors" maps each one to its task.

**Architecture:** `plugin.Processor` keeps the last device event and the last sample of every observed signal,
updated by `Offer`; when a `Process` stream opens it discards the queued copies and sends that state first, then the
live copies. `output.Loop` ends a backoff early when its `Sender` implements a new optional `output.Waker`, which
`plugin.OutputSender` does from the host's generation changes. The plugin host chooses its socket directory from a new
optional bootstrap key `run_dir`, falling back to a private directory under the system temp directory when the
default path would not fit.

**Tech Stack:** Go (module `github.com/getlaraplate/laraedge`), `laraedge-sdk` v0.1.1 (unchanged), gRPC.

**Spec:** stack root `docs/superpowers/specs/2026-10-22-laraedge-plugins-design.md` (P4 processor model, the
Processor protocol, the plugin lifecycle step 3), with plan 3's delivery status
(`docs/superpowers/plans/2026-10-29-plugin-processors-outputs.md`, "Left open") as the list of limits this plan
closes. Decisions taken with the user on 2026-11-02: discard the queued copies when the state is sent; the fallback
socket directory lives under the system temp directory; every deferred minor of plan 3 is resolved here or carried,
named, to the next plan.

## Global Constraints

- No new dependency; `laraedge-sdk` stays at v0.1.1 and is not modified.
- `Offer` never waits on the plugin: a mutex held only for a map update is allowed, a channel send that blocks is not.
- The processor state is bounded: at most `processorStateLimit = 10000` cached signals per processor instance.
- Machine data is never touched by this work: the router's delivery, the buffers and the HTTP output's wire behaviour
  stay as today.
- A `Suspend` (credentials) is never ended by a plugin wake, only by `ReloadCredentials` or its 5-minute wait.
- Fallback socket directory: `<os.TempDir()>/laraplate-edge-<first 12 hex chars of sha256(absolute data_dir)>`, mode
  `0700`, owned by the agent's effective uid, not a symbolic link; anything else refuses to start the plugin.
- An explicit `run_dir` is never replaced by the fallback: too long is the SDK's `socket path too long` error.
- Windows keeps named pipes; the socket directory logic runs on Unix builds only.
- Tests wait on conditions (`waitFor`, fake clock timers), never on `runtime.Gosched` spins, in every test this plan
  writes or touches.
- Code, comments and docs in English; Go files follow the sibling style (doc comment on every exported and most
  unexported identifiers, `wg.Go`, fake clock in tests).
- Commits without `Co-Authored-By` trailers.

## Review Focus

1. A device that died during the outage: the state sends its death and none of its samples, and a later birth brings
   its signals back. Test in Task 1.
2. A record offered while the state is being sent reaches the plugin once or twice, never zero times. Test in Task 1.
3. An observed input with more signals than the limit: the extra signals are not cached, one warning is logged, memory
   does not grow. Test in Task 1.
4. A plugin output suspended for an unresolvable secret is not woken by a plugin restart. Test in Task 2.
5. A fallback directory that already exists as a symbolic link, with a wider mode or another owner: the plugin stays
   down with an error naming the directory, and nothing is created through it. Test in Task 3.

## Plan 3 deferred minors

Every minor plan 3's task and final reviews deferred, with where it is resolved. "Resolved" means fixed, or a
recorded decision with its documentation; none is left silent.

| # | Finding (plan 3 task) | Resolution |
|---|---|---|
| M1 | `check.go` processor rules doc comment wrapping (T1) | Task 4 |
| M2 | Duplicate names inside one processor's `observe` accepted (T1) | Task 4: refused, schema `uniqueItems` and check |
| M3 | No test that observing another processor's derived device is refused (T1) | Task 4 |
| M4 | `TestSilentUnitDoesNotStarveTheOthers` (modbus) flaked once under `-race` (T1) | Task 5 |
| M5 | `Validate` doc comment names only inputs (T2) | Task 4 |
| M6 | Input loop duplicates `configProblems` rendering (T2) | Task 4 |
| M7 | A Retry arriving during shutdown is not recorded as last error (T3) | Task 2 |
| M8 | Redundant `tokenStep` in HTTP `Send` after `prepare` (T3) | Task 2: kept as a guard, commented as such |
| M9 | Log wording drift ("backend" to "destination"), the delivered debug line lost its status (T3) | Task 2 |
| M10 | `TestLoopDropsWithARejectedInvalidMarker` asserts only a non-empty last error (T3) | Task 2 |
| M11 | No direct test of a `preparer` answering not ready (T3) | Task 2 |
| M12 | `OutputSender` adds the output id to the logger only when `log` is nil (T4) | Task 2 |
| M13 | No test of `ReloadCredentials` or `Close` concurrent with an in-flight `Send` (T4) | Task 2 |
| M14 | The `Watch` drain in `Send` can force one extra `Configure` (T4) | Task 2: drain removed, generation compare only |
| M15 | `ConfigRefusedError{Input: ...}` reused for outputs and processors (T4) | Task 4: field renamed `ID` |
| M16 | `recordToWire` before the non-blocking select (T5) | Task 1 |
| M17 | Processor `goDown` near-duplicates `pluginInput.goDown` (T5) | Task 1: one shared helper |
| M18 | `Stats().Up` set before an `ALREADY_EXISTS` refusal (T5, final review Minor 4) | Task 1 |
| M19 | No test that copies queued while down reach the next stream (T5) | Task 1: behaviour replaced by the state; its tests |
| M20 | Refusal reasons: an invalid and undeclared sample counts as invalid; unspecified priority and malformed ts refused (T5) | Task 1: kept (the SDK refuses the same), documented |
| M21 | Processor counters restart from zero with the instance (T6) | Task 1: kept (Prometheus handles resets), metric help and docs say it |
| M22 | Clock offset presence decided twice, type prefix and interface (T6, final review Minor 5) | Task 4: carried in `OutputStatus` |
| M23 | Reload test does not show that records held at reload are delivered after it (T6) | Task 5 |
| M24 | Rollback test checks `Status` only, not that the plugin process exited (T6) | Task 5 |
| M25 | Crash test busy-spins `runtime.Gosched` per tick (T6) | Task 5 |
| M26 | e2e jsonl kill does not assert that a `DUPLICATE` resend happened (T7) | Task 5 |
| M27 | e2e `lastIndex(..., "death")` of -1 would panic `records[-1:]` (T7) | Task 5 |
| M28 | e2e: no `sending` assert after the jsonl restart (T7) | Task 5 |
| M29 | `Router.Births()` includes devices whose input reports them dead (final review, Task 6 note) | Task 1 |
| — | `CONFIGURATION.md` "not used" for `url`/`token`; `observe`/`devices` `minItems` (T7) | already resolved in plan 3 (`dc5ecc0`, schema `minItems: 1`) |

Carried to the next plan, by name, because they are not plan 3 findings or need a design of their own:

- **`runtime.Gosched` waits across the test suite** (about 25 sites in `internal/`, from agent core onward): one
  plan that gives the fake clock a way to wait for timers and replaces the spins. This plan only fixes the sites it
  touches (Global Constraints).
- **`Release` of removed instances** (plan 3 Q7, SDK ruling R18): needs a `Release` RPC in `laraedge-sdk`, a protocol
  change and an SDK release; its own plan across both repositories.

---

### Task 1: Processor state on stream open

**Files:**
- Modify: `internal/plugin/processor.go`, `internal/plugin/input.go` (shared bad-sample helper)
- Modify: `internal/pipeline/router.go` (`Births` skips dead devices)
- Modify: `internal/plugin/testdata/plugin/main.go` (behaviour `busy_process`)
- Modify: `internal/status/metrics.go` (processor counter help text)
- Test: `internal/plugin/processor_test.go`, `internal/pipeline/router_test.go`
- Docs: `docs/CONFIGURATION.md` (processors section), `docs/USAGE.md` (processor known limits and metrics rows),
  `examples/plugin-line/README.md` if it states the old limits

**Interfaces:**
- Consumes: `model.Record`, `model.DeviceEvent` (`Kind` `model.Birth` or `model.Death`), `model.Sample`.
- Produces: no new exported API. `ProcessorStats` keeps its fields; `Up` changes meaning (Step 5). In package
  `plugin`: `func badSamples(now time.Time, points []badPoint) []model.Record` with
  `type badPoint struct { device, signal string; kind model.ValueKind; priority model.Priority }`, used by both
  `goDown`s (M17).

- [ ] **Step 1: Write the failing tests** in `internal/plugin/processor_test.go`, using `newProcessorHarness` and
  `records_file`:
  - `TestProcessorStreamOpensWithTheCurrentState`: offer a birth of `d1`, then `observed("d1", 1, model.Good)` and
    `observed("d1", 2, model.Good)` before `Start`; after the stream is up, `records` holds the birth of `d1` then one
    sample `s1` with value 2, and no sample with value 1.
  - `TestProcessorStateAfterAPluginRestart`: start, offer a birth of `d1` and a sample of value 5, wait for both in
    `records`, kill the plugin (the restart helper used by `TestProcessorCrashGoesBadThenDeathThenBirthAfterRestart`);
    after the new stream is up, the new process's `records` starts with the birth of `d1` then the sample of value 5,
    without any new `Offer`.
  - `TestProcessorStateSendsTheDeathOfADeadDevice`: birth, sample, death of `d1` offered while the plugin is down; the
    reopened stream gets the death of `d1` and no sample of `d1` (Review Focus 1). A birth offered afterwards and a new
    sample are forwarded normally.
  - `TestProcessorRecordOfferedDuringTheStateIsNotLost`: offer 1,000 samples of distinct signals while the stream
    opens; every last value is present in `records` at least once (Review Focus 2).
  - `TestProcessorStateIsBounded`: offer samples of 10,001 distinct signals of `d1` while the plugin is down; the
    reopened stream gets exactly 10,000 cached samples, the log has one warning naming the processor, not the
    signals' values (Review Focus 3).
  - `TestProcessorIsNotUpWhileThePluginAnswersAlreadyExists`: behaviour `"busy_process":3`; `Stats().Up` stays false
    until the fourth `Process` call, then turns true (M18).
  - `internal/pipeline/router_test.go` `TestBirthsSkipsDeadDevices`: register an input with `d1` and `d2`, emit a death
    of `d2`; `Births()` returns only `d1`; after a birth of `d2` is emitted, both (M29).

- [ ] **Step 2: Run them to see them fail**

  Run: `go test ./internal/plugin ./internal/pipeline -run 'TestProcessor(StreamOpens|StateAfter|StateSends|RecordOffered|StateIsBounded|IsNotUp)|TestBirthsSkipsDeadDevices' -count=1`
  Expected: FAIL (records of value 1 present, no records after a restart, `busy_process` unknown, dead `d2` born).

- [ ] **Step 3: Add `busy_process` to the fake plugin**: the first N `Process` calls of the process answer
  `codes.AlreadyExists` at once, as `busy_runs` does for `Run`; document it in the behaviour list.

- [ ] **Step 4: Keep the state in `Offer`.** Add to `Processor` a `stateMu sync.Mutex` guarding
  `events map[string]model.DeviceEvent` (last event per observed device), `samples map[string]map[string]model.Sample`
  (device, signal) and `cached int`; constant `processorStateLimit = 10000`. `Offer` updates the state before the
  non-blocking queue send, for every observed record, the queue being full or not: an event replaces the device's last
  event and a death also deletes the device's samples; a sample replaces its signal's last sample, and a new signal past
  the limit is not cached (warn once per instance, `"processor"` and the limit only). `recordToWire` runs inside the
  `select` case only, after the `p.observed` check (M16).

- [ ] **Step 5: Send the state when the stream opens.** In `stream`, after the start message is sent and before
  `feed` starts: drain `p.queue` without counting (the state supersedes it), take a copy of the state under `stateMu`,
  and send it synchronously: devices in name order, for each its last event, then, when that event is not a death or
  there is none, its samples in signal order. A message over `handshake.MaxMessageSize` is skipped and counted as
  dropped; a failed `Send` ends the stream like a failed `Recv`. Move `p.up.Store(true)` from after the start message to
  the first tick and the first emitted sample, so `Up` means "the plugin is processing" (M18). Update the `Processor`,
  `ProcessorStats.Up` and `stream` doc comments.

- [ ] **Step 6: One bad-sample helper (M17).** Both `Processor.goDown` and `pluginInput.goDown` build their bad
  samples through `badSamples`; their once-per-outage guards stay where they are.

- [ ] **Step 7: `Births` skips dead devices (M29).** The router records, under its lock, the devices whose last event
  through `Emit` was a death and clears the mark on a birth or when the device's input is removed; `Births` leaves them
  out. `BirthInput` and the outputs' `birthTo` are unchanged (plan 1 ruling R5).

- [ ] **Step 8: Run the packages**

  Run: `go test ./internal/plugin ./internal/pipeline ./internal/supervisor -count=1 -race`
  Expected: PASS, including the existing processor, input and supervisor processor tests.

- [ ] **Step 9: Document.** `docs/CONFIGURATION.md` processors section: at every opening of its stream (first start,
  outage, plugin restart) a processor receives the current state of its observed devices (last event, then last sample
  of every signal of a living device), then the live copies; copies queued during the outage are not replayed, so a
  processor needing every change cannot rely on the tap; a record can arrive twice around the opening; a processor
  should keep its state per stream, as `line-state` does. Say which emitted samples are refused and counted: an invalid
  sample (malformed ts, unspecified priority, value not matching its kind) before the declared-name check, then an
  undeclared device or signal, then a declared one of the wrong kind (M20). `docs/USAGE.md`: remove the two known
  limits this closes; `processor_up` is 1 once the plugin processes, not when the stream is merely open; the processor
  counters restart from zero when the instance restarts, which `rate()` handles (M21, same words in the metric help in
  `internal/status/metrics.go`).

- [ ] **Step 10: Commit**

```bash
git add internal docs examples
git commit -m "feat(plugin): a processor stream opens with the current state of its observed devices"
```

### Task 2: Plugin output woken when its plugin is back

**Files:**
- Modify: `internal/output/loop.go`, `internal/output/http.go`
- Modify: `internal/plugin/output.go`
- Test: `internal/output/loop_test.go`, `internal/plugin/output_test.go`
- Docs: `docs/USAGE.md` (plugin output behaviour, remove the "up to 5 minutes later" known limit)

**Interfaces:**
- Produces: in package `output`:

```go
// Waker is implemented by a Sender that knows when trying again at once is
// worth it, such as a plugin output whose plugin came back. A receive on
// Wake ends a backoff early; it never ends a suspension.
type Waker interface {
	Wake() <-chan struct{}
}
```

  `plugin.OutputSender` implements it (`var _ output.Waker = (*OutputSender)(nil)`).

- [ ] **Step 1: Write the failing tests**
  - `internal/output/loop_test.go`:
    - `TestLoopWakeEndsABackoff`: a `fakeSender` variant implementing `Waker` answers `Retry` then `Confirmed`; after
      the first send the loop is in `Backoff`; a value on the wake channel makes the second send happen without
      advancing the fake clock.
    - `TestLoopWakeDoesNotEndASuspension`: same sender answering `Suspend` then `Confirmed`; a wake leaves the loop in
      `SuspendedAuth` with no second send until the clock passes 5 minutes (Review Focus 4).
    - `TestLoopRecordsARetryArrivingDuringShutdown`: the sender cancels the loop's context and answers
      `Retry` with reason `"503"`; after `Run` returns, `LastError()` is `"503"` (M7).
    - `TestLoopPreparerNotReady`: a sender implementing the unexported `preparer` (test in package `output`, file
      `loop_internal_test.go`) answering not ready with `Suspend`, then with `Retry`: no batch is sealed, the state is
      `SuspendedAuth`, then `Backoff` (M11).
    - `TestLoopDropsWithARejectedInvalidMarker` asserts the exact last error text and the marker's kind and count
      (M10).
  - `internal/plugin/output_test.go`:
    - `TestOutputWakesWhenThePluginIsBack`: a sender on the harness; `restartHost()` makes `Wake()` receive within the
      wait helper's bound; after `Close` a further restart sends nothing.
    - `TestOutputConcurrentReloadAndCloseWithASend` (M13): behaviour `send_block`; one `Send` in flight while another
      goroutine calls `ReloadCredentials` and then `Close`; run under `-race`, the `Send` returns a Retry when its
      context ends and a following `Send` is a Retry ("the output is closed").
    - `TestOutputConfiguresOncePerGeneration` (M14): `Configure` at apply, then three `Send`s on the same generation:
      `trace` holds one `configure` line.
    - `TestOutputLogsCarryTheOutputID` (M12): a sender built with a non-nil logger logs a refused configuration with
      `output=<id>`.

- [ ] **Step 2: Run them to see them fail**

  Run: `go test ./internal/output ./internal/plugin -run 'TestLoop(Wake|RecordsARetry|PreparerNotReady|DropsWith)|TestOutput(Wakes|ConcurrentReload|ConfiguresOnce|LogsCarry)' -count=1`
  Expected: FAIL (`output.Waker` and `OutputSender.Wake` undefined, retry not recorded, extra configure, no `output`
  attribute).

- [ ] **Step 3: Implement the wake.** In `Loop`, keep the sender's wake channel (nil when the Sender is not a
  `Waker`), and in `pause` select on it only when `wakeOnReload` is false, returning on a receive; the backoff step is
  not reset. A wake that arrives outside a pause stays buffered and shortens the next backoff: accepted, one early
  retry. In `OutputSender`, a goroutine started in `NewOutputSender` forwards each generation of the `h.Watch()`
  subscription to a `chan struct{}` of capacity 1 without blocking; `Close` stops it and unwatches.

- [ ] **Step 4: The minors of the loop and the senders.**
  - M7: `Run` records the outcome's reason (and the `Backoff` state) before it stops on a done context.
  - M8: `httpSender.Send`'s `tokenStep` stays, with a comment saying it guards a `Send` called without `prepare`.
  - M9: log texts say "destination" consistently, and the delivered debug line carries the destination's status again.
  - M12: `NewOutputSender` always adds `"output", cfg.ID` to the logger it is given or the host's.
  - M14: `Send` no longer drains a `Watch` channel; it configures when `cfgGen != gen` or after `ReloadCredentials`.
    The `Watch` subscription now only feeds the wake goroutine.

- [ ] **Step 5: Run the packages**

  Run: `go test ./internal/output ./internal/plugin ./internal/supervisor -count=1 -race`
  Expected: PASS.

- [ ] **Step 6: Document** in `docs/USAGE.md`: a plugin output in backoff tries again as soon as its plugin is back up;
  a suspended one still waits for SIGHUP or its 5 minutes. Remove the known limit.

- [ ] **Step 7: Commit**

```bash
git add internal/output internal/plugin docs
git commit -m "feat(output): a plugin output in backoff tries again when its plugin is back"
```

### Task 3: Short plugin socket directory

**Files:**
- Modify: `internal/bootstrap/bootstrap.go` (`File.RunDir`), its validation
- Modify: `internal/plugin/host.go` (`HostOptions.RunDir`; socket directory resolved at each launch)
- Create: `internal/plugin/socketdir_unix.go` (`//go:build unix`), `internal/plugin/socketdir_other.go` (`//go:build !unix`)
- Modify: `internal/supervisor/supervisor.go` and its config struct, `cmd/laraplate-edge/run.go`, `probe.go`, `browse.go`
  (pass `RunDir` wherever `DataDir` reaches a host)
- Test: `internal/bootstrap/bootstrap_test.go`, `internal/plugin/socketdir_unix_test.go`
- Docs: `docs/CONFIGURATION.md` (bootstrap `run_dir`; the socket limit paragraph), `docs/USAGE.md` (systemd unit,
  troubleshooting row), `examples/plugin-line/README.md` (the deep checkout note)

**Interfaces:**
- Produces:
  - `bootstrap.File.RunDir string` (`yaml:"run_dir"`), optional, absolute when set.
  - `plugin.HostOptions.RunDir string`: "directory for runtime files; empty means DataDir/run".
  - `func socketDir(dataDir, runDir, id string) (dir string, fallback bool, err error)` in package `plugin`, Unix
    build; the `!unix` build returns `filepath.Join(base, "plugins", id)` with no checks.

- [ ] **Step 1: Write the failing tests**
  - `TestLoadRefusals` gains `run_dir: relative/run` refused with `run_dir must be an absolute path`; `TestLoadValid`
    reads `run_dir: /run/laraplate-edge`.
  - `internal/plugin/socketdir_unix_test.go`:
    - `TestSocketDirDefault`: short `dataDir` and empty `runDir` give `<abs dataDir>/run/plugins/<id>`, `fallback`
      false.
    - `TestSocketDirExplicitRunDirWins`: a `runDir` too deep gives `<runDir>/plugins/<id>`, `fallback` false, no error
      (the SDK reports the length later).
    - `TestSocketDirFallsBackWhenTooLong`: a `dataDir` of 120 bytes gives
      `<os.TempDir()>/laraplate-edge-<12 hex>/plugins/<id>`, `fallback` true, the root directory created with mode
      `0700`; the same `dataDir` gives the same directory twice. Set `TMPDIR` to a short `t.TempDir()` for isolation.
    - `TestSocketDirRefusesAnUnsafeFallback` (Review Focus 5): the expected root pre-created as a symbolic link, then
      as a directory with mode `0755`: both return an error naming the directory, and nothing is created inside the
      link target.
  - `internal/plugin` host test `TestHostStartsWithADeepDataDir` (Unix): a `DataDir` deep enough that the default
    socket path exceeds the limit; the fake plugin reaches `StateUp`.

- [ ] **Step 2: Run them to see them fail**

  Run: `go test ./internal/bootstrap ./internal/plugin -run 'TestLoad|TestSocketDir|TestHostStartsWithADeepDataDir' -count=1`
  Expected: FAIL (`run_dir` unknown, `socketDir` undefined, host down with `socket path too long`).

- [ ] **Step 3: Implement.** The Unix limit is `len(syscall.RawSockaddrUnix{}.Path) - 1`, as in the SDK; a directory
  fits when `len(dir) + len("/p00000000.sock")` is within it. `socketDir`: base is `runDir`, or
  `<filepath.Abs(dataDir)>/run`; when `runDir` is set or the path fits, return it; otherwise the fallback root of the
  Global Constraints, created with `os.Mkdir(root, 0o700)` (an existing one is checked, not chmod-ed): `os.Lstat` must
  show a directory, not a symbolic link, permissions exactly `0700`, and `Stat_t.Uid == os.Geteuid()`. The host calls
  `socketDir` at every launch (so `last_error` shows a refusal), logs once at info when it uses the fallback, and
  passes the result as `LaunchConfig.SocketDir`; its removal at stop removes `<root>/plugins/<id>` only, never the
  root.

- [ ] **Step 4: Run the packages**

  Run: `go test ./internal/bootstrap ./internal/plugin ./internal/supervisor ./cmd/... -count=1 -race`
  Expected: PASS.

- [ ] **Step 5: Document.** `docs/CONFIGURATION.md`: `run_dir` in the bootstrap table (optional, absolute, holds the
  plugin sockets; default `data_dir/run`; when the default does not fit the Unix socket limit the agent uses
  `<temp>/laraplate-edge-<hash>`, private to its user, and logs it). `docs/USAGE.md`: the systemd unit gains
  `RuntimeDirectory=laraplate-edge` and `RuntimeDirectoryMode=0700`, the bootstrap example `run_dir: /run/laraplate-edge`;
  the troubleshooting row for `socket path too long` now covers only an explicit `run_dir`, and a new row covers the
  unsafe fallback directory error. `examples/plugin-line/README.md`: drop the deep checkout workaround.

- [ ] **Step 6: Commit**

```bash
git add internal cmd docs examples
git commit -m "feat(plugin): run_dir and a private fallback keep plugin sockets within the Unix limit"
```

### Task 4: Configuration, validation and status minors

**Files:**
- Modify: `schema/laraplate-edge-config-1.schema.json`, `internal/config/check.go`, `internal/config/config.go`
- Modify: `internal/plugin/validate.go`, `internal/plugin/*.go` (`ConfigRefusedError` users), `internal/supervisor/*.go`
- Modify: `internal/supervisor/status.go`, `internal/status/server.go`
- Test: `internal/config/*_test.go` (the processor rule tests), `internal/plugin/validate_test.go`,
  `internal/status/server_test.go`

**Interfaces:**
- Produces: `plugin.ConfigRefusedError.ID string` replaces the `Input` field (M15); `supervisor.OutputStatus` gains
  `HasClockOffset bool` (M22), set where `clockOffsetter` is checked today.

- [ ] **Step 1: Write the failing tests**
  - `TestProcessorObserveListedTwiceIsRefused` (M2): `observe: [d1, d1]` refused at `processors.0.observe.1` by both the
    schema and the check (the message the sibling duplicate refusals use).
  - `TestProcessorCannotObserveADerivedDevice` (M3): processor `p2` observing `line-1`, a derived device of `p1`,
    refused at `processors.1.observe.0`.
  - `internal/status/server_test.go`: an output whose type is not `laraplate-http` but reports `HasClockOffset` shows the
    clock offset field; one that does not, hides it (M22).

- [ ] **Step 2: Run them to see them fail**

  Run: `go test ./internal/config ./internal/status -run 'TestProcessorObserveListedTwice|TestProcessorCannotObserve|ClockOffset' -count=1`
  Expected: FAIL for the duplicate and the clock offset; `TestProcessorCannotObserveADerivedDevice` may already pass
  (correct by construction): keep it as the regression test M3 asked for.

- [ ] **Step 3: Implement.**
  - M2: `uniqueItems: true` on `observe` and the same refusal in `check.go`.
  - M1, M5: reflow the processor rules doc comment in `check.go`; the `Validate` doc comment names inputs, processors
    and plugin outputs.
  - M6: the input loop of `Validate` renders through `configProblems(prefix, in.Raw, problems)`.
  - M15: rename `ConfigRefusedError.Input` to `ID` with its doc comment ("the id of the input, processor or output"),
    and every construction and read of it.
  - M22: `hasClockOffset` in `status/server.go` reads `OutputStatus.HasClockOffset`; the type-prefix test goes.

- [ ] **Step 4: Run the affected packages**

  Run: `go test ./internal/config ./internal/plugin ./internal/supervisor ./internal/status -count=1 -race`
  Expected: PASS; an existing validation test whose expected text changes means the rendering differed: stop and report.

- [ ] **Step 5: Commit**

```bash
git add schema internal
git commit -m "fix(config): refuse duplicate observed devices; one clock offset decision; validation cleanup"
```

### Task 5: Test minors and the modbus flake

**Files:**
- Modify: `internal/supervisor/processor_test.go`, `internal/supervisor/plugin_test.go` (or the files holding the
  reload, rollback and crash tests named below), `internal/e2e/plugin_test.go`
- Modify: `internal/input/modbus/scheduler_test.go` or `server_test.go` (wherever
  `TestSilentUnitDoesNotStarveTheOthers` lives), and the code only if the investigation finds a defect there

**Interfaces:** none; tests only, unless Step 1 finds a defect in the modbus scheduler.

- [ ] **Step 1: The modbus flake (M4).** Use superpowers:systematic-debugging. Reproduce with
  `go test ./internal/input/modbus -run TestSilentUnitDoesNotStarveTheOthers -race -count=200` (and with `-cpu 1,2,8`).
  Find the cause before changing anything: a timing assumption in the test is fixed in the test with a condition wait;
  a scheduler defect gets a failing test first, then the fix. Record the cause in the commit message.

- [ ] **Step 2: Supervisor tests.**
  - M23: the reload test asserts that the records held while `o1` reopened reach its destination after the reload, in
    order, with consecutive `source_seq`.
  - M24: the rollback test asserts that the rolled-back plugin's process has exited (its pid no longer alive, the Unix
    helper `alive` or the host status `Pid == 0` on other platforms).
  - M25: the crash test waits on the fake clock's pending timers or on a `waitFor` condition, not a `runtime.Gosched`
    spin per tick.

- [ ] **Step 3: End-to-end test.**
  - M26: after the jsonl kill, assert that a resend was answered `DUPLICATE` (the plugin's log line or the output's
    `/status` counters, whichever the test can already read).
  - M27: clamp the `lastIndex(..., "death")` result at 0 before slicing, and fail with a message when it is -1.
  - M28: assert the output is `sending` again after the jsonl restart.

- [ ] **Step 4: Run them, repeatedly for the timing-sensitive ones**

  Run: `go test ./internal/supervisor ./internal/input/modbus -race -count=20` and
  `go test ./internal/e2e -run TestPluginProcessorOutput -count=3`
  Expected: PASS every time.

- [ ] **Step 5: Commit**

```bash
git add internal
git commit -m "test: deterministic waits and the assertions plan 3 deferred; fix the modbus flake"
```

### Task 6: Verify, record and close

**Files:**
- Modify: this plan (ticks, delivery status), `docs/superpowers/plans/INDEX.md`
- Stack root, on its own branch and pull request: `docs/superpowers/specs/2026-10-22-laraedge-plugins-design.md`
  (Status line amendment)

- [ ] **Step 1: Full suite and CI checks**

  Run: `go test ./... -count=1 -race`, `go vet ./...` and the remaining steps of `.github/workflows/ci.yml`.
  Expected: PASS, no findings.

- [ ] **Step 2: End to end** — `go test ./internal/e2e -count=1 -run TestPluginProcessorOutput`; expected PASS (the
  SDK examples are unchanged and `line-state` keeps its state per stream).

- [ ] **Step 3: Check the minors table**: every row M1 to M29 points to a commit of this branch or to a recorded
  decision; any row left open goes, named, into the delivery status and the next plan.

- [ ] **Step 4: Stack spec amendment.** On a stack branch `docs/edge-processor-state`, append to the Status line of
  the plugin design: plan `laraplate-edge/docs/superpowers/plans/2026-11-02-processor-state-output-wake-socket-dir.md`
  amends it: a processor's stream opens with the current state of its observed devices; plugin sockets may live in
  `run_dir` or a private temp fallback. Open the pull request; the user merges it and bumps no submodule pointer.

- [ ] **Step 5: Close the plan**: `status: closed`, Status "Closed (date)", every box ticked, a
  `## Delivery status (date): shipped` section with divergences, the carried items, and
  `**Documented in:** docs/CONFIGURATION.md, docs/USAGE.md, examples/plugin-line/README.md`; mark the INDEX row closed.

- [ ] **Step 6: Commit**

```bash
git add docs/superpowers/plans
git commit -m "docs(plans): close the processor state, output wake and socket directory plan"
```
