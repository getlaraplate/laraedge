# Changelog

All notable changes to this project will be documented in this file.

## [0.1.0] - 2026-10-29

### 🚀 Features

- Go module, CLI skeleton, clock abstraction and CI
- Internal model and laraplate-machine/1 envelope builder
- Bootstrap file loader and secret reference resolver
- Disk buffer with segmented queues, eviction and durable loss markers
- Sealed batches with a durable inflight file
- HTTP output driving the batcher against the ingest route
- Configuration document schema, parser, build check and last-valid store
- Pipeline, supervisor, file and laraplate providers, run command
- Simulated input (scenario state machine, seeded per device), built-in driver registry
- Status endpoint, metrics, JSON logs and systemd watchdog
- End-to-end test, operator docs, logs on stdout; close the agent core plan
- *(transform)* Chain compiler and stateless transforms
- *(transform)* Stateful and time-driven transforms
- *(transform)* Processor with deadline goroutine and edge counter state
- *(config)* Exact transform parameters, chain type check, birth data type
- *(supervisor)* Apply transform chains, count their runtime errors
- *(cmd)* Probe and config test, config validate checks transform chains
- *(input)* Driver kit (backoff, rate limiter, health) and input health reporting
- *(modbus)* Connection and address parsing
- *(modbus)* Block planner
- *(modbus)* Register and bit decoder with byte orders
- *(modbus)* TCP driver with poll loop, probe and health
- *(modbus)* Register the driver and validate inputs offline
- *(modbus)* RTU and RTU over TCP transports
- *(modbus)* End-to-end test, example and operator docs
- *(opcua)* Configuration, read.subscribe long form, gopcua dependency
- *(opcua)* Own certificate, trust store and thumbprints
- *(opcua)* Convert data values to sample values, quality and timestamp
- *(logging)* Bridge the standard log package into slog at debug
- *(opcua)* Session layer over gopcua with explicit trust, limits and loss detection
- *(opcua)* Input with reconnect loop, subscription, poll groups and watchdog
- Register the opcua driver in run, config validate and probe
- Opcua trust and browse commands
- *(opcua)* Browse lists the access level and skips nodes it may not browse; trust normalises --thumbprint
- Plugin configuration, plugin drivers in CheckBuild and plugin hash
- Offline validation of plugin entries and plugin inputs
- Plugin host supervising one plugin process
- Plugin input adapter
- Run plugins in the supervisor, report them in status and metrics
- Probe and browse plugin inputs, serve the simulated driver as a plugin
- Plugins_dir in the bootstrap, a local trust anchor for plugin paths

### 🐛 Bug Fixes

- Never echo values in bootstrap and secrets errors; reject oversized secret files
- Never echo YAML keys or malformed secret references in errors
- Hide anchor names, map keys and tagged scalars in YAML errors; parse secrets before ctx check
- Sync rotated segments, durable markers before eviction, floor-breach drops
- Floor eviction order, coalesce every drop, bound the pinned segment size
- Durable markers before acks, interior gaps on split, shared atomic writes
- Config review round 1 (int64 version, fixed internal errors, names without /, derived transforms, secret key suffixes)
- Task 8 review round 1 (cancelled apply, provider state, output hold)
- Task 8 review round 2 (applied outcome on shutdown, failed output hold)
- Task 8 review round 3 (failed output across a rollback, hold route)
- Task 8 review round 4 (input registration owned by its instance)
- Task 9 review round 1 (initial counters at 0, state change abandons the part)
- Task 11 review round 1 (env tokens fixed at start, log level case, e2e backend file)
- Final review (runtime corrupt frames, drop log limit, samples-only markers)
- Final review (agent_version cap, file provider on shutdown, buffer.max_size minimum)
- *(atomicfile)* A missing directory is a SyncDir error on Windows too
- *(transform)* Round decimal ties, chain-exit kind, map numeric unification
- *(transform)* Bad samples through time-driven steps, deadband tolerance
- *(transform)* Prune unused counter keys, end quiet error episodes
- *(cmd)* Config test fires deadlines between samples up to a horizon
- *(transform)* A device death reaches its chains; error summary on the clock
- *(supervisor)* Remove the transform state of inputs not in the document
- *(cmd)* Config test handles a deadline tied with a sample after it
- *(transform)* Deadband and debounce forget their last sent value at a death
- *(supervisor)* A rolled-back rename restarts the old input from 0
- *(modbus)* A silent unit no longer starves the other blocks
- *(modbus)* A protocol error or short frame on tcp reopens the connection
- *(opcua)* OnLost contract, single-node limit reads, Close during a read
- *(opcua)* Backoff and liveness only after an exchange, exceptions per exchange
- *(opcua)* Bounded subscription lifetime; tests outlive the library server's orphan trap
- *(config)* A numeric deadband reaches drivers in plain decimal
- Keep backend fixtures verbatim and harden plugin hash
- Dotted paths in plugin validation messages
- Serialize plugin host Start and Stop
- Plugin input restart race, start/close and review minors
- Bound the busy Subscribe retry by time, deflake plugin input tests
- Run the plugin binary at its resolved path; plugins_dir is a restart-only key

### 💼 Other

- First commit
- *(deps)* Upgrade golang.org/x/text to v0.43.0 (BSD-3-Clause)

### 📚 Documentation

- Agent core implementation plan and plans index
- Controller rulings and final residuals recorded in the agent core plan
- Transforms, Modbus and OPC UA drivers design
- Level-1 transforms implementation plan
- Transforms, probe and config test; close the transforms plan
- Amend the transforms design record to the shipped behaviour
- Transforms final review fixes; record R14 and R15 in the plan
- *(plans)* Transforms residual fixes R16, R17 and the x/text upgrade
- Record ruling R18 in the transforms plan
- Fix the R18 ruling line in the transforms plan
- Modbus driver implementation plan
- Add the work in progress notice
- Use the Laraplate brand logo in the README
- *(plans)* Modbus plan preflight: module path, dependency approved, no trailer
- *(plans)* Tick Modbus Task 1
- *(plans)* Tick Modbus Task 2
- *(plans)* Tick Modbus Task 3
- *(plans)* Tick Modbus Task 4
- *(plans)* Tick Modbus Task 5
- *(plans)* Tick Modbus Task 6
- *(modbus)* Read_errors always has the four kinds, serial port advice
- *(plans)* Tick Modbus Task 7 steps 1 and 2
- *(modbus)* Shared lines, read error counts per input start, backoff
- *(modbus)* The rtu timeout limit and the tcp garbled-frame rows
- *(plans)* Close the Modbus plan with delivery status and controller rulings
- OPC UA driver implementation plan
- *(plans)* OPC UA plan started, dependency approved
- *(plans)* Tick OPC UA Task 1
- *(plans)* Tick OPC UA Task 2
- *(plans)* Tick OPC UA Task 3
- *(plans)* Tick OPC UA Task 4
- *(plans)* Tick OPC UA Task 5
- *(plans)* Tick OPC UA Task 6
- Opcua driver, opcua trust and browse commands, example
- Unknown connection fields are refused by modbus and opcua alike
- *(plans)* Tick OPC UA Task 7 steps 1 and 2
- *(opcua)* Subscription lifetime count, a negative server limit counts as 100
- *(plans)* Close the OPC UA plan with delivery status and controller rulings
- Plugin host and plugin inputs implementation plan
- *(plans)* Plugin host plan approved
- *(plans)* Plugin host plan started
- *(plans)* Plugin host Task 1 done
- *(plans)* Plugin host Task 2 done
- *(plans)* Plugin host Task 3 done
- *(plans)* Plugin host Task 4 done
- *(plans)* Plugin host Task 5 done
- Plugins in the configuration and usage guides, simulated-counter example
- *(plans)* Close the plugin host and inputs plan

### 🧪 Testing

- Restore the Shrink eviction guard, cover a gap between held records
- *(supervisor)* Count a resent unconfirmed batch once
- *(e2e)* A run with transform chains checked against raw companion points
- *(supervisor)* Make the sample-value log check robust; docs: clarify rollback wording, device death in spec
- *(config)* A valid JSON document for config validate, top of the address space
- *(output)* Next may return before the second wake-up
- *(e2e)* Opcua input against the library server behind a proxy
- *(opcua)* Trust store list test survives case-insensitive file systems
- Rollback after a failed replacement plugin, socket dir after a failed start
- End to end plugin run, kill and wrong hash rollback
- Plugin e2e proves rejection leaves the plugin running; docs on secrets and hash drift
- No plugin socket directory on Windows (named pipe)
- *(modbus)* A request timeout an answering unit can meet on a slow runner
- *(config)* Backend fixtures carry the plugin hashes again

### ⚙️ Miscellaneous Tasks

- Licence, agent rules and repository configuration
- Ci, docs: gofmt gate, 386 and e2e jobs; troubleshooting and known limits

- CI: gofmt -l gate, GOARCH=386 unit tests (32-bit stand-in for armv7),
  end-to-end job on linux amd64
- USAGE: corrupt buffer record and corrupt state file rows, lost death row,
  drop log summary, metrics wording
- CONFIGURATION: bare ? defect removed, max_size minimum, samples-only
  markers, known limits (held records on a crash during a reopen, markers
  the backend refuses)
- plan: final review paragraph in the delivery status

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
- Move the Go module path to github.com/getlaraplate/laraplate-edge
- Call the organization CLA workflow
- Run govulncheck v1.9.0
- Rename the repository and package from laraplate-<name> to lara<name>
- Ignore the Windows built-in plugin binary; plugin host Task 6 done

<!-- generated by git-cliff -->
