# Transforms, Modbus and OPC UA drivers: design

**Status:** Approved (2026-10-19), section by section. Not built. Three implementation plans follow, in order:
transforms, `modbus`, `opcua` (section 7).

**Date:** 2026-10-19

**Parent spec:** `../../../../docs/superpowers/specs/2026-10-12-laraplate-edge-agent-design.md` (stack root),
sections 7.1, 7.2, 7.3 (`config test` only), 9.1, 9.2, 10.3. This document settles what the parent leaves open
for the level-1 transform catalogue and the first two protocol drivers; where the two disagree, this one is
newer and wins for these subjects.

**Builds on:** the delivered agent core, `../plans/2026-10-16-agent-core.md` (input interface and contract,
supervisor, router, configuration document, status endpoint).

---

## 1. Purpose and success

The core reads only the `simulated` driver and refuses transforms. This step makes the agent useful on real
plant protocols: values decoded by OPC UA and Modbus drivers, cleaned by the full level-1 transform catalogue.

No real machine is available. Success is measured against simulated servers: each driver works against a
server started inside the tests (unit tests and an end-to-end test with the real binary), in CI, with no
hardware.

## 2. Decisions

| # | Topic | Decision |
|---|---|---|
| D1 | Where transforms run | A processor per input instance, between the driver and the router (approach 1 of the brainstorming). Drivers emit decoded, typed samples (parent 7.1); every driver and future plugin gets transforms without doing anything. |
| D2 | Catalogue | The whole level-1 catalogue of parent 7.2, in this step. CEL derived signals (7.3) stay out. |
| D3 | Libraries | `github.com/simonvetter/modbus` v1.6.4 (MIT; brings `github.com/goburrow/serial`, MIT) and `github.com/gopcua/opcua` v0.9.1 (MIT; brings `google/uuid`, `golang.org/x/exp`, `golang.org/x/term`, BSD, and `testify`, MIT, test-only). Both are pure Go and ship a server usable in tests. Licences read from the module files on 2026-10-19; each adoption is approved by the user in its plan. |
| D4 | Delivery | One spec, three plans in order: transforms, `modbus`, `opcua`. |
| D5 | Birth data type | The `data_type` of a birth comes from the output type of the point's transform chain, no longer from the point's decoding `type`. |
| D6 | Network drivers start | `Start` opens local resources only (serial port, certificates) synchronously and connects to the machine in the background, so it returns at once; an unreachable machine is death after `death_tolerance`, never a failing `Start` (core ruling R47). This also removes, for these drivers, the core's open issue of an abandoned start closing an exclusive resource while the rollback reopens it. |

## 3. Transform engine

**Package** `internal/transform`. The supervisor wraps each input instance's `emit` with a processor built
from the input's configuration. Device events pass through unchanged. The processor owns one goroutine, stopped
with the input, that serves the time-driven transforms on the injected clock.

**Types.** Values keep the four kinds of `model.Value`: boolean, integer, float, string. Each transform declares
the kind it accepts and the kind it produces. The chain is type-checked when a document is applied, starting from
the kind of the point's decoding `type` (`bool`, integer types, `float32`/`float64`, `string`); an error refuses
the document with a message naming the input, device, signal and step. "Number" below means integer or float.

**State.** Per device, signal and chain position. All state starts clean after a restart except `edge_counter`,
written atomically to `data_dir/inputs/{input_id}/transform-state.json` on every increment (pulses are slow and a
production counter must not lose parts).

**Quality.** A `bad` sample passes through the chain without being computed (parent 7.2). A runtime error of a
transform (an invalid BCD digit, a regex with no match, a failed conversion, a missing `map` key with no
default) produces a `bad` sample of the step's output kind, with the zero value, and increments
`laraplate_edge_transform_errors_total{input}`. It never stalls the input.

**Configuration.** Each step is an object with one key naming the transform (`{ scale: 0.1 }`). The published
JSON Schema gains the exact parameters of every transform, so `config validate` checks them offline.
`config.CheckBuild` stops refusing transforms; it still refuses derived signals and plugins.

## 4. Catalogue

| Transform | Parameter | Accepts → produces | Behaviour |
|---|---|---|---|
| `scale` | number | number → float | multiplies |
| `offset` | number | number → float | adds |
| `round` | integer 0–10 (decimals) | float → float; an integer passes unchanged | rounds half away from zero |
| `bit` | integer 0–63 | integer → boolean | extracts one bit |
| `bcd` | `true` | integer → integer | decodes BCD; a nibble above 9 is a runtime error |
| `map` | `{table: {key: value}, default?}` | any → kind of the table values | the input rendered as text is the key; every value and the default share one kind; a missing key with no default is a runtime error |
| `valid_range` | `{min?, max?}` (at least one) | number → number | out of range: quality `bad`, value kept |
| `deadband` | number (absolute) or `"N%"` (of the last sent value) | number → number | sends when the change from the last sent value reaches the threshold or the quality changes; the first value always passes |
| `heartbeat` | duration | any → same | resends the last value with the current time when nothing left the step for the duration |
| `min_interval` | duration | any → same | at most one sample per interval: the latest of the window, with its own timestamp, at the window's end |
| `debounce` | duration | any → same | sends a value only after it stayed unchanged for the duration, with the timestamp of the change |
| `edge_counter` | `rising` \| `falling` \| `both` | boolean → integer | cumulative count of edges, persisted (section 3) |
| `derivative` | `true` | number → float | change per second over the sample timestamps; the first sample and a backward jump (totaliser reset) produce nothing |
| `trim` | `true` | string → string | removes surrounding white space |
| `regex_extract` | `{pattern, group?, as?}` | string → string, integer or float | RE2 (linear time); `group` defaults to 1; `as` (`string`, `integer`, `float`) converts the capture; no match or a failed conversion is a runtime error |

`as` in `regex_extract` and the missing-key rule of `map` are additions to the parent: without `as` a reading
taken from a serial gauge would stay text. The catalogue only grows: an existing transform never changes
behaviour; a variant takes a new name (parent 7.2).

## 5. Modbus driver

Read-only (parent 9.1).

**Connection** (`connection` of the input):

| Field | Values |
|---|---|
| `transport` | `tcp`, `rtu`, `rtuovertcp` (RTU framing over TCP, for serial-to-Ethernet gateways and for testing RTU without a port) |
| `host`, `port` | `tcp` and `rtuovertcp`; port 502 by default |
| `device`, `baud`, `data_bits` (8), `parity` (`none`, `even`, `odd`), `stop_bits` (1, 2) | `rtu` |
| `timeout` | duration, default 1s |
| `unit_id` | default unit of the input's points, default 1 |
| `rate_limit` | requests per second, default 20; one request in flight per connection |

**Point address:** `<area>:<offset>[#<registers>][@<unit>]`, with areas `hr` (holding registers), `ir` (input
registers), `co` (coils), `di` (discrete inputs) and offsets 0–65535. `#n` is required for `string` only (its length in
registers) and refused otherwise; `@unit` overrides the input's unit id, so one RTU bus serves several units.
The documentation maps the legacy notation (`40011` is `hr:10`).

**Types and order (parent 7.1):** `bool` on `co`/`di` (a bit of a register uses the `bit` transform);
`int16`/`uint16` take 1 register, `int32`/`uint32`/`float32` 2, `int64`/`uint64`/`float64` 4, `string` `n`.
`byte_order` is `ABCD` (default), `CDAB`, `BADC` or `DCBA`. A type that does not fit its area refuses the
document.

**Reading:** `poll` only; `subscribe` and `event` refuse the document. Points with the same unit, area and
interval are grouped into blocks of strictly contiguous addresses, at most 125 registers or 2000 bits per
request; gaps are never read, since an unmapped address fails the whole block. The timestamp is the agent clock
at read time.

**Errors to quality:**

| Outcome | Result |
|---|---|
| Exceptions 1–4 (illegal function, address or value; device failure) | `bad` samples for the block's points |
| Exceptions 5, 6 (acknowledge, busy) | retried at the next poll, no sample |
| Timeout, connection lost, exceptions 10, 11 (gateway path or target unavailable) | no sample; reconnection with backoff 1 s to 1 min; death after `death_tolerance`, birth on recovery |

## 6. OPC UA driver

Read-only.

**Connection:** `endpoint` (`opc.tcp://…`); `security_policy` `None`, `Basic256Sha256` (default),
`Aes128_Sha256_RsaOaep`, `Aes256_Sha256_RsaPss`; `security_mode` `Sign` or `SignAndEncrypt` (default); `auth`
anonymous or `{user, password: secret://…}`; `publishing_interval` (default 1s). `None` is allowed for
simulators and closed networks and logs a WARN at every start that the connection is unprotected.

**Certificates (trusted explicitly, never on first use, parent 9.2):**

- The agent's certificate is generated at the first start of an `opcua` input in `data_dir/opcua/pki/own/`:
  RSA 2048, application URI `urn:laraplate-edge:<agent_id>`, private key mode `0600`.
- Trusted server certificates live in `data_dir/opcua/pki/trusted/`. An unknown server certificate is refused:
  the input logs its thumbprint and the command to trust it, keeps retrying, and gets death after
  `death_tolerance`.
- `laraplate-edge opcua trust <input>` connects, shows subject, thumbprint and expiry and asks for
  confirmation; `--thumbprint <hex>` confirms without a prompt for scripted installs; `opcua trust --list`
  lists the trusted certificates.

**Points:** the address is a NodeId (`ns=3;s=Machine.State`, or `nsu=<namespace URI>;s=…`, stable when the
server renumbers its namespaces).

- `read: subscribe` creates a monitored item. The long form `read: {subscribe: {sampling, queue, deadband}}`
  sets the sampling interval, the queue (default 10, discard oldest) and a server-side deadband (absolute or
  `N%`).
- `read: {poll: 1s}` uses the Read service, in requests within the server's `MaxNodesPerRead`.
- Timestamp: `SourceTimestamp`, else `ServerTimestamp`, else the agent clock.
- Quality: the status code's Good, Uncertain and Bad classes give `good`, `uncertain`, `bad`.
- The value is converted to the point's `type` when the conversion loses nothing (integer widths, float32 to
  float64); otherwise the sample is `bad` and the transform error metric does not apply (a read error is
  counted in `laraplate_edge_input_read_errors_total`).

**Reconnection:** our own loop, backoff 1 s to 1 min, recreating the session and the subscriptions (parent 9.2);
death and birth through the lifecycle helper.

**`browse`:** `laraplate-edge browse <input> [--node <NodeId>] [--depth 2]` lists NodeId, browse name, data
type and access level.

## 7. Common parts, commands, tests, plans

**Driver kit** (`internal/input/driverkit`): reconnection backoff (1 s to 1 min with jitter, injected clock),
request rate limiter, the wiring of the core's lifecycle helper (`death_tolerance`), and the metrics
`laraplate_edge_input_connected{input}` (parent 9.1, "the connection state is a metric") and
`laraplate_edge_input_read_errors_total{input,kind}`.

**Commands:**

- `laraplate-edge probe <input> [--record samples.jsonl]`: reads every point once and shows value, type,
  quality and response time; works for every driver, `simulated` included; `--record` writes the samples.
- `laraplate-edge config test <file> --input samples.jsonl`: replays recorded samples through the transforms,
  deterministically and without a machine (parent 7.3).
- `browse` and `opcua trust` come with the OPC UA plan.

**Tests:** unit tests on the fake clock; for each driver, the library's server (OPC UA, Modbus TCP) or a small
test server (RTU over TCP) started in process; one end-to-end test per driver with the real binary pointed at a
server started by the test. A real serial port cannot be exercised in CI; this is a declared limit.

**Plans, in order:**

1. **Transforms:** processor, catalogue, schema parameters, `CheckBuild`, birth data type from the chain,
   persisted `edge_counter`, `config test`, `probe` (on `simulated`), the transform error metric.
2. **`modbus`:** driver kit and driver (TCP, RTU, RTU over TCP).
3. **`opcua`:** certificates and trust, subscriptions and poll, `browse`.

Each plan closes with its documentation in `docs/CONFIGURATION.md` and `docs/USAGE.md`.

## 8. Out of scope

CEL derived signals (parent 7.3); the `mqtt`, `serial` and `file` drivers; the MQTT output; plugins; a custom
CA bundle for the HTTP output; OPC UA user-certificate authentication and Alarms & Events; Modbus over TLS.
