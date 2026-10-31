# Configuration document `laraplate-edge-config/1`

The configuration document says what the agent reads (inputs), what it derives from them (processors),
where it sends (outputs) and how much it may buffer. It comes from a local file or from Laraplate (see the providers in [USAGE.md](USAGE.md)).
Its JSON Schema, draft 2020-12, is [`schema/laraplate-edge-config-1.schema.json`](../schema/laraplate-edge-config-1.schema.json),
embedded in the binary.

The schema describes the full format of the design. This build provides only part of it and refuses the
rest with `not available in this build` (see "What this build refuses"). Check a document with:

```bash
laraplate-edge config validate config.yaml
```

## Format and validation

- JSON, or YAML for hand-written files. A document whose first significant character is `{` or `[` is
  read as JSON, so a YAML document must use block style at the top level (flow style `{...}` only
  below it). At most 4 MiB.
- Validation has two layers, run by `config validate` and before every apply: the schema plus the rules
  a schema cannot express (unique ids, durations and sizes in range, the URL rule), then what this build
  provides, with the type check of every transform chain (see "Transforms"). Every problem is reported
  with its path (`outputs.0.batch.max_samples: must be at most 5000`), never with its value.
- An unknown key is an error everywhere but inside `connection` and `config`.
- Durations are positive, written as `500ms`, `2s`, `1m`, `1h30m`. Sizes are positive, written as
  `512KiB`, `16MiB`, `1GiB` (or `B`).
- Ids (`inputs[].id`, `outputs[].id`, `plugins[].id`, `processors[].id`) match `^[a-z0-9][a-z0-9._-]{0,63}$` and are
  unique in their list. An output id names its buffer directory.

## Top level

| Key | Required | Meaning |
|---|---|---|
| `schema` | yes | `laraplate-edge-config/1` |
| `version` | no | integer from 1. A document older than the applied one is refused; a missing version counts as 0. Laraplate writes the published version into the document it serves. |
| `inputs` | yes | list of inputs, may be empty |
| `outputs` | yes | list of outputs, may be empty |
| `plugins` | no | external plugin processes, see [Plugins](#plugins) |
| `processors` | no | plugin components deriving signals from the devices of the inputs, see [Processors](#processors) |

## Inputs

| Key | Required | Default | Meaning |
|---|---|---|---|
| `id` | yes | | input id |
| `driver` | yes | | `simulated`, `modbus`, `opcua` or `plugin:<id>` in this build. The schema also knows `mqtt`, `serial` and `file`. |
| `connection` | no | `{}` | driver settings, an object the schema does not interpret (see the secret rule below) |
| `death_tolerance` | no | `30s` | how long without a successful read before the input reports its devices dead; `birth` follows at the next successful read |
| `devices` | yes | | at least one device |

A **device** has `device` (its name in the messages: 1 to 128 characters, no `/`), `points` (at least
one) and `derived` (refused by this build). The pair (device, signal) is unique across all inputs.

A **point** is one signal read from the machine:

| Key | Required | Default | Meaning |
|---|---|---|---|
| `signal` | yes | | signal name: 1 to 160 characters, no `/` |
| `address` | yes | | driver-specific address of the value |
| `read` | yes | | `subscribe`, `event`, `{poll: <duration>}`, or `{subscribe: {sampling, queue, deadband}}`, the long form of `subscribe` whose settings only the `opcua` driver uses (the others ignore them). What each driver accepts is in its section. |
| `priority` | no | `critical` | buffer class: `critical` or `bulk` (see "Buffer") |
| `type` | no | | decoding type: `bool`, `int16`, `uint16`, `int32`, `uint32`, `int64`, `uint64`, `float32`, `float64`, `string`. It sets the kind of the values the driver produces (see "Transforms") and, for a point without transforms, the data type announced in the device's `birth`: `bool` is `boolean`, `string` is `string`, any other type (or none) is `number`. A point with transforms announces the data type of what its chain produces. |
| `byte_order` | no | | `ABCD`, `CDAB`, `BADC`, `DCBA` (drivers that decode registers) |
| `unit` | no | | engineering unit, at most 16 characters, announced in the `birth` |
| `transforms` | no | | the transform chain, applied in order to every sample of the point (see "Transforms") |

### The `simulated` driver

A deterministic machine simulator for demos, development and tests. Its `connection`:

| Key | Meaning |
|---|---|
| `scenario` | the scenario, inline (an object) |
| `scenario_file` | the path of a YAML or JSON scenario file, absolute or relative to the directory of the configuration file. With the `laraplate` provider there is no such directory: use `scenario` or an absolute path. |
| `seed` | integer, default `1` |

Exactly one of `scenario` and `scenario_file`. Every device of the input runs its own instance of the
scenario, seeded from `seed` and the device name, so a run is repeatable. A point's `address` names a
scenario signal: `state` (a string: the point's `type` must be `string`), `good_count`, `scrap_count`
(cumulative integers) or a measurement (a number); those three need a numeric `type` or none. The driver
ignores `read`, `byte_order` and `death_tolerance`: it emits the initial state and both counters at 0 at
start, then, at each tick, what changed. Qualities are always `good`.

The scenario (at most 1 MiB, unknown keys refused; see `examples/simulated/scenario.yaml`):

| Key | Default | Meaning |
|---|---|---|
| `tick` | `1s` | simulation step, from `1ms` to `1h` |
| `initial` | | the state the machine starts in |
| `states` | | at least one `{name, producing, transitions}`. `transitions` maps another state to its probability per tick (0 to 1, summing to at most 1; the rest stays). A part is made only in a `producing` state, and any change of state abandons the part in progress. |
| `cycle_time` | | `{mean, stddev}`, durations; `mean` is required when a state produces, from the tick to 24 h. A part completes when its drawn cycle time has elapsed, at most one per tick. |
| `scrap_rate` | `0` | probability, 0 to 1, that a part is scrap (`scrap_count`), else good (`good_count`) |
| `measurements` | | `{signal, mean, stddev, unit}` drawn once per completed part; the name is not `state`, `good_count` or `scrap_count`; `unit` is informative (the point's `unit` is reported) |

### The `modbus` driver

Reads coils, discrete inputs, input registers and holding registers from a Modbus device or gateway, by
polling. The driver is read-only: it sends only the four read functions (1, 2, 3, 4), never a write.
`examples/modbus` is a complete document.

**Connection.** An unknown field is an error here, named with its path (`connection.<key>: unknown field`).

| Key | Default | Meaning |
|---|---|---|
| `transport` | | required: `tcp` (Modbus TCP), `rtu` (RTU framing on a serial port) or `rtuovertcp` (RTU framing over TCP, for serial-to-Ethernet gateways) |
| `host` | | `tcp` and `rtuovertcp`: required, host name or address of the device |
| `port` | `502` | `tcp` and `rtuovertcp`: 1 to 65535 |
| `device` | | `rtu`: required, the serial port path (`/dev/ttyUSB0`, `COM3`) |
| `baud` | | `rtu`: required, no default |
| `data_bits` | `8` | `rtu`: 5, 6, 7 or 8 |
| `parity` | `none` | `rtu`: `none`, `even` or `odd` |
| `stop_bits` | `1` | `rtu`: 1 or 2 |
| `timeout` | `1s` | how long one request may take, a positive duration |
| `unit_id` | `1` | the unit (slave id) the points are read from, 0 to 255; a point may override it (`@unit`). Unit 0 is accepted: some TCP devices answer on it. On an RTU bus it is the broadcast address, and a read of it gets no answer. |
| `rate_limit` | `20` | requests per second, a positive integer |

The serial fields are refused on `tcp` and `rtuovertcp` (`only for rtu`), and `host` and `port` on `rtu`.
There is one connection per input and one request in flight on it; the driver sets the unit before each
request. Put every device of a serial line in one input: a serial port belongs to one input. Stopping an
input, for a reload or a shutdown, waits for its request in flight, so a very long `timeout` delays a
reload by as much.

**Points.** `read` must be `{poll: <duration>}`: `subscribe` and `event` are refused. `type` is required.
`unit` and `priority` work as for any driver. The `address` is
`<area>:<offset>[#<registers>][@<unit>]`:

| Part | Meaning |
|---|---|
| `<area>` | `hr` holding registers (function 3), `ir` input registers (4), `co` coils (1), `di` discrete inputs (2) |
| `<offset>` | 0 to 65535, **zero-based**: the offset is what travels on the wire |
| `#<registers>` | the length of a `string` in registers, 1 to 125; only for `string`, and required there |
| `@<unit>` | 0 to 255, overrides the connection's `unit_id` for this point |

Documentation and vendor tools often use the legacy five-digit notation, which is one-based and carries
the area in its first digit. Convert it by dropping that digit and subtracting 1: `40011` is `hr:10`,
`30001` is `ir:0`, `00001` is `co:0`, `10001` is `di:0`. A device manual that already gives the offset
"on the wire" or "protocol address" gives the number to use as is. The driver does not accept the legacy
form (`address: want <area>:<offset>[#<registers>][@<unit>]`).

**Types.** The point's `type` fixes the registers it occupies; the point must fit before offset 65535.

| `type` | Area | Registers | Value |
|---|---|---|---|
| `bool` | `co`, `di` only | 1 bit | boolean |
| `int16`, `uint16` | `hr`, `ir` | 1 | integer |
| `int32`, `uint32` | `hr`, `ir` | 2 | integer |
| `float32` | `hr`, `ir` | 2 | float |
| `int64`, `uint64` | `hr`, `ir` | 4 | integer |
| `float64` | `hr`, `ir` | 4 | float |
| `string` | `hr`, `ir` | the `#n` of the address | string |

A signed type sign-extends (`0xFFFB` as `int16` is -5). A `uint64` above 9223372036854775807 cannot be
carried (integer values are 64-bit signed) and a float that is not finite (NaN, infinity) is no value:
each is a `bad` sample (read error kind `decode`). A `string` is the register bytes, high byte first
within each register, up to the first NUL; invalid UTF-8 is a `bad` sample too. Its `byte_order` is
ignored.

**Byte order.** `byte_order` is `ABCD` (the default), `CDAB`, `BADC` or `DCBA`. Read a 32-bit number as
its four bytes A (the most significant) to D; the name is the order in which the device sends them. It is
two independent swaps:

| `byte_order` | Bytes inside each register | Register order | Meaning |
|---|---|---|---|
| `ABCD` | as sent | high word first | big-endian, the Modbus default |
| `CDAB` | as sent | low word first | words swapped, common on PLCs |
| `BADC` | swapped | high word first | bytes swapped inside each word |
| `DCBA` | swapped | low word first | little-endian |

A 16-bit type is one register, so only the byte swap matters (`BADC` and `DCBA` swap its two bytes, `ABCD`
and `CDAB` keep them). For 64 bits, low word first reverses all four registers. If a value is wrong by
a huge factor or looks like noise, try the other orders: the `float32` 21.5 is the registers
`0x41AC 0x0000` in `ABCD` and `0x0000 0x41AC` in `CDAB`.

**Blocks.** The driver does not read point by point. The points of one input with the same unit, area and
`poll` interval are sorted by offset and merged into one request when their addresses are strictly
contiguous or overlap. A gap of even one address starts a new block, so an address no point names is never
read. A block holds at most 125 registers (`hr`, `ir`) or 2000 bits (`co`, `di`); a point that would
cross the limit starts the next block and a point is never split. Points that overlap with different types
(`float32` and its two `uint16`) share a block and are decoded from the same reply. Points with different
intervals are read by different requests, at their own interval.

Because a block is one request, one unmapped address fails the whole block: the device answers an
exception and every point of the block is `bad` (below). Keep the points around an address the device does
not map in separate blocks: leave a gap, or give them another interval.

**Errors.** What the driver does with each answer, and whether the device counts as alive for
`death_tolerance` (a successful exchange keeps it alive; `connected` and `read_errors` are in the health
below):

| Answer | Samples | Device | `read_errors` kind |
|---|---|---|---|
| values | `good` | alive | |
| value that does not decode (`uint64` above the range, non-finite float, invalid UTF-8 string) | `bad` for that point, the others `good` | alive | `decode` |
| exception 1, 2, 3, 4 or 8 (illegal function or address or value, device failure, parity error) | `bad` for every point of the block, carrying the zero of the point's kind (`false`, `""`, `0`) | alive: the device answered | `exception` |
| exception 5 or 6 (acknowledge, device busy) | none; the block is read again at its next poll | alive | `exception` |
| an exception code the driver does not know | none; the block is read again at its next poll | alive: the device answered | `exception` |
| exception 10 or 11 (a gateway cannot reach the device) | none; the connection is kept | counts toward death: no successful read | `exception` |
| no answer within `timeout` | none; the connection is kept and the next block is read on it | counts toward death | `timeout` |
| on `rtu` and `rtuovertcp`, a frame that is not valid (bad CRC, another unit, short frame, protocol error) | none; the frame is dropped (after a bad CRC, a short frame or a header that cannot be read, the line is flushed), and the connection is kept | counts toward death | `connection` |
| on `tcp`, an answer of another unit | none; the frame, read whole, is dropped and the connection is kept | counts toward death | `connection` |
| on `tcp`, a short frame or a protocol error (a length the header cannot carry, an answer that does not match the request) | none; the rest of the frame may still be on the connection, so it is closed and opened again | counts toward death | `connection` |
| connection refused, reset or closed, a write that fails, a serial port that fails | none; the connection is closed and opened again | counts toward death | `connection` |

On `tcp`, a frame of another transaction (a late answer to an earlier request) or of another protocol is
skipped without an error, and the driver keeps waiting for its own answer.

The connection is closed and opened again only on such an I/O error, on a `tcp` short frame or protocol
error, or when nothing has answered on it (no unit, not even a gateway exception) for `death_tolerance`: a
dead gateway or a connection the network dropped without closing it. Reconnection waits an exponential
backoff with jitter, from 1 second up to 1 minute. The wait starts again from 1 second after an answer of
the device: values or an exception other than 10 and 11, which come from the gateway and leave the backoff
as it is. An unreachable device is never a failing start: the agent starts, the input runs, and once
`death_tolerance` passes without a successful read the devices of the input are reported dead; at the
first successful read afterwards they are born again (a `birth` with their signals, then the samples). A
device that always answers busy (5 or 6) counts as alive, so it is never reported dead.

Requests are spaced by `rate_limit` (at most that many a second, across all blocks). The block that has
waited longest is read first. When the reads fall behind their intervals, a block is read once and its
next read is one interval after that read: no backlog builds up.

**Shared line or gateway.** On a serial line, or behind a gateway, several units share the input's one
connection. A unit that does not answer only times out its own blocks: the other units keep being read at
their intervals, and only the silent unit's devices are reported dead after `death_tolerance`. The
connection is reopened only on an I/O error (on `tcp` also a short frame or a protocol error) or when no
unit has answered for `death_tolerance`. Meanwhile `connected` stays true and `read_errors` `timeout`
grows by one at every poll of each block of the silent unit. Each of those polls holds the line for
`timeout`, which delays the reads of the other units: on a shared line keep `timeout` short, but on `rtu`
and `rtuovertcp` never below the limit that follows.

**Choosing `timeout`.** `timeout` covers the whole exchange: the request, the device's turnaround and the
answer.

- On `tcp` it can be short: an answer that arrives after `timeout` is recognised by its transaction id and
  skipped. A timeout that cuts an answer in the middle leaves its rest on the connection, which is kept:
  the next exchange then usually ends in a protocol error, which reopens it, or nothing answers until
  `death_tolerance` reopens it.
- On `rtu` and `rtuovertcp` set `timeout` above the slowest unit's answer time for its largest block, at
  the line's baud rate. RTU frames carry no transaction id, and the line is not flushed before a request
  nor after a timeout: an answer that arrives after `timeout` stays on the line (the serial port, or the
  gateway's TCP connection for `rtuovertcp`) and is read as the answer of the next request. When that
  request goes to the same unit with the same function and length, nothing detects it: the block gets
  the late values of the previous one, and a unit that is always that slow shifts its values by one
  request at every poll. Any other mismatch is dropped and counted under `connection`.
- The arithmetic: a byte takes 10 or 11 bit times on the line (start, 8 data, parity or a second stop bit
  when set, stop); count 11, about 1.15 ms a byte at 9600 baud. A full block of 125 registers is answered
  in 255 bytes, about 290 ms at 9600 baud (145 ms at 19200), to which add the 8-byte request (about 9 ms
  at 9600 baud) and the device's turnaround from its manual. The default `1s` covers a full block at 9600
  baud with a turnaround of up to about 700 ms.

**Serial port.** For `rtu` the port is opened when the input starts, as the one local resource the driver
needs: a port that cannot be opened (wrong path, no permission) fails the start with `cannot open the
serial port` (the path is not repeated), so the document is `rolled_back` and the previous one keeps
running. On Linux the user running the agent usually needs the `dialout` group (or the group that owns
the device). A port that fails later is opened again in the background, like a lost network connection.
`config validate` checks the serial fields but cannot open the port; `probe` opens it itself: stop the
agent before probing an `rtu` input, because both would read the same port and corrupt each other's
frames (on Windows the second open is refused). For the same reason two inputs must never name the same
`device`: on Linux both opens succeed and the two inputs corrupt each other's frames. A real serial port
is not exercised by the automated tests (see "Known limits of this build"); `rtuovertcp` is.

**Health.** `/status` shows, for the input, `connected` (true while the connection to the device is up)
and `read_errors` (the counts since the input was last started, always the four kinds `timeout`,
`connection`, `exception` and `decode`, at 0 until the first error). The counts restart from 0 when the
input is started again: when its configuration changes, or when a rollback restores the previous
document; the Prometheus functions `rate()` and `increase()` handle such counter resets. The metrics are
`laraplate_edge_input_connected` and `laraplate_edge_input_read_errors_total`
([USAGE.md](USAGE.md#metrics)). Values and connection settings (host, port, device path) are never
written to the logs at `info` and above.

### The `opcua` driver

Reads variables of an OPC UA server, by subscription or by polling. The driver is read-only: it reads the
`Value` attribute and never sends a Write, a Call or a node management request. `examples/opcua` is a
complete document; [USAGE.md](USAGE.md#opc-ua-servers) describes the `opcua trust`, `browse` and `probe`
commands that go with it.

**Connection.** An unknown field in `connection` or in `auth` is an error, named with its path (`connection.<key>: unknown field`).

| Key | Default | Meaning |
|---|---|---|
| `endpoint` | | required: `opc.tcp://<host>[:<port>]`, without a user name or password in it. Give the port (the registered one is 4840). |
| `security_policy` | `Basic256Sha256` | `None`, `Basic256Sha256`, `Aes128_Sha256_RsaOaep` or `Aes256_Sha256_RsaPss` |
| `security_mode` | `SignAndEncrypt` | `Sign` or `SignAndEncrypt`; checked but ignored with `None` |
| `auth` | anonymous | `anonymous`, or `{user, password}` with `password` a `secret://` reference |
| `publishing_interval` | `1s` | the subscription's publishing interval, at least `1ms` |

The agent asks the server for its endpoints and takes the one with the configured policy and mode: a server
that offers none, or whose endpoint offers no user token of the kind `auth` needs, is a connection failure
(`connection`, below).

**`None`.** With `security_policy: None` nothing is signed or encrypted: anyone on the network can read the
values and change them on their way. The driver does not check any certificate, and writes a `WARN` at every
start of the input: `input <id>: OPC UA connection is unprotected (security_policy None)`. A `user` with
`None` is refused by `config validate` (`connection.auth: user and password need a security_policy other
than None`), because the password would travel in the clear. Use `None` only on a network you control, and
prefer the default policy and trust the server's certificate (below).

**User name.** `auth: {user, password: secret://...}`. The password is resolved through the secret
resolver at every connect, not once: a `secret://file/` password that was rotated takes effect at the next
connect. A password that does not resolve is a `connection` error and the input tries again with its
backoff. Neither the user name nor the password is written to a log or to an error.

**Certificates.** Under `data_dir/opcua/pki/`:

| Directory | Holds |
|---|---|
| `own/` | the agent's own application certificate `cert.der` and its key `key.pem` (PKCS#8, mode `0600`) |
| `trusted/` | the server certificates you trusted, `<thumbprint>.der` |
| `rejected/` | the server certificates the agent met and refused, `<thumbprint>.der`, for you to trust later |

A thumbprint is the SHA-1 of the server's certificate in DER form, written as 40 lowercase hexadecimal
characters. Only the leaf certificate is stored and compared.

- **The agent's certificate.** Created when the first `opcua` input starts (a failure to create or read it
  fails the start of the input, so the document is `rolled_back`), or by `opcua trust`, `browse` and `probe`
  when missing: RSA 2048, SHA-256, valid for 10 years, with the application URI `urn:laraplate-edge:<agent_id>`
  and the host name as alternative names. It is never renewed and its expiry is not checked; to replace it, stop
  the agent and delete the `own/` directory. Most servers refuse a client certificate they were not told to
  trust: add `own/cert.der` to the list of trusted client certificates of the server. The key file mode
  `0600` is a no-op on Windows: protect `data_dir` with ACLs.
- **The server's certificate is trusted explicitly.** With a policy other than `None`, at every connect the
  agent discovers the server's endpoints, takes the one for the policy and mode, computes the thumbprint of
  its certificate and goes on only when `trusted/<thumbprint>.der` exists. Otherwise the attempt fails (a
  `connection` error), the certificate is stored in `rejected/`, and one `WARN` names the input, the thumbprint
  and the command to run (`laraplate-edge opcua trust <input>`). That warning comes once per thumbprint per run
  of the input, not at every retry. There is no certificate chain or revocation check: your decision on the
  thumbprint is the whole trust. Its expiry is shown by `opcua trust`, not enforced.
- **A newly trusted certificate is picked up at the next retry, without restarting the agent.** The
  retries follow the reconnection backoff (up to 1 minute). A server that was reinstalled, or whose
  certificate was renewed, presents another thumbprint: it is untrusted again until you trust it. The old
  file stays in `trusted/`; delete it by hand to withdraw that trust.

**Points.** `type` is required: `bool`, `int16`, `uint16`, `int32`, `uint32`, `int64`, `uint64`, `float32`,
`float64` or `string`. `byte_order` is refused (`byte_order: not for opcua`) and so is `read: event`. `unit`,
`priority` and `transforms` work as for any driver. The `address` is a NodeId in its textual form:

| Address | Meaning |
|---|---|
| `ns=<n>;i=<number>` | numeric identifier in namespace index `n` (0 to 65535) |
| `ns=<n>;s=<text>` | string identifier |
| `ns=<n>;g=<guid>` | GUID identifier |
| `ns=<n>;b=<base64>` | opaque identifier |
| `i=<number>`, `s=<text>`, `g=<guid>`, `b=<base64>` | the same in namespace 0 |
| `nsu=<uri>;<i\|s\|g\|b>=<id>` | the namespace by its URI, looked up in the server's `NamespaceArray` at every connect |

The namespace index of a server can change when it is reinstalled or reconfigured; a `nsu=` address
survives that. If the server has no such namespace, that point alone is `bad` (and `exception` counts once for
that connect). The parsing is stricter than the OPC UA text form: the identifier needs its type letter (a
bare `Name` is refused, since a mistyped letter would otherwise name another node), the namespace index is
plain digits, and `nsu=` needs both a URI and an identifier. Only the syntax is checked offline
(`config validate`); whether the node exists is the server's answer. `browse` prints addresses in the `nsu=`
form (USAGE.md).

**Subscribe.** `read: subscribe` monitors the point in the input's one subscription, which runs at
`publishing_interval`. The server samples the variable at its publishing interval, keeps up to 10 values
between two publications and discards the oldest when the queue is full, and applies no filter. The long
form sets those for the point:

```yaml
read:
  subscribe: {sampling: 200ms, queue: 20, deadband: 0.5}
```

| Key | Default | Meaning |
|---|---|---|
| `sampling` | the publishing interval | how often the server samples the variable; sent in milliseconds, and the server may revise it |
| `queue` | `10` | monitored item queue size, 1 to 4294967295 |
| `deadband` | none | the change a value needs to be reported: a non-negative number (absolute), or `N%` with `N` up to 100 (percent of the variable's range) |

A deadband is sent to the server as a data change filter (it reports on a change of status or value). A
percent deadband works only on a variable that has an `EURange` on the server: a server that cannot apply it
refuses the item, and the point is `bad` (see "Health"). The server decides whether to apply any deadband;
the test server of the library ignores them. A monitored item the server refuses is created again at the
next reconnect only. Only `opcua` uses `sampling`, `queue` and `deadband`: other drivers that accept
`subscribe` ignore them.

**Poll.** `read: {poll: <duration>}` reads the point with the Read service. The points of one input with
the same interval are read together, in as few requests as the server's limit allows (below), and the group
due first is read first. When the reads fall behind their intervals, a group is read once and its next read is
one interval after that read: no backlog builds up. Subscribe and poll points can be mixed in an input.

**Conversion.** A value is carried lossless or the sample is `bad` with a `decode` error:

| `type` | Accepts |
|---|---|
| `bool` | a Boolean variant, and nothing else |
| `int16`, `uint16`, `int32`, `uint32`, `int64`, `uint64` | any integer variant (SByte, Byte, Int16, UInt16, Int32, UInt32, Int64, UInt64) whose value fits the type; `uint64` holds up to 9223372036854775807, since integer values are 64-bit signed |
| `float64` | a Float or Double variant; an integer variant of at most 2^53 in magnitude |
| `float32` | a Float variant; a Double that is exactly a float32; an integer variant of at most 2^24 in magnitude |
| `string` | a String variant that is valid UTF-8, and nothing else |

A float never converts to an integer type; a NaN or an infinity is no value; a Boolean never converts to a
number nor a number to a string. Arrays, DateTime, ByteString, structures and every other variant are `bad`.
A `bad` sample from a failed conversion carries the zero of the point's kind (`false`, `0`, `""`), as for
`modbus`.

**Quality and timestamps.** The quality is the class of the value's OPC UA status: Good is `good`,
Uncertain is `uncertain` (its value is kept), Bad is `bad`. A Bad status is the server's verdict, not a
failed conversion: the sample is `bad`, carries the zero of the kind, whatever the server sent with it, and
counts as `exception`, not `decode`. A Good or Uncertain status without a value is `bad` with a `decode` error.
The sample time is the server's source timestamp when the value carries one, else its server timestamp, else
the agent clock.

**Reconnection and liveness.** Starting the input opens only local resources (the certificates) and returns;
the server is connected in the background, so an unreachable or untrusted server is never a failing start:
the agent starts, and once `death_tolerance` passes without a successful exchange the devices are reported
dead, and born again at the first successful exchange afterwards (a `birth` with their signals, then the
samples).

- **The liveness is the server's.** All the devices of an input sit behind one server. Every successful
  exchange with it keeps every device of the input alive: an answer to the watchdog read, a Read response,
  a data change notification. So a subscription whose values never change keeps its devices alive, and a node
  with a Bad status is a `bad` sample, not a dead device. A server whose machine is off, but which still
  answers, does not get its devices reported dead.
- **A watchdog** reads the server's `CurrentTime` every `min(5s, death_tolerance / 3)`, at least `1s`. The
  library detects no half-open connection (a cable cut without a reset), so a failed watchdog read, and the
  request timeout below, end the session.
- A session ends on a connect error, a failed request, a lost connection or a lost subscription. The agent then
  closes it and connects again with an exponential backoff with jitter, from 1 second up to 1 minute, a new
  session with the subscription and monitored items created again. The backoff starts again from 1 second
  only after a session that completed at least one exchange, so a server that accepts sessions but fails every
  request is not flooded. A session lost without being closed stays on the server until its own timeout.

**Limits of the server.** After connecting, the agent reads the server's `MaxNodesPerRead` and
`MaxMonitoredItemsPerCall` and chunks its requests at them. 0, which means no limit, and any value above
1000 count as 1000; a limit the server does not report (a Bad status, or not a number) and a negative one
count as 100. A request that exceeds the server's limits is answered with a service fault, which tears the
library client down, so the driver never sends one, nor an empty one.

**Fixed timing.** There are no settings for these:

| Timing | Value |
|---|---|
| TCP dial | 5 s |
| each request | 5 s; discovery and the connection are each bounded by both |
| requested session timeout | 60 s |
| subscription keep-alive count | 10 |
| subscription lifetime count | `max(30, 60s / publishing_interval)` (at least three keep-alive counts): a server drops the subscription of a lost session after about a minute |
| watchdog | `min(5s, death_tolerance / 3)`, at least 1 s |
| the request that closes a session | 1 s |

**Health.** `/status` shows, for the input, `connected` (true while a session is open) and `read_errors`
(always the four kinds, at 0 until the first error; the counts restart when the input is started again, see
the `modbus` driver's Health). The metrics are `laraplate_edge_input_connected` and
`laraplate_edge_input_read_errors_total` ([USAGE.md](USAGE.md#metrics)). The kinds mean:

| Kind | Counted |
|---|---|
| `timeout` | a connection attempt or request the server did not answer in time |
| `connection` | any other failure of the session: a refused or reset connection, an untrusted certificate, no endpoint with the configured policy and mode, a password that does not resolve, a lost session or subscription, a library panic recovered by the driver |
| `exception` | a Bad status from the server (a refused monitored item, an unknown NodeId, a device behind the server in fault), counted once per exchange that carries at least one: a Read response of a poll group, the answer that creates the monitored items, a data change message; and once for a connect where an `nsu=` namespace is missing |
| `decode` | a value that does not convert to the point's `type`, per point |

Values, endpoints, user names and passwords are never written to the logs at `info` and above. The library
writes its own lines through the standard logger: the agent sends them to its log at `debug` with
`source=gopcua`.

**Declared limits.**

- A panic of the library in one of its own goroutines (on malformed data from a server, say) cannot be caught
  and stops the process; the service manager restarts it (systemd does, with `Restart=`). A panic in a call the
  driver makes itself is caught and becomes a `connection` error and a reconnect.
- The automated tests run the agent against the Go library's own server only. Interoperability with other
  vendors' servers is not tested in CI, and neither are `Aes128_Sha256_RsaOaep` and `Aes256_Sha256_RsaPss`
  against a server of another vendor: try a new server with `probe` before relying on it.
- That test server ignores deadbands and does not check user names, so the deadband and the user name
  authentication are exercised only up to the request they send.
- Only the `Value` attribute of scalar variables is read: no arrays, structures, events, alarms or history.

## Transforms

A point's `transforms` is a chain of steps applied, in order, to every sample the driver reads, before
the sample is routed to the outputs and buffered. A step is an object with one key, the transform, whose
value is its parameter:

```yaml
- signal: press_force
  address: press_force
  type: float64
  unit: N
  read: subscribe
  transforms:
    - scale: 1000          # kN to N
    - deadband: 2000       # send a change of at least 2000 N
    - heartbeat: 5m        # and the last value at least every 5 minutes
```

A sample produced by a step goes through the steps after it. A point without `transforms` is sent as the
driver reads it. Try a chain offline on recorded samples with `probe --record` and `config test`
([USAGE.md](USAGE.md#trying-transforms-probe-and-config-test)).

### Kinds and the type check

Values have one of four kinds: boolean, integer, float, string; "number" below means integer or float.
The point's `type` gives the kind the driver produces: `bool` a boolean; `int16`, `uint16`, `int32`,
`uint32`, `int64`, `uint64` an integer; `float32`, `float64` a float; `string` a string; a point without
`type` a float. Each transform accepts some kinds and produces one (the catalogue below).

`config validate` and every apply check each chain from the point's kind, step by step. A chain that
cannot work refuses the document with one message per chain:

```
inputs.0.devices.0.points.3.transforms: input *, device *, signal press_force, step 2 (bit): accepts integer, got float
```

The message names the step by its position (from 1) and its transform, never a parameter value. The input
id, device and signal are named only when they have the shape of a document key (a lowercase letter, then
up to 31 lowercase letters, digits or `_`); any other name, such as `press-07` or `press-line`, is shown
as `*`, and the path locates the chain. Parameters are checked by the schema first, with their own path
(`inputs.0.devices.0.points.3.transforms.1.round: must be at most 10`); a key of a `map` table appears
in such a path under the same rule.

### Catalogue

| Transform | Parameter | Accepts → produces | Behaviour |
|---|---|---|---|
| `scale` | number | number → float | multiplies |
| `offset` | number | number → float | adds |
| `round` | integer 0 to 10 (decimals) | float → float; an integer passes unchanged and stays an integer | rounds half away from zero, on the decimal digits of the value as written (`1.005` with 2 decimals is `1.01`) |
| `bit` | integer 0 to 63 | integer → boolean | the bit at that index, 0 the least significant |
| `bcd` | `true` | integer → integer | decodes binary-coded decimal (`0x1234` is `1234`); a nibble above 9 or a negative value is a runtime error |
| `map` | `{table: {key: value}, default?}` | any → the kind of the table values | see "map" below |
| `valid_range` | `{min?, max?}`, at least one, `min` not above `max` | number → the same kind | a value outside `[min, max]` keeps its value and gets quality `bad` (not a runtime error) |
| `deadband` | number of at least 0 (absolute), or `"N%"` (of the last sent value) | number → the same kind | sends a sample when it moved from the last sent value by at least the threshold, or when its quality changed; the first value always passes |
| `heartbeat` | duration | any → the same kind | resends the last value with the current time when nothing left the step for the duration |
| `min_interval` | duration | any → the same kind | at most one sample per interval after the first |
| `debounce` | duration | any → the same kind | sends a value only after it stayed unchanged for the duration, with the timestamp of its change |
| `edge_counter` | `rising`, `falling` or `both` | boolean → integer | cumulative count of the counted edges, persisted (see "State") |
| `derivative` | `true` | number → float | change per second between two samples, over their timestamps |
| `trim` | `true` | string → string | removes the white space around the string |
| `regex_extract` | `{pattern, group?, as?}` | string → string, integer or float | the capture `group` (default 1; 0 is the whole match) of an RE2 `pattern` (linear time), converted as `as` says: `string` (default), `integer` (decimal) or `float`; no match or a failed conversion is a runtime error |

The catalogue only grows: an existing transform never changes behaviour, a variant takes a new name.

**`map`.** The sample's value, rendered as text, is the key: a boolean as `true` or `false`, an integer
in decimal (`-3`, `42`), a float in its shortest form (`12.5`, `100`, `1e+21`), a string as it is. The
table values and the `default` are booleans, numbers or strings, all of one kind, which is the kind the
step produces; integers and floats unify to float (a number written with a `.` or an exponent is a
float, and then every value is a float), and only integers give an integer. A value with no key in the
table takes the `default`; without a `default` it is a runtime error. The table may be empty only with a
`default`.

```yaml
transforms:
  - map: {table: {idle: 0, running: 1, setup: 2, fault: 3}}      # string -> integer
```

### Birth data type

The `birth` of a device announces each signal with the data type of what its chain produces: a boolean
is `boolean`, a string is `string`, an integer or a float is `number`. An `int16` point with `bit: 0` is
announced `boolean`; a `string` point mapped to integers, or with `regex_extract` and `as: integer`, is
`number`. A point without transforms is announced from its `type`, as before.

### Bad samples and runtime errors

- A sample of quality `bad`, from the driver or from `valid_range`, is not computed: it passes through
  the following steps unchanged. `deadband`, `heartbeat`, `min_interval` and `debounce` take note of it
  (below); `derivative` and `edge_counter` ignore it. A sample of quality `uncertain` is computed.
- A runtime error of a step (a non-finite `scale` or `offset` result, an invalid BCD digit, a
  value missing from a `map` without a `default`, a regex with no match or a failed conversion, an
  `edge_counter` count that cannot be saved) turns the sample into a `bad` sample with the zero value of
  the step's output kind (`0`, `false` or `""`), which passes through the following steps. It is counted
  in `laraplate_edge_transform_errors_total{input}` and `transform_errors` of `/status`, and logged at
  `WARN` with a rate limit, never with the value ([USAGE.md](USAGE.md#logs)). It never stalls the input
  or refuses a record.
- Every sample leaves the chain with the chain's output kind. A `bad` sample of another kind, for example
  a `bad` integer that passed a `scale` uncomputed, leaves with the zero value of the output kind and its
  original value is lost; a good integer leaving a chain that produces floats becomes a float.

### Time-driven and stateful transforms

All the state of a chain lives in memory, per device, signal and step, and starts clean when the agent
or the input restarts; only the `edge_counter` counts persist. Deadlines are computed from the samples'
timestamps and served on the agent's clock, so a device clock far from the agent's shifts them.

- **`deadband`.** The threshold is absolute, or a percentage of the magnitude of the last sent value; with
  a last sent value of 0 any change passes. A change of exactly the threshold passes (the comparison has
  a relative tolerance of 1e-9 for float rounding: 0.2 to 0.3 passes a deadband of 0.1); an unchanged
  value never passes, so `deadband: 0` drops only repeats. A `bad` sample passes, and the next good
  sample passes as a quality change.
- **`heartbeat`.** Every sample passes. When nothing left the step for the duration, it resends the last
  sample that left, with its quality and the agent's clock time as timestamp, and again every duration
  while nothing new comes. A `bad` sample that left is the last value: a bad source is resent bad. The
  wait counts from the later of the last sample's timestamp and the last time the chain's deadlines
  fired, so a sample held back by an earlier step and released late is not resent at once.
- **`min_interval`.** The first sample of a quiet period passes at once and opens a window of the
  interval; samples inside the window are held, only the latest kept; at the window's end the held
  sample leaves with its own timestamp and the next window opens; a window that held nothing closes. A
  `bad` sample passes at once and drops the held one, which is older; a good sample after it, in the
  window still open, waits for the window's end.
- **`debounce`.** Compares values only: a change of quality alone is not a change. A different value
  restarts the wait; a return to the last sent value cancels it; the last sent value is not sent again.
  A `bad` sample passes at once, cancels the waiting value and forgets the last sent one, so the value
  that comes back after it is sent again once it stays for the duration.
- **`derivative`.** The first sample, a sample not later than the previous one, and any decrease produce
  nothing and become the new baseline: a decrease is read as a totaliser reset. It suits monotonic
  totalisers (energy, flow, part counters); on a value that goes up and down only the rising slopes come
  out.
- **`edge_counter`.** The first sample after a start sends the current count as a baseline (0, or the
  saved count) and sets the level; each counted edge then adds one, saves the count and sends it; other
  samples send nothing. The count only grows, and goes back to 0 when it restarts (see "State"), which
  the backend reads as a counter reset: do not set the backend's `rollover_max` on the signal of an
  `edge_counter`, or a baseline of 0 after a reset would read as a rollover, that is a huge count of parts.

When a device's `death` reaches the chains of its points (the driver lost the machine), the steps of
those chains that keep state learn of it before the `death` is sent: `heartbeat` resends nothing until a
new sample leaves the step, so the backend sees the device unreachable instead of fresh values;
`min_interval` sends its held sample before the `death` and closes its window, so the first sample after
it passes at once; `debounce` drops its waiting value, which was not seen unchanged for the duration.
`deadband` and `debounce` also forget their last sent value, so the first value after the `death` is sent
even when it is unchanged (by `debounce` once it stays for the duration) and a later `heartbeat` re-arms:
the backend shows the device offline after a `death`, and a value equal to the one it had before would
not bring it back.

### State

The `edge_counter` counts of an input are saved in `data_dir/inputs/{input id}/transform-state.json`,
written atomically (directory mode `0700`, file `0600`) on every count:

```json
{"version": 1, "counters": {"<device>/<signal>/<position>/<sha256 of the step>": 42}}
```

- A count belongs to its device, signal, position in the chain and exact step (`{edge_counter: rising}`):
  a step whose mode, position, device or signal changes starts at 0, and so does every count of an input
  whose id changes (another directory), while the other points of the input keep their counts. The
  backend reads a lower cumulative value as a counter reset.
- When the input starts, the counts no current chain uses are dropped from the file. A step removed, or
  changed and then changed back, restarts at 0 and never resumes an older count, which the backend would
  read as parts produced.
- The backend keys a count by device and signal, not by input, so the directory of an input id the
  applied document does not have is removed: once a document is applied, after a rolled-back document
  (against the document still applied), and when the agent starts, before any input starts (against
  its first document). An input renamed and then renamed back, or removed and added again, therefore
  counts from 0 instead of resuming the count of its old directory. A rollback that restarts an input
  the failed document does not have (for example a rename rolled back) removes its directory first and starts it
  from 0: the failed attempt may have counted the same device and signal under the new id.
- A file that cannot be read (truncated, corrupt, of another version, with a negative count) is renamed
  `transform-state.json.corrupt-<unix seconds>` and logged at `ERROR` with the input id; the counts start
  from 0 and the input starts normally.
- When the file cannot be written, the sample of that count is sent `bad` (a runtime error) and the count
  stays in memory; the next successful write saves it.

`config test` keeps no state file: its counts start from 0.

## Outputs

| Key | Required | Default | Meaning |
|---|---|---|---|
| `id` | yes | | output id, also its buffer directory `data_dir/outputs/{id}` |
| `type` | yes | | `laraplate-http` or `plugin:<id>` (an output served by a plugin, see [Plugin outputs](#plugin-outputs)) in this build. The schema also knows `laraplate-mqtt`, which this build refuses. |
| `url` | with `laraplate-http` | | Laraplate base URL: `https`, or `http` only for a loopback host; no user or password. The agent posts to `{url}/api/v1/insert/mes/machine-messages`; a query or fragment is ignored. |
| `token` | with `laraplate-http` | | `secret://` reference to the token of the source's service account |
| `config` | no, `plugin:<id>` only | | the plugin's settings, an object the schema does not interpret (see the secret rule below); refused on any other type |
| `route.include` | no | `["*"]` | patterns matched against `device/signal` (Go `path.Match`: `*`, `?`, `[...]`). `*` alone matches everything; a pattern without `/` means `<pattern>/*`. A device's `birth` and `death` go to every output routing at least one of its signals, and the `birth` lists only those. |
| `batch.max_samples` | no | `1000` | records per batch, 1 to 5000 (the backend default of `mes.machine.max_samples`) |
| `batch.max_body` | no | `1024KiB` | bytes per request body, at most `1024KiB` (`mes.machine.max_body_kb`) |
| `batch.max_wait` | no | `2s` | how long a batch waits to fill, from its first record |
| `buffer.max_size` | no | `1GiB` | bytes both queues of the output may use on disk, at least `32MiB` (two segments of 16 MiB) |
| `buffer.min_free_percent` | no | `5` | share of the disk kept free, above 0 and at most 50 |
| `buffer.fsync_interval` | no | `200ms` | group sync period of written records |

An installation that raises the backend limits raises the agent's caps in a later change.

## Secret fields

Secrets are only `secret://env/NAME`, `secret://file/PATH` or `secret://keystore/NAME` references (how
they resolve: [USAGE.md](USAGE.md#secret-references)); a plain value is refused. The secret fields are:

- `outputs[].token`;
- inside an input's `connection`, a processor's `config` and a plugin output's `config`, at any depth,
  every key equal to or ending with `password`, `token`, `secret` or `api_key`, in any letter case
  (`password`, `db_password`, `Token`, `API_KEY`).

Other spellings such as `apikey`, `api-key` or `apiKey` are not recognised as secret keys: do not use
them for a credential.

## Plugins

`plugins` lists the external processes, built on `laraedge-sdk`, that provide input drivers
(`driver: plugin:<id>`), processors (`processors[].plugin`) and outputs (`type: plugin:<id>`). One plugin
may offer several of these kinds. Keys of an entry:

| Key | Required | Meaning |
|---|---|---|
| `id` | yes | the id used as `plugin:<id>` and in `processors[].plugin` |
| `path` | yes | the plugin binary. Where it resolves and what may contain it depends on the provider and on `plugins_dir` of the bootstrap, see "Where plugins may live" below. |
| `sha256` | yes | lowercase hex SHA-256 of the binary |
| `manifest_sha256` | yes | lowercase hex SHA-256 of the `plugin.json` file next to the binary |
| `max_memory` | no | a size; above it the agent restarts the plugin. No limit when missing. The memory is checked every 10 seconds: the resident set size on Linux, the working set on Windows. |

#### Where plugins may live

`plugins_dir` in the bootstrap (see USAGE.md) is the local trust anchor: an absolute directory, no default,
that the backend cannot change. The rules for `path`:

- `laraplate` provider, `plugins_dir` unset: plugins are disabled and every entry is refused with
  `plugins.<i>.path: plugins are disabled: set plugins_dir in the bootstrap`.
- `laraplate` provider, `plugins_dir` set: a relative `path` resolves against `plugins_dir`; an absolute
  one must lie inside it.
- `file` provider: a relative `path` resolves against the directory of the configuration file. When
  `plugins_dir` is set the binary must lie inside it; when it is unset there is no constraint.

Inside means after resolving symlinks in both the directory and the binary, so `..` and a link pointing
out are refused with `plugins.<i>.path: must be inside plugins_dir`. The manifest sits next to the binary,
so it is covered. A missing binary is still reported as missing. `config validate --plugins-dir <path>`
applies the same check offline, with the `file` provider rules.

`laraplate-edge plugin hash <dir>` prints `sha256:` and `manifest_sha256:` for the plugin in `<dir>`:
the binary named in `<dir>/plugin.json` (with `.exe` on Windows) and the manifest file. Copy the two
values into the entry; run it again after every rebuild of the plugin.

The manifest is the file `plugin.json` in the same directory as the binary at `path`. It names the
plugin and says which kinds it provides: for an input the address syntax, the read modes, the types and
whether it can `browse`; for a processor and an output the schema of their `config`. The agent hashes the binary and the manifest at every start of the plugin and refuses to run
a plugin whose files do not match the entry. The binary at `path` need not carry the manifest's name,
but `plugin hash` looks for the binary by that name.

```yaml
plugins:
  - id: simulated-counter
    path: plugins/simulated-counter
    sha256: <sha256 printed by plugin hash>
    manifest_sha256: <manifest_sha256 printed by plugin hash>
    max_memory: 64MiB
```

### Plugin inputs

An input uses a plugin with `driver: plugin:<id>`, where `<id>` is an entry of `plugins`. Every input
key (`devices`, `points`, `death_tolerance`, `transforms`, ...) is the same as for the other drivers.
Which values are valid is the plugin's: `connection` and each point's `address`, `type` and `read` are
checked against the schemas, types and read modes its `plugin.json` declares, and the plugin checks the
section again when it is configured. A plugin that no input, processor or output uses is not started.

`config validate` checks the plugin entries (files present, both hashes, manifest) and the inputs
against the manifest, without starting the plugin. A problem is reported with its path, for instance
`plugins.0.sha256: does not match the plugin binary` or `inputs.0.devices.0.points.0.address: ...`.

A `secret://` value inside `connection` is not resolved by `config validate`: a manifest problem on a
key that is a secret key (see "Secret fields") is dropped offline. The agent resolves the secrets when
it configures the plugin at start, passes them in the configuration of that input only (never in the
plugin's environment) and the plugin validates the resolved values then; a refused value shows as a
config error at start, naming its path and never its value. A reference is resolved only under a secret key
(see "Secret fields"); anywhere else the plugin receives the `secret://` text as it is and refuses it at
start, which offline validation does not report.

### Processors

A processor observes the samples and device events of some devices of the inputs and derives signals on
devices of its own, through a plugin that offers the processor kind (the SDK's example is `line-state`,
which derives the state of a line from the state of its machines). It never changes, drops or delays
machine data: it receives copies, and the machine data reaches the outputs whether the processor runs
or not.

| Key | Required | Default | Meaning |
|---|---|---|---|
| `id` | yes | | processor id |
| `plugin` | yes | | the id of a `plugins` entry whose manifest offers the processor kind |
| `observe` | yes | | at least one device name; each must be a device of an input |
| `devices` | yes | | the derived devices, at least one |
| `death_tolerance` | no | `30s` | how long the processor may be down before its derived devices are reported dead |
| `config` | no | `{}` | the plugin's settings, checked against the `config` schema of its manifest (see the secret rule above) |

A **derived device** has `device` (its name, as for a device of an input) and `signals` (at least one).
Its name is neither the name of a device of an input nor that of another processor's derived device.
A **derived signal** has:

| Key | Required | Default | Meaning |
|---|---|---|---|
| `signal` | yes | | signal name, unique on its derived device |
| `type` | yes | | `int`, `float`, `bool` or `string`; the `birth` announces it as `number`, `number`, `boolean` or `string` |
| `priority` | no | `critical` | buffer class: `critical` or `bulk` |
| `unit` | no | | engineering unit, at most 16 characters |

```yaml
processors:
  - id: line
    plugin: line-state
    observe: [press-1, press-2]
    devices:
      - device: line-1
        signals:
          - {signal: state, type: int, priority: critical}
    death_tolerance: 10s
```

What the agent does with it:

- **Copies.** After an input's transforms, a copy of every sample and device event of an observed device
  goes to the processor through a queue of 1,000 records, the `birth` an input sends when it starts
  included. A processor that starts after its inputs (a new or changed processor, a rollback) is sent
  the current `birth` of each device it observes. The input never waits for it: when the queue is full
  the copy is dropped and counted (`dropped` in `/status`).
- **Derived samples.** A sample the plugin sends for a declared derived device and signal, with a value
  of the declared `type`, is routed to the outputs like a machine sample: it keeps the plugin's `ts` and
  quality and takes the declared `priority`. Anything else (a device it did not declare, an undeclared
  signal of a declared device, a value of another kind) is refused, never routed, and counted
  (`refused` in `/status`).
- **Births and deaths.** The agent, not the plugin, sends the `birth` of the derived devices, within a
  second of the processor's stream to the plugin opening. A derived device is alive while that stream
  is up, whether or not it sends samples. When the stream goes down (the plugin died, restarts, or
  refused its configuration after a restart), each derived signal is sent `bad` once per outage, with
  the zero value of its type, and the derived devices are reported dead once the stream has been down
  for `death_tolerance`. A derived device is born again only after it died: when the stream is back
  after a death, it is born again within a second. After an outage shorter than `death_tolerance` the
  derived devices stay alive and are not born again, and their signals keep the `bad` value until the
  plugin sends a new sample for them. The new stream does not replay what the plugin missed: it
  receives the copies still in the queue (those beyond its 1,000 records were dropped and counted) and
  the copies from then on; a plugin that restarted has lost its own state and is not sent the births of
  the observed devices again.

`config validate` checks `observe`, the derived names and the `config` against the plugin's manifest
offline, reporting each problem with its path, for instance `processors.0.observe.1: is not a device of
any input` or `processors.0.devices.0.device: is the name of a device of an input`.

### Plugin outputs

An output with `type: plugin:<id>` delivers its batches through a plugin that offers the output kind
(the SDK's example is `jsonl`, which appends each batch to a file). Every key of an output applies as
for `laraplate-http` (`route`, `batch`, `buffer`) except `url` and `token`, which it does not use: they
are refused (`outputs.0.url: is not allowed`). The plugin's own settings go in `config`, checked against the `config` schema of its manifest. A plugin output has its own disk
buffer, its own `source_seq` and the same delivery loop as `laraplate-http` (see "Buffer"): the plugin
never buffers.

| The plugin answers | Effect |
|---|---|
| `ACCEPTED`, `DUPLICATE` | confirmed; the next batch is sealed |
| `RETRY_AFTER` | `backoff` from 1 second to 5 minutes with jitter, at least the wait the plugin asked for (capped at 5 minutes) |
| `SPLIT` | split in two; the first half keeps the refused `source_seq`. One record is dropped with a `rejected_invalid` marker. |
| `REJECT_INVALID` | dropped with a `rejected_invalid` loss marker; the plugin's reason is logged |
| `SUSPEND_AUTH` | `suspended_auth`: records keep buffering; retried every 5 minutes, at once on `SIGHUP` |
| no answer within 30 seconds, plugin down or restarting | `backoff` as for `RETRY_AFTER`; the batch stays in the buffer and is resent identical, under the same `message_id`, once the plugin is back |

A batch resent after a plugin crash may already have been stored by the plugin: the plugin is expected
to answer `DUPLICATE` for a `message_id` it already holds, which confirms it. The secrets of `config`
are resolved at every configuration of the instance (at apply, after a plugin restart, at `SIGHUP`);
one that cannot be resolved at run time suspends the output (`suspended_auth`) until a `SIGHUP`
resolves it.

```yaml
outputs:
  - id: file
    type: plugin:jsonl
    config:
      path: /var/lib/laraplate-edge/machine.jsonl
    batch: {max_samples: 100, max_wait: 1s}
```

A relative path in a plugin's `config` means whatever the plugin makes of it: plugins run in the
directory of their binary.

### Starting, restarting and applying

- A plugin restarts, with all its inputs, processors and outputs, only when its `plugins` entry changes
  or when the hash of its binary on disk differs from the running one. A change in the section of an
  input, a processor or an output reconfigures only that instance and leaves the plugin process and its
  other components running; a restarted plugin output keeps its buffer.
- A plugin the new document no longer uses is stopped; one it newly uses is started.
- A new document whose plugin files do not match their hashes, or whose manifest does not accept the
  inputs, processors or outputs, is `rejected` before anything changes: the previous document keeps
  running.
- At apply the agent configures every new or changed processor and plugin output on its running plugin.
  A plugin that refuses the section, or a secret of `config` that cannot be resolved, rolls the
  document back (`rolled_back`, naming the path, never the value). A plugin that is down at that moment
  does not: the agent logs it and configures the instance when the plugin is up.
- A plugin that dies, fails three health checks in a row or exceeds `max_memory` restarts on its own,
  with a backoff from 1 second to 1 minute (see [USAGE.md](USAGE.md#running-plugins)). After a restart
  the agent configures every input, processor and output again. If the plugin then refuses an input's
  or a processor's section, that component stays down (its devices die after the `death_tolerance`);
  if it refuses an output's section, the output stays in `backoff` and logs the paths. The document is
  not rolled back.

## Applying a document

A document is applied by difference: each input, processor and output is compared with the running one
by its content. Unchanged components keep running untouched (an unchanged output keeps its buffer and its
`source_seq`, an unchanged input the state of its transform chains); changed ones restart (a restarted
input starts its transforms clean, except the `edge_counter` counts, see "Transforms"); removed ones
stop (an output's directory is kept); added ones start. While a changed output restarts, the records
routed to it wait in memory (at most 10,000; beyond that they become a `buffer_full` loss marker).

Outcomes, as reported to Laraplate:

- `rejected`: the document is invalid, older than the applied one, has a transform chain that does not
  type-check, or asks for something this build does not provide. Nothing changed.
- `rolled_back`: a component did not start (an input has 30 seconds). What the apply started is stopped
  and what it stopped is started again. A plugin that does not start counts as a component that did not start.
- `applied`: the document runs and is kept as `data_dir/config/last-valid.json`, from which the agent
  starts next time.

## What this build refuses

`config validate` and every apply refuse these parts, valid in the schema, with
`<path>: not available in this build`:

| Part | Comes with |
|---|---|
| drivers other than `simulated`, `modbus` and `opcua` | the driver plans, in the design's order: `mqtt`, `serial` and `file` |
| output types other than `laraplate-http` and `plugin:<id>` | the MQTT output plan (`laraplate-mqtt`) |
| `derived` signals and their `transforms` | the CEL expressions plan (design section 7.3) |
| `secret://keystore/...` references, `outputs[].config` and `processors[].config` included | a later plan with the OS keystore |

## Buffer

Every output has its own disk buffer under `data_dir/outputs/{id}`: two append-only queues, `critical`
and `bulk`, in segments of 16 MiB, each record with its length and a CRC32C checksum. A record is
written at once and synced in groups every `fsync_interval`: a killed process loses nothing it wrote, a
power loss at most what was written during the last interval. After a crash an incomplete last record is
cut and sending resumes from the confirmed position. A fully confirmed segment is deleted.

**Priorities.** `critical` is for states, counts, alarms and the device `birth` and `death`; `bulk` for
process values. A signal belongs to one class, so its samples keep their order. Each batch carries the
`critical` records first.

**Sending.** The output seals a batch (one `laraplate-machine/1` envelope with a UUIDv7 `message_id`
and the next `source_seq`), writes it to `inflight`, and sends it until the backend settles it. After a
temporary error or a restart the identical batch is sent again, so the backend answers `200 duplicate`
instead of storing it twice. A single record whose envelope alone exceeds `batch.max_body` can never be
sent: it is dropped with a `rejected_invalid` marker.

| Answer | Effect |
|---|---|
| `202`, `200` duplicate (any `2xx`) | confirmed; the next batch is sealed |
| `401`, `403` | `suspended_auth`: nothing is sent, records keep buffering; retried every 5 minutes, at once on `SIGHUP` |
| `413` | split in two; the first half keeps the refused `source_seq`. One record that is still too large is dropped with a `rejected_invalid` marker. |
| `422` | dropped with a `rejected_invalid` marker |
| `429`, `5xx`, network error | `backoff` from 1 second to 5 minutes with jitter, at least `Retry-After` (capped at 5 minutes) |
| `3xx`, other `4xx` | `backoff` as above, with an error log: data is never dropped for a misconfiguration |

**Full buffer.** The limits are `buffer.max_size` and the free disk share `buffer.min_free_percent`
(checked at most once a second).

1. Over `max_size`, the oldest `bulk` segments are dropped first; `critical` loses its oldest segments
   only when it fills the buffer by itself.
2. Under the free disk floor, the order is: older `bulk` segments, then the `bulk` segment being written
   (once), then older `critical` segments. When only the two segments being written are left, incoming
   records are dropped instead, so at most 2 x 16 MiB stay on disk while other files fill it.
3. A record that cannot be written even after an eviction, or that is larger than a segment or than
   `max_size`, is dropped.

The records of the batch in flight are never counted as lost: the batch carries its own copy.

**Loss markers.** Every dropped sample leaves a loss marker `{priority, from_ts, to_ts, samples, reason}`,
with `reason` `buffer_full` or `rejected_invalid`, kept in the output's `losses` file. Markers and
`laraplate_edge_dropped_samples_total` count samples only: a device `birth` or `death` is not a sample, so
a drop of events alone leaves no marker (the protocol wants `samples` of at least 1), and a lost `death` is
logged at `WARN` with its device. Markers travel in the
next batch that has records, in its `losses`, and are gone once that batch is confirmed; the backend
records each one as a `data_dropped` incident, so a gap is always visible. A `413` split keeps them in
the first half. A crash between sealing a batch and acknowledging its markers may report them twice:
the agent over-reports, never under-reports. Repeated drops (under the disk floor, or while the `losses`
file cannot be written) coalesce into the newest pending marker of the same priority: its sample count
is exact, but its `ts` range can span records that are still in the buffer.

## Known limits of this build

- The `opcua` driver is tested against the Go library's own server only, and a panic of the library in one of its goroutines stops the agent: see "Declared limits" under the `opcua` driver.
- A real serial port is not exercised by the automated tests: the `modbus` `rtu` transport is covered
  through a fake client, and the RTU framing through `rtuovertcp` against a test server. Try a new serial
  line with `probe` before relying on it.
- On `modbus` `rtu` and `rtuovertcp`, an answer that arrives after `timeout` stays on the line and is read
  by the next request; when that request goes to the same unit with the same function and length, the
  late values are taken as its answer and nothing detects it. Keep `timeout` above the slowest unit's
  answer time for its largest block (about 290 ms for 125 registers at 9600 baud, plus the device's
  turnaround; "Choosing `timeout`" under the `modbus` driver).
- No custom CA bundle: HTTPS trusts the system roots only, so a network that inspects TLS cannot be
  used yet.
- A component left `failed` by a rollback is not retried until a new document is applied.
- The records held in memory for a `failed` output are discarded at shutdown, with a warning giving
  their count and `ts` range per priority.
- The records held in memory while a changed output reopens (at most 10,000) are lost without a loss
  marker if the agent crashes before the reopened buffer takes them, for instance during a slow reopen
  of a large buffer.
- YAML flow style at the top level of a document is read as JSON and refused; use block style.
- Transform deadlines (`heartbeat`, `min_interval`, `debounce`) come from sample timestamps and are
  served on the agent's clock: a device clock far from the agent's shifts them, and a backward step of
  the agent's clock delays a `heartbeat` by the size of the step.
- `valid_range` compares integers as floats, exactly up to 2^53 in magnitude.
- Every counted edge of an `edge_counter` writes and syncs the input's state file while the input's
  transform processor holds its lock: the other signals of that input wait for the write, tens of
  milliseconds on an SD card. A fast pulse train on a slow disk delays the input's samples accordingly.
- The state of the time-driven transforms (a held `min_interval` or `debounce` sample, the `heartbeat`
  timer) is lost when the input restarts; only the `edge_counter` counts are kept.
- A `death` from one input marks the device dead for every input that reads it.
- Connection and `config` keys spelled `apikey`, `api-key` or `apiKey` are not treated as secrets.
- A processor or plugin output removed from the document, or renamed, keeps its state inside the
  running plugin until the plugin restarts: the agent stops using the instance but does not ask the
  plugin to release it.
- After a processor's stream was down for less than its `death_tolerance` (a plugin crash with a quick
  restart, for instance), its derived signals stay at the `bad` value sent at the outage until the plugin
  sends a new sample for each of them: the derived devices are not born again, nothing tells the plugin
  to send its current values, and the new stream replays nothing beyond the copies still queued. A
  plugin that restarted on its own is not sent the births of the observed devices again; it receives
  them only at the next `birth` of each device, or when a document restarts the processor.
- A processor's `refused` and `dropped` counts start from 0 again when the processor restarts (a changed
  section, a restart of its plugin by a new document, a rollback); a plugin crash alone does not reset
  them.
- On Unix the path of a plugin's socket, a file under `data_dir/run/plugins/<id>/`, must fit the
  platform's limit (107 bytes on Linux): a deep `data_dir` makes every plugin fail to start with `socket path too
  long`. Use a short absolute `data_dir`.
- A corrupt record of the buffer, found when it opens (inside an older segment) or while it runs
  (anywhere), drops the rest of that segment with an error log and
  `laraplate_edge_corrupt_records_total`, without a loss marker (the protocol has no reason for it).
  Sending goes on from the next segment. See the troubleshooting table in [USAGE.md](USAGE.md).
- A loss marker the backend refuses, or pending markers larger than the backend's body limit, travel
  again with every next batch (a refused batch hands its markers back), so every later batch is refused
  and its records dropped as `rejected_invalid`, after a `413` has split it down to one record. It
  needs a misconfigured backend (a stricter marker validation, a body limit below the agent's
  `batch.max_body`): fix the backend, or lower `batch.max_body`.
- A wrong `url` that answers `3xx` or `4xx` is retried with backoff forever rather than stopping.
- The status endpoint is plain HTTP; its token is resolved once at start; `--log-level debug` logs
  sample values and does not expire; the watchdog ping is timer-driven and does not detect a stuck
  supervisor or component. See [USAGE.md](USAGE.md).
- `laraplate_edge_buffer_oldest_unconfirmed_seconds` is the agent clock minus the record's device
  `ts`, so a device clock that drifts skews it.
- Bootstrap changes other than credentials apply at the next restart. A `secret://env/` value is fixed
  when the process starts: rotating it needs a restart, while a `secret://file/` token rotates with a
  `SIGHUP`.
