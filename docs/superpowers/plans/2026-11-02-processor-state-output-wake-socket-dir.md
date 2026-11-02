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
in a deep `data_dir`. Plus the plan 3 review findings that touch the same code.

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
socket directory lives under the system temp directory.

## Global Constraints

- No new dependency; `laraedge-sdk` stays at v0.1.1 and is not modified.
- `Offer` never waits on the plugin: a mutex held only for a map update is allowed, a channel send that blocks is not.
- The processor state is bounded: at most `processorStateLimit = 10000` cached signals per processor instance.
- Machine data is never touched by this work: the router, the buffers and the HTTP output behave as today.
- A `Suspend` (credentials) is never ended by a plugin wake, only by `ReloadCredentials` or its 5-minute wait.
- Fallback socket directory: `<os.TempDir()>/laraplate-edge-<first 12 hex chars of sha256(absolute data_dir)>`, mode
  `0700`, owned by the agent's effective uid, not a symbolic link; anything else refuses to start the plugin.
- An explicit `run_dir` is never replaced by the fallback: too long is the SDK's `socket path too long` error.
- Windows keeps named pipes; the socket directory logic runs on Unix builds only.
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

---

### Task 1: Processor state on stream open

**Files:**
- Modify: `internal/plugin/processor.go`
- Modify: `internal/plugin/testdata/plugin/main.go` (behaviour `busy_process`)
- Test: `internal/plugin/processor_test.go`
- Docs: `docs/CONFIGURATION.md` (processors section), `docs/USAGE.md` (processor known limits and the processor
  metrics rows), `examples/plugin-line/README.md` if it states the old limits

**Interfaces:**
- Consumes: `model.Record`, `model.DeviceEvent` (`Kind` `model.Birth` or `model.Death`), `model.Sample`.
- Produces: no new exported API. `ProcessorStats` keeps its fields; `Up` changes meaning (see Step 5).

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
    until the fourth `Process` call, then turns true.

- [ ] **Step 2: Run them to see them fail**

  Run: `go test ./internal/plugin -run 'TestProcessor(StreamOpens|StateAfter|StateSends|RecordOffered|StateIsBounded|IsNotUp)' -count=1`
  Expected: FAIL (records of value 1 present, no records after a restart, `busy_process` unknown).

- [ ] **Step 3: Add `busy_process` to the fake plugin**: the first N `Process` calls of the process answer
  `codes.AlreadyExists` at once, as `busy_runs` does for `Run`; document it in the behaviour list.

- [ ] **Step 4: Keep the state in `Offer`.** Add to `Processor` a `stateMu sync.Mutex` guarding
  `events map[string]model.DeviceEvent` (last event per observed device), `samples map[string]map[string]model.Sample`
  (device, signal) and `cached int`; constant `processorStateLimit = 10000`. `Offer` updates the state before the
  non-blocking queue send, for every observed record, the queue being full or not: an event replaces the device's last
  event and a death also deletes the device's samples; a sample replaces its signal's last sample, and a new signal past
  the limit is not cached (warn once per instance, `"processor"` and the limit only). `recordToWire` moves after the
  `p.observed` check, inside the `select` case only (plan 3 Task 5 minor).

- [ ] **Step 5: Send the state when the stream opens.** In `stream`, after the start message is sent and before
  `feed` starts: drain `p.queue` without counting (the state supersedes it), take a copy of the state under `stateMu`,
  and send it synchronously: devices in name order, for each its last event, then, when that event is not a death or
  there is none, its samples in signal order. A message over `handshake.MaxMessageSize` is skipped and counted as
  dropped; a failed `Send` ends the stream like a failed `Recv`. Move `p.up.Store(true)` from after the start message to
  the first tick and the first emitted sample, so `Up` means "the plugin is processing" (plan 3 Task 5 minor: `Up`
  before `ALREADY_EXISTS`). Update the `Processor`, `ProcessorStats.Up` and `stream` doc comments.

- [ ] **Step 6: Run the package tests**

  Run: `go test ./internal/plugin ./internal/supervisor -count=1 -race`
  Expected: PASS, including the existing processor and supervisor processor tests.

- [ ] **Step 7: Document.** `docs/CONFIGURATION.md` processors section: at every opening of its stream (first start,
  outage, plugin restart) a processor receives the current state of its observed devices (last event, then last sample
  of every signal of a living device), then the live copies; copies queued during the outage are not replayed, so a
  processor needing every change cannot rely on the tap; a record can arrive twice around the opening; a processor
  should keep its state per stream, as `line-state` does. `docs/USAGE.md`: remove the two known limits this closes
  (derived `bad` after a short outage, births after a plugin restart) and say that `processor_up` is 1 once the plugin
  processes, not when the stream is merely open.

- [ ] **Step 8: Commit**

```bash
git add internal/plugin docs examples
git commit -m "feat(plugin): a processor stream opens with the current state of its observed devices"
```

### Task 2: Plugin output woken when its plugin is back

**Files:**
- Modify: `internal/output/loop.go`
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
  - `internal/output/loop_test.go` `TestLoopWakeEndsABackoff`: a `fakeSender` variant implementing `Waker` answers
    `Retry` then `Confirmed`; after the first send the loop is in `Backoff`; a value on the wake channel makes the
    second send happen without advancing the fake clock.
  - `TestLoopWakeDoesNotEndASuspension`: same sender answering `Suspend` then `Confirmed`; a wake leaves the loop in
    `SuspendedAuth` with no second send until the clock passes 5 minutes (Review Focus 4).
  - `internal/plugin/output_test.go` `TestOutputWakesWhenThePluginIsBack`: a sender on the harness; `restartHost()`
    makes `Wake()` receive within the wait helper's bound; `Close` stops it (no receive after a further restart).

- [ ] **Step 2: Run them to see them fail**

  Run: `go test ./internal/output ./internal/plugin -run 'TestLoopWake|TestOutputWakes' -count=1`
  Expected: FAIL to compile (`output.Waker` and `OutputSender.Wake` undefined).

- [ ] **Step 3: Implement.** In `Loop`, keep the sender's wake channel (nil when the Sender is not a `Waker`), and in
  `pause` select on it only when `wakeOnReload` is false, returning on a receive; the backoff step is not reset. A wake
  that arrives outside a pause stays buffered and shortens the next backoff: accepted, one early retry. In
  `OutputSender`, a second `h.Watch()` subscription feeds a goroutine started in `NewOutputSender` that forwards each
  generation to a `chan struct{}` of capacity 1 without blocking; `Close` stops the goroutine and unwatches.

- [ ] **Step 4: Run the packages**

  Run: `go test ./internal/output ./internal/plugin ./internal/supervisor -count=1 -race`
  Expected: PASS.

- [ ] **Step 5: Document** in `docs/USAGE.md`: a plugin output in backoff tries again as soon as its plugin is back up;
  a suspended one still waits for SIGHUP or its 5 minutes. Remove the known limit.

- [ ] **Step 6: Commit**

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
    - `TestSocketDirDefault`: short `dataDir` (`t.TempDir()` when it fits) and empty `runDir` give
      `<abs dataDir>/run/plugins/<id>`, `fallback` false.
    - `TestSocketDirExplicitRunDirWins`: a `runDir` too deep gives `<runDir>/plugins/<id>`, `fallback` false, no error
      (the SDK reports the length later).
    - `TestSocketDirFallsBackWhenTooLong`: a `dataDir` of 120 bytes gives
      `<os.TempDir()>/laraplate-edge-<12 hex>/plugins/<id>`, `fallback` true, the root directory created with mode
      `0700`; the same `dataDir` gives the same directory twice. Set `TMPDIR` to a `t.TempDir()` for isolation.
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

### Task 4: Validate renders input problems through configProblems

**Files:**
- Modify: `internal/plugin/validate.go`
- Test: existing `internal/plugin/validate_test.go`, `validate_components_test.go`

**Interfaces:** none; behaviour unchanged (plan 3 Task 2 minor: the input loop duplicates `configProblems`).

- [ ] **Step 1: Replace the input loop's own rendering** with `configProblems(prefix, in.Raw, problems)` (the shape
  the processor and output loops use), and correct the `Validate` doc comment so it names inputs, processors and
  plugin outputs.

- [ ] **Step 2: Run the validation tests**

  Run: `go test ./internal/plugin -run 'TestValidate|TestResolve' -count=1`
  Expected: PASS with no test changed; a test that needs a change means the rendering differed, so stop and report.

- [ ] **Step 3: Commit**

```bash
git add internal/plugin/validate.go
git commit -m "refactor(plugin): render input problems through configProblems"
```

### Task 5: Verify, record and close

**Files:**
- Modify: this plan (ticks, delivery status), `docs/superpowers/plans/INDEX.md`
- Stack root, on its own branch and pull request: `docs/superpowers/specs/2026-10-22-laraedge-plugins-design.md`
  (Status line amendment)

- [ ] **Step 1: Full suite and lint**

  Run: `go test ./... -count=1 -race` and `go vet ./...`, then the remaining steps of `.github/workflows/ci.yml`.
  Expected: PASS, no findings.

- [ ] **Step 2: End to end** — run `go test ./internal/e2e -count=1 -run TestPluginProcessorOutput`; expected PASS
  (the SDK examples are unchanged and `line-state` keeps its state per stream).

- [ ] **Step 3: Stack spec amendment.** On a stack branch `docs/edge-processor-state`, append to the Status line of
  the plugin design: plan `laraplate-edge/docs/superpowers/plans/2026-11-02-processor-state-output-wake-socket-dir.md`
  amends it: a processor's stream opens with the current state of its observed devices; plugin sockets may live in
  `run_dir` or a private temp fallback. Open the pull request; the user merges it and bumps no submodule pointer.

- [ ] **Step 4: Close the plan**: `status: closed`, Status "Closed (date)", every box ticked, a
  `## Delivery status (date): shipped` section with divergences and what stays open, and
  `**Documented in:** docs/CONFIGURATION.md, docs/USAGE.md, examples/plugin-line/README.md`; add the INDEX row.

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/plans
git commit -m "docs(plans): close the processor state, output wake and socket directory plan"
```
