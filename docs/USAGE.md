# Using laraplate-edge

This guide is for whoever builds, installs and runs the agent. The configuration document (inputs,
outputs, buffer) is described in [CONFIGURATION.md](CONFIGURATION.md). The design is in the stack:
`docs/superpowers/specs/2026-10-12-laraplate-edge-agent-design.md`.

What this build provides: the `simulated` and `modbus` input drivers, the level-1 transforms of points, the
`laraplate-http` output, the disk buffer with loss markers, the `file` and `laraplate` configuration
providers, the `probe` and `config test` commands, the status endpoint, JSON logs and the systemd
watchdog. The other real drivers, derived signals (CEL), the MQTT output, plugins, the OS keystore and packages
come with later plans (see "What this build refuses" in CONFIGURATION.md).

## Build

Go 1.27 or newer, no CGO, one static binary:

```bash
go build -o laraplate-edge ./cmd/laraplate-edge
./laraplate-edge version
```

Set the reported version at link time:

```bash
go build -ldflags "-X github.com/getlaraplate/laraedge/internal/version.version=1.2.3" \
  -o laraplate-edge ./cmd/laraplate-edge
```

Without it the version is `dev`. The version is sent to Laraplate in every configuration report and in
the `User-Agent` header.

## Commands

| Command | What it does | Exit code |
|---|---|---|
| `run --bootstrap <path> [--log-level debug\|info\|warn\|error]` | runs the agent until SIGTERM or SIGINT; logs at `info` by default | 0 after a clean stop, 1 when it cannot start, 2 on a usage error |
| `config validate <file>` | checks a configuration document with both validation layers (schema and rules, then what this build provides and the type check of every transform chain), and builds every input through its driver without connecting; relative paths in a `connection` resolve against the file's directory, as with the `file` provider; applies nothing | 0 and `<file>: valid` on stdout, or 1 and one `<file>: <path>: <problem>` line per problem on stderr |
| `config test <file> --input <samples.jsonl>` | replays recorded samples through the transform chains of a document and writes the resulting samples to stdout; applies nothing (see "Trying transforms") | 0; 1 when the document is invalid, the sample file is unreadable or has a bad line; 2 on a usage error |
| `probe <input> (--config <file> \| --bootstrap <path>) [--record <file>]` | reads every point of one input once and prints the raw samples, before transforms; never starts the input or an output (see "Trying transforms") | 0; 1 when the document is invalid, the input is unknown or the read fails; 2 on a usage error |
| `status [--listen addr]` | prints the `/status` answer of a running agent, indented; `--listen` defaults to `127.0.0.1:9180`; the bearer token, when the endpoint has one, is read from `LARAPLATE_EDGE_STATUS_TOKEN` | 0, or 1 when the agent does not answer |
| `version` | prints the version | 0 |

`run` writes its logs to stdout. Usage errors and the error that stops `run` go to stderr. Error
messages name the key or the path of a problem and never repeat a value, so a secret written in the
wrong place does not reach a terminal or a log.

## Quick start with the simulated machine

`examples/simulated` runs two simulated presses against a Laraplate on `http://127.0.0.1:8000`:

```bash
cd examples/simulated
export LARAPLATE_EDGE_TOKEN=...        # the token of the source's service account
../../laraplate-edge run --bootstrap bootstrap.yaml
```

The buffer and the applied configuration go under `examples/simulated/data`. In another terminal:

```bash
./laraplate-edge status
```

To run against a real local Laraplate, see "Running against a local Laraplate" below.

## The bootstrap file

The small local file the agent reads before anything else: `/etc/laraplate-edge/bootstrap.yaml` on
Linux, `%ProgramData%\laraplate-edge\bootstrap.yaml` on Windows by convention (the path is always given
with `--bootstrap`). One YAML document; an unknown key is an error.

```yaml
agent_id: plant-a-edge-01
config:
  provider: laraplate                 # or: file
  file: /etc/laraplate-edge/config.yaml
  laraplate:
    url: https://erp.example.com
    token: secret://file/etc/laraplate-edge/token
    poll_seconds: 60
data_dir: /var/lib/laraplate-edge
status_listen: 127.0.0.1:9180
status_token: secret://env/LARAPLATE_EDGE_STATUS_TOKEN
```

| Key | Required | Default | Meaning |
|---|---|---|---|
| `agent_id` | yes | | Name of this agent in logs and `/status`. Laraplate identifies the agent by its token, not by this name. |
| `config.provider` | yes | | `file` (a local document) or `laraplate` (polled from the backend). |
| `config.file` | with `file` | | Path of the configuration document, YAML or JSON. Relative paths are relative to the agent's working directory. |
| `config.laraplate.url` | with `laraplate` | | Laraplate base URL: `https`, or `http` only for a loopback host (`127.0.0.1`, `::1`, `localhost`); no user or password in it. |
| `config.laraplate.token` | with `laraplate` | | `secret://` reference to the token of the source's service account. A plain value is refused. |
| `config.laraplate.poll_seconds` | no | `60` | Configuration polling interval, at least `5`. |
| `data_dir` | no | `/var/lib/laraplate-edge`, on Windows `%ProgramData%\laraplate-edge` | Buffers, applied configuration, provider state. Created with mode `0700` when missing. Relative paths are relative to the working directory. |
| `status_listen` | no | `127.0.0.1:9180` | `host:port` of the status endpoint; port `0` picks a free port (logged at start). |
| `status_token` | no | | `secret://` reference to the read-only bearer token of the status endpoint. Required when `status_listen` is not a loopback address. |

### Secret references

Secrets never appear as values, in the bootstrap file or in the configuration document:

| Reference | Resolves to |
|---|---|
| `secret://env/NAME` | the environment variable `NAME`, which must be set and not empty |
| `secret://file/PATH` | the content of a file, one trailing newline removed, at most 1 MiB. On Linux and macOS the path is `/PATH` (`secret://file/etc/laraplate-edge/token` reads `/etc/laraplate-edge/token`) and the file must not be readable or writable by group or others (`chmod 600`). On Windows the path is taken as written. |
| `secret://keystore/NAME` | the OS keystore: **not available in this build**, refused at validation |

The output and provider tokens are resolved when they are used: at the first attempt, after a
suspension, on `SIGHUP`, and, for the `laraplate` provider, at every poll. What a new resolution can
see depends on the kind:

- `secret://file/...`: the file is read again each time, so a rotated token needs only a new file
  content and a `SIGHUP` (see "Signals"), without a restart. Prefer it for tokens that rotate.
- `secret://env/...`: the agent reads its own environment, which is fixed when the process starts; a
  variable exported or changed afterwards is never seen. A new or rotated value needs a restart (for
  systemd: change `Environment=` or the `EnvironmentFile=`, then restart the service).

The status token is resolved once at start; rotating it needs a restart.

## Signals and lifecycle

- **SIGTERM, SIGINT:** clean stop. The agent stops polling, stops the status endpoint, stops the
  inputs, then the outputs, and syncs and closes every buffer. Under systemd it sends `STOPPING=1`.
- **SIGHUP:** reloads the bootstrap file. A file that no longer loads is reported and changes nothing.
  Otherwise every output resolves its token again and leaves `suspended_auth` at once (a
  `secret://file/` token is read again; a `secret://env/` one keeps the value the process started
  with), and the `file` provider rereads its document. Every other changed key (`agent_id`, `data_dir`, `status_listen`,
  `status_token`, `config.*`) is logged at `WARN` and applies at the next restart.

At start the agent creates `data_dir` if needed, applies the last valid configuration it kept (so it
runs offline from its last document), then hands over to the provider.

### The data directory

```
data_dir/
  format                        layout version of data_dir (1); an agent refuses a newer one
  config/last-valid.json        the last applied document, canonical JSON
  config/laraplate-state.json   laraplate provider: last attempted version and undelivered reports
  inputs/{input id}/            removed when the applied document has no input of that id
    transform-state.json        the edge_counter counts of the input's transform chains
  outputs/{output id}/
    critical/  bulk/            the two queues: segment files and segment_size
    acked                       confirmed position of each queue
    losses                      loss markers not yet sent
    inflight                    the sealed batch being sent and the next source_seq
```

Do not edit these files while the agent runs. A removed output's directory is kept, with a warning
when it still holds unsent records; delete it by hand once it is no longer needed.

## Configuration providers

- **`file`:** the document is read at start and whenever its modification time or size changes
  (checked every 2 seconds), and on `SIGHUP`. A document that does not parse or is refused is logged
  with its messages and the running configuration stays. The same `version` applies again, so hand
  edits work without a bump; a lower one is refused.
- **`laraplate`:** polls `GET {url}/api/v1/detail/mes/machine-agent-configs` at start, then every
  `poll_seconds`, with `?version=N` for the version it runs (or the last one it attempted, when that
  attempt failed). `304` means no change. A new document is applied and its outcome (`applied`,
  `rejected`, `rolled_back`, with the errors and the agent version) is posted to
  `{url}/api/v1/insert/mes/machine-agent-config-reports`. A report that cannot be delivered is kept in
  `data_dir` and sent again at every poll, also after a restart, until the backend records it. A
  rejected or rolled back version is reported once and not attempted again; publish a new version.

## Trying transforms: probe and config test

Two commands let a technician see what a machine sends and what a transform chain makes of it, without
running the agent or sending anything.

**`probe <input> (--config <file> | --bootstrap <path>) [--record <file>]`** reads every point of one
input once and prints the raw samples, before any transform:

```bash
laraplate-edge probe press-line --config config.yaml --record samples.jsonl
```

```
press-07	state	"idle"	string	good
press-07	good_count	0	integer	good
press-07	press_force	182	float	good
...
response time: 3µs
```

- The document is the `--config` file, checked like `config validate` (its problems are printed and the
  exit code is 1); relative paths in its `connection` resolve against the file's directory. Without
  `--config`, it is the last valid document kept in the `data_dir` of the `--bootstrap` file. With both,
  `--config` wins.
- The input is built through the same drivers as `run`, read once (at most 30 seconds) and closed. One
  line per sample, tab-separated: device, signal, value (a string quoted), kind (`boolean`, `integer`,
  `float`, `string`), quality; then the total response time. `probe` never starts the input, an output or
  the status endpoint. It opens its own connection to the machine: for a network transport (`tcp`,
  `rtuovertcp`) beside a running agent's; for a serial port see the `rtu` advice below.
- `--record <file>` also writes the samples, as a sample file (below), atomically.
- A `modbus` input is probed through a connection of its own (with `tcp` and `rtuovertcp`, beside a
  running agent's; for `rtu`, see the end of this item): it reads every block once and prints one line
  per point. A `bad` sample appears for the points of a block the device refused with exceptions 1 to 4
  (an unmapped address, for one) and for a value that does not decode. The probe stops with an error, and
  exit code 1, when the device cannot be reached, does not answer within `timeout`, answers busy
  (exception 5 or 6) or with an exception code the driver does not know, sends an answer that cannot be
  read (bad CRC, protocol error) or sits behind a gateway that fails (10 or 11); try again for a busy
  device. On `rtu` the probe opens the serial port itself: stop the agent before probing an `rtu` input,
  because both would read the same port and corrupt each other's frames (on Windows the second open is
  refused).
- An input id the document does not have exits 1 with the ids it has.

**`config test <file> --input <samples.jsonl>`** replays recorded samples through the transform chains
of a document and writes what comes out to stdout, as a sample file:

```bash
laraplate-edge config test config.yaml --input samples.jsonl
```

- The document is checked with every validation layer first (exit 1 with its messages).
- The samples are applied in file order on a virtual clock that starts at the first sample's `ts`.
  Before a sample, every pending deadline before its `ts` fires in time order, the clock moving to each,
  so a `heartbeat` repeats across a gap; a deadline equal to the sample's `ts` comes after the sample, as
  in the running agent (a `heartbeat` the sample re-arms resends nothing). A sample whose `ts` is earlier than the clock does not move the
  clock back: it is applied as it comes, with its own `ts`, so time-driven steps may treat it as late;
  keep the file in time order. After the last sample the deadlines fire up to a horizon: the later of
  the last sample's `ts` and the clock, plus the longest `heartbeat`, `min_interval` or `debounce`
  duration of the document, so held samples come out and a `heartbeat` stops. The output is in emission
  order; a sample of a point without transforms passes through.
- Nothing else is read or written: the same files always give the same output. No state file is used,
  so `edge_counter` counts start from 0.
- Runtime errors appear as `bad` samples, and their count goes to stderr (`laraplate-edge config test:
  transform errors: N (the samples are marked bad)`); the exit code stays 0.
- Exit 1 for a document or sample file that cannot be used, and for a replay that would need more than
  1,000,000 firings (a very short period over a long gap); 2 on a usage error, such as a missing
  `--input`.

**Sample files** are JSON Lines, one object per sample with the keys `device`, `signal`, `ts`, `value`
and `quality`, all required (no other key is accepted; key names are matched without regard to case):

```
{"device":"press-07","signal":"press_force","ts":"2026-10-19T08:00:01.250Z","value":182.5,"quality":"good"}
```

- `ts` is RFC 3339; it is written in UTC with milliseconds, and read with any offset or precision.
- `value` has the kind the point's `type` gives (CONFIGURATION.md, "Kinds and the type check"): `true`
  or `false`, an integral number for an integer point, a finite number for a float point, a JSON string.
- `quality` is `good`, `uncertain` or `bad`. Blank lines are skipped; a line holds at most 1 MiB.
- The device and signal must be a point of the document. The first bad line stops the command with
  `<file>: line N: <reason>`, never quoting the line.

## Status endpoint

Read-only HTTP on `status_listen`, `GET` and `HEAD` only.

| Path | Answer |
|---|---|
| `/healthz` | `200 ok` while the process serves; never needs the token |
| `/readyz` | `200 {"ready":true}` once a configuration is applied and every input runs; otherwise `503 {"ready":false,"reason":"..."}` |
| `/status` | JSON: `agent_id`, `agent_version`, `uptime_seconds`, `config_applied`, `config_version`, `inputs` (`id`, `driver`, `state` `running` or `failed`, `last_error`, `transform_errors`, and for an input that reports its health, such as `modbus`, `connected` and `read_errors`, the counts since the input was last started, restarting from 0 when its configuration changes or a rollback restarts it, always the four kinds `timeout`, `connection`, `exception` and `decode`, at 0 until the first error) and `outputs` (`id`, `type`, `state`, `clock_offset_seconds`, `buffer.critical` and `buffer.bulk` with `records`, `bytes`, `oldest_unconfirmed_age_seconds`, `dropped` by reason, `corrupt_records`, `last_confirmation`, `last_error`, `last_error_at`) |
| `/metrics` | Prometheus text format 0.0.4, below |

With `status_token` set, every path but `/healthz` needs `Authorization: Bearer <token>`. The endpoint
is plain HTTP: on a non-loopback address the bearer crosses the network in clear, so bind a monitoring
network you trust (TLS on the endpoint is a later plan). If the status listener fails while the agent
runs, the error is logged and the agent keeps running without it.

### Metrics

Every name starts with `laraplate_edge_`.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `laraplate_edge_config_version` | gauge | | version of the applied document, 0 when none |
| `laraplate_edge_input_up` | gauge | `input` | 1 when the input runs, 0 when it failed |
| `laraplate_edge_input_connected` | gauge | `input` | 1 when the input's connection to the machine is up, 0 when it is down |
| `laraplate_edge_input_read_errors_total` | counter | `input`, `kind` | read errors since the input was last started, by `kind`: `timeout` (no answer in time), `connection` (refused, reset or closed, or an invalid frame), `exception` (the device answered a Modbus exception), `decode` (a value that does not decode); all four series exist from the start, at 0, and restart from 0 when the input's configuration changes or a rollback restarts it, a counter reset that `rate()` and `increase()` handle |
| `laraplate_edge_transform_errors_total` | counter | `input` | runtime errors of the input's transform chains since the agent started, across the restarts of the input; each one sent its sample `bad` (CONFIGURATION.md, "Transforms"). The series of a removed input disappears. |
| `laraplate_edge_buffer_records` | gauge | `output`, `priority` | records not yet confirmed |
| `laraplate_edge_buffer_bytes` | gauge | `output`, `priority` | bytes of the queue on disk |
| `laraplate_edge_buffer_oldest_unconfirmed_seconds` | gauge | `output`, `priority` | age of the oldest unconfirmed record: agent clock minus the record's device `ts`, 0 when none |
| `laraplate_edge_dropped_samples_total` | counter | `output`, `reason` | samples dropped with a loss marker since the buffer was opened (`buffer_full`, `rejected_invalid`); a dropped device `birth` or `death` is not a sample and is not counted |
| `laraplate_edge_corrupt_records_total` | counter | `output` | corrupt records found since the buffer was opened, at the opening or while it runs; each one dropped the rest of its segment |
| `laraplate_edge_last_confirmation_timestamp_seconds` | gauge | `output` | Unix time of the last confirmed batch, 0 before any |
| `laraplate_edge_output_state` | gauge | `output`, `state` | 1 for the current state among `sending`, `backoff`, `suspended_auth`, `failed` |
| `laraplate_edge_clock_offset_seconds` | gauge | `output` | backend clock minus agent clock, from the `Date` header of the last response |
| `laraplate_edge_last_error_timestamp_seconds` | gauge | `output` | Unix time of the output's last error, 0 when none; the text is in `/status` |

The connection and read error series exist only for inputs that report their health (`modbus`, not `simulated`). The buffer series and the drop counters exist only for outputs that run (a `failed` output has no open
buffer). Counters restart from 0 when the agent restarts.

## Logs

One JSON object per line on stdout (`time`, `level`, `msg`, then attributes), for journald or the
service manager to collect and rotate. `--log-level` takes `debug`, `info` (default), `warn` or
`error`; the JSON `level` field is written in upper case: `DEBUG`, `INFO`, `WARN`, `ERROR`.

- At `info` and above the agent never logs a secret or a sample value. As a second line of defence any
  attribute whose key ends with `token`, `password`, `secret` or `api_key` is written as `[redacted]`.
- `--log-level debug` adds the details of each batch and the backend's `422` answer, which may quote
  sample values. It never logs secrets. It does not expire by itself (the expiring `debug --for` is a
  later plan): set it back to `info` after the investigation.
- On a disk that refuses every write the buffer logs its first failed write and its first dropped
  record of each queue in full, then at most one summary a minute (`write and drop failures repeated
  since the last report`, with `suppressed` and a count per `<queue>.<kind>`); after a quiet minute the
  next failure is logged in full again. The router still logs each record the buffer refuses at
  `WARN` ("router: cannot buffer a record", with the output, device and signal).
- A transform runtime error is logged at `WARN` the first time for each device, signal and step of an
  episode ("transform: runtime error, the sample is sent bad", with `input`, `device`, `signal`, `step`,
  `transform` and `error`, never the value); later ones are only counted, in at most one summary a
  minute ("transform: runtime errors repeated since the last report", with `suppressed` and
  `window_seconds`). A minute without errors ends the episode: the summary of the errors only counted
  is logged then, without waiting for another error. The count is in
  `laraplate_edge_transform_errors_total` and `/status`.

## Running under systemd

The agent speaks `sd_notify` without any library when `NOTIFY_SOCKET` is set (Linux only):

- `READY=1` once the last valid configuration is applied at start, or else once the provider's first
  attempt ended, whatever its outcome;
- `WATCHDOG=1` every half of `WATCHDOG_USEC` while the agent runs;
- `STOPPING=1` when the shutdown starts.

A unit sketch (packages with a sandboxed unit are a later plan):

```ini
[Service]
Type=notify
User=laraplate-edge
ExecStart=/usr/local/bin/laraplate-edge run --bootstrap /etc/laraplate-edge/bootstrap.yaml
ExecReload=/bin/kill -HUP $MAINPID
WatchdogSec=30s
TimeoutStartSec=90s
Restart=on-failure
```

`TimeoutStartSec` must leave about 60 seconds for the first `laraplate` poll (each request is bounded
at 30 seconds) on a first start without a last valid document. The watchdog pings come from a timer:
they prove the process is alive, not that every component makes progress.

## Network

The agent only opens outbound connections to the URLs of its bootstrap and configuration document; it
sends nothing anywhere else. HTTPS uses the system root certificates and TLS 1.2 or later; certificate
verification cannot be disabled. `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY` are honoured (loopback
addresses are never proxied). A custom CA bundle for networks that inspect TLS is not available in this
build.

## Troubleshooting

`/status` shows each output's `state` and `last_error`; the logs give the details. The backend's side
is documented in `laraplate/Modules/MES/docs/MACHINE_CONNECTIVITY.md`.

| Symptom | Meaning | What to do |
|---|---|---|
| output `suspended_auth`, `last_error` `HTTP 401: token refused` | the token is missing, invalid, expired or revoked | issue a new token on the service account's user page and update the secret: with `secret://file/`, write the new token and send `SIGHUP`; with `secret://env/`, set the new value in the service environment and restart the agent. The agent keeps buffering and tries again every 5 minutes on its own. |
| output `suspended_auth`, `last_error` `HTTP 403: token refused` | the backend API is off (Core `expose_api`), the account or token lacks the permission (role `mes-machine-agent`), the address is outside the token's `allowed_cidrs`, the account is linked to no source, the source is inactive, or it is not an HTTP `canonical` source | fix it in Laraplate, then `SIGHUP` or wait for the next 5-minute retry. Nothing is lost while suspended, until the buffer is full. |
| output `suspended_auth`, `last_error` `token unavailable: ...` | the secret reference does not resolve (variable unset or empty, file missing or readable by others) | a `secret://file/` token: fix the file, then `SIGHUP`. A `secret://env/` token: the variable must be set in the environment the agent starts with; set it and restart the agent (a `SIGHUP` cannot see it). |
| `WARN` "the backend refused the batch as too large; split" | `413`: the batch exceeds the backend's `mes.machine.max_samples` or `mes.machine.max_body_kb` | nothing: the agent splits the batch and the first half keeps its `source_seq`. Lower `batch.max_samples` or `batch.max_body` if it happens often. A single record that is still too large is dropped with a `rejected_invalid` loss marker. |
| `WARN` "the backend refused the batch as invalid; dropping it ..." | `422`: the envelope failed the backend's validation; the field paths are logged | a defect to report with the log line. The batch is dropped with a `rejected_invalid` loss marker, so the backend records the gap. `--log-level debug` logs the backend's answer. |
| output `backoff`, `last_error` `HTTP 429` | rate limited (`mes.machine.rate_limit_per_minute`, 600 a minute per service account) | nothing, or raise the limit or `batch.max_samples`: the agent backs off, honouring `Retry-After` |
| output `backoff`, `last_error` `HTTP 5xx` or `network error: ...` | the backend or the network is down | nothing: backoff from 1 second to 5 minutes with jitter, the batch is resent identical, the buffer holds the data |
| `ERROR` "unexpected backend response; check the output url ..." | a `3xx` or another `4xx`, often a wrong `url` or a proxy page | fix `url`; the agent retries with backoff and never drops data for it |
| `WARN` "the agent clock differs from the backend's; check NTP" | offset above 2 seconds | fix NTP on the gateway; `laraplate_edge_clock_offset_seconds` shows the offset |
| `WARN` "the backend refused the agent token or its permissions" (provider) | `401`/`403` on the configuration route | as for the output; the provider tries again at the next poll |
| `INFO` "the backend has no active configuration for this agent" | `404`: nothing published for the source | publish a configuration on the source page in Laraplate |
| `/status` `connected` false for a `modbus` input, `read_errors` `connection` or `timeout` growing, devices reported dead after `death_tolerance` | the agent cannot reach the device or gets no answer (after timeouts alone, `connected` turns false only once no unit of the input has answered for `death_tolerance`) | check from the gateway: the `host` and `port` (the device may use another port than 502), the cable and the firewall between the two, that no other master holds a device that accepts one TCP client (many PLCs do), and the `unit_id`; for a serial-to-Ethernet gateway, its serial settings and the `unit_id` of the device behind it. Try `probe` for the exact failure. The agent reconnects on its own with backoff up to 1 minute and reports the devices born again at the first successful read. |
| `/status` `connected` true for a `modbus` input, but one device reported dead and `read_errors` `timeout` growing | on a shared serial line or behind a gateway, one unit does not answer: it is off, its unit id is not the configured `unit_id` or `@unit`, or its wiring or serial settings are wrong | the other units keep being read at their intervals and the connection is kept: it is reopened only on an I/O error or when no unit has answered for `death_tolerance`. Check that device and its unit id; `timeout` grows by one at every poll of each of its blocks. Each such timeout holds the line for `timeout`, so keep `timeout` short on a shared line. |
| a `modbus` block always `bad`, `read_errors` `exception` growing | the device refuses the request with exception 1 to 4: usually one address of the block is not mapped, or the area is wrong (`hr` for `ir`) | a block is one request, so a single unmapped address fails all its points. Split the points around the unmapped address (leave a gap of at least one address between them, or give them another `poll` interval) and check the area and the offset in the device manual. `probe` shows which points are `bad`. |
| a `modbus` value is wrong, a huge number or noise | the byte order or the offset is wrong | try the other `byte_order` values (a `float32` from a PLC is often `CDAB`); offsets are zero-based, so the legacy `40011` is `hr:10`; a point read one register off pairs unrelated words. Check with `probe`. A `decode` error is a value that cannot be carried (`uint64` above 9223372036854775807, NaN or infinity, a string that is not UTF-8). |
| the document is `rolled_back` with `cannot open the serial port` | the `rtu` input could not open the port: wrong `device` path, or the user has no permission (on Windows also a port another process already has open) | check the path (`ls -l /dev/ttyUSB0`, adapters change number when replugged: prefer `/dev/serial/by-id/...`), and that the agent's user belongs to the group that owns the port (`dialout` on most Linux distributions). On Linux a second open of the port succeeds, so never run `probe` on an `rtu` input while the agent runs: both would read the same port and corrupt each other's frames. |
| `/readyz` `503` "input ... is failed" | an input could not start again during a rollback | see its `last_error`; it is retried only when a new document is applied |
| start fails with `data_dir ... has format N, newer than this agent supports` | the data directory was written by a newer agent | run the newer agent; an older one never rewrites a newer layout |
| `ERROR` "buffer: corrupt record inside a segment, dropped the rest of the segment" (or "... inside an evicted segment ..."), `corrupt_records` grows | a record of the output buffer failed its checksum or could not be read back: a disk fault, bit rot, a file changed by hand. It is found at start or while the agent runs. | nothing for the agent: it drops the rest of that segment (at most 16 MiB of that queue), counts it and keeps sending; the log line names the queue, the segment and the offset, never a value. The gap has no loss marker (the protocol has no reason for it), so the backend does not see it: check the disk (kernel log, SMART, file system). |
| `WARN` "a device death was dropped; the backend will not see it" | a `death` was lost with a full buffer or a refused batch; a `birth` or `death` is not a sample, so no loss marker counts it | nothing: the next `death` or `birth` of the device sets its state again. Look at the drop that caused it. |
| `WARN` "transform: runtime error, the sample is sent bad", `transform_errors` grows | a step of the logged device, signal and `step` cannot compute a sample; `error` says why: `no mapping for the value` (`map`), `no match` or `capture cannot be converted` (`regex_extract`), `invalid BCD digit` or `negative value is not BCD` (`bcd`), `result is not finite` (`scale`, `offset`), `count cannot be saved` (`edge_counter`) | the samples go on, sent `bad`. Record what the machine sends with `probe --record`, fix the chain (a missing `map` key or a `default`, the pattern) and check it with `config test` before publishing it. |
| `ERROR` "transform: the state file cannot be read and was renamed aside; edge counters start from 0" | `inputs/{id}/transform-state.json` was truncated, corrupt or of another version | nothing for the agent: the input runs, its `edge_counter` counts restart from 0 (the backend reads a counter reset). Keep the `transform-state.json.corrupt-<unix seconds>` file named in the log for the report and check the disk. |
| `ERROR` "transform: the state file cannot be written; dropped counters stay in it until the next count" | the state file could not be rewritten at start without the counts no chain uses any more | check the permissions and the free space of `data_dir/inputs/{id}`; the next count writes the file again. Until then, a restart with the old chain back could resume its old count. |
| `WARN` "supervisor: cannot remove the transform state of an input not in the configuration" | the directory `data_dir/inputs/{id}` of an input the applied document does not have could not be removed (`error` names it) | check the permissions of `data_dir/inputs`; the next applied document, or the next start, tries again. Until then, an input of that id added back would resume its old counts. |
| `WARN` "supervisor: cannot remove the transform state of an input restarted by a rollback" | a rollback restarted an input the failed document did not have (a rename rolled back), and its directory `data_dir/inputs/{id}` could not be removed (`error` names it) | check the permissions of `data_dir/inputs`; the input then resumes its old counts, which the backend may read as parts produced after the failed attempt counted the same signal under the new id |

**A corrupt state file.** The small state files of `data_dir` are written atomically, so a crash never
leaves one half written; a disk fault or a hand edit can. An input's `transform-state.json` is moved
aside by the agent itself (see the table above). An output whose state file does not load does not
start: the document that starts it is `rolled_back` (at start nothing runs and `/readyz` answers
`503`), with an error naming the output and the file. Stop the agent, move the file aside (keep it for
the report), start again:

| Error in the outcome or the log | File to move aside | What is lost |
|---|---|---|
| `outputs.N: output: parse inflight: ...`, `output: inflight ...` | `outputs/{id}/inflight` | `source_seq` restarts at 0 and the batch in flight is sealed again under a new `message_id`: if the backend had accepted it, its samples are stored twice. The backend raises no gap for a lower `source_seq`, and its gap check stays blind until the new numbering passes the old one. |
| `outputs.N: buffer: parse losses: ...`, `buffer: losses ...` | `outputs/{id}/losses` | the pending loss markers: the backend never hears of those drops |
| `outputs.N: buffer: parse acked: ...`, `buffer: acked has version ...` | `outputs/{id}/acked` | the confirmed positions: every record still in the queues is sent again under new message ids, so the samples already delivered are stored twice |
| `outputs.N: buffer: critical queue: ... segment_size ...` (or `bulk`) | `outputs/{id}/critical/segment_size` (or `bulk/`) | nothing in this build: every queue was created with 16 MiB segments, the size written again |
| `ERROR` "provider: cannot read the last valid configuration" with `config: ... last valid document ...` | `config/last-valid.json` | the offline start: the agent runs nothing until the provider brings a document (the `file` provider at once, `laraplate` at its first answer), which writes the file again |
| `WARN` "provider: cannot read the provider state ..." with `laraplate-state.json is corrupt` | `config/laraplate-state.json` (optional: the agent already goes on without it and rewrites it at the next report) | the undelivered configuration reports and the last attempted version: a rejected or rolled back version may be attempted, and reported, once more |

## Running the end-to-end test

The end-to-end test builds the binary and runs it as a subprocess against a fake Laraplate that validates
every envelope against the backend's `laraplate-machine-1.schema.json` (Unix only):

```bash
go test -tags e2e -race ./internal/e2e
```

It covers a network cut (`503`, then dropped connections, across several backoff steps), two `kill -9`
with restarts on the same `data_dir` (a batch held unanswered, and a batch accepted whose answer never
left), the `laraplate` provider applying version 1, then version 2, reporting both and polling with
`304` afterwards, and a run with transform chains (`map` on `state`, `scale` and `deadband` on
`press_force`), whose samples are checked against companion points without a chain and whose births
announce the chains' data types. It checks that `source_seq` has no gap and is never reused, that every
resend is byte-identical, that the simulated counters arrive as 0, 1, 2, ... with nothing lost or
repeated, and that the four status endpoints answer. A separate run has a `modbus` input read the Modbus TCP server
of the Go library, started by the test: it checks the decoded values (a `float32` in byte order `CDAB`, a
negative `int16`, a string, a coil, a unit override, a transform chain), the births, that no write is ever
sent, the death of both devices after the death tolerance when the server stops, `connected` and
`read_errors` in `/status` and the metrics, and the births and data again when the server is back. It is not part of `go test ./...`; CI runs it in
its own job on Linux amd64.

## Running against a local Laraplate

Optional, by hand, with Laraplate and its MES module running on `http://127.0.0.1:8000`:

1. Seed the MES module (`MESDatabaseSeeder` creates the `api` role `mes-machine-agent`) and turn on
   Core's `expose_api` setting.
2. Create a service account (a user marked as service account) in the Core user resource and give it
   the `api` role `mes-machine-agent`.
3. Create a machine source with transport `http` and normalizer `canonical`, linked to that account.
4. On the service account's user page, issue an API token with the role's three abilities and
   `allowed_cidrs` that include `127.0.0.1`. It is shown once.
5. `export LARAPLATE_EDGE_TOKEN=<token>` and run `examples/simulated` as in the quick start. To try the
   `laraplate` provider instead, publish the document of `examples/simulated/config.yaml` as JSON on the
   source page, with the scenario inline (`connection.scenario`) or `scenario_file` as an absolute path,
   and point a bootstrap with `provider: laraplate` at `http://127.0.0.1:8000`.
6. Check `laraplate-edge status` and the source's messages and agent reports in Laraplate.
