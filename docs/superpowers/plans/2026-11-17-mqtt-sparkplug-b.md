---
status: open
created_on: 2026-11-17
---
# MQTT driver, Sparkplug B. Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax. Tick each step when it is done, in the same work block.

**Goal:** `format: sparkplug_b` on the `mqtt` input. The input:
- subscribes to the edge nodes and devices its points address;
- decodes Sparkplug B 3.0 payloads into typed samples;
- maps NBIRTH and DBIRTH to birth and NDEATH and DDEATH to death, honouring `bdSeq`;
- learns metric aliases from the births and counts `seq` gaps;
- with `rebirth: true` only, asks a node to publish its births again;
- offers `probe` and `browse` for the format.

**Architecture:** everything stays in `internal/input/mqtt`, on the base of plan 1: the same client, reconnect loop, subscription set, router and lifecycle tracker. Sparkplug adds four pieces:
- an address and plan parser;
- a hand-written protowire decoder of the payload;
- a pure session state machine (births, deaths, aliases, `seq`, `bdSeq`, rebirth requests) driven by the injected clock;
- the router branch that feeds it.

The client gains `Publish`, used only by the rebirth request.

**Tech Stack:** Go 1.27; `google.golang.org/protobuf/encoding/protowire` (already a dependency, v1.36.12); paho v1.5.1 and, in tests, mochi v2.7.9 as in plan 1. No new dependency.

**Spec:** `../specs/2026-11-06-mqtt-driver-design.md`, sections 2 (M1, M4, M7, M8), 3, 5, 6, 7 and 8 (plan 2). Plan 1 and its follow-up are `2026-11-06-mqtt-json.md`.

## Global Constraints

- **Repo and branch:**
  - AGPL-3.0 repo, permissive dependencies only, no CGO, module `github.com/getlaraplate/laraedge`.
  - Work on a feature branch from `master`, merged through a pull request into `master`. `develop` was deleted after the release of 2026-11-16, and the user chose `master` as the base.
- **Checks before every commit:**
  - `gofmt -l .` empty, `go vet ./...`, `go test -race ./...`, `GOARCH=386 go test ./...`, `GOOS=windows go vet ./...`.
  - Table-driven tests; fakes over mocks.
  - Fake clock for scheduling (`clock.Fake.BlockUntil` to wait for timers); no sleeps or timing assertions.
- **No Tahu code.** No file of Eclipse Tahu is copied, and no code is generated from `sparkplug_b.proto`: it is `EPL-2.0` only (M7). The field numbers below were read from that file on 2026-11-17 as facts, and the decoder is written by hand with `protowire`.
- **The only publish** in `internal/input/mqtt` outside test files is the rebirth request of Task 4. With `rebirth: false`, the default, nothing is ever published; a test proves it.
- **Input contract:** unchanged from plan 1 (core R47, spec D6).
- **No values in logs or errors.** Payload values, metric values, passwords and connection settings never appear in errors or in logs at info and above.
- **Spec values exactly:**
  - namespace `spBv1.0`;
  - address `<group>/<edge_node>[/<device>]|<metric>`, split at the first `|`;
  - subscriptions `spBv1.0/<group>/+/<edge_node>` and `spBv1.0/<group>/+/<edge_node>/<device>`;
  - message types handled: `NBIRTH`, `NDEATH`, `NDATA`, `DBIRTH`, `DDEATH`, `DDATA`; ignored: `NCMD`, `DCMD`, `STATE`;
  - the `bdSeq` metric name;
  - rebirth: an `NCMD` on `spBv1.0/<group>/NCMD/<edge_node>` with the Boolean metric `Node Control/Rebirth` = true, at most one every 30 s per node, `rebirth` default `false`, refused with `json`;
  - `timestamp`, `topics` and `availability` refused with `sparkplug_b`;
  - Quality property `Quality`: 192 and above `good`, 64 to 191 `uncertain`, below 64 `bad`;
  - read error kinds `unknown_alias` and `sequence_gap`, plus `malformed_payload` (decision S6).
- **Ownership:** commit only your own files in laraedge; never push; no `Co-Authored-By` trailer; do not commit at the stack root.

### Sparkplug B wire format (read from `sparkplug_b.proto`, Tahu, 2026-11-17)

| Message | Fields used (number, wire type) |
|---|---|
| `Payload` | `timestamp` 1 uint64; `metrics` 2 repeated `Metric`; `seq` 3 uint64; `uuid` 4 and `body` 5 skipped |
| `Metric` | `name` 1 string; `alias` 2 uint64; `timestamp` 3 uint64; `datatype` 4 uint32; `is_historical` 5 bool; `is_transient` 6 bool; `is_null` 7 bool; `metadata` 8 skipped; `properties` 9 `PropertySet`; value oneof: `int_value` 10 uint32, `long_value` 11 uint64, `float_value` 12 float (fixed32), `double_value` 13 double (fixed64), `boolean_value` 14 bool, `string_value` 15 string, `bytes_value` 16, `dataset_value` 17, `template_value` 18, `extension_value` 19 |
| `PropertySet` | `keys` 1 repeated string; `values` 2 repeated `PropertyValue` (paired by position) |
| `PropertyValue` | `type` 1 uint32; `is_null` 2 bool; oneof: `int_value` 3 uint32, `long_value` 4 uint64, `float_value` 5, `double_value` 6, `boolean_value` 7, `string_value` 8, 9 to 11 skipped |
| `DataType` | Int8 1, Int16 2, Int32 3, Int64 4, UInt8 5, UInt16 6, UInt32 7, UInt64 8, Float 9, Double 10, Boolean 11, String 12, DateTime 13, Text 14, UUID 15, DataSet 16, Bytes 17, File 18, Template 19, PropertySet 20, PropertySetList 21, arrays 22 to 34 |

Signed integers are two's complement. Int8, Int16 and Int32 are carried in `int_value`, sign-extended from the type's width; Int64 is the `long_value` bits read as int64. Unknown field numbers are skipped by wire type, never an error.

## Decisions taken in this plan

- **S1. Address.** `<group>/<edge_node>[/<device>]|<metric>`, split at the first `|`.
  - The left part has two or three levels, each non-empty and free of `+`, `#` and `/`.
  - The metric is any non-empty string. It may contain `/`, as in `Node Control/Rebirth`, and a later `|`.
  - Parsed by `ParseSparkplugAddress`. Under `sparkplug_b` the JSON `ParseAddress` is not used.
- **S1b. Node filter for every target.** `Filters` holds `spBv1.0/<group>/+/<edge_node>` for the node of every target, devices included, plus `spBv1.0/<group>/+/<edge_node>/<device>` for each device target. Without the node filter a device would miss its node's NBIRTH (`bdSeq`, aliases) and NDEATH. A node that only devices address is still a known node: its messages are handled, not `unmapped`, and give no samples.
- **S2. One target per agent device.** A target is a node (group and edge node) or a Sparkplug device (group, edge node and device).
  - Every point of an agent device must address the same target.
  - Two agent devices may not share a target.
  - Either case refuses the document with a fixed message naming the device positions. Life and death then map one to one (spec section 7).
  - Two points of one device may not address the same metric; the document is refused.
- **S3. Life and death reuse the tracker of plan 1.** `Tracker` gains `Offline(device)` (held dead, `Die`) and `Online(device)` (released, `Read`). The availability topics of plan 1 call them too, so the behaviour is unchanged.
  - NBIRTH puts the node's agent device online, DBIRTH the device's.
  - NDEATH puts offline the node's agent device and every agent device of a Sparkplug device under that node.
  - DDEATH puts offline that device only.
  - A device with no birth seen since the agent started is kept alive by `KeepAlive` while the broker is connected, as every `json` device is (plan 1, Q2). Births are not retained, so waiting for one would leave every device dead until its node restarts.
  - A device put offline stays held dead until its own birth. A data message does not revive it: under Sparkplug a death is authoritative until the next birth. This differs from availability, where a sample revives. The samples of a device held offline are dropped without a count: after a death they can only come from reordered messages.
  - A sample of a live device renews it through `Tracker.Seen`, which reads it unless it is held.
- **S4. Births.**
  - An NBIRTH clears the node's alias table and the aliases of its devices. It records the payload's `bdSeq` metric, records `seq` as the last seen, learns the aliases and datatypes of its metrics, and emits the samples of the metrics points address.
  - A DBIRTH learns its aliases into the same per-node table (Sparkplug aliases are unique per edge node) and emits its samples.
  - A DBIRTH for a node with no NBIRTH seen is accepted, so an agent started late still gets device births.
- **S5. Deaths and `bdSeq`.**
  - An NDEATH whose `bdSeq` metric differs from the last NBIRTH's is stale and ignored.
  - When no NBIRTH has been seen, or the NDEATH has no `bdSeq`, the NDEATH is honoured.
  - An NDEATH ends the node's `seq` tracking.
  - The alias tables survive deaths and reconnections. Only an NBIRTH replaces them.
- **S6. Malformed payloads.** A payload `protowire` cannot parse is dropped with the new kind `malformed_payload`. The spec lists no kind for it, so it is amended with this plan. A metric with a value field that does not match its datatype is a `bad` sample with kind `conversion`.
- **S7. Values.** These convert with the lossless rules of spec section 4, reusing the decoder's integer and float rules:
  - integer types and DateTime (milliseconds, as an integer);
  - Float and Double;
  - Boolean to `bool` only;
  - String, Text and UUID to `string` only.

  Other cases are a `bad` sample with kind `conversion`: DataSet, Bytes, File, Template, PropertySet, PropertySetList, arrays, Unknown, a missing datatype, and a value that does not fit the point type.
  - The datatype is the metric's own, else the one learnt from the birth for that name or alias.
  - `is_null` gives a `bad` sample without a read error, since it is the device's own report.
  - The `Quality` property, read from `int_value` or `long_value`, maps 192+ to good, 64 to 191 to uncertain and below 64 to bad. Values of other types are ignored.
- **S8. Timestamps.** The metric's timestamp, else the payload's, else reception. A timestamp outside 1970 to 9999 makes the sample `bad` with kind `bad_timestamp`, as plan 1 R12. `is_historical` is an ordinary sample.
- **S9. Aliases.**
  - A data metric with a name is resolved by name.
  - A data metric with only an alias is resolved through the node's table. An unknown alias drops the metric, counts `unknown_alias` once per metric, and is a rebirth trigger.
  - Metrics no point addresses are ignored without a count, since edge nodes publish many metrics the document does not use.
- **S10. `seq`.**
  - NBIRTH sets the last `seq`.
  - NDATA, DBIRTH, DDATA and DDEATH expect `(last + 1) mod 256`. A different value counts `sequence_gap` and is a rebirth trigger; either way it becomes the last.
  - A message without `seq`, or the first message of a node since the connection or its NDEATH, sets the last without a check.
  - A reconnection resets every node's tracking, since a gap across an outage is expected.
- **S11. Rebirth.**
  - Only with `rebirth: true`. On a trigger (S9, S10) the session asks for at most one rebirth per node every 30 s of the injected clock.
  - The input publishes it with QoS 0, not retained. The payload carries the payload timestamp (now, in ms) and one metric: name `Node Control/Rebirth`, datatype Boolean (11), `boolean_value` true. It is built by `encodeRebirth` in production code.
  - A failed publish is logged at debug. The next trigger after the 30 s retries it.
- **S12. Routing.** Under `sparkplug_b` the router keeps the `max_payload` check first, then parses the topic.
  - A topic outside `spBv1.0/<group>/<type>/<node>[/<device>]` counts `unmapped`. So does a message of a node or device that is not in the plan (persistent sessions after a configuration change); a node known through its devices is in the plan (S1b).
  - `NCMD`, `DCMD` and `STATE` are dropped without a count.
  - Acknowledgement is as in plan 1: every routed message is handled.
- **S13. probe.**
  - It subscribes the same filters with the `-probe` client and waits as in plan 1.
  - A point whose node publishes with aliases only shows `unknown_alias` until a birth arrives.
  - With `rebirth: true`, probe publishes one rebirth per addressed node when it has subscribed, and this is its only write. The running agent sees the births too and only learns from them.
- **S14. browse.** The default filter is `spBv1.0/#` under `sparkplug_b`; `json` keeps `#`. The result is one `SparkplugEntry` per group, node and device seen. Each entry has:
  - the message count;
  - whether a birth was seen in the window;
  - the malformed message count;
  - from the births, every metric as an `Address` in the `group/node[/device]|metric` form, with a suggested point `Type`:
    - Int8 and Int16 → `int16`; Int32 → `int32`; Int64 and DateTime → `int64`;
    - UInt8 and UInt16 → `uint16`; UInt32 → `uint32`; UInt64 → `uint64`;
    - Float → `float32`; Double → `float64`; Boolean → `bool`; String, Text and UUID → `string`;
    - otherwise `unsupported:<DataType name>`.

  With `rebirth: true`, browse publishes one rebirth to each node at its first message in the window.

## Review Focus

- **Agent started after the births**, with a node that publishes data by alias only. Each metric counts `unknown_alias` and emits no sample, and there is no panic. With `rebirth: true` exactly one NCMD is published within 30 s, its births yield samples, and the following data resolves. Test in Task 4.
- **Stale NDEATH:** the will of the previous session, with the old `bdSeq`, delivered after the new NBIRTH. It is ignored and the device stays alive. Test in Task 3.
- **Hostile payloads:**
  - a truncated payload, a garbage payload and a zero-length payload;
  - an Int8 metric carrying `int_value` 0xFFFFFF80, which is -128, and one carrying 0x80, which is -128 sign-extended from 8 bits;
  - a Float metric carrying `string_value`, and a metric with an unknown datatype number.

  Each gives the kind of S6 or S7, never a panic. Test in Task 2, including a `go test -fuzz` seed corpus run for a few seconds.
- **`seq` 255 followed by 0** is not a gap. A reconnection followed by any `seq` is not a gap. Test in Task 3.
- **NDEATH with devices under the node:** every agent device under that node dies. A following NBIRTH revives only the node device, and each Sparkplug device waits for its DBIRTH. Test in Task 3.

---

### Task 1: Configuration, addresses and the Sparkplug plan

**Files:** modify `internal/input/mqtt/config.go`, `config_test.go`, `driver.go` (`checkPoint` and `newInput` by format); create `internal/input/mqtt/sparkplug_config.go`, `sparkplug_config_test.go`.

**Interfaces:**
- Consumes:
  - `ParseConnection`, `Connection`, `CheckPoint`, `pointTypes` (plan 1).
- Produces:
  - `Connection.Rebirth bool`.
  - `mqtt.SparkplugAddress{Group, Node, Device, Metric string}`, with `Device` empty for a node metric.
  - `mqtt.ParseSparkplugAddress(s string) (SparkplugAddress, error)`.
  - `mqtt.SparkplugTarget{Group, Node, Device string}` with `func (t SparkplugTarget) Key() string` (`group/node[/device]`).
  - `mqtt.SparkplugPlan{Targets map[SparkplugTarget]*TargetPlan; ByDevice map[string]SparkplugTarget; Filters []string /* sorted, deduplicated */}`.
  - `mqtt.TargetPlan{Target SparkplugTarget; Device string /* the agent device */; Points map[string]PlannedPoint /* by metric name */}`.
  - `mqtt.PlanSparkplug(cfg config.Input) (*SparkplugPlan, error)`.

- [x] **Step 1: Failing tests.**
  - `format: sparkplug_b` is accepted; `rebirth: true|false` is accepted with it.
  - `rebirth` is refused with `json` (`connection.rebirth: needs format sparkplug_b`).
  - `timestamp`, `topics` and `availability` are refused with `sparkplug_b` (`connection.<field>: not with format sparkplug_b`).
  - `rebirth: null` and a non-boolean are refused (`connection.rebirth: must be a boolean`).
  - The removed `notInThisBuild` paths no longer exist.
  - Addresses accepted:
    - `g/n|m` gives Device "";
    - `g/n/d|Node Control/Rebirth`;
    - `g/n/d|a|b` gives Metric `a|b`.
  - Addresses refused: `g|m`, `g/n/d/x|m`, `g//n|m`, `g/+|m`, `g/n#|m`, `g/n|`, `g/n` (no metric).
  - Plan refusals:
    - one device with points on `g/n|a` and `g/n/d|b`;
    - two devices on `g/n/d`;
    - two points of one device on the same metric.
  - Filters for devices on `g/n` and `g/n/d`: `["spBv1.0/g/+/n", "spBv1.0/g/+/n/d"]`.
  - A device on `g/n/d` alone also subscribes its node: `["spBv1.0/g/+/n", "spBv1.0/g/+/n/d"]`. The node's NBIRTH carries the `bdSeq` and the alias table, and its NDEATH kills the devices under it (S1b).
- [x] **Step 2: Implement.**
  - `newInput` builds `SparkplugPlan` instead of `Plan` under `sparkplug_b`.
  - `checkPoint` uses the address parser of the format.
  - `CheckClientIDs` is unchanged.
- [x] **Step 3: Run the checks; commit** `feat(mqtt): accept format sparkplug_b, its addresses and its plan`.

### Task 2: Payload decoder, values and the rebirth encoder

**Files:** create `internal/input/mqtt/sparkplug_decode.go`, `sparkplug_decode_test.go`, `sparkplug_fuzz_test.go`; create `internal/input/mqtt/mqtttest/sparkplug.go` (test-only encoder); modify `decode.go` only to expose the integer and float conversion it already has to the new file, if needed.

**Interfaces:**
- Consumes:
  - the conversion helpers of `decode.go` (`convertNumber`, `fitsFloat32`, `intInRange`);
  - `plausibleTimestamp`;
  - `KindConversion`, `KindBadTimestamp`.
- Produces:
  - `spPayload{Timestamp uint64; HasTimestamp bool; Seq uint64; HasSeq bool; Metrics []spMetric}`.
  - `spMetric{Name string; HasName bool; Alias uint64; HasAlias bool; Timestamp uint64; HasTimestamp bool; Datatype uint32 /* 0: absent */; Historical, Null bool; Quality int64; HasQuality bool; Value spValue}`.
  - `spValue{Field protowire.Number /* 0: none */; Uint uint64; Float64 float64; Bool bool; Text string}`.
  - `decodePayload(b []byte) (spPayload, error)`.
  - `metricValue(m spMetric, datatype uint32, pointType string) (model.Value, string /* kind, "" ok */)`.
  - `metricQuality(m spMetric) model.Quality`.
  - `metricTime(m spMetric, p spPayload, received time.Time) (time.Time, bool /* plausible */)`.
  - `encodeRebirth(now time.Time) []byte`.
  - `KindMalformedPayload = "malformed_payload"`.
  - `DatatypeName(dt uint32) string` and `SuggestedType(dt uint32) string`, for browse.
- Test-only package `mqtttest`:
  - `SparkplugPayload{Timestamp, Seq *uint64; Metrics []SparkplugMetric}`;
  - `SparkplugMetric{Name *string; Alias, Timestamp *uint64; Datatype *uint32; Historical, Null bool; Quality *int64; Int *uint32; Long *uint64; Float *float32; Double *float64; Bool *bool; String *string; Bytes []byte}`;
  - `func (p SparkplugPayload) Encode() []byte` (protowire, test-only);
  - `func BdSeq(v uint64) SparkplugMetric`.

- [x] **Step 1: Failing tests (table-driven).**
  - Round trip through `mqtttest.Encode` and `decodePayload` for every field of the wire table, including unknown field numbers, which are skipped.
  - Each of the following is an error: a truncated varint, a length beyond the buffer, and a group wire type.
  - `metricValue` for every datatype to every point type, at least:
    - Int8 `0xFFFFFF80` gives `int16(-128)`, and Int8 `0x80` gives -128;
    - Int16 `0xFFFF8000` gives -32768;
    - UInt32 `0xFFFFFFFF` into `int32` is `conversion`, and into `int64` gives 4294967295;
    - Int64 `long_value` `1<<63` gives `math.MinInt64` in `int64`;
    - UInt64 `1<<63` is `conversion` (plan 1 Q4);
    - Float 1.5 into `int32` is `conversion`, and Float 3.0 into `int32` gives 3;
    - Double NaN is `conversion`;
    - Boolean into `string` is `conversion`;
    - String into `string` gives the string;
    - DateTime `1700000000000` into `int64` gives the same number;
    - Bytes, DataSet, Template and Int8Array are `conversion`;
    - datatype 0 and 99 are `conversion`;
    - Float carrying `string_value` is `conversion`.
  - `metricQuality`: absent is Good; 192 and 255 are Good; 64 and 191 are Uncertain; 0 and 63 are Bad; a `string_value` Quality is Good (ignored).
  - `metricTime`: the metric's, else the payload's, else reception; year 10000 is implausible.
  - `encodeRebirth` decodes back to one metric named `Node Control/Rebirth`, datatype 11, `boolean_value` true, with the timestamp.
- [x] **Step 2: Fuzz.** `FuzzDecodePayload` with a seed corpus of the encoded fixtures plus `nil`, `0x0a`, `0xff×16`. Run `go test -run x -fuzz FuzzDecodePayload -fuzztime 10s ./internal/input/mqtt/`; no panic. The seeds run in the normal suite.
- [x] **Step 3: Implement** with `protowire.ConsumeTag`, `ConsumeVarint`, `ConsumeFixed32`, `ConsumeFixed64`, `ConsumeBytes`, `ConsumeFieldValue`.
- [x] **Step 4: Run the checks; commit** `feat(mqtt): decode Sparkplug B payloads by hand with protowire`.

### Task 3: The Sparkplug session and the tracker

**Files:** create `internal/input/mqtt/sparkplug_session.go`, `sparkplug_session_test.go`; modify `availability.go` (`Online`, `Offline`, used by `Message`), `availability_test.go`.

**Interfaces:**
- Consumes:
  - `SparkplugPlan` (Task 1);
  - `decodePayload`, `metricValue`, `metricQuality`, `metricTime` (Task 2);
  - `Tracker`;
  - `clock.Clock`.
- Produces:
  - `func (t *Tracker) Online(device string)`, `func (t *Tracker) Offline(device string)`, and `func (t *Tracker) Seen(device string) (alive bool)`, which reads the device unless it is held and reports whether it did.
  - `sparkplugSession`, built by `newSparkplugSession(plan *SparkplugPlan, rebirth bool, clk clock.Clock)`, with:
    - `Handle(topic string, payload []byte, received time.Time) spResult`;
    - `Reset()`, which ends every node's `seq` tracking at a reconnection;
    - `RebirthNodes(now time.Time) []SparkplugTarget`, which returns and marks the nodes due a rebirth, empty when `rebirth` is false.
  - `spResult{Samples []model.Sample; Online, Offline []string /* agent devices */; Kinds []string; Mapped bool /* false: unmapped */; Ignored bool /* NCMD, DCMD, STATE */}`.
  - Constants `KindUnknownAlias = "unknown_alias"`, `KindSequenceGap = "sequence_gap"`, `rebirthInterval = 30 * time.Second`, `metricRebirth = "Node Control/Rebirth"`, `metricBdSeq = "bdSeq"`.

- [x] **Step 1: Failing tests (pure, fake clock).** One test per decision S3 to S5 and S8 to S11:
  - NBIRTH gives samples, Online for the node device and the learnt aliases.
  - NDATA by alias resolves to a sample, and by name too.
  - An unknown alias counts `unknown_alias` with no sample, and the node is due a rebirth only when `rebirth` is true.
  - Rebirth requests: two triggers within 30 s give one request, and a trigger after 30 s gives a second.
  - `seq` 254, 255, 0 is clean; 3 after 1 counts `sequence_gap`; after `Reset` any `seq` is clean.
  - NDEATH with a matching `bdSeq` puts offline the node device and each device under it; a mismatched `bdSeq` is ignored; NDEATH with no NBIRTH seen is honoured.
  - DDEATH puts that device only offline.
  - After NDEATH, NDATA does not put the node online, and the following NBIRTH does.
  - DBIRTH with no NBIRTH seen is accepted.
  - A metric not addressed is ignored silently; a malformed payload counts `malformed_payload`.
  - These topics are `Mapped: false`: a non-Sparkplug topic, `spBv1.0/g/NDATA` (too short), and a node not in the plan.
  - `NCMD`, `DCMD` and `STATE` are `Ignored`.
  - `is_null` is a bad sample with no kind; a historical metric keeps its own timestamp.
  - Tracker: `Offline` holds a device so `KeepAlive` does not revive it; `Online` releases it. The availability tests of plan 1 stay green unchanged.
- [x] **Step 2: Implement.** The session is used from the router's single goroutine under the router's mutex, so it needs no lock of its own; say so in its doc comment.
- [x] **Step 3: Run the checks; commit** `feat(mqtt): Sparkplug births, deaths, aliases, seq and bdSeq`.

### Task 4: The input under `sparkplug_b`: subscriptions, routing, rebirth

**Files:** modify `internal/input/mqtt/client.go` (`Publish`), `driver.go`, `run.go`, `client_test.go`, `run_test.go`; extend `harness_test.go` with Sparkplug helpers; create `internal/input/mqtt/sparkplug_run_test.go`.

**Interfaces:**
- Consumes: Tasks 1 to 3; the `mqtttest` broker. Its publish hook, or a recording subscriber on `spBv1.0/+/NCMD/#`, sees the agent's NCMD.
- Produces:
  - On the `client` interface: `Publish(ctx context.Context, topic string, payload []byte, qos byte) error`, which waits for the token; paho's `Publish` with `retained` false.
  - `Input.topics` returns `SparkplugPlan.Filters` under `sparkplug_b`, so the subscription set file stores the filters.
  - `router.message` dispatches to `sparkplugSession.Handle`. It applies `Online` and `Offline` through the tracker, emits each sample only when `Tracker.Seen` reports its device alive (S3), counts the kinds, and returns handled.
  - After each message the run loop publishes, outside the router lock, the rebirths `RebirthNodes` returns, on the current client.
  - `session` calls `Reset` on every new connection.

- [x] **Step 1: Failing integration tests on mochi** (`go test -race`, each run `-count=20` before commit):
  - **Births and data:** NBIRTH then NDATA by alias gives samples with the right types and timestamps, and birth before the first sample.
  - **Node death:** NDEATH emits death for the node device and its devices. DDEATH emits death for one device.
  - **Stale will:** the agent connected; NBIRTH with bdSeq 1; an NDEATH with bdSeq 0. No death.
  - **Late start:** the input started after the NBIRTH, data by alias only.
    - With `rebirth: false`, it counts `unknown_alias` and nothing is published (assert that no message from the agent's client id reaches the broker).
    - With `rebirth: true`, one NCMD `Node Control/Rebirth` reaches `spBv1.0/g/NCMD/n`. The test node answers with NBIRTH, and the next NDATA gives samples.
    - Two further unknown aliases within 30 s of the fake clock publish nothing more.
  - **Persistent session and reload:** the session survives a stop and start of the input. After a reload that drops a node, its filter is unsubscribed and its queued NDATA counts `unmapped`. This reuses the plan 1 tests in table form over both formats where they apply.
  - **Ignored types:** NCMD, DCMD and STATE on subscribed filters change nothing and count nothing.
  - **Payload size:** a payload above `max_payload` counts `payload_too_large`.
- [x] **Step 2: Implement.**
- [x] **Step 3: Run the checks; commit** `feat(mqtt): run the input under format sparkplug_b`.

### Task 5: probe and browse for Sparkplug

**Files:** modify `internal/input/mqtt/probe.go`, `browse.go`, `probe_browse_test.go`; modify `cmd/laraplate-edge/probe.go`, `browse.go` and their MQTT tests.

**Interfaces:**
- Consumes: Tasks 1 to 4; `commandClient` (plan 1).
- Produces:
  - `ProbePoints` handles both formats. Under `sparkplug_b` a point's reason is one of `unknown_alias`, `malformed_payload`, `payload_too_large` or `subscribe_refused`.
  - `mqtt.SparkplugEntry{Group, Node, Device string; Messages int; Born bool; Malformed int; Metrics []SparkplugMetricEntry}` and `SparkplugMetricEntry{Address, Type string}`.
  - `func (in *Input) BrowseSparkplug(ctx context.Context, filter string, duration time.Duration) ([]SparkplugEntry, bool /* truncated at 1000 entries */, error)`.
  - The `browse` command picks `Browse` or `BrowseSparkplug` by the input's format. Its default `--filter` is `#` for `json` and `spBv1.0/#` for `sparkplug_b`.
  - Output: one line per entry, and below it one indented line per metric, `address` then `type`, sorted by address.

- [x] **Step 1: Failing tests.**
  - **probe, birth seen:** the probe gets samples when a retained NDATA by name or a birth arrives in the window.
  - **probe, aliases only:** a node using aliases shows `unknown_alias`.
  - **probe, rebirth:** with `rebirth: true` the probe publishes one NCMD per addressed node and gets the births. With `rebirth: false` it publishes nothing.
  - **browse:** it lists group, node and device entries, the metrics of the births with their suggested types, `unsupported:DataSet` for a DataSet metric, and `Malformed` for a garbage payload.
  - **browse, rebirth:** with `rebirth: true` it publishes one NCMD per node seen.
  - **browse, filter default:** `spBv1.0/#` under `sparkplug_b`.
  - **cmd output:** the printed lines match the formats above. Wait times stay as plan 1's follow-up set them.
- [x] **Step 2: Implement.**
- [x] **Step 3: Run the checks; commit** `feat(mqtt): probe and browse Sparkplug B inputs`.

### Task 6: End to end, example, documentation, closing

**Files:**
- modify `internal/e2e/mqtt_test.go`;
- add a second input, `format: sparkplug_b`, to `examples/mqtt/config.yaml` on the same broker (its default client id differs by input id), so `TestMQTTExampleValidates` in `cmd/laraplate-edge/mqtt_test.go` covers it;
- modify `docs/CONFIGURATION.md`, `docs/USAGE.md`, the spec, this plan, `docs/superpowers/plans/INDEX.md` and `docs/superpowers/specs/INDEX.md`.

- [ ] **Step 1: End to end.** Run the real binary against a broker started by the test, with a `sparkplug_b` input whose points cover a node and a device. The test publishes NBIRTH, DBIRTH, NDATA by alias, DDEATH and NDEATH. The backend fake receives:
  - births with the declared data types;
  - the samples;
  - the device death and then the node death.

  Run with `-tags e2e -race`.
- [ ] **Step 2: Example.** The `sparkplug_b` input has a node point and a device point, and the header comment shows how to `browse` it; `TestMQTTExampleValidates` passes.
- [ ] **Step 3: Documentation.**
  - `CONFIGURATION.md`, "The `mqtt` driver":
    - `format: sparkplug_b`, the address form, the S2 rules and `rebirth`;
    - what is refused with the format;
    - the datatype to point type table of S14.
  - `USAGE.md`:
    - Sparkplug in "MQTT brokers": births and deaths, `bdSeq`, aliases, `seq`, and why a late start shows `unknown_alias`;
    - `probe` and `browse` output;
    - the new kinds in Metrics: `unknown_alias`, `sequence_gap` and `malformed_payload`;
    - Troubleshooting:
      - no samples after an agent restart (aliases: enable `rebirth` or restart the node);
      - devices dying after an NDEATH;
      - a will with an old `bdSeq`.
- [ ] **Step 4: Spec amendments**, dated 2026-11-17 and naming this plan:
  - section 6 gains `malformed_payload` (S6);
  - section 7 gains the S3 rule that a Sparkplug death holds until the next birth, the S2 refusals, the node filter of S1b, the reset of `seq` at reconnection, and probe and browse publishing a rebirth only with `rebirth: true`;
  - the status line says plan 2 is delivered.
- [ ] **Step 5: Close.**
  - Tick every box.
  - Set `status: closed`, with a `## Delivery status (date)` section that holds every divergence from this plan and the rulings of the execution.
  - Add the `**Documented in:**` line.
  - Update both INDEX lines.
- [ ] **Step 6: Run the checks, including `go test -tags e2e -race ./internal/e2e/...`; commit** `docs(mqtt): document Sparkplug B and close plan 2`.
