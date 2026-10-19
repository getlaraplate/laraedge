# Using laraplate-edge

This guide is for whoever builds, installs and runs the agent. The configuration document (inputs,
outputs, buffer) is described in [CONFIGURATION.md](CONFIGURATION.md). The design is in the stack:
`docs/superpowers/specs/2026-10-12-laraplate-edge-agent-design.md`.

What this build provides: the `simulated` input driver, the `laraplate-http` output, the disk buffer
with loss markers, the `file` and `laraplate` configuration providers, the status endpoint, JSON logs
and the systemd watchdog. Real drivers, transforms, the MQTT output, plugins, the OS keystore and
packages come with later plans (see "What this build refuses" in CONFIGURATION.md).

## Build

Go 1.27 or newer, no CGO, one static binary:

```bash
go build -o laraplate-edge ./cmd/laraplate-edge
./laraplate-edge version
```

Set the reported version at link time:

```bash
go build -ldflags "-X github.com/swolley/laraplate-edge/internal/version.version=1.2.3" \
  -o laraplate-edge ./cmd/laraplate-edge
```

Without it the version is `dev`. The version is sent to Laraplate in every configuration report and in
the `User-Agent` header.

## Commands

| Command | What it does | Exit code |
|---|---|---|
| `run --bootstrap <path> [--log-level debug\|info\|warn\|error]` | runs the agent until SIGTERM or SIGINT; logs at `info` by default | 0 after a clean stop, 1 when it cannot start, 2 on a usage error |
| `config validate <file>` | checks a configuration document with both validation layers (schema and rules, then what this build provides); applies nothing | 0 and `<file>: valid` on stdout, or 1 and one `<file>: <path>: <problem>` line per problem on stderr |
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
suspension, on `SIGHUP`, and, for the `laraplate` provider, at every poll. A rotated token therefore
needs only a `SIGHUP` (see "Signals"). The status token is resolved once at start; rotating it needs a
restart.

## Signals and lifecycle

- **SIGTERM, SIGINT:** clean stop. The agent stops polling, stops the status endpoint, stops the
  inputs, then the outputs, and syncs and closes every buffer. Under systemd it sends `STOPPING=1`.
- **SIGHUP:** reloads the bootstrap file. A file that no longer loads is reported and changes nothing.
  Otherwise every output resolves its token again and leaves `suspended_auth` at once, and the `file`
  provider rereads its document. Every other changed key (`agent_id`, `data_dir`, `status_listen`,
  `status_token`, `config.*`) is logged at `warn` and applies at the next restart.

At start the agent creates `data_dir` if needed, applies the last valid configuration it kept (so it
runs offline from its last document), then hands over to the provider.

### The data directory

```
data_dir/
  format                        layout version of data_dir (1); an agent refuses a newer one
  config/last-valid.json        the last applied document, canonical JSON
  config/laraplate-state.json   laraplate provider: last attempted version and undelivered reports
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

## Status endpoint

Read-only HTTP on `status_listen`, `GET` and `HEAD` only.

| Path | Answer |
|---|---|
| `/healthz` | `200 ok` while the process serves; never needs the token |
| `/readyz` | `200 {"ready":true}` once a configuration is applied and every input runs; otherwise `503 {"ready":false,"reason":"..."}` |
| `/status` | JSON: `agent_id`, `agent_version`, `uptime_seconds`, `config_applied`, `config_version`, `inputs` (`id`, `driver`, `state` `running` or `failed`, `last_error`) and `outputs` (`id`, `type`, `state`, `clock_offset_seconds`, `buffer.critical` and `buffer.bulk` with `records`, `bytes`, `oldest_unconfirmed_age_seconds`, `dropped` by reason, `corrupt_records`, `last_confirmation`, `last_error`, `last_error_at`) |
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
| `laraplate_edge_buffer_records` | gauge | `output`, `priority` | records not yet confirmed |
| `laraplate_edge_buffer_bytes` | gauge | `output`, `priority` | bytes of the queue on disk |
| `laraplate_edge_buffer_oldest_unconfirmed_seconds` | gauge | `output`, `priority` | age of the oldest unconfirmed record: agent clock minus the record's device `ts`, 0 when none |
| `laraplate_edge_dropped_samples_total` | counter | `output`, `reason` | samples dropped with a loss marker since the buffer was opened (`buffer_full`, `rejected_invalid`) |
| `laraplate_edge_corrupt_records_total` | counter | `output` | corrupt records found when the buffer was opened |
| `laraplate_edge_last_confirmation_timestamp_seconds` | gauge | `output` | Unix time of the last confirmed batch, 0 before any |
| `laraplate_edge_output_state` | gauge | `output`, `state` | 1 for the current state among `sending`, `backoff`, `suspended_auth`, `failed` |
| `laraplate_edge_clock_offset_seconds` | gauge | `output` | backend clock minus agent clock, from the `Date` header of the last response |
| `laraplate_edge_last_error_timestamp_seconds` | gauge | `output` | Unix time of the output's last error, 0 when none; the text is in `/status` |

The buffer series and the drop counters exist only for outputs that run (a `failed` output has no open
buffer). Counters restart from 0 when the agent restarts.

## Logs

One JSON object per line on stdout (`time`, `level`, `msg`, then attributes), for journald or the
service manager to collect and rotate. Levels: `debug`, `info` (default), `warn`, `error`.

- At `info` and above the agent never logs a secret or a sample value. As a second line of defence any
  attribute whose key ends with `token`, `password`, `secret` or `api_key` is written as `[redacted]`.
- `--log-level debug` adds the details of each batch and the backend's `422` answer, which may quote
  sample values. It never logs secrets. It does not expire by itself (the expiring `debug --for` is a
  later plan): set it back to `info` after the investigation.

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
| output `suspended_auth`, `last_error` `HTTP 401: token refused` | the token is missing, invalid, expired or revoked | issue a new token on the service account's user page, update the secret, send `SIGHUP`. The agent keeps buffering and tries again every 5 minutes on its own. |
| output `suspended_auth`, `last_error` `HTTP 403: token refused` | the backend API is off (Core `expose_api`), the account or token lacks the permission (role `mes-machine-agent`), the address is outside the token's `allowed_cidrs`, the account is linked to no source, the source is inactive, or it is not an HTTP `canonical` source | fix it in Laraplate, then `SIGHUP` or wait for the next 5-minute retry. Nothing is lost while suspended, until the buffer is full. |
| output `suspended_auth`, `last_error` `token unavailable: ...` | the secret reference does not resolve (variable unset or empty, file missing or readable by others) | fix the secret, then `SIGHUP` |
| `warn` "the backend refused the batch as too large; split" | `413`: the batch exceeds the backend's `mes.machine.max_samples` or `mes.machine.max_body_kb` | nothing: the agent splits the batch and the first half keeps its `source_seq`. Lower `batch.max_samples` or `batch.max_body` if it happens often. A single record that is still too large is dropped with a `rejected_invalid` loss marker. |
| `warn` "the backend refused the batch as invalid; dropping it ..." | `422`: the envelope failed the backend's validation; the field paths are logged | a defect to report with the log line. The batch is dropped with a `rejected_invalid` loss marker, so the backend records the gap. `--log-level debug` logs the backend's answer. |
| output `backoff`, `last_error` `HTTP 429` | rate limited (`mes.machine.rate_limit_per_minute`, 600 a minute per service account) | nothing, or raise the limit or `batch.max_samples`: the agent backs off, honouring `Retry-After` |
| output `backoff`, `last_error` `HTTP 5xx` or `network error: ...` | the backend or the network is down | nothing: backoff from 1 second to 5 minutes with jitter, the batch is resent identical, the buffer holds the data |
| `error` "unexpected backend response; check the output url ..." | a `3xx` or another `4xx`, often a wrong `url` or a proxy page | fix `url`; the agent retries with backoff and never drops data for it |
| `warn` "the agent clock differs from the backend's; check NTP" | offset above 2 seconds | fix NTP on the gateway; `laraplate_edge_clock_offset_seconds` shows the offset |
| `warn` "the backend refused the agent token or its permissions" (provider) | `401`/`403` on the configuration route | as for the output; the provider tries again at the next poll |
| `info` "the backend has no active configuration for this agent" | `404`: nothing published for the source | publish a configuration on the source page in Laraplate |
| `/readyz` `503` "input ... is failed" | an input could not start again during a rollback | see its `last_error`; it is retried only when a new document is applied |
| start fails with `data_dir ... has format N, newer than this agent supports` | the data directory was written by a newer agent | run the newer agent; an older one never rewrites a newer layout |

## Running the end-to-end test

The end-to-end test builds the binary and runs it as a subprocess against a fake Laraplate that validates
every envelope against the backend's `laraplate-machine-1.schema.json` (Unix only):

```bash
go test -tags e2e -race ./internal/e2e
```

It covers a network cut (`503`, then dropped connections, across several backoff steps), two `kill -9`
with restarts on the same `data_dir` (a batch held unanswered, and a batch accepted whose answer never
left), and the `laraplate` provider applying version 1, then version 2, reporting both and polling with
`304` afterwards. It checks that `source_seq` has no gap and is never reused, that every resend is
byte-identical, that the simulated counters arrive as 0, 1, 2, ... with nothing lost or repeated, and
that the four status endpoints answer. It is not part of `go test ./...`.

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
