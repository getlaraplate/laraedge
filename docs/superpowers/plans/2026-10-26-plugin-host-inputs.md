---
status: open
created_on: 2026-10-26
---
# Plugin host and plugin inputs. Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax. Tick each step when it is done, in the same work block.

**Status:** Open. No task started.

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

- [ ] **Step 1: Failing tests:** schema refuses a plugin without `manifest_sha256` and a bad `max_memory`; parse fills
  the fields; `CheckBuild` no longer reports plugins or `plugin:<id>` drivers but still reports `type: plugin:<id>`
  outputs; `plugin hash` prints the two lines (lowercase hex SHA-256) and fails clearly when files are missing.
- [ ] **Step 2: Implement** (`go get github.com/getlaraplate/laraedge-sdk@v0.1.0`; licences in the commit message).
- [ ] **Step 3: Run the checks; commit.**

### Task 2: Offline validation

**Files:** create `internal/plugin/validate.go`, tests; modify `cmd/laraplate-edge/main.go` (`config validate` calls
it), `internal/supervisor/supervisor.go` (`check` calls it).

**Interfaces:**
- Produces: `plugin.Resolve(doc config.Document, baseDir string, provider string) ([]Spec, []string)` with
  `Spec{ID, Path, Dir string; Manifest manifest.Manifest; ManifestRaw []byte; MaxMemory int64; Raw json.RawMessage}`;
  `plugin.Validate(doc config.Document, specs []Spec) []string` (references: `plugin:<id>` exists and offers `input`;
  every plugin input section against `Manifest.ValidateInput`, secret-key problems dropped (H3); messages prefixed
  with the document path, e.g. `inputs[2].connection.port: ...`); `Options{RequireFiles bool}` for H2.

- [ ] **Step 1: Failing tests** (fixtures: a test plugin directory with a manifest and a fake binary): hashes mismatching
  binary or manifest; a manifest failing `Check`; `plugin:<id>` unknown or without the input kind; a bad address and a
  read mode or type the manifest refuses at their document paths; Review Focus secret test; relative path resolution
  per H1; `config validate` reports these offline and a missing file as a problem; no message contains a value.
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Run the checks; commit.**

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

- [ ] **Step 1: Failing tests:** starts and answers health; `Info` differing from the manifest refuses the start; a
  binary changed on disk refuses the next restart (Review Focus); crash loop backoff 1 s, 2 s, 4 s … capped at 1 min on
  the fake clock, reset after one stable minute (Review Focus); three failed health checks restart; memory above
  `max_memory` restarts with reason `memory` (Linux test; Windows compiled); stderr flood rate limited, plugin not
  blocked (Review Focus); stop sequence including a plugin ignoring Shutdown; environment is exactly the minimal list;
  socket directory under `data_dir/run/plugins/<id>/`.
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Run the checks (also `-race -count=5 ./internal/plugin`); commit.**

### Task 4: Plugin input adapter

**Files:** create `internal/plugin/input.go`, tests.

**Interfaces:**
- Consumes: Task 3 `Host`; `pluginv1` Input client; `input.Lifecycle`, `driverkit.Health`; the secret resolver.
- Produces: `plugin.InputFactory(h *Host, resolver SecretResolver) input.Factory`; the `input.Input` it builds
  (`Start`, `Close`, `Probe`, `Health`, and `Browse(ctx, node string, depth int) ([]Node, error)` when the manifest
  offers browse); `SecretResolver interface{ Resolve(ctx context.Context, ref string) (string, error) }`.

- [ ] **Step 1: Failing tests** (against the test plugin): `Start` configures the instance with secrets resolved (the
  plugin receives the value, the agent logs never contain it) and returns at once; samples reach emit with the
  plugin's ts and quality and pass the point's transforms; `answered` keeps devices alive and `health` feeds
  `connected`/`read_errors`; a `ConfigError` fails `Start` with the paths; on plugin crash one `bad` sample per point
  then `death` after the tolerance, and after the restart `Configure`+`Subscribe` again then birth (H6); `Close` ends
  the stream and nothing is emitted afterwards; `Probe` returns the plugin's samples within 30 s.
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Run the checks; commit.**

### Task 5: Supervisor and wiring

**Files:** modify `internal/supervisor/supervisor.go` (plugin registry per document: start used plugins before
inputs, H4, H5, rollback, Close stops plugins), `internal/supervisor/status.go`, `internal/status/metrics.go`,
`internal/status/server.go`, `cmd/laraplate-edge/run.go`, tests.

- [ ] **Step 1: Failing tests:** a document with a plugin input runs; a plugin failing to start rolls the document back;
  Review Focus reload of one instance keeps the pid; changing the `plugins` entry restarts the plugin and re-runs its
  instances; removing the last component stops the plugin; `/status` `plugins` and the two metric families; `Close`
  stops plugins within the stop deadline; a plugin crash leaves other inputs and outputs running.
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Run the checks; commit.**

### Task 6: Commands and conformance of built-in drivers

**Files:** modify `cmd/laraplate-edge/probe.go`, `cmd/laraplate-edge/browse.go` (plugin inputs: start the plugin,
probe or browse, stop it), `cmd/laraplate-edge/main.go` (H8 dispatch on the executable name); create
`cmd/laraplate-edge/builtin.go` (serve `simulated` over the SDK; its `plugin.json` and `conformance.json` in
`cmd/laraplate-edge/testdata/builtin-simulated/`), tests; modify `.github/workflows/ci.yml` (a step that builds the
agent as `laraedge-builtin-simulated[.exe]` next to those files and runs `laraedge-conformance` from the SDK module
on Linux and Windows).

- [ ] **Step 1: Failing tests:** `probe` and `browse` on a plugin input; the binary run as
  `laraedge-builtin-simulated` serves the plugin protocol (`--manifest` equals the committed `plugin.json`); the
  conformance suite passes on it (test that runs `conformance.Run`).
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Run the checks; commit.**

### Task 7: End to end, documentation, closing

**Files:** add `internal/e2e/plugin_test.go`; modify `examples/` (a config with a plugin input using the SDK's
`simulated-counter`), `docs/CONFIGURATION.md` (plugins: fields, paths, hashes, `plugin hash`, manifest validation,
secrets, restart and limits), `docs/USAGE.md` (running plugins, logs, status and metrics, `probe`/`browse`, the
built-in conformance, troubleshooting: hash mismatch, handshake timeout, crash loop, memory limit), `README.md`, this
plan, `docs/superpowers/plans/INDEX.md`.

- [ ] **Step 1: End to end.** The built agent with the SDK's `simulated-counter` (built from the SDK module at v0.1.0)
  delivers data to the fake backend; killing the plugin produces bad samples, restart and data again; a wrong hash in
  a new document rolls back to the previous one.
- [ ] **Step 2: Docs.**
- [ ] **Step 3: Close.** Delivery status with divergences, `**Documented in:**`, ticks, `status: closed`, index; the
  stack spec status line; commit.
