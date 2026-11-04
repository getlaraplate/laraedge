---
status: open
created_on: 2026-11-04
---
# Deferred minors cleanup. Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax. Tick each step when it is done, in the same work block.

**Status:** Open.

**Goal:** resolve the minor review findings the closed agent plans deferred (agent core, transforms, modbus,
opcua, plugin host), and the items plan 2026-11-02 carried: the `runtime.Gosched` waits across the test suite, the
`waitFile` and `restartClock` helpers, the modbus flake. Each finding ends fixed, or decided with its reason written
where a reader finds it. Nothing is left silent.

**Architecture:** no new feature. One new test primitive, `(*clock.Fake).BlockUntil`, replaces the polling loops
every package copied; the other changes are local fixes, small hardening, tests that pin behaviour, and three pure
file splits. Tasks are grouped by package so each review sees one area.

**Tech Stack:** Go (module `github.com/getlaraplate/laraedge`), `laraedge-sdk` v0.1.1 (unchanged).

**Spec:** none of its own. The authority for each item is the plan whose review deferred it
(`docs/superpowers/plans/2026-10-16-agent-core.md`, `2026-10-19-transforms.md`, `2026-10-20-modbus.md`,
`2026-10-21-opcua.md`, `2026-10-26-plugin-host-inputs.md`, `2026-11-02-processor-state-output-wake-socket-dir.md`)
and the stack spec those plans implement. The inventory below was verified against the code on 2026-11-04
(`origin/develop` at `ce7bded`); an item marked "verify" was matched by area only, and its task starts by confirming
it.

## Global Constraints

- No new dependency. No behaviour change visible to an operator unless the item says so; each such change is named
  in the delivery status and in `docs/`.
- Machine data handling (router delivery, buffer, batches, HTTP wire format) is unchanged except where an item fixes
  a defect in it.
- An item marked "verify" that turns out FIXED or OBSOLETE gets no code: its task records the evidence (file:line or
  commit) in the report, and Task 9 copies it to the delivery status.
- An item resolved by a decision instead of code gets the decision written in the code comment or doc the reader
  would look at, not only in this plan.
- Tests wait on conditions: `BlockUntil` for fake-clock timers, a bounded sleep-poll helper for anything else. No
  `runtime.Gosched` spin remains in a wait loop at the end of Task 2, except the `-race` pollers named there.
- Pure moves (Task 8) change no identifier, no behaviour and no test; `git diff --stat` shows moved lines only, and
  `go test ./...` is green before and after with the same test count.
- Code, comments and docs in English; sibling style; commits without `Co-Authored-By` trailers.

## Review Focus

1. `BlockUntil` waiting for N timers when a timer fires or is stopped meanwhile: it must count live timers, wake on
   every change, and fail the test (not hang) at its bound. Test in Task 1.
2. A test converted from a Gosched spin to `BlockUntil` that waited for "at least n" timers before: the converted wait
   must keep "at least", not "exactly". Checked per site in Task 2.
3. The router's rate-limited warning on a refusing buffer: the first refusal is logged at once, later ones are
   summarised, and no refusal is lost from the counters. Test in Task 5.
4. The status server's new read and write timeouts: a slow `/status` or `/metrics` answer on a large agent is not cut
   off. Test in Task 5.
5. Natural ordering of configuration messages: `inputs.2` before `inputs.10`, and equal paths keep their message
   order. Test in Task 6.

## Inventory

Status as verified on 2026-11-04. "verify" = matched by area only.

| # | Plan/task | Finding | Evidence | Task |
|---|---|---|---|---|
| C1 | agent-core T1 | `Fake.Advance` with a negative duration moves time backwards | `internal/clock/fake.go:69-96` | 1 |
| C2 | agent-core T1 | `fake_test` waits with real `time.Sleep(1ms)`; redundant `started` channel | `internal/clock/fake_test.go:110-116,203` | 1 |
| C3 | agent-core T1 | timer firing order by deadline is not pinned by a test | `fake.go:87` | 1 |
| C4 | 2026-11-02 carried | 24 `runtime.Gosched` spin sites in tests (11 fake-clock, 11 condition, 2 `-race` pollers) | see Task 2 | 2 |
| C5 | 2026-11-02 carried | `waitFile` spins; `restartClock` recognises the restart wait by stack frame name | `internal/supervisor/processor_test.go:76-86,124-133` | 2 |
| C6 | agent-core T5 | `waitTimers` spins with a 50M-iteration guard; weak "not before max_wait" check | `internal/output/batch_test.go:263` | 2 |
| C7 | agent-core T1 | `run --bootstrap x extra` says "--bootstrap is required" | `cmd/laraplate-edge/main.go:145-146` | 3 |
| C8 | agent-core T1 | help branch untested | `main.go:115-118` | 3 |
| C9 | transforms T6 | `parseArgs` re-parses flags after `--` | `main.go:347-359` | 3 |
| C10 | agent-core T10 | `status` command: an answer over 4 MiB is reported as invalid JSON | `cmd/laraplate-edge/status.go:20,54` | 3 |
| C11 | agent-core T2 | `EventRecord` aliases the caller's `Signals` slice | `internal/model/model.go:287` | 4 |
| C12 | transforms T1 | `-0` reachable through `regex_extract` and map float literals | `model.go:119-125` | 4 |
| C13 | agent-core T2 | invalid UTF-8 strings reach the envelope (JSON replaces them silently) | no check in `internal/model`, `internal/envelope` | 4 |
| C14 | agent-core T2 | uuid test `ClockGoingBackwards` overstates; counter layout not pinned | `internal/envelope/uuid_test.go:120` | 4 |
| C15 | agent-core T4 | `Ack` re-reads frames between head and `upTo` to count them | `internal/buffer/buffer.go:648` | 4 |
| C16 | agent-core T4 | no test that a lasting floor breach rotates the bulk segment once over several Appends | `buffer_test.go:798-812` | 4 |
| C17 | agent-core final | `Ack` inside a region just cut by `dropCorrupt` fails once (spurious error, one duplicate send) | verify, `buffer.go` / `queue.go` | 4 |
| C18 | agent-core final | a segment that cannot be opened during recount leaves records uncounted | verify, `queue.go` | 4 |
| C19 | agent-core T5 | `MaxWait` timer starts when `Next` sees records, not at the first record's append | `internal/output/batch.go:314-320` | 4 |
| C20 | agent-core T6 | `Run` never calls `CloseIdleConnections` (transport goroutines outlive a reload) | no call in `internal/` | 4 |
| C21 | agent-core T6 | unchecked `http.DefaultTransport.(*http.Transport)` assertion can panic | `internal/output/http.go:194` | 4 |
| C22 | agent-core T6 | a permanently failing `Drop`/`Shrink` retries forever with only a log line | `internal/output/loop.go:319-320,334-336` | 4 |
| C23 | agent-core T6 | failing `Drop` on a one-record 413 untested; token-leak test checks only the suffix | verify, `loop_test.go`, `http_test.go` | 4 |
| C24 | transforms T0 | fake backend dedups by `message_id` only and answers 202 to a resend | verify, `fakebackend_test.go` | 4 |
| C25 | agent-core final | router logs a warning per refused record on a refusing buffer | `internal/pipeline/router.go:473` | 5 |
| C26 | agent-core T10 | status server has no `ReadTimeout`/`WriteTimeout` | `internal/status/server.go:146` | 5 |
| C27 | agent-core T10 | token compare leaks the length; `Bearer` scheme is case-sensitive | `server.go:238-239` | 5 |
| C28 | agent-core T10 | uptime counted from `Listen`, not from process start | `server.go:364` | 5 |
| C29 | agent-core T10 | metrics validator has no negative self-test; integer samples skipped by the log scan | verify, `metrics_test.go`, `logscan_test.go` | 5 |
| C30 | modbus T1 | `input_connected`/`read_errors` families vanish for a failing reporting input | verify, `internal/status/metrics.go` | 5 |
| C31 | transforms T5 | an input's metric series vanish when it is removed | verify, `metrics.go:105-117` | 5 |
| C32 | agent-core T7 | configuration messages sorted lexicographically (`inputs.10` before `inputs.2`) | `internal/config/schema.go:84` | 6 |
| C33 | agent-core T7, modbus T5 | `CheckBuild` doc comment wrap; `build.yaml` header out of date | verify, `check.go`, `testdata` | 6 |
| C34 | transforms T1 | `valid_range` compares integers through float64 with no code comment (documented in CONFIGURATION.md:1007) | `internal/transform/stateless.go:482` | 6 |
| C35 | transforms T1 | `integerIn` accepts `3.0` | verify, `stateless.go` | 6 |
| C36 | transforms T2 | heartbeat `lastNow` only moves forward: a backward wall-clock step delays it | `internal/transform/stateful.go:218-219` | 6 |
| C37 | transforms T3 | dead `downstream == nil` branch in `fire` | `internal/transform/processor.go:364` | 6 |
| C38 | transforms T3 | fake-clock window between `Now` and `NewTimer`; nondeterministic "Close while due" path | verify, `processor_test.go` | 6 |
| C39 | transforms T4 | `OutputDataType` compiles per call | `internal/transform/check.go:75` | 6 |
| C40 | transforms T4 | safe-shape rule hides ids as `*`; no `CheckDocument` nil assertion; no exponent cases | verify, `check_test.go` | 6 |
| C41 | transforms T6 | `ReadSamples`: case-insensitive keys, no total size cap | verify, `samplefile.go` | 6 |
| C42 | transforms T5 | an unreachable `NewProcessor` failure leaks the built input; timeout subtest calls `t.Fatal` off the test goroutine | verify, `internal/supervisor` | 7 |
| C43 | agent-core T8 | an abandoned start may close an old instance while the rollback restarts the same id | verify, `supervisor.go` | 7 |
| C44 | agent-core T8 | an older active document is re-downloaded at every poll if the backend misbehaves | verify, `internal/provider/laraplate.go` | 7 |
| C45 | agent-core T8 | emits of one input instance serialise on its mutex (by design) | verify | 7 |
| C46 | agent-core T9 | simulated: `emitAll` has no ctx check; oversize `scenario_file` only fails at Start; doc comments | `internal/input/simulated/simulated.go:456-460` | 7 |
| C47 | modbus T1 | `driverkit` backoff duplicates `output/backoff.go` | both files | 7 |
| C48 | modbus T2 | point types listed twice (`pointTypes` and the size switch) | `internal/input/modbus/config.go:373-392` | 7 |
| C49 | modbus T2-T7 | message style, misnamed test, missing negative tests, ties near the 65535 cap, DCBA int64, answer after cancel, vacuous black-hole test, `rtuServer.t` unused, R10 tests without `t.Cleanup`, serial open under `in.mu`, backoff wording | verify, `internal/input/modbus` | 7 |
| C50 | modbus T6 | serial open error is one fixed text | `driver.go:154` | 7 |
| C51 | modbus T5 | Probe stops at the first block answering 5/6 or 10/11 (documented in USAGE.md:208-211) | verify | 7 |
| C52 | modbus T7 | stack spec D6 says the serial port is exclusive, false on Linux | `docs/superpowers/specs/2026-10-19-transforms-modbus-opcua-design.md` (stack root) | 9 |
| C53 | 2026-11-02 carried (M4) | `TestSilentUnitDoesNotStarveTheOthers` flaked once, never reproduced | `internal/input/modbus/scheduler_test.go:49` | 7 |
| C54 | opcua T1 | numeric deadband in exponent form refused, while config renders numbers with `g` | `internal/input/opcua/config.go:379-384`, `internal/config/decode.go:113,246` | 7 |
| C55 | opcua T1 | unknown connection keys echoed verbatim (also modbus) | no safe-key gate in `internal/input` | 7 |
| C56 | opcua T2 | stale `own-*.tmp` from a crash may keep a private key; redundant `SyncDir(tmp)`; existing `key.pem` mode unchecked; `Trusted` checks the name only | `internal/input/opcua/pki.go:94-119,342` | 7 |
| C57 | opcua T6 | one bad child status aborts the whole browse | `internal/input/opcua/browse.go:412-413` | 7 |
| C58 | opcua T7 | `opcuatest` `Server.Drop` unused | `internal/input/opcua/opcuatest/server.go:230` | 7 |
| C59 | opcua T1-T7 | non-subscribing drivers ignore sampling/queue/deadband silently; -0 and MinInt64 tests; pre-1970 `ReadTime`; session lost before arm; proxy conns not pruned; unknown handle not liveness; `--node` on a leaf prints nothing; blank ExpandedNodeId types; negative browse limit; opcuatest duplicate cert code; e2e log scan at info only; doc wording | verify, `internal/input/opcua`, `cmd/laraplate-edge/browse.go` | 7 |
| C60 | agent-core T7, plugin-host left open, opcua T4/T5 | `config.go` 1025 lines; `supervisor.go` 1397 lines; opcua `session.go` 1111 and `driver.go` 797 lines | file sizes | 8 |

Outside this plan, by name:

- **TLS on the status endpoint** (agent-core follow-up): a feature with a product choice (certificates, rotation),
  not a minor. It needs its own spec.
- **`Release` of removed processor and output instances** (plan 3 Q7): needs a `Release` RPC in `laraedge-sdk` and an
  SDK release; its own plan across both repositories.
- **The `laraedge-sdk` deferred minors** (14 open items from `laraedge-sdk/docs/superpowers/plans/2026-10-23-sdk.md`):
  they belong to that repository and get a cleanup plan there.
- **Windows-only behaviours** (plugin memory reader, kill after the deadline): judged only by the Windows CI job,
  which runs (`44a7c8c`); no local action.

---

### Task 1: `clock.Fake.BlockUntil` and the fake clock minors

**Files:**
- Modify: `internal/clock/fake.go`
- Test: `internal/clock/fake_test.go`

**Interfaces:**
- Produces:

```go
// BlockUntil waits until at least n timers are pending (created, neither fired
// nor stopped), or fails t after the bound. It wakes on every NewTimer, Stop
// and Advance.
func (f *Fake) BlockUntil(t interface{ Helper(); Fatalf(format string, args ...any) }, n int)
```

  The bound is 10 s (the `guard` the test packages use). `BlockUntil` lives in `fake.go` so every package's tests can
  call it; it takes a small interface instead of `testing.TB`, so `internal/clock` does not import `testing` into the
  agent binary.

- [ ] **Step 1: Write the failing tests**
  - `TestBlockUntilReturnsWhenTimersArePending`: a goroutine creates 2 timers after a delay; `BlockUntil(t, 2)`
    returns once both exist.
  - `TestBlockUntilCountsLiveTimers`: 3 timers, 1 stopped, 1 fired by `Advance`: `BlockUntil(t, 1)` returns,
    `BlockUntil` with 2 fails the test through a recording fake of that interface (Review Focus 1).
  - `TestAdvanceNegativeDoesNotMoveBack` (C1): `Advance(-time.Second)` leaves `Now()` unchanged and fires nothing.
  - `TestAdvanceFiresInDeadlineOrder` (C3): timers at 3 s, 1 s, 2 s; one `Advance(5s)`; a single receiver records the
    order 1 s, 2 s, 3 s and each value equals its deadline.

- [ ] **Step 2: Run them to see them fail**

  Run: `go test ./internal/clock -run 'TestBlockUntil|TestAdvanceNegative|TestAdvanceFires' -count=1`
  Expected: FAIL (`BlockUntil` undefined; negative advance moves back).

- [ ] **Step 3: Implement.** `Fake` gains a `sync.Cond` (or a channel closed and replaced on every change) signalled
  in `NewTimer`, `Stop` and `Advance`; `BlockUntil` waits on it with the bound. `Advance` treats `d < 0` as 0. In
  `fake_test.go`, replace `waitForTimers`'s `time.Sleep` poll with `BlockUntil` and delete the redundant `started`
  channel (C2).

- [ ] **Step 4: Run the package**

  Run: `go test ./internal/clock -count=1 -race`
  Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/clock
git commit -m "feat(clock): Fake.BlockUntil, a negative Advance is a no-op, firing order pinned"
```

### Task 2: No `runtime.Gosched` waits in the tests

**Files:**
- Modify (tests only): `internal/watchdog/sdnotify_test.go`, `internal/input/opcua/driver_test.go`,
  `internal/input/simulated/simulated_test.go`, `internal/input/driverkit/backoff_test.go`,
  `internal/input/modbus/server_test.go`, `internal/input/modbus/scheduler_test.go`,
  `internal/output/batch_test.go`, `internal/output/fakebackend_test.go`, `internal/supervisor/plugin_unix_test.go`,
  `internal/supervisor/plugin_test.go`, `internal/supervisor/supervisor_test.go`,
  `internal/supervisor/processor_test.go`, `internal/plugin/host_test.go`, `internal/provider/fakes_test.go`,
  `internal/input/lifecycle_test.go`, `internal/transform/processor_test.go`

**Interfaces:**
- Consumes: `(*clock.Fake).BlockUntil(t, n)` from Task 1.
- Produces: in each test package that needs one, a `waitPoll(t, what string, cond func() bool)` helper polling every
  1 ms up to the package's `guard` (the shape `internal/output/loop_test.go` already has).

- [ ] **Step 1: Inventory.** Run `grep -rn "runtime.Gosched" --include=*_test.go internal` and list each site in the
  report with its kind: fake-clock wait, condition wait, drive loop (advances the clock until a condition), `-race`
  poller. The 2026-11-04 count is 24 sites in 21 files; the two `-race` pollers are
  `internal/output/fakebackend_test.go:257` and `internal/supervisor/supervisor_test.go:90`.

- [ ] **Step 2: Convert.**
  - Fake-clock waits ("until `PendingTimers()` reaches n"): `BlockUntil(t, n)`, keeping "at least n" semantics
    (Review Focus 2). C6: `batch_test.go`'s `waitTimers` and its 50M-iteration guard go; its "not before max_wait"
    check becomes: `BlockUntil` for the batch timer, `Advance(maxWait - 1ns)`, assert no batch, `Advance(1ns)`,
    assert the batch.
  - Drive loops (`host_test.go:229`, `supervisor_test.go:622,750`): advance in fixed steps after `BlockUntil` for the
    timer the code under test is waiting on, then a `waitPoll` for the condition.
  - Condition waits: `waitPoll`.
  - C5: `waitFile` becomes a `waitPoll`. `restartClock` drops its stack-name match: it counts restart waits through
    `BlockUntil` on the hosts' pending timers when that count identifies them; when another timer makes the count
    ambiguous, keep the wrapper but match on a counter the test increments around the code that sleeps, never on a
    frame name. Record which in the report.
  - `-race` pollers: stay; each gets a comment saying it hammers the API on purpose for the race detector and is not a
    wait.

- [ ] **Step 3: Verify**

  Run: `grep -rn "runtime.Gosched" --include=*_test.go internal` — only the two `-race` pollers remain.
  Run: `go test ./... -race -count=3` — PASS every time; the test count equals the count before this task.

- [ ] **Step 4: Commit**

```bash
git add internal
git commit -m "test: wait on BlockUntil and bounded polls instead of Gosched spins"
```

### Task 3: Command line minors

**Files:**
- Modify: `cmd/laraplate-edge/main.go`, `cmd/laraplate-edge/status.go`
- Test: `cmd/laraplate-edge/main_test.go`, the status command test

- [ ] **Step 1: Write the failing tests**
  - C7: `run --bootstrap x extra` fails with `unexpected argument "extra"`; `run` without `--bootstrap` keeps
    `--bootstrap is required`.
  - C8: `help`, `--help` and `-h` print the usage and exit 0 (table cases).
  - C9: `parseArgs` stops at `--`: `probe --input a -- --not-a-flag` keeps `--not-a-flag` as a positional argument
    (or refuses it with the command's own message, if the command takes no positionals; follow the existing
    command's contract and say which).
  - C10: a status answer of 4 MiB + 1 byte is reported as `answer too large (over 4 MiB)`, not as invalid JSON.

- [ ] **Step 2: Run them to see them fail**

  Run: `go test ./cmd/laraplate-edge -count=1 -run 'Unexpected|Help|ParseArgs|TooLarge'`
  Expected: FAIL.

- [ ] **Step 3: Implement.** Separate message for extra arguments; the `parseArgs` loop stops at `--`; the status
  client reads `limit+1` bytes and reports the overflow.

- [ ] **Step 4: Run, commit**

  Run: `go test ./cmd/... -count=1 -race`. Expected: PASS.

```bash
git add cmd
git commit -m "fix(cmd): extra argument, help, -- and oversize status answers"
```

### Task 4: Model, envelope, buffer and output minors

**Files:**
- Modify: `internal/model/model.go`, `internal/envelope/*.go`, `internal/buffer/buffer.go`, `internal/buffer/queue.go`,
  `internal/output/batch.go`, `internal/output/http.go`, `internal/output/loop.go`, `internal/status/metrics.go`
  (C22 counter)
- Test: the matching `_test.go` files
- Docs: `docs/USAGE.md` (C22 metric row, C13 refusal if visible)

**Interfaces:**
- Produces: metric `laraplate_edge_output_settle_failures_total{output}` (C22): counts each failed `Drop` or
  `Shrink` attempt of a refused batch; `/status` `last_error` already carries the text.

- [ ] **Step 1: Verify the "verify" items** C17, C18, C23, C24: read the code, write a test that shows the defect or
  record FIXED/OBSOLETE with evidence. C18 (a segment that cannot be opened during recount): if real, decide fix or
  documented limit; it is rare (I/O error during a rescan), so a documented known limit in
  `docs/CONFIGURATION.md` "Known limits" is acceptable.

- [ ] **Step 2: Write the failing tests**
  - C11: mutating the caller's slice after `EventRecord` does not change the record.
  - C12: `FloatValue(math.Copysign(0, -1))` is `+0` (bit pattern), and a `regex_extract` result of `-0` is `+0`.
  - C13: a string value with invalid UTF-8 is refused where samples enter the model (the `model` validation the
    pipeline already calls; find it), with a message naming device and signal, never the value; or, if the inputs
    already guarantee valid UTF-8 at every entry point (`config/decode.go:68`, `opcua/convert.go:141`,
    `modbus/decode.go:140` and the plugin input's wire conversion), record that and add the plugin wire check only.
  - C14: rename `ClockGoingBackwards` to what it tests; add a test pinning the 12-bit counter layout and the +1 ms
    roll.
  - C15: `Ack` keeps its count without re-reading frames (a per-segment record count or a running unacked counter);
    a test counts frame reads (an existing read hook, or a test-only counter) during an `Ack` of 1,000 records.
  - C16: three Appends under a standing floor breach rotate the bulk active segment once.
  - C19: with `max_wait` 1 s, a record appended at t0 and `Next` first called at t0+0.8 s seals at t0+1 s, not
    t0+1.8 s.
  - C20: `HTTP.Run` returning closes the idle connections of its transport (a test transport records the call).
  - C21: when `http.DefaultTransport` is not an `*http.Transport`, `NewClient` uses a fresh transport and does not
    panic.
  - C22: a `Drop` that keeps failing increments `output_settle_failures_total` once per attempt.
  - C23, C24: as found in Step 1.

- [ ] **Step 3: Run them to see them fail**, then implement each, then run
  `go test ./internal/model ./internal/envelope ./internal/buffer ./internal/output ./internal/status ./internal/pipeline -count=1 -race`.
  Expected: PASS.

- [ ] **Step 4: Document** C22 in the USAGE.md metrics table, and any refusal C13 makes visible.

- [ ] **Step 5: Commit** (one or a few commits, by package)

```bash
git add internal docs
git commit -m "fix: model, buffer and output minors deferred by agent core"
```

### Task 5: Router warning, status server and metrics minors

**Files:**
- Modify: `internal/pipeline/router.go`, `internal/status/server.go`, `internal/status/metrics.go`,
  `cmd/laraplate-edge/run.go` (process start time)
- Test: `internal/pipeline/router_test.go`, `internal/status/server_test.go`, `internal/status/metrics_test.go`,
  `cmd/laraplate-edge/logscan_test.go`
- Docs: `docs/USAGE.md` (router log line, status timeouts if documented)

- [ ] **Step 1: Write the failing tests**
  - C25: 1,000 refused records log one warning at once and then one summary per interval (reuse the
    `buffer/failurelog.go` reporter's shape, moved or shared, not copied); the drop counters still count all 1,000
    (Review Focus 3).
  - C26: the server sets `ReadTimeout` and `WriteTimeout`; a `/metrics` answer of a large agent (test fixture with many
    series) completes within them (Review Focus 4). Values: `ReadTimeout` 10 s, `WriteTimeout` 30 s, unless the
    existing `ReadHeaderTimeout` constant suggests another pair; state the values in the report.
  - C27: a token of a different length is refused without the comparison time depending on it (compare SHA-256
    digests with `subtle.ConstantTimeCompare`); `bearer <token>` and `BEARER <token>` are accepted.
  - C28: uptime counts from the process start passed by `run.go`, not from `Listen`.
  - C29, C30, C31 (verify): the metrics validator gains a negative self-test (a malformed family is reported); the
    log scan covers integer samples; for C30 and C31 decide whether a vanishing series is a defect (Prometheus treats
    an absent series as stale) and either keep the series at its last value until the component is removed or record
    the decision in the metric help.

- [ ] **Step 2: Run them to see them fail**, implement, then run
  `go test ./internal/pipeline ./internal/status ./cmd/... -count=1 -race`. Expected: PASS.

- [ ] **Step 3: Document** the router's summary line in USAGE.md where the log lines are listed.

- [ ] **Step 4: Commit**

```bash
git add internal cmd docs
git commit -m "fix(status,pipeline): rate-limited refusal warning, server timeouts, token compare, uptime"
```

### Task 6: Configuration and transform minors

**Files:**
- Modify: `internal/config/schema.go`, `internal/config/check.go`, `internal/transform/stateless.go`,
  `internal/transform/stateful.go`, `internal/transform/processor.go`, `internal/transform/check.go`,
  `internal/transform/samplefile.go`
- Test: the matching `_test.go` files

- [ ] **Step 1: Verify** C33, C35, C38, C40, C41 and record each result.

- [ ] **Step 2: Write the failing tests**
  - C32: messages for `inputs.2` and `inputs.10` come out in that order; two messages on the same path keep their
    relative order; path segments compare numerically when both are integers (Review Focus 5).
  - C35 (if real): `integerIn` refuses `3.0` in an integer list parameter, with the parameter's path.
  - C36: after a backward wall-clock step of 1 h, the heartbeat fires at its interval from the new time.
  - C37: delete the dead `downstream == nil` branch (no test; the compiler and existing tests prove it).
  - C39: decide: cache `OutputDataType` per input if a benchmark shows it in the router's event path, else record
    "compiled per event, events are rare" in its doc comment.
  - C41 (if real): a sample file over the total cap (state the cap: 64 MiB unless the per-line cap's constant
    suggests another) is refused; key matching is case-sensitive as documented, or the doc says it is not.
  - C34: the one-line code comment at `buildValidRange` pointing to the documented float64 comparison.

- [ ] **Step 3: Run them to see them fail**, implement, then run
  `go test ./internal/config ./internal/transform -count=1 -race`. Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal
git commit -m "fix(config,transform): natural message order, heartbeat after a clock step, transform minors"
```

### Task 7: Supervisor, provider and input driver minors

**Files:**
- Modify: `internal/supervisor/*.go`, `internal/provider/laraplate.go`, `internal/input/simulated/simulated.go`,
  `internal/input/driverkit/backoff.go`, `internal/output/backoff.go`, `internal/input/modbus/*.go`,
  `internal/input/opcua/*.go`, `internal/input/opcua/opcuatest/*.go`, `cmd/laraplate-edge/browse.go`
- Test: the matching `_test.go` files
- Docs: `docs/CONFIGURATION.md`, `docs/USAGE.md` where an item changes what an operator sees

This task is large but each item is local; commit per package. If the report shows it cannot fit one review, the
controller splits it into 7a (supervisor, provider, simulated, backoff), 7b (modbus), 7c (opcua).

- [ ] **Step 1: Verify** C42, C43, C44, C45, C49, C51, C59, item by item, and record each result. C45 is by design:
  record the decision in the input instance's doc comment.

- [ ] **Step 2: Fix what is real, test first for each:**
  - C42: `startInput` closes the built input on a `NewProcessor` failure; the timeout subtest returns its error over a
    channel instead of calling `t.Fatal` from a goroutine.
  - C43: if real, the rollback waits for the abandoned start of the same id to finish closing before restarting it
    (bounded by the start timeout), with a test using a fake input that holds an exclusive resource.
  - C44: on an active document older than the applied one, the provider backs off (its poll interval doubles up to
    its maximum) and logs once, instead of downloading it at every poll.
  - C46: `emitAll` checks `ctx.Err()` per sample; an oversize `scenario_file` is refused at configuration check with
    its path; reflow the doc comments.
  - C47: one exponential-jitter backoff in `internal/input/driverkit` (or a new small package if import direction
    forbids it; `output` must not import `input`), used by both; existing backoff tests pass unchanged.
  - C48: one point-type table drives both the type list and the size.
  - C50: the serial open error distinguishes "does not exist" and "permission denied" with fixed texts
    (`errors.Is(err, fs.ErrNotExist)`, `fs.ErrPermission`).
  - C53: `TestSilentUnitDoesNotStarveTheOthers` gets failure diagnostics (on failure it logs each unit's reply times
    and the timeout used), so a CI failure carries the evidence the 2026-11-02 investigation lacked; no change to its
    assertions.
  - C54: `ParseDeadband` accepts exponent form (`1e-07`), matching how the configuration renders numbers.
  - C55: unknown connection keys of modbus and opcua are echoed only when they pass the `docrules` safe-key rule,
    otherwise shown as `*`, as other paths do.
  - C56: stale `own-*.tmp` directories under the PKI directory are removed at startup (logged); the redundant
    `SyncDir(tmp)` goes; an existing `key.pem` with a mode wider than 0600 is refused with its path (Unix);
    `Trusted` also compares the thumbprint of the stored file, refusing a mismatch.
  - C57: a child that answers a bad status is reported in the browse output with that status and not expanded; the
    browse goes on.
  - C58: delete `opcuatest.Server.Drop`.
  - C49, C59: each real item fixed or decided as found in Step 1.

- [ ] **Step 3: Run the packages**

  Run: `go test ./internal/supervisor ./internal/provider ./internal/input/... ./internal/output ./cmd/... -count=1 -race`
  Expected: PASS.

- [ ] **Step 4: Document** every operator-visible change (C44 log, C46 refusal, C50 texts, C54, C56 refusal, C57
  browse output) in CONFIGURATION.md or USAGE.md.

- [ ] **Step 5: Commit** per package.

### Task 8: Split the three largest files

**Files:**
- Modify: `internal/config/config.go` (split into `config.go` and builder files, e.g. `build_inputs.go`,
  `build_processors.go`, `build_outputs.go`), `internal/supervisor/supervisor.go` (plugin code to
  `internal/supervisor/plugins.go`), `internal/input/opcua/session.go` and `driver.go` (dial code to `dial.go`,
  subscription code to `subscribe.go`)

- [ ] **Step 1: Record the baseline**: `go test ./... -count=1` test count and `go vet ./...`.

- [ ] **Step 2: Move code, one package per commit.** No identifier renamed, no line changed except package-level
  declarations moving between files and their imports. Each new file gets a short file comment naming what it holds.

- [ ] **Step 3: Verify per commit**: `go build ./... && go vet ./... && go test ./<package> -count=1`; at the end
  `go test ./... -count=1` with the baseline count, and `git diff --stat` per commit showing balanced moved lines.

- [ ] **Step 4: Commits**

```bash
git commit -m "refactor(config): split the builder out of config.go"
git commit -m "refactor(supervisor): move the plugin code to plugins.go"
git commit -m "refactor(opcua): split dial and subscribe out of session.go and driver.go"
```

### Task 9: Verify, record and close

**Files:**
- Modify: this plan (ticks, inventory resolution, delivery status), `docs/superpowers/plans/INDEX.md`
- Stack root, on its own branch and pull request: `docs/superpowers/specs/2026-10-19-transforms-modbus-opcua-design.md`
  (C52)

- [ ] **Step 1:** `go test ./... -race -count=1`, `go vet ./...`, `go vet -tags e2e ./...`,
  `go test -tags e2e -race ./internal/e2e`, gofmt. Expected: PASS, clean.

- [ ] **Step 2:** every inventory row C1-C60 gets its resolution in a new "Resolution" column: the commit, or
  FIXED/OBSOLETE with evidence, or the decision and where it is written.

- [ ] **Step 3: C52.** On a stack branch, amend spec D6: the serial port is an exclusive resource on Windows, not on
  Linux, where two processes can open it and interleave frames; the agent's advice (stop the agent before probing an
  RTU line) stands. Open the pull request; the user merges it and bumps no submodule pointer.

- [ ] **Step 4: Close the plan**: `status: closed`, Status "Closed (date)", boxes ticked, a delivery status with the
  divergences and the items outside this plan, and `**Documented in:**` naming the docs that changed; mark the INDEX
  row closed.

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/plans
git commit -m "docs(plans): close the deferred minors cleanup plan"
```
