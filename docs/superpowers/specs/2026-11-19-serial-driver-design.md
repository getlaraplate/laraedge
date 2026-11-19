# Serial driver: design

**Status:** Approved (2026-11-19). Delivered 2026-11-19 by `../plans/2026-11-19-serial.md`; the spec is complete.

**Date:** 2026-11-19

**Parent spec:** `../../../../docs/superpowers/specs/2026-10-12-laraplate-edge-agent-design.md` (stack root),
sections 4, 9.1 and 9.2 (`serial` row). This document settles what the parent leaves open for the `serial` input
driver; where the two disagree, this one is newer and wins for this subject.

**Builds on:** `2026-10-19-transforms-modbus-opcua-design.md` (driver kit, D6 non-blocking start, `probe`, the
`regex_extract` transform, the serial fields of `modbus` `rtu`), `2026-11-06-mqtt-driver-design.md` (plain payload
conversion, read error kinds) and `2026-11-18-file-driver-design.md` (the sample context, `context` mapped in
`connection`, the latest conventions for `probe` and `browse`).

---

## 1. Purpose and success

Measuring stations next to a machine send readings over a serial line: dial and digital gauges, calipers and
micrometers behind a Digimatic-to-RS-232 cable or a multiplexer, bench instruments, and barcode readers set to
serial mode. The `serial` driver opens one port, cuts the byte stream into messages (frames), and turns each frame
into a sample of the point it belongs to. A barcode scanned at the station (the part's serial, the order) becomes
the sample context of the measurements that follow on the same port, so the backend knows which part a measurement
belongs to (`Modules/MES/docs/MACHINE_CONNECTIVITY.md`, "Probe measurements and quality checks"), as the `file`
driver does for a CMM report.

With this driver the built-in catalogue of the parent's section 9.2 is complete.

The design is generic: no plant and no real instrument are available. Success is measured against a fake port in
unit tests on the fake clock and against a Linux pseudo-terminal in integration and end-to-end tests, in CI, with no
hardware.

**Goals:** unsolicited frames (an operator presses the gauge's data button or a foot switch) and instruments that
answer a request; several instruments on one port behind a multiplexer; the part's identity carried from a scanned
barcode to the measurements that follow; a technician can find the right port with `browse` and see with `probe`
what the instrument sends before turning the input on.

**Non-goals:** USB HID keyboard-wedge readers (section 2); binary protocols (a frame is text); writing anything to
an instrument except the configured request commands (section 6).

**Anti-goals:** a measurement attached to the wrong part (a stale context); a USB adapter unplugged at boot that
stops the agent from starting; a port held by the agent while a technician needs it, without a clear message; a
half frame read as a value.

## 2. Approaches

Two choices were open: what talks to the port, and how barcode readers that act as a keyboard fit in.

**Port library.** The binary is static and has no CGO (parent 10.1.1), so the library must be pure Go on Linux
amd64, arm64 and armv7 and on Windows.

| | Approach | Trade-off |
|---|---|---|
| **A (chosen)** | **`go.bug.st/serial`** (BSD-3-Clause; brings `golang.org/x/sys`, already direct; v1.8.0 no longer brings `github.com/creack/goselect`, see the amendment below) | Maintained, widely used (the Arduino CLI). Lists the ports with USB VID, PID and serial number on Linux and Windows without CGO (its enumerator needs CGO only on macOS, which has no production support), so `browse` can say which COM port is the instrument. Read timeouts per port. A new direct dependency. |
| B | `github.com/goburrow/serial` (MIT), already in the module graph through `modbus` | No new dependency, but unmaintained since 2017 and no port listing: `browse` would scan `/dev` and the Windows registry by hand, platform code of our own. |
| C | Our own layer on `golang.org/x/sys` (termios, the Windows DCB) | No dependency, every line of platform code ours to keep, on armv7 and Windows where such bugs hide best. |

A was chosen for `browse`: "which of these COM ports is the gauge" is the first question at commissioning, and only
A answers it without platform code of our own. The licence and the absence of CGO on the target platforms are
verified again in the plan, before the dependency is added (parent 9.1).

**Keyboard-wedge readers.** Most USB barcode readers default to HID keyboard mode. The parent spec supported them on
Linux only (evdev with an exclusive grab).

| | Approach | Trade-off |
|---|---|---|
| **A (chosen)** | **Out of this driver.** Readers are set to serial mode: nearly all offer USB-CDC (a virtual COM port), switched on by scanning a programming barcode from the reader's manual. | One kind of device, one code path, every platform. Costs one setup step per reader. |
| B | HID as a separate input, with the context shared between inputs that name the same station | Reopens the context scope decided below (one port), adds cross-input state and evdev code for Linux only. |
| C | HID as a second source of the same input | Keeps the context in one input, but still evdev, keymaps and an exclusive grab, Linux only. |

This reverses the parent's HID line; the parent's `serial` row is amended to point here (section 11).

## 3. Decisions

| # | Topic | Decision |
|---|---|---|
| S1 | Scope and delivery | One spec, one plan. |
| S2 | One input, one port | An input opens one port. Several instruments on one port (a multiplexer) are several devices of the input, or several points of one device. |
| S3 | Library | `go.bug.st/serial` (section 2, approach A). |
| S4 | Framing | A frame ends at a terminator, after a fixed length, or after a silence gap; an optional start character discards what comes before it (section 5). The parent's "regex" framing is not built: a regular expression over a stream cannot tell where a frame ends, since a prefix of a longer frame may already match. A regular expression selects frames instead (S5). |
| S5 | Routing | A point's address is a regular expression matched against the frame text. The first point that matches, in document order across the devices of the input, takes the frame. Its first capture group, if it has one, is the value text; otherwise the whole frame is. |
| S6 | Context | Rules in `connection.context` match frames before the points. A frame matching a rule sets that context key for the input and gives no sample. A key keeps its value until a new frame sets it again, or until `context_ttl` passes; keys are independent. The context is kept in memory only. |
| S7 | Requests | `connection.requests` sends configured commands at fixed intervals, for instruments that answer only when asked. A command is a read request, as a Modbus read is, not a write in the sense of parent 9.1. The answers are routed like any frame. |
| S8 | Start and the port | `Start` returns at once; the port is opened in the background with the common backoff (D6 as amended for `file`): a USB adapter plugged in later, or a port a retired instance still holds during a reload, is opened when it becomes free, never a failed `Start`. |
| S9 | Death | The port not open beyond `death_tolerance`. Silence never kills a device, since a gauge may send nothing for hours; the exception is an input with `requests`, where no frame at all for `death_tolerance` is death too (an instrument that is asked and never answers is gone). |
| S10 | Timestamp | The agent's clock when the frame's last byte was read. Instruments on a serial line do not send a time. |
| S11 | Read | `event` only; `poll` and `subscribe` refuse the document (requests are an input setting, S7). `type` is required. |
| S12 | Dependencies | `go.bug.st/serial`, new (its `creack/goselect` is gone as of v1.8.0, amended 2026-11-19 by `../plans/2026-11-19-serial.md`, R1); `golang.org/x/text/encoding/charmap`, already direct since `file`. No other. |

## 4. Connection

`connection` of the input. An unknown field is an error named with its path, as for the other drivers. The serial
fields keep the names and rules of `modbus` `rtu`.

| Field | Default | Meaning |
|---|---|---|
| `device` | | required: the port, `/dev/ttyUSB0`, `/dev/serial/by-id/...` (recommended on Linux: stable across replugging) or `COM4` |
| `baud` | | required: a positive integer |
| `data_bits` | `8` | 5 to 8 |
| `parity` | `none` | `none`, `even` or `odd` |
| `stop_bits` | `1` | 1 or 2 |
| `framing` | `{terminator: "\r"}` | section 5 |
| `max_frame` | `1024` | bytes; a longer frame is dropped, kind `frame_too_long`, and the reader resynchronises at the next terminator or start character |
| `encoding` | `utf-8` | `utf-8`, `windows-1252` or `iso-8859-1`, as for `file` |
| `context` | | a list of rules, section 7 |
| `context_ttl` | none | a duration; a context key older than this is dropped. None means a key lasts until it is set again |
| `requests` | | a list of `{command, every}`, section 6 |
| `request_timeout` | `1s` | with `requests`: how long an answer is awaited before the next command may be sent |

JSON escapes (`"\r"`, `"\n"`, `"\u0002"`) write control characters in `framing` and `requests`; no escape grammar of
our own.

## 5. Framing

`connection.framing`, exactly one of `terminator`, `length` or `gap`:

| Field | Meaning |
|---|---|
| `terminator` | a string of 1 to 4 bytes ending a frame, not part of it (`"\r"`, `"\r\n"`, `"\u0003"`) |
| `length` | a frame is exactly this many bytes (1 to `max_frame`) |
| `gap` | a frame ends after this much silence on the line (at least `20ms`), for instruments with neither terminator nor fixed length |
| `start` | optional, with any of the three: one character that begins a frame (`"\u0002"`); bytes before it are discarded, the character itself is not part of the frame |

Rules:

- Before routing, CR, LF and NUL around the frame are removed, so a `"\r"` terminator also reads `"\r\n"` lines, and
  the text is decoded from `encoding`. An empty frame is ignored without a count.
- With `terminator` or `length`, bytes of an unfinished frame are dropped, kind `incomplete_frame`, when nothing
  more has arrived for `2s` after the last byte, so a half frame from a cable pulled mid-message is never joined to
  the next. The timer counts from the last byte, not the first: a slow line (300 baud) may take longer than `2s` to
  deliver a long frame and still completes it; a line that keeps sending noise is bounded by `max_frame` instead. (Amended 2026-11-19 by `../plans/2026-11-19-serial.md`: the bound is a buffer of `max_frame` plus the terminator plus 2 bytes, so a frame of exactly `max_frame` bytes followed by more than two trailing CR, LF or NUL bytes counts `frame_too_long`; the documentation advises raising `max_frame` for such an instrument.)
  With `gap` the silence itself ends the frame, so this rule does not apply.
- A frame that does not decode in `encoding` is dropped, kind `decode`.
- When the port is lost, what is in the reader is dropped without a count.

## 6. Requests

```yaml
requests:
  - {command: "?\r", every: 1s}          # a single gauge asked once a second
  - {command: "01\r", every: 5s}         # channel 1 of a multiplexer
  - {command: "02\r", every: 5s}
```

- `every` is at least `100ms`. Commands are sent one at a time: after a command the driver waits for a frame or
  `request_timeout` before sending the next one due, so the instrument never sees a queue (the instrument comes
  first, parent 9.1). A command whose turn passes while another is waiting is sent once, late, not twice.
- A command that gets no frame within `request_timeout` counts kind `no_response`; the frame that arrives later is
  still routed. The driver does not pair answers with commands: the routing of section 7 already tells them apart
  (a multiplexer prefixes the channel), and an instrument that answers in an unrelated form would be addressed the
  same way unsolicited.
- `command` is 1 to 64 bytes. Requests start once the port is open and stop when it is lost.

## 7. Routing and context

```yaml
connection:
  device: /dev/serial/by-id/usb-FTDI_FT232R_USB_UART_A10K8XYZ-if00-port0
  baud: 9600
  framing: {terminator: "\r"}
  context:
    - {key: serial, match: '^SN(\w+)$'}
    - {key: order_ref, match: '^PO-(\d{4}-\d+)$', group: 0}
  context_ttl: 10m
devices:
  - device_id: gauge_bore
    points:
      - {signal: bore_d, address: '^01A([+-]?\d+\.\d+)$', read: event, type: float64}
      - {signal: depth, address: '^02A([+-]?\d+\.\d+)$', read: event, type: float64}
```

**Order.** Each frame is tried against the `context` rules in order, then against the points in document order
across the devices of the input. The first match takes it; the frame is offered to nothing else.

**Context rules.** `key` is one of `serial`, `order_ref`, `operation_ref`, `lot` (the keys the backend accepts);
`match` is a regular expression (Go RE2 syntax); `group` picks the capture group holding the value, default `1`,
`0` for the whole match. A match with an empty value clears that key. From then on every sample of the input
carries the input's current context; a key set at time t applies to frames whose last byte was read after t. The
sample carries the context as it was when its frame was read, so a later scan never changes samples already made. (Amended 2026-11-19 by `../plans/2026-11-19-serial.md`: the router keeps the current and the previous value of each key, so a frame older than two scans of the same key finds no value for it.)

**Points.** `address` is a regular expression; the first capture group, if the expression has any, is the value
text, else the whole frame. The text is converted to the point's `type` with the rules of the `mqtt` plain payloads
(`driverkit.ConvertText`): lossless or a `bad` sample, kind `conversion`. Transforms follow as usual, so
`regex_extract`, `scale` and the rest still apply to the extracted text or value.

**Unmatched frames** give no sample, count kind `unmatched` and are logged at `debug`, rate-limited, with the frame
quoted and truncated to 80 bytes, so a technician can turn the log up to see what an unknown instrument sends.

**Lifetime of the context.** It is kept in memory: a restart or a reload of the input starts with none, logged at
`info` when the input had one. Losing the port does not clear it (the operator is still working on the same part
when a loose cable is pushed back in); `context_ttl` is the guard against a stale one. The documentation advises
setting `context_ttl` to a little more than the longest time a part stays at the station.

A point has no `group` field (refused with its path): an expression that needs more groups than one uses
non-capturing groups `(?:...)` for the rest.

**Validation at apply.** Every expression compiles; a rule's `group` exists in its expression; a
`context` key appears once; two points with the same address are allowed (the first wins) but logged at `warn` at
apply, since the second never receives a frame.

## 8. Lifecycle

**Start.** `Start` validates and returns; a goroutine opens the port with the common backoff. Open failures count
kind `open` (the port missing, busy, or refused by permissions; the log says which, and on Linux names the
`dialout` group when permission is denied).

**Reading.** One reader per input reads with a short timeout (100 ms) so that `gap` and the stale-bytes rule of
section 5 run on the clock, and stops promptly on `Stop`. Reads, framing and routing run on the fake clock in
tests.

**Losing the port.** A read or write error (a USB adapter unplugged) closes the port, counts kind `port_lost` and
goes back to opening with backoff. Requests stop until the port is open again.

**Death and birth.** While the port is open the devices of the input are renewed on every read cycle, frames or
not (without `requests`), or on every frame (with `requests`). Beyond
`death_tolerance` with the port not open, or, with `requests`, without any frame, every device of the input dies;
the next open (or frame) is their birth. `laraplate_edge_input_connected{input}` follows whether the port is open.

**Exclusive use.** The port is exclusive on Windows; on Linux the driver asks for exclusive use where the library
offers it, so a second program (or `probe` while the agent runs) gets a clear "port busy" instead of interleaved
frames. If the library does not offer it on Linux, the plan records that and the documentation keeps the advice of
`modbus` `rtu`: stop the input before probing its port. (Amended 2026-11-19 by `../plans/2026-11-19-serial.md`, R1: v1.8.0 of the library offers it, with `TIOCEXCL` on open on Linux and the BSDs, so a second non-root open fails with "busy"; the advice to stop the input before probing stays, since the port is exclusive.)

## 9. Commands and metrics

**`probe <input>`:** opens the port, sends the configured requests, and for up to 30 s, the time limit of every `probe`
(or until every point has a value) shows each point's value, type, quality and time, or `no frame`; then the context it would
set, and up to ten unmatched frames quoted, so a technician can write the expressions. It never sends anything but
the configured requests, and if the port is busy it says so and that the input may be running. (Amended 2026-11-19 by `../plans/2026-11-19-serial.md`, R3: there is no `--wait`, the agent's own 30 s limit applies, as for `mqtt`, and a point that stays silent, a duplicate address included, makes the probe wait all of it. R4: the context and the unmatched frames are printed through the optional `probeNoter` interface of `cmd/laraplate-edge/probe.go`.)

**`browse <input>`:** lists the serial ports of the machine with, where known, the USB VID and PID, the product
and the serial number, and on Linux the stable `/dev/serial/by-id/` path for each. Its configured
`device` is marked, or reported missing. (Amended 2026-11-19 by `../plans/2026-11-19-serial.md`, R2: `browse` needs an input, as every `browse` of the agent does; a technician without a document writes a one-input document with any `device`.)

**Metrics:** those of the driver kit, `laraplate_edge_input_connected{input}` and
`laraplate_edge_input_read_errors_total{input,kind}` with the kinds `open`, `port_lost`, `frame_too_long`,
`incomplete_frame`, `decode`, `unmatched`, `conversion` and `no_response`. A context change is logged at `info`
with the key and the new value.

## 10. Tests

- **The port behind an interface** (open, read with a timeout, write, close), with a fake in memory driven by the
  fake clock: framing in its three modes, `start`, `max_frame` and resynchronisation, stale bytes, decoding, routing
  order, capture groups, context rules, `context_ttl`, the context frozen on each sample, requests one at a time,
  a late answer, `no_response`, loss and reopening, death and birth with and without requests.
- **A Linux pseudo-terminal** opened through `golang.org/x/sys` (no new test dependency): the real library on a
  real file descriptor, for opening, reading, writing commands, and a peer that closes (a cable pulled). The
  library's handling of a pseudo-terminal (baud setting, exclusive use) is checked by the plan before it relies
  on it.
- **One end-to-end test** with the real binary on Linux over a pseudo-terminal: a scanned serial followed by two
  measurements reaches the fake backend with `context.serial` on both.
- **Windows:** built and unit-tested in CI; a real COM port cannot be exercised there, a declared limit, as for the
  RTU transport of `modbus`.
- **Fuzzing** of the framer with arbitrary bytes, as for the Sparkplug decoder: no panic, no frame above
  `max_frame`.

## 11. Plan, documentation and what is left out

**Plan:** one, in order: the port interface and the dependency (after verifying licence and no CGO), the
connection and its validation, the framer, routing and context, requests, the driver with lifecycle and death,
`probe` and `browse`, `CheckBuild` no longer refusing `serial`, documentation and the example, the end-to-end test.

**Documentation:** `docs/CONFIGURATION.md` and `docs/USAGE.md` (the connection, a multiplexer example, a barcode
reader switched to USB-CDC mode, `context_ttl`, the Linux `dialout` group, `probe` and `browse`), an example in
`examples/serial`, and the parent spec's `serial` row amended to point here, recording that keyboard-wedge readers
and regex framing are not built.

**Out of scope:** USB HID keyboard-wedge readers (section 2); binary frames and checksums; pairing answers with
requests; a context shared between inputs; the context kept across restarts; writing to instruments beyond the
configured requests; RS-485 multi-drop addressing beyond what a request command and a regular expression can do;
the `context` field in the plugin SDK.
