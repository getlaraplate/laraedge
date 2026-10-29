---
status: closed
created_on: 2026-10-26
---
# Plugin host and plugin inputs. Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax. Tick each step when it is done, in the same work block.

**Status:** Closed (2026-10-29). Shipped; see "Delivery status" below.

**Goal:** the agent runs plugins built on `laraedge-sdk`: it validates their configuration offline against their
manifests, starts and supervises their processes (verified hashes, handshake, health, restart with backoff, memory
limit, logs, orderly stop), and uses a plugin input (`driver: plugin:<id>`) exactly like a built-in driver; `probe`
and `browse` work on plugin inputs and the built-in `simulated` driver passes the SDK conformance suite.

**Architecture:** a new package `internal/plugin` in three parts: offline validation (manifests, hashes, sections),
the host (one supervised process per plugin id, over the SDK's `handshake` host side) and the input adapter (an
`input.Input` over the plugin's `Input` service, with the agent's lifecycle helper deciding birth and death). The
supervisor starts the plugins the document uses before the inputs that need them, applies by difference and rolls
back on failure as for inputs. Processors and plugin outputs are plan 3.

**Tech Stack:** Go 1.27; new dependency `github.com/getlaraplate/laraedge-sdk` v0.1.0 (Apache-2.0; brings
`google.golang.org/grpc` v1.84.0 Apache-2.0, `google.golang.org/protobuf` v1.36.12 BSD-3-Clause,
`github.com/Microsoft/go-winio` v0.6.3 MIT, `golang.org/x/net` v0.60.0, `golang.org/x/sys`, `golang.org/x/text`
BSD-3-Clause) — approved by the user on 2026-10-26.

**Spec:** `../../../../docs/superpowers/specs/2026-10-22-laraedge-plugins-design.md` (stack root) sections 4 (Input),
5, 6, 7 (built-in drivers and conformance), 9 plan 2, as amended by the SDK plan's delivery status
(`../../../laraedge-sdk/docs/superpowers/plans/2026-10-23-sdk.md`, rulings R7, R10, R11, R17 and the "carried to the
agent's host plans" list); the SDK's `docs/handshake.md` is the contract reference.

## Global Constraints

- AGPL-3.0 repo; module `github.com/getlaraplate/laraedge`; never copy SDK code, import it; permissive dependencies
  only; no CGO; work on branch `develop`; commit only your own files; never push; no `Co-Authored-By` trailer; do not
  commit at the stack root.
- `gofmt -l .` empty, `go vet ./...`, `GOOS=windows go vet ./...`, `go test -race ./...`, `GOARCH=386 go test ./...`
  before every commit; table-driven tests; fakes over mocks; injected clock for timing, no sleeps.
- Spec section 6 values exactly: start only plugins a component uses; hashes of binary and manifest recomputed at
  every start; no shell; minimal environment (`PATH`, `TZ`, `SystemRoot` on Windows) and never secrets in it; socket
  directory `data_dir/run/plugins/<id>/`; handshake within 10 s; `Info` must match the manifest (canonical forms);
  `Configure` per instance with secrets resolved at that moment; stderr and stdout after the handshake to the agent
  log with `plugin=<id>`, lines truncated at 4 KiB, rate limited; health check every 5 s with a 2 s deadline, three
  failures or the process exit restart it with backoff 1 s to 1 min, reset after one minute of stable running; while
  down: one `bad` sample per point (zero value of the kind) then `death` after the input's tolerance; `max_memory`
  checked every 10 s (RSS on Linux, working set on Windows), above it WARN and restart; stop: `Shutdown` with a 5 s
  deadline, then SIGTERM, then kill after 5 s more (Windows: kill after the deadline); a plugin crash never stops the
  agent; deadlines `Info` 5 s, `Configure` 10 s, `Probe` 30 s.
- From the SDK plan: one `Subscribe` stream per instance (R10); binary named after the manifest is a suite rule (R11),
  the agent uses the configured `path`; compare canonical manifests (R7); poll health with `Check`; treat
  `UNAVAILABLE` or EOF on `Shutdown` as the answer; a refused re-Configure leaves the instance unconfigured; stderr
  writers must drop, not block.
- Metrics `laraplate_edge_plugin_up{plugin}` and `laraplate_edge_plugin_restarts_total{plugin,reason}`; `/status`
  gains `plugins` (state, pid, restarts, last error); messages name paths, never values.

## Decisions taken in this plan

- **H1. Relative plugin paths** resolve against the configuration file's directory (file provider, like
  `scenario_file`); with the `laraplate` provider `path` must be absolute (refused offline otherwise). The manifest is
  `plugin.json` in the binary's directory.
- **H2. Offline hash check:** `config validate` checks hashes and manifests when the files exist and reports a missing
  file as a problem; the supervisor always requires them.
- **H3. Secrets in offline validation:** a manifest problem whose path ends in a secret key (the existing secret-key
  rule) is dropped offline; `Configure` validates the resolved values at start.
- **H4. Plugin factories are dynamic:** the supervisor resolves `plugin:<id>` through the plugin registry of the
  document being applied, not the static driver map; factories never start processes (M1 of the Modbus plan).
- **H5. Apply by difference:** a plugin restarts (with all its instances) only when its `plugins` entry (`Raw`) changes
  or its binary hash on disk differs from the running one; an input section change reconfigures only that instance;
  a plugin no component uses is stopped.
- **H6. Plugin restart and instances:** after a restart the host re-runs `Configure` and `Subscribe` for every live
  instance; a `ConfigError` then is logged and the instance stays down (devices die), the document is not rolled back
  (it was valid when applied).
- **H7. stderr limiter:** per plugin, 50 lines per second with a burst of 200, excess lines dropped and counted in one
  WARN per minute; level INFO.
- **H8. Built-in drivers as plugins (busybox name):** when the agent binary runs under the base name
  `laraedge-builtin-<driver>` (`.exe` stripped) it serves that built-in driver over the plugin protocol with the SDK
  and its manifest name is that base name; implemented for `simulated` only (spec 7: modbus and opcua need servers).

## Review Focus

- The plugin binary is replaced on disk while it runs: the next restart refuses it (hash), its devices die, the agent
  and the other plugins keep running. Test in Task 3.
- A plugin that floods stderr: the agent log is rate limited, the plugin is not blocked. Test in Task 3.
- A plugin that crashes at start in a loop: backoff grows to 1 min, no tight loop, its devices die once. Test in
  Task 3.
- A `connection` field `secret://env/X` where the manifest schema says `integer`: offline validation does not report
  it; the resolved value is validated by `Configure`. Test in Task 2.
- A reload that changes only one plugin input's points: the plugin process keeps its pid, only that instance is
  reconfigured. Test in Task 5.

---

### Task 1: Dependency and configuration

**Files:** modify `go.mod`/`go.sum`, `schema/laraplate-edge-config-1.schema.json` (`plugin`: `manifest_sha256`
required, `max_memory` optional size string as `buffer.max_size`), `internal/config/config.go` (`Plugin.ManifestSHA256`,
`Plugin.MaxMemory int64`), `internal/config/check.go` (accept `plugins` and `plugin:<id>` drivers; plugin output types
stay refused until plan 3), tests; create `cmd/laraplate-edge/pluginhash.go` (`plugin hash <dir>`), tests.

**Interfaces:**
- Produces: `config.Plugin{ID, Path, SHA256, ManifestSHA256 string; MaxMemory int64; Raw json.RawMessage}`; command
  `laraplate-edge plugin hash <dir>` printing `sha256: <hex>` and `manifest_sha256: <hex>` for the binary named in
  `plugin.json` (`<name>`, `.exe` on Windows) and `plugin.json`.

- [x] **Step 1: Failing tests:** schema refuses a plugin without `manifest_sha256` and a bad `max_memory`; parse fills
  the fields; `CheckBuild` no longer reports plugins or `plugin:<id>` drivers but still reports `type: plugin:<id>`
  outputs; `plugin hash` prints the two lines (lowercase hex SHA-256) and fails clearly when files are missing.
- [x] **Step 2: Implement** (`go get github.com/getlaraplate/laraedge-sdk@v0.1.0`; licences in the commit message).
- [x] **Step 3: Run the checks; commit.**

### Task 2: Offline validation

**Files:** create `internal/plugin/validate.go`, tests; modify `cmd/laraplate-edge/main.go` (`config validate` calls
it), `internal/supervisor/supervisor.go` (`check` calls it).

**Interfaces:**
- Produces: `plugin.Resolve(doc config.Document, baseDir string, provider string) ([]Spec, []string)` with
  `Spec{ID, Path, Dir string; Manifest manifest.Manifest; ManifestRaw []byte; MaxMemory int64; Raw json.RawMessage}`;
  `plugin.Validate(doc config.Document, specs []Spec) []string` (references: `plugin:<id>` exists and offers `input`;
  every plugin input section against `Manifest.ValidateInput`, secret-key problems dropped (H3); messages prefixed
  with the document path, e.g. `inputs[2].connection.port: ...`); `Options{RequireFiles bool}` for H2.

- [x] **Step 1: Failing tests** (fixtures: a test plugin directory with a manifest and a fake binary): hashes mismatching
  binary or manifest; a manifest failing `Check`; `plugin:<id>` unknown or without the input kind; a bad address and a
  read mode or type the manifest refuses at their document paths; Review Focus secret test; relative path resolution
  per H1; `config validate` reports these offline and a missing file as a problem; no message contains a value.
- [x] **Step 2: Implement.**
- [x] **Step 3: Run the checks; commit.**

### Task 3: Host

**Files:** create `internal/plugin/host.go` (one supervised plugin), `internal/plugin/stderr.go` (line splitter,
truncation, H7 limiter), `internal/plugin/memory_linux.go`, `memory_windows.go`, `memory_other.go`, tests with a test
plugin built from `internal/plugin/testdata/plugin` (on the SDK, behaviours by environment variable: crash at start,
crash after N s, flood stderr, ignore Shutdown, allocate memory).

**Interfaces:**
- Consumes: Task 2 `Spec`; `handshake.Launch`, `Process.Dial/Kill/Signal/Wait`; `driverkit.Backoff`.
- Produces: `plugin.NewHost(spec Spec, opts HostOptions) *Host` with `HostOptions{DataDir string; Clock clock.Clock;
  Log *slog.Logger}`; `(*Host) Start(ctx) error` (hash check, launch, dial, `Info` canonical compare, first health
  check; error = document start failure), `Conn() (*grpc.ClientConn, uint64)` (connection and generation, nil while
  down), `Changes() <-chan uint64` (generation bumps on every successful (re)start, buffered, latest wins),
  `Stop(ctx) error` (Shutdown 5 s → SIGTERM → kill 5 s), `Status() Status` (`State` up/starting/down/stopped, `Pid`,
  `Restarts`, `LastError`); restart reasons `exit`, `health`, `memory`, `hash`.

- [x] **Step 1: Failing tests:** starts and answers health; `Info` differing from the manifest refuses the start; a
  binary changed on disk refuses the next restart (Review Focus); crash loop backoff 1 s, 2 s, 4 s … capped at 1 min on
  the fake clock, reset after one stable minute (Review Focus); three failed health checks restart; memory above
  `max_memory` restarts with reason `memory` (Linux test; Windows compiled); stderr flood rate limited, plugin not
  blocked (Review Focus); stop sequence including a plugin ignoring Shutdown; environment is exactly the minimal list;
  socket directory under `data_dir/run/plugins/<id>/`.
- [x] **Step 2: Implement.**
- [x] **Step 3: Run the checks (also `-race -count=5 ./internal/plugin`); commit.**

### Task 4: Plugin input adapter

**Files:** create `internal/plugin/input.go`, tests.

**Interfaces:**
- Consumes: Task 3 `Host`; `pluginv1` Input client; `input.Lifecycle`, `driverkit.Health`; the secret resolver.
- Produces: `plugin.InputFactory(h *Host, resolver SecretResolver) input.Factory`; the `input.Input` it builds
  (`Start`, `Close`, `Probe`, `Health`, and `Browse(ctx, node string, depth int) ([]Node, error)` when the manifest
  offers browse); `SecretResolver interface{ Resolve(ctx context.Context, ref string) (string, error) }`.

- [x] **Step 1: Failing tests** (against the test plugin): `Start` configures the instance with secrets resolved (the
  plugin receives the value, the agent logs never contain it) and returns at once; samples reach emit with the
  plugin's ts and quality and pass the point's transforms; `answered` keeps devices alive and `health` feeds
  `connected`/`read_errors`; a `ConfigError` fails `Start` with the paths; on plugin crash one `bad` sample per point
  then `death` after the tolerance, and after the restart `Configure`+`Subscribe` again then birth (H6); `Close` ends
  the stream and nothing is emitted afterwards; `Probe` returns the plugin's samples within 30 s.
- [x] **Step 2: Implement.**
- [x] **Step 3: Run the checks; commit.**

### Task 5: Supervisor and wiring

**Files:** modify `internal/supervisor/supervisor.go` (plugin registry per document: start used plugins before
inputs, H4, H5, rollback, Close stops plugins), `internal/supervisor/status.go`, `internal/status/metrics.go`,
`internal/status/server.go`, `cmd/laraplate-edge/run.go`, tests.

- [x] **Step 1: Failing tests:** a document with a plugin input runs; a plugin failing to start rolls the document back;
  Review Focus reload of one instance keeps the pid; changing the `plugins` entry restarts the plugin and re-runs its
  instances; removing the last component stops the plugin; `/status` `plugins` and the two metric families; `Close`
  stops plugins within the stop deadline; a plugin crash leaves other inputs and outputs running.
- [x] **Step 2: Implement.**
- [x] **Step 3: Run the checks; commit.**

### Task 6: Commands and conformance of built-in drivers

**Files:** modify `cmd/laraplate-edge/probe.go`, `cmd/laraplate-edge/browse.go` (plugin inputs: start the plugin,
probe or browse, stop it), `cmd/laraplate-edge/main.go` (H8 dispatch on the executable name); create
`cmd/laraplate-edge/builtin.go` (serve `simulated` over the SDK; its `plugin.json` and `conformance.json` in
`cmd/laraplate-edge/testdata/builtin-simulated/`), tests; modify `.github/workflows/ci.yml` (a step that builds the
agent as `laraedge-builtin-simulated[.exe]` next to those files and runs `laraedge-conformance` from the SDK module
on Linux and Windows).

- [x] **Step 1: Failing tests:** `probe` and `browse` on a plugin input; the binary run as
  `laraedge-builtin-simulated` serves the plugin protocol (`--manifest` equals the committed `plugin.json`); the
  conformance suite passes on it (test that runs `conformance.Run`).
- [x] **Step 2: Implement.**
- [x] **Step 3: Run the checks; commit.**

### Task 7: End to end, documentation, closing

**Files:** add `internal/e2e/plugin_test.go`; modify `examples/` (a config with a plugin input using the SDK's
`simulated-counter`), `docs/CONFIGURATION.md` (plugins: fields, paths, hashes, `plugin hash`, manifest validation,
secrets, restart and limits), `docs/USAGE.md` (running plugins, logs, status and metrics, `probe`/`browse`, the
built-in conformance, troubleshooting: hash mismatch, handshake timeout, crash loop, memory limit), `README.md`, this
plan, `docs/superpowers/plans/INDEX.md`.

- [x] **Step 1: End to end.** The built agent with the SDK's `simulated-counter` (built from the SDK module at v0.1.0)
  delivers data to the fake backend; killing the plugin produces bad samples, restart and data again; a wrong hash in
  a new document rolls back to the previous one.
- [x] **Step 2: Docs.**
- [x] **Step 3: Close.** Delivery status with divergences, `**Documented in:**`, ticks, `status: closed`, index; the
  stack spec status line; commit.

## Delivery status (2026-10-29): shipped

All seven tasks are built on branch `develop`, each reviewed (Task 7 inside the whole-branch review), then a
whole-branch review and one fix wave. The agent runs input plugins built on `laraedge-sdk` v0.1.0 (Apache-2.0):
`plugins` entries carry `manifest_sha256` (required) and `max_memory`; `plugin hash` prints both hashes; `config
validate` and the supervisor resolve each plugin (H1 paths), verify binary and manifest hashes, check the manifest and
validate every plugin input section against it, with dotted document paths and no values in any message. A `Host`
supervises one process per plugin: hashes recomputed from the exact file immediately before every launch, minimal
environment, socket directory `data_dir/run/plugins/<id>/` (removed on stop), handshake, canonical `Info` compare,
health every 5 s, restart with backoff 1 s to 1 min, memory limit, stdout/stderr to the agent log with the H7
limiter, and the stop sequence. The input adapter configures each instance with secrets resolved at that moment,
subscribes, feeds the device lifecycle and health, and re-runs `Configure` and `Subscribe` after a plugin restart
(H6). The supervisor starts only the plugins a document uses, applies by difference (H5), rolls back on failure,
stops plugins after their inputs, and reports plugins in `/status` and in `laraplate_edge_plugin_up` and
`laraplate_edge_plugin_restarts_total`. `probe` and `browse` work on plugin inputs; the agent binary named
`laraedge-builtin-simulated` serves the `simulated` driver as a plugin and passes the SDK conformance suite, locally
and in CI on Linux and Windows. The end-to-end test runs the SDK's `simulated-counter`. Commits from `1188525` to
`e2416d3`, and the closing commit. The divergences from the plan, each with its reason (the controller rulings are
summarised below):

- **No `Options{RequireFiles}` (R4).** H2 makes a missing file a problem both offline and in the supervisor, so the
  option changed nothing; `Resolve` has three arguments.
- **Secret references outside secret keys (R5).** Offline validation also drops a manifest problem whose value is a
  `secret://` reference, not only one under a secret key, so the Review Focus case is not reported offline. At run
  time a reference is resolved only under a secret key; elsewhere the plugin refuses it at start.
- **A wrong hash is rejected, not rolled back (R23).** The supervisor checks plugin files before applying anything, so
  a document with a wrong hash is `rejected` and the running plugin keeps its pid; the end-to-end test asserts
  exactly that.
- **The kill scenario reaches no death in the end-to-end test.** The plugin restarts within the input's tolerance;
  death after the tolerance is proven by the adapter's tests.
- **Busy `Subscribe` retry (R18).** A `Subscribe` refused with `ALREADY_EXISTS` before any event is retried every
  100 ms for up to 10 s (the SDK's stop timeout) without going down. It guards the window in which an old
  `Subscribe` is still in flight; on an ordinary reconfigure the SDK's `Configure` already waits for the old stream.
- **Start and Stop serialised; old host stopped before the new one (R12).** `Stop` removes the socket directory, so
  the supervisor waits for the old host's `Stop` before starting a new host with the same id.
- **API beyond the plan (R8).** `HostOptions.Rand`, `Status.RestartsByReason`, `Host.BinarySHA256`, `Host.Watch`
  (one latest-wins channel per subscriber, beside `Changes`), `plugin.HashFile`, `Spec.SHA256` and
  `Spec.ManifestSHA256`; `plugin.Browser` exists only when the manifest declares browse (R15).
- **Restart counting (R9, R19, R20).** Each lost process and each failed start attempt counts one restart labelled by
  its cause; `/status` shows the total, the metric the split; counters restart from zero when a new document
  restarts the plugin.
- **A replaced binary under an unchanged entry rejects every later document (R20).** The H5 hash branch is reachable
  only together with an entry change.
- **The committed built-in `plugin.json` says version `dev` (R22).** Operators generate theirs with
  `laraedge-builtin-simulated --manifest` from the binary they install.
- **`probe` and `browse` skip offline validation of plugin inputs** (the plugin's `Configure` checks) and use a fresh
  temporary data directory; `browse --depth` counts from 1 and is passed to the plugin as `depth - 1`.
- **Backend follow-up (R2).** The backend's fixtures carry plugin entries without `manifest_sha256`, which the agent now
  refuses with `plugins.<i>.manifest_sha256: is required`. The MES module's test arrays and the agent configuration
  editor must carry `manifest_sha256` before the backend publishes plugin documents.

Left open, for decisions outside this plan:

- **Local trust anchor for plugins.** With the `laraplate` provider the backend supplies `path` and both hashes, so a
  compromised backend can make the agent run any absolute-path binary that completes the handshake. An allowed plugin
  directory in the bootstrap, or hashes pinned locally, would close it.
- **SDK v0.1.0 `acquire`** (`sdk/sdk.go:412`) does not check the parent context, so a late old `Subscribe` can take
  an instance after a new `Configure` and its teardown stops the new configuration. The fix belongs in
  `laraedge-sdk`; the busy retry above mitigates the symptom.
- **Plugin output is logged as written.** A plugin that prints its configuration puts resolved secrets in the agent
  log; the spec leaves this to the plugin author.
- **`internal/supervisor/supervisor.go`** is about 1200 lines; the plugin part could move to `supervisor/plugins.go`
  with no API change.
- The Windows memory reader and kill-after-deadline are compiled and vetted but not run locally; the Windows CI
  conformance step is to be confirmed on its first run.

**Documented in:** `docs/CONFIGURATION.md`, `docs/USAGE.md` (and `README.md`, `examples/simulated-counter/`).
