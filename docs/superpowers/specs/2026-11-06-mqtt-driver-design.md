# MQTT driver: design

**Status:** Approved (2026-11-06), section by section. Plan 1 (`mqtt` with JSON and plain payloads, `../plans/2026-11-06-mqtt-json.md`)
was delivered on 2026-11-06. Plan 2 (Sparkplug B, sections 7 and the rest of 8, `../plans/2026-11-17-mqtt-sparkplug-b.md`)
was delivered on 2026-11-17.

**Date:** 2026-11-06

**Parent spec:** `../../../../docs/superpowers/specs/2026-10-12-laraplate-edge-agent-design.md` (stack root),
sections 3, 9.1, 9.2 (`mqtt` row and commissioning). This document settles what the parent leaves open for the
`mqtt` input driver; where the two disagree, this one is newer and wins for this subject.

**Builds on:** `2026-10-19-transforms-modbus-opcua-design.md` (driver kit, D6 non-blocking start, `probe`,
`death_tolerance` wiring) and the delivered `modbus` and `opcua` drivers.

---

## 1. Purpose and success

Third-party devices and gateways (Node-RED, Kepware IoT Gateway, Shelly, Tasmota, Sparkplug B edge nodes)
publish machine data to an MQTT broker. The `mqtt` driver subscribes to that broker and turns the messages into
the agent's typed samples, with birth and death per device. The agent is a plain subscriber, never a Sparkplug
primary host.

No real device is available. Success is measured against a broker started inside the tests (unit tests,
integration tests and one end-to-end test per payload format with the real binary), in CI, with no hardware.

## 2. Decisions

| # | Topic | Decision |
|---|---|---|
| M1 | Scope and delivery | One spec, two plans in order: `mqtt` with JSON and plain payloads, then Sparkplug B on the same base. |
| M2 | Client library | `github.com/eclipse/paho.mqtt.golang` v1.5.1, MQTT 3.1.1, dual-licensed `EPL-2.0 or BSD-3-Clause` (its `NOTICE.md`); the agent takes it under BSD-3-Clause, which meets the parent's permissive-only rule (section 3). It brings `gorilla/websocket`, `golang.org/x/net`, `golang.org/x/sync`. MQTT 3.1.1 works with every broker, those built into gateways and PLCs included, and Sparkplug B 3.0 runs on it. The same client will serve the `laraplate-mqtt` output. Licence read from the module files on 2026-11-06; adoption approved by the user in plan 1. |
| M3 | Test broker | `github.com/mochi-mqtt/server/v2` v2.7.9 (MIT), in process, test-only (parent: test tools are not distributed, so the licence rule does not apply to them). |
| M4 | Payloads | `json` (values read through dot paths) and plain (the whole payload is the value, as text); Sparkplug B in plan 2. |
| M5 | Death | The broker connection missing beyond `death_tolerance`, plus an optional availability topic per device. Silence alone never kills a device: MQTT devices often publish on change only. |
| M6 | Timestamp | An optional path and format in the payload, else the agent clock at reception. |
| M7 | Sparkplug B decoding | By hand with `google.golang.org/protobuf/encoding/protowire` (already a dependency, v1.36.12), field numbers from the Sparkplug 3.0 specification. Eclipse Tahu's `sparkplug_b.proto` is `EPL-2.0` only (checked 2026-11-06), so it is not copied and no code is generated from it. |
| M8 | Rebirth | Off by default. When enabled, the only write the driver ever makes is a Sparkplug rebirth request. |

## 3. Connection

`connection` of the input. An unknown field is an error named with its path, as for `modbus`.

| Field | Default | Meaning |
|---|---|---|
| `broker` | | required: `mqtt://host[:1883]` or `mqtts://host[:8883]` |
| `client_id` | `laraplate-edge-<agent_id>-<input_id>` | must be unique on the broker |
| `auth` | anonymous | `{user, password: secret://…}`, as for `opcua` |
| `tls` | system CA pool | `{ca_file?, cert_file?, key_file?, server_name?}`; mutual TLS needs `cert_file` and `key_file` together. There is no option to skip certificate verification. Refused on `mqtt://`. |
| `format` | `json` | `json` or `sparkplug_b` (plan 2). One input, one format: mixing them takes two inputs on the same broker. `json` covers plain payloads too (section 4). |
| `qos` | `1` | QoS of the subscriptions, 0 or 1 |
| `persistent_session` | `true` | clean session false in 3.1.1 terms: with QoS 1 the broker keeps the messages published while the agent is down, so a restart loses nothing |
| `keepalive` | `30s` | a positive duration |
| `max_payload` | `256KiB` | a larger message is dropped and counted as a read error |
| `timestamp` | reception | `{path, format}` for every topic of the input; `format` is `rfc3339`, `unix_s` or `unix_ms` |
| `topics` | | per-topic overrides, keyed by topic; only `timestamp` for now |
| `availability` | | `[{device, topic, online, offline}]`, at most one per device (section 5) |
| `rebirth` | `false` | `sparkplug_b` only (section 7) |

The `timestamp`, `topics` and `availability` fields are refused with `sparkplug_b`, and `rebirth` with `json`.

## 4. Points and messages (`json` format)

**Address:** `<topic>[|<path>]`.

- The topic is concrete: `+` and `#` are refused, since every sample belongs to one signal. A topic containing
  `|` cannot be addressed; the address splits at the first `|`.
- `path` is dot notation, as the backend's `mapped_json` reads with `data_get`: `a.b.0.c`, where a numeric segment
  indexes a list. A key containing `.` cannot be addressed.
- Without a path the value is the whole payload, read as plain text.

**Read:** `subscribe` only, short or long form (the long form's settings are ignored); `poll` and `event` refuse
the document. `type` is required, so the data type announced in the birth is deterministic.

**Subscriptions:** one per distinct topic. Several points on one topic with different paths produce several
samples from one message, with the same timestamp.

**Retained messages** are ordinary samples and give the initial value at once. With the reception timestamp an
old retained value takes the current time; the documentation says so.

**Conversion to the point's `type`**, lossless or `bad`:

| Value | Result |
|---|---|
| JSON number | integer types: only when it is integral and fits the type, else `bad`; `float32`/`float64`: as is |
| JSON boolean | `bool` |
| JSON string | `string` |
| Plain payload | parsed by `type`: `bool` accepts `true`/`false`, `1`/`0`, `on`/`off`, case-insensitive; numbers by the JSON number grammar with one leading `+` allowed, with the same integer rule; `string` is the text as is (amended 2026-11-17 by the follow-up on branch `fix/mqtt-known-limits`: "the standard parser" also read hex floats, `inf`, `nan` and underscores, now `conversion`; a non-zero value that `float32` turns into 0 is `conversion` too) |
| Anything else (a JSON value of another kind, an object or list at the path) | `bad`, read error kind `conversion` |

**Message outcomes:**

| Case | Result |
|---|---|
| The path is missing from the message | no sample for that point (devices send partial updates) |
| Malformed JSON | `bad` samples for the points of that topic that have a path; kind `malformed_json` |
| Unreadable timestamp | `bad` samples for the points of that topic; kind `bad_timestamp` |
| Payload above `max_payload` | dropped, no sample; kind `payload_too_large` |
| A message on a topic no point subscribes to | dropped; kind `unmapped` (happens with persistent sessions after a configuration change) |

## 5. Lifecycle

**Start (D6 of the transforms design):** `Start` reads the TLS files and returns at once; the connection to the
broker runs in the background. An unreachable broker is death after `death_tolerance`, never a failing `Start`.

**Reconnection:** our own loop with the driver kit's backoff (1 s to 1 min with jitter, injected clock); paho's
automatic reconnection is off. The subscriptions are made again on every connection, also with a persistent
session, since a broker may have lost it. `laraplate_edge_input_connected{input}` follows the connection, from
the broker's answer to the subscriptions on.

**Refused subscription** (amended 2026-11-17 by the follow-up on branch `fix/mqtt-known-limits`; before it, a
refusal failed the connection, which was retried forever and shown only as `connection` errors): a topic the
broker refuses (SUBACK `0x80`) keeps the connection and the other topics. It is logged at warn once per
connection, naming the topic and the input, and counted with kind `subscribe_refused` once per point on it. A
device whose points are all on refused topics is not kept alive, so it dies after `death_tolerance`. The topic is
left out of the subscription set unless the previous set held it, and subscribed again at the next connection (a reconnection, a reload or a
restart); there is no retry timer.

**Death and birth:**

- The broker disconnected beyond `death_tolerance`: death for every device of the input.
- A device with `availability`: the `offline` payload is its death at once, for that device only. The `online`
  payload, or its first sample if no `online` came (a Last Will that is not retained is seen only when it changes),
  is its birth.
- Every device not held dead by its availability topic is alive while the broker is connected: the input renews it on
  connection and then every half `death_tolerance`, so silence never kills it and the reconnection gives its birth at
  once (amended 2026-11-06 by plan 1, decision Q2: the core's lifecycle kills a device without reads).
- An availability payload that is neither `online` nor `offline` is ignored and counted with kind `availability`.

**Configuration changes:** stopping an input for a reload disconnects without unsubscribing, so the persistent
session keeps the messages. At restart the input subscribes to the topics of the new document and unsubscribes
from those it no longer uses (the previous set is kept in `data_dir/mqtt/topics/{input_id}.json`, outside the
input's state directory that the supervisor deletes on a rollback; amended 2026-11-06 by plan 1's final review,
decision Q7).

## 6. Commands and metrics

**Client id of the commands:** `probe` and `browse` connect as `client_id` + `-probe` or `-browse`, with a clean
session and QoS 0. Reusing the agent's id would make the broker disconnect the running agent and take its
persistent session.

**`probe <input>`:** subscribes to the points' topics and waits for the first sample of each point up to the
command's timeout; retained messages often answer at once. It shows value, type, quality and arrival time, or
"no message". (Amended 2026-11-17 by the follow-up on branch `fix/mqtt-known-limits`: a `bad` sample ends with
the read error kind that made it bad, and a point without a sample shows the known reason instead of "no
message": `payload_too_large`, `no value at path` or `subscribe_refused`. The points of a refused topic do not
hold the probe. The arrival time is still not shown, R9 of plan 1.)

**`browse <input> [--filter <topic filter>] [--duration 10s]`:** subscribes to the filter (`#` by default) and, for
the duration, lists the observed topics with message count, retained flag and size. For JSON payloads it lists
the leaf paths with a value preview, already in the `topic|path` form to copy into the document; for plain
payloads a preview of the text. (Amended 2026-11-17 by plan 2: under `sparkplug_b` the default filter is
`spBv1.0/#` and the output is one entry per group, node and device seen, see section 7.)

**Metrics:** those of the driver kit, `laraplate_edge_input_connected{input}` and
`laraplate_edge_input_read_errors_total{input,kind}`, with the kinds `payload_too_large`, `malformed_json`,
`bad_timestamp`, `conversion`, `availability`, `unmapped` and `subscribe_refused` (added 2026-11-17 by the
follow-up on branch `fix/mqtt-known-limits`); plan 2 adds `unknown_alias` and `sequence_gap`, and, amended
2026-11-17 by plan 2 (`../plans/2026-11-17-mqtt-sparkplug-b.md`, decision S6), `malformed_payload`: a payload that
`protowire` cannot parse is dropped and counted once per message. The spec listed no kind for it. A metric whose
value field does not match its datatype is a `bad` sample with kind `conversion`, as before. Under `sparkplug_b`,
`sequence_gap` and `malformed_payload` are counted even when the message comes from a device no point addresses,
since they describe the stream of the node.

## 7. Sparkplug B (plan 2)

**Amendments of 2026-11-17** (plan 2, `../plans/2026-11-17-mqtt-sparkplug-b.md`, decisions S1b, S2, S3, S6, S10,
S13 and S14 and the rulings of its execution). They refine what follows, which is kept as first written:

- **Subscriptions (S1b, amended by ruling R3).** The two filters below were refined twice. The first
  amendment added the node filter for every target, since a device alone would miss the NBIRTH (`bdSeq`, aliases)
  and NDEATH of its node. The second replaced the filters with one, `spBv1.0/<group>/+/<edge_node>/#`, per edge
  node of every target: it takes the node and all its devices, because `seq` is one counter shared by a node and all its devices. The messages of a
  device no point addresses are handled silently (sequence, aliases), give no samples and are not `unmapped`; a
  node only devices address is still a known node. The cost is more broker traffic for a node with many unused
  devices. A message of a node no point addresses, or off the Sparkplug topic shape, is `unmapped`.
- **One target per agent device (S2, with ruling R1).** Devices entries with one name are one agent device: all
  its points address one target, no metric twice (the check spans entries), and two agent devices may not share
  a target. Otherwise the document is refused with a fixed message naming the positions. `timestamp`, `topics` and
  `availability` are refused with `sparkplug_b`, and `rebirth` with `json`.
- **Death holds until the next birth (S3).** A Sparkplug death puts the device offline until its next birth: a data message does not revive it
  (unlike an availability topic), and the samples of a device held offline are dropped uncounted. A device with
  no birth seen since the agent started is alive while the broker is connected, as every `json` device is, since
  births are not retained. The tracker gained `Offline` and `Online` for this.
- **`seq` (S10, with ruling R4).** The tracking is reset at every reconnection, since a gap across an outage is
  expected; an NDEATH ends it; a message without `seq` ends the node's tracking until the next one. An NBIRTH
  clears the node's aliases and a DBIRTH overwrites those of its device; aliases are kept per node and device
  (ruling R6, equivalent for conforming nodes).
- **Rebirth (S11, S13, S14, rulings R7).** The request is an `NCMD` at QoS 0, not retained, built in production
  code; the running input logs a failed publish at debug, `probe` and `browse` at warn. `probe` and `browse`
  publish a rebirth, one per addressed node, only with `rebirth: true`; with `false` nothing is published.
  `probe` shows `unknown_alias`, `malformed_payload`, `payload_too_large` or `subscribe_refused` for a point with
  no sample.
- **Float and Double (ruling R9).** A whole, in-range Float or Double also converts to an integer point type, a fraction or
  an out-of-range value is `conversion`: it is a typed binary number, so the value is lossless. JSON text `3.0` stays refused
  for an integer type (plan 1, Q4).
- **`browse` output (S14).** Default filter `spBv1.0/#`. One line per group, node and device seen,
  `<group>/<node>[/<device>]  <n> msg  born|-` with `  <m> malformed` when there were malformed payloads,
  followed by indented `  <address>  <type>` lines from the births, with the suggested point type (Int8 and Int16
  `int16`, Int32 `int32`, Int64 and DateTime `int64`, UInt8 and UInt16 `uint16`, UInt32 `uint32`, UInt64 `uint64`,
  Float `float32`, Double `float64`, Boolean `bool`, String, Text and UUID `string`, else
  `unsupported:<DataType name>`).

**Address:** `<group>/<edge_node>[/<device>]|<metric>`. The driver subscribes to `spBv1.0/<group>/+/<edge_node>`
and `spBv1.0/<group>/+/<edge_node>/<device>` for what the points address. STATE, NCMD and DCMD are ignored. All
points of an agent device address the same edge node or Sparkplug device, so life and death map one to one;
otherwise the document is refused.

**Birth and death:**

- NBIRTH and DBIRTH are the `birth` of the mapped device and carry the current values, which become samples.
- NDEATH is the death of the node's device and of every Sparkplug device under that node. An NDEATH whose `bdSeq`
  differs from the last NBIRTH's is a stale Last Will and is ignored.
- DDEATH is the death of that device only.
- Losing the broker beyond `death_tolerance` works as for `json`.

**Aliases and sequence:**

- Aliases are learnt from the birth. A data message with an unknown alias (the agent started after the birth,
  which is never retained) is dropped with kind `unknown_alias`.
- A gap in `seq` is counted (`sequence_gap`) and the driver goes on.
- `rebirth: true` (default `false`): on an unknown alias or a sequence gap the agent publishes an NCMD
  `Node Control/Rebirth`, at most one every 30 s per node. It is the driver's only write and must be enabled
  explicitly, since the parent calls the agent a plain subscriber.

**Values:**

- The scalar types (integers, Float, Double, Boolean, String, Text, UUID, DateTime as an int64 in milliseconds)
  convert to the point's `type` with the lossless rules of section 4. DataSet, Template, Bytes, File and arrays give
  a `bad` sample with kind `conversion`.
- Timestamp: the metric's, else the payload's, else reception.
- Quality: `is_null` is `bad`. A `Quality` property (the Ignition / OPC DA code) of 192 or more is `good`, 64 to 191
  `uncertain`, below 64 `bad`.
- `is_historical` is an ordinary sample with its own timestamp.

**`browse`** with this format lists groups, nodes, devices and, from the births seen in the window, the metric
names and types in the `…|metric` form.

## 8. Tests and plans

**Tests:**

- Unit tests on the fake clock: address and path parsing, conversions, timestamps, availability; for Sparkplug,
  the protowire decoder, aliases, `seq` and `bdSeq`.
- Integration tests on mochi in process, TLS and authentication included: reconnection and death after the
  tolerance; a persistent session delivering the messages published while the input was stopped; after a
  configuration change, the topics no longer used are unsubscribed.
- One end-to-end test per format with the real binary pointed at a broker started by the test.
- Sparkplug payloads in tests come from a minimal encoder in test files only.

**Plans, in order:**

1. **`mqtt` with JSON and plain payloads:** connection and TLS, addresses, subscriptions, conversions,
   timestamps, availability, `probe`, `browse`, the validation of the connection by the driver's factory (the schema
   keeps `connection` open, as for `modbus`), `CheckBuild` no longer refusing `mqtt`. The adoption of paho (under BSD-3-Clause) and mochi is approved by the user in this plan.
2. **Sparkplug B:** the format, the decoder, birth and death, aliases, `seq` and `bdSeq`, `rebirth`, `browse` for
   Sparkplug.

Each plan closes with its documentation in `docs/CONFIGURATION.md` and `docs/USAGE.md` and an example in
`examples/mqtt`.

## 9. Out of scope

The `laraplate-mqtt` output (it will reuse the client); MQTT 5 and websockets; the Sparkplug primary host role and
STATE messages; wildcards in point addresses; writing to devices, beyond the optional rebirth; the `serial` and
`file` drivers.
