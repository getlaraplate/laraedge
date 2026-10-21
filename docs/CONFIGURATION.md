# Configuration document `laraplate-edge-config/1`

The configuration document says what the agent reads (inputs), where it sends (outputs) and how much it
may buffer. It comes from a local file or from Laraplate (see the providers in [USAGE.md](USAGE.md)).
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
- An unknown key is an error everywhere but inside `connection`.
- Durations are positive, written as `500ms`, `2s`, `1m`, `1h30m`. Sizes are positive, written as
  `512KiB`, `16MiB`, `1GiB` (or `B`).
- Ids (`inputs[].id`, `outputs[].id`, `plugins[].id`) match `^[a-z0-9][a-z0-9._-]{0,63}$` and are
  unique in their list. An output id names its buffer directory.

## Top level

| Key | Required | Meaning |
|---|---|---|
| `schema` | yes | `laraplate-edge-config/1` |
| `version` | no | integer from 1. A document older than the applied one is refused; a missing version counts as 0. Laraplate writes the published version into the document it serves. |
| `inputs` | yes | list of inputs, may be empty |
| `outputs` | yes | list of outputs, may be empty |
| `plugins` | no | refused by this build |

## Inputs

| Key | Required | Default | Meaning |
|---|---|---|---|
| `id` | yes | | input id |
| `driver` | yes | | `simulated` or `modbus` in this build. The schema also knows `opcua`, `mqtt`, `serial`, `file` and `plugin:<id>`. |
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
| `read` | yes | | `subscribe`, `event`, or `{poll: <duration>}` |
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

**Connection.** An unknown field is an error here, unlike in other drivers' `connection`.

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
request. Put every device of a serial line in one input: a serial port belongs to one input.

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
| exception 10 or 11 (a gateway cannot reach the device) | none | counts toward death: no successful read | `exception` |
| no answer within `timeout` | none; the connection is closed and opened again | counts toward death | `timeout` |
| connection refused, reset or closed; a frame that is not valid (bad CRC, wrong unit or function, short frame) | none; the connection is closed and opened again | counts toward death | `connection` |

Reconnection waits an exponential backoff with jitter, from 1 second up to 1 minute, reset by the first
answer of the device. An unreachable device is never a failing start: the agent starts, the input runs,
and once `death_tolerance` passes without a successful read the devices of the input are reported dead;
at the first successful read afterwards they are born again (a `birth` with their signals, then the
samples). A device that always answers busy (5 or 6) counts as alive, so it is never reported dead.

Requests are spaced by `rate_limit` (at most that many a second, across all blocks). When the reads fall
behind their intervals, a block is read once and its next read is one interval later: no backlog builds
up.

**Serial port.** For `rtu` the port is opened when the input starts, as the one local resource the driver
needs: a port that cannot be opened (wrong path, no permission) fails the start with
`cannot open the serial port` (the path is not repeated), so the document is `rolled_back` and the
previous one keeps running. On Linux the user running the agent usually needs the `dialout` group (or
the group that owns the device). A port that fails later is opened again in the background, like a lost
network connection. `config validate` checks the serial fields but cannot open the port; `probe` opens it
itself: stop the agent before probing an `rtu` input, because both would read the same port and corrupt each
other's frames (on Windows the second open is refused). A real serial port is not exercised by
the automated tests (see "Known limits of this build"); `rtuovertcp` is.

**Health.** `/status` shows, for the input, `connected` (true while the connection to the device is up) and
`read_errors` (the counts since the agent started, always the four kinds `timeout`, `connection`, `exception`
and `decode`, at 0 until the first error). The metrics are
`laraplate_edge_input_connected` and `laraplate_edge_input_read_errors_total` ([USAGE.md](USAGE.md#metrics)).
Values and connection settings (host, port, device path) are never written to the logs at `info` and above.

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
| `type` | yes | | `laraplate-http` in this build. The schema also knows `laraplate-mqtt` and `plugin:<id>`. |
| `url` | with `laraplate-http` | | Laraplate base URL: `https`, or `http` only for a loopback host; no user or password. The agent posts to `{url}/api/v1/insert/mes/machine-messages`; a query or fragment is ignored. |
| `token` | with `laraplate-http` | | `secret://` reference to the token of the source's service account |
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
- inside an input's `connection`, at any depth, every key equal to or ending with `password`, `token`,
  `secret` or `api_key`, in any letter case (`password`, `db_password`, `Token`, `API_KEY`).

Other spellings such as `apikey`, `api-key` or `apiKey` are not recognised as secret keys: do not use
them for a credential.

## Applying a document

A document is applied by difference: each input and output is compared with the running one by its
content. Unchanged components keep running untouched (an unchanged output keeps its buffer and its
`source_seq`, an unchanged input the state of its transform chains); changed ones restart (a restarted
input starts its transforms clean, except the `edge_counter` counts, see "Transforms"); removed ones
stop (an output's directory is kept); added ones start. While a changed output restarts, the records
routed to it wait in memory (at most 10,000; beyond that they become a `buffer_full` loss marker).

Outcomes, as reported to Laraplate:

- `rejected`: the document is invalid, older than the applied one, has a transform chain that does not
  type-check, or asks for something this build does not provide. Nothing changed.
- `rolled_back`: a component did not start (an input has 30 seconds). What the apply started is stopped
  and what it stopped is started again.
- `applied`: the document runs and is kept as `data_dir/config/last-valid.json`, from which the agent
  starts next time.

## What this build refuses

`config validate` and every apply refuse these parts, valid in the schema, with
`<path>: not available in this build`:

| Part | Comes with |
|---|---|
| drivers other than `simulated` and `modbus` | the driver plans, in the design's order: `opcua`, then `mqtt`, `serial` and `file` |
| output types other than `laraplate-http` | the MQTT output plan (`laraplate-mqtt`) and the plugin plan (`plugin:<id>`) |
| `plugins`, `plugin:<id>` drivers and outputs | the plugin plan (design section 8) |
| `derived` signals and their `transforms` | the CEL expressions plan (design section 7.3) |
| `secret://keystore/...` references | a later plan with the OS keystore |

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

- A real serial port is not exercised by the automated tests: the `modbus` `rtu` transport is covered
  through a fake client, and the RTU framing through `rtuovertcp` against a test server. Try a new serial
  line with `probe` before relying on it.
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
- Connection keys spelled `apikey`, `api-key` or `apiKey` are not treated as secrets.
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
