# Changelog

All notable changes to this project will be documented in this file.

## [0.7.0] - 2026-11-18

### 🚀 Features

- *(model)* Optional context per sample, written by the envelope
- *(file)* Connection and point validation
- *(file)* Stability watcher and ledger of read files
- *(file)* CSV parser for wide and long layouts
- *(file)* The file input, with ledger, deferred commit and moves
- *(file)* Probe and browse

### 🐛 Bug Fixes

- *(file)* Refuse a seen_retention beyond the duration range
- *(file)* Count header_row in lines, refuse dots under decimal comma and a missing timestamp column
- *(file)* Expire ledger entries by name and modification time, stamp the pending time after the emit
- *(file)* Ignore rows of only blank fields
- *(file)* Skip temporary names in any case, validate the lowercased pattern, refuse digit and decimal delimiters
- *(file)* Probe fails when the directory cannot be listed

### 🚜 Refactor

- *(input)* Share text conversion, size parsing and PointRef

### 📚 Documentation

- *(file)* Design of the file input driver
- *(file)* Plan 1, sample context and CSV
- *(file)* Tick plan 1 task 1
- *(file)* Document the file driver and close plan 1
- *(file)* Document the final-review fixes, balance the spec amendments

### 🧪 Testing

- *(e2e)* File input end to end

## [0.6.0] - 2026-11-18

### 🚀 Features

- *(mqtt)* Accept format sparkplug_b, its addresses and its plan
- *(mqtt)* Decode Sparkplug B payloads by hand with protowire
- *(mqtt)* Sparkplug births, deaths, aliases, seq and bdSeq
- *(mqtt)* Run the input under format sparkplug_b
- *(mqtt)* Probe and browse Sparkplug B inputs

### 🐛 Bug Fixes

- *(mqtt)* Treat device entries of one name as one Sparkplug device
- *(mqtt)* Convert integral floats and narrow unsigned Sparkplug values exactly
- *(mqtt)* Subscribe each Sparkplug node with all its devices so seq is complete
- *(mqtt)* Revive a Sparkplug target whose data proves a missed birth
- *(mqtt)* Make refused subscriptions format-aware under sparkplug_b
- *(mqtt)* Browse asks a rebirth only of the nodes the input addresses
- *(mqtt)* Revive a dead Sparkplug node on a DBIRTH and keep null metrics kindless
- *(mqtt)* Queue one Sparkplug rebirth request per node
- *(mqtt)* List device-only Sparkplug nodes in browse and skip refused nodes in probe

### 🚜 Refactor

- *(mqtt)* Simplify the Sparkplug metric decoder and its test encoder

### 📚 Documentation

- *(mqtt)* Plan 2 of the mqtt driver, Sparkplug B
- *(plans)* Tick Task 1 of the Sparkplug B plan
- *(plans)* Tick Task 2 of the Sparkplug B plan
- *(plans)* Tick Task 3 of the Sparkplug B plan
- *(plans)* Tick Task 4 of the Sparkplug B plan
- *(plans)* Tick Task 5 of the Sparkplug B plan
- *(mqtt)* Document Sparkplug B, its example and end-to-end test
- *(mqtt)* State the Sparkplug float to integer rule and the alias and NDEATH details
- *(plans)* Tick Task 6 steps of the Sparkplug B plan
- *(plans)* Close the Sparkplug B plan of the mqtt driver
- *(mqtt)* Rewrap the refused subscriptions paragraph
- *(mqtt)* Document the DBIRTH missed birth and record the Sparkplug follow-up
- *(mqtt)* Reword the rebirth clock comment, the DBIRTH rebirth trigger and the R13 reversal

### 🧪 Testing

- *(mqtt)* Make the Sparkplug rebirth rate-limit test able to fail
- *(mqtt)* Restore the json reload case of a topic dropped while its device stays
- *(e2e)* Wait for the Sparkplug input's own node filter

## [0.5.1] - 2026-11-17

### 🐛 Bug Fixes

- *(mqtt)* Refuse a JSON null for string and boolean connection fields and an empty broker port
- *(mqtt)* Read plain text numbers by the JSON number grammar and refuse a float32 underflow
- *(mqtt)* Keep the connection when the broker refuses a subscription
- *(mqtt)* Refuse two mqtt inputs with one client id on one broker
- *(mqtt)* Warn when connections keep dropping right after connecting
- *(mqtt)* Show in probe why a point has no good sample and mark the leaves browse drops
- *(mqtt)* Prefer the broker's answer when it and the end of the context are both ready
- *(mqtt)* Keep in the subscription set a refused topic the previous set held
- *(mqtt)* Count a connection that ends on a failed SUBSCRIBE or UNSUBSCRIBE as a short one

### 🚜 Refactor

- *(mqtt)* Inject the backoff jitter and the trace observer at construction

### 📚 Documentation

- *(mqtt)* Document the closed known limits of the mqtt driver
- *(mqtt)* Say that config validate compares explicit client ids only
- *(plans)* Record the cmd test waits after the review fixes
- *(mqtt)* Say browse counts --duration from the accepted subscription

### 🧪 Testing

- *(mqtt)* Drop the two trailing tracker calls that asserted nothing
- *(mqtt)* Warm up and wait for the acknowledgements before the reconnection in the refused subscription test
- *(cli)* Time the mqtt browse tests from the subscription and give probe a margin
- *(cli)* Give the log scan's traffic loop its own deadline for slow Windows runners

## [0.5.0] - 2026-11-16

### 🚀 Features

- *(mqtt)* Parse the connection and point addresses
- *(mqtt)* Decode JSON and plain payloads into samples
- *(input)* Let a driver report a device dead at once
- *(mqtt)* Track device availability topics
- *(mqtt)* Connect through paho behind a client interface
- *(mqtt)* Run the input: subscribe, decode, keep devices alive
- *(mqtt)* Probe and browse a broker

### 🐛 Bug Fixes

- *(mqtt)* Renew the devices when the connection is lost
- *(mqtt)* Refuse implausible payload timestamps
- *(mqtt)* Refuse availability and override topics that do not match the points
- *(mqtt)* Keep the subscription set across rollbacks
- *(mqtt)* Acknowledge a message only after the router handled it

### 💼 Other

- *(deps)* Bump laraedge-sdk to v0.1.2
- *(deps)* Add paho.mqtt.golang (BSD-3-Clause) and mochi-mqtt (MIT, test only)

### 📚 Documentation

- *(specs)* Design the mqtt input driver
- *(plans)* Plan the mqtt driver with JSON and plain payloads
- *(plans)* Tick Task 1 of the mqtt plan
- *(plans)* Tick Task 2 of the mqtt plan
- *(plans)* Tick Task 3 of the mqtt plan
- *(plans)* Tick Task 4 of the mqtt plan
- *(plans)* Tick Task 5 of the mqtt plan
- *(plans)* Tick Task 6 of the mqtt plan
- Document the mqtt driver
- *(plans)* Close the mqtt JSON plan
- Correct the mqtt read error series, add R11, sort the browse example
- *(mqtt)* Browse lists the leaves sorted
- *(mqtt)* Record the final review fixes and amend Q7

### 🧪 Testing

- *(e2e)* Run the mqtt input against a broker
- *(mqtt)* Wait for the broker to record the acknowledgement
- *(cli)* Ignore the temporary directory when checking config validate echoes no values

## [0.4.0] - 2026-11-04

### 🚀 Features

- *(clock)* Fake.BlockUntil, a negative Advance is a no-op, firing order pinned
- *(opcua)* Browse shows the status of a child it could not expand
- *(browse)* Say so when the start node has nothing below it
- *(browse)* A plugin node with nothing below it prints the same note

### 🐛 Bug Fixes

- *(cmd)* Extra argument, help, -- and oversize status answers
- *(cmd)* Check the HTTP status before the status answer size; pin -- at command level
- *(model)* Copy birth signals, normalise -0, refuse non-UTF-8 plugin strings, pin the uuid counter
- *(buffer)* Ack inside a cut region, pin one rotation under a lasting floor breach, document the recount limit
- *(output)* Max_wait runs from the append of the first record waiting
- *(output)* Count failed settlements, close idle connections, tolerate a foreign default transport
- *(status,pipeline)* Rate-limited refusal warning, server timeouts, token compare, uptime
- *(pipeline,status)* Flush the router's refusal summary, keyed by kind; lograte tests; sturdier scan tests
- *(config,transform)* Natural message order, heartbeat after a clock step, transform minors
- *(config,transform)* Clamp deadlines at a backward clock step, order unknown transform names
- *(supervisor)* Wait for an abandoned start before restarting its input; t.Fatal only on the test goroutine
- *(provider)* Poll less often while the backend answers an attempted or older version
- *(simulated)* Stop emitting once cancelled; refuse an oversize scenario_file at the check
- *(supervisor)* Wait for every abandoned start of an input, once; docs placement
- *(supervisor)* Count only the abandoned starts still running in the wait warning
- *(modbus,opcua)* Name an unknown connection key only when it is safe
- *(opcua)* Sweep stale own temporaries, refuse a wide key mode, check the trusted file
- *(opcua)* Ignore implausible timestamps, name ExpandedNodeId data types
- *(opcua)* Browse names a refusal with the standard status spelling
- *(opcua)* Sweep plain own temporaries once per PKI directory
- *(opcua)* Claim a stale own temporary by rename instead of a process-wide guard
- *(opcua)* Remove a claimed own temporary without following a symlink
- *(opcua)* Read the claimed directory's identity before the rename
- *(buffer,output,provider)* Ack at the end of a cut segment, count a failed confirm, reword the stale warning
- *(opcua)* On Windows, wait for renames in flight before a claim is removed
- *(opcua)* Settle a long claimed path, wait up to a second, name Linux mount points

### 🚜 Refactor

- *(backoff)* One exponential-jitter backoff for drivers and outputs
- *(docrules)* One UnknownKey refusal for the modbus and opcua connections
- *(config)* Split the builder out of config.go
- *(supervisor)* Move the plugin code to plugins.go
- *(opcua)* Split dial and subscribe out of session.go and driver.go

### 📚 Documentation

- *(plans)* Plan for the deferred minors cleanup
- *(buffer)* Ack re-reads the acknowledged frames by design
- *(configuration)* Reflow the serial port paragraph
- *(spec)* D6, the serial port is exclusive on Windows only
- *(plans)* Close the deferred minors cleanup plan

### 🧪 Testing

- *(clock)* Make BlockUntil wake test real, tighten assertions
- Wait on BlockUntil and bounded polls instead of Gosched spins
- Restore the advanceUntil deadline, guard the busyWait gap
- *(output)* Discriminating max_wait check; document UnsealedAge bounds and the client's own transport
- *(supervisor,cmd)* Count timers above the router flush loop; drop time from the log scan
- *(plugin)* Wait for the SIGSTOP and for the counted health failure before going on
- *(plugin,supervisor)* Share the stopped-process wait; wait for SIGSTOP in the supervisor stop test
- *(modbus)* Unify the type-area message style, rename an area test, add connection refusals
- *(modbus)* Safe dial-end signal, assert every point type size; rewrap a doc line
- *(opcua)* The key mode check is Unix only
- *(cmd)* Time the uptime test from the status token lookup, not the watchdog's
- *(opcua)* Swap the claimed directory by moving it aside, not deleting it
- *(plugin)* Await Start's result after Stop instead of polling it
- *(cmd)* Send the uptime test's status request through httpGet with a bearer option

### ⚙️ Miscellaneous Tasks

- *(modbus)* One point-type table, serial open error texts, test hygiene and diagnostics
- *(opcuatest)* Remove the unused Server.Drop

## [0.3.0] - 2026-11-02

### 🚀 Features

- *(plugin)* A processor stream opens with the current state of its observed devices
- *(output)* A plugin output in backoff tries again when its plugin is back
- *(plugin)* Run_dir and a private fallback keep plugin sockets within the Unix limit

### 🐛 Bug Fixes

- *(plugin)* Processor state review fixes: shared router lock, dead devices, lost state records counted
- *(output)* Wake and loop test review fixes
- *(plugin)* Socket fallback review fixes: /tmp when the temp dir is deep, umask-proof root, cleanup asserted
- *(config)* Refuse duplicate observed devices; one clock offset decision; validation cleanup

### 📚 Documentation

- *(plans)* Plan for processor state on stream open, output wake and socket directory
- *(plans)* Map every deferred minor of plan 3 to a task
- Repair the socket troubleshooting rows and rewrap the socket subsection
- Socket rows tell the first apply from a restart; processor_dropped_total across an outage
- *(plans)* Close the processor state, output wake and socket directory plan
- The superseded-copies note belongs to processor_dropped_total, not refused

### 🧪 Testing

- *(config)* Each layer refuses a device observed twice; metrics help names failed outputs
- Deterministic waits and the assertions plan 3 deferred
- *(supervisor)* WaitFor sleeps 1 ms between polls; restartClock notes what it mirrors
- Fix two Windows-only test failures

## [0.2.0] - 2026-10-31

### 🚀 Features

- *(config)* Processors section and plugin output config, SDK v0.1.1
- *(plugin)* Validate processors and plugin outputs offline against their manifests
- *(plugin)* Plugin output sender over the Output service
- *(plugin)* Processor adapter over the Process stream
- *(plugin)* Synchronous Configure of a plugin output at apply
- *(supervisor)* Run processors and plugin outputs by difference
- *(status)* Processors and plugin outputs in /status and /metrics

### 🐛 Bug Fixes

- *(supervisor)* Offer the first births of observed devices to processors
- *(plugin)* Count the copy a failed processor Send loses as dropped
- *(schema)* Refuse url and token on plugin outputs

### 🚜 Refactor

- *(output)* One delivery loop behind a Sender
- *(plugin)* Processor converts derived samples with sampleFromWire

### 📚 Documentation

- *(plans)* Plugin processors and outputs plan approved
- *(plans)* Plan 3 task 1 done
- *(plans)* Plan 3 task 2 done
- *(plans)* Plan 3 task 3 done
- *(plans)* Plan 3 task 4 done
- *(plans)* Plan 3 task 5 done
- *(supervisor)* Processors and plugin outputs in the apply and lock order notes
- *(plans)* Plan 3 task 6 done
- Processors, plugin outputs and the plugin-line example
- *(plans)* Plan 3 task 7 steps 1-2 done
- Processor outages shorter than death_tolerance, births and refused keys
- *(plans)* Close plan 3, plugin processors and outputs

### 🧪 Testing

- *(e2e)* Plugin processor and output with the SDK examples

### ⚙️ Miscellaneous Tasks

- Publish a GitHub Release with the binaries on every version tag

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
