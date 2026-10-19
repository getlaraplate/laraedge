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
  provides. Every problem is reported with its path (`outputs.0.batch.max_samples: must be at most
  5000`), never with its value.
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
| `driver` | yes | | `simulated` in this build. The schema also knows `opcua`, `modbus`, `mqtt`, `serial`, `file` and `plugin:<id>`. |
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
| `type` | no | | decoding type: `bool`, `int16`, `uint16`, `int32`, `uint32`, `int64`, `uint64`, `float32`, `float64`, `string`. It sets the data type announced in the device's `birth`: `bool` is `boolean`, `string` is `string`, any other type (or none) is `number`. |
| `byte_order` | no | | `ABCD`, `CDAB`, `BADC`, `DCBA` (drivers that decode registers) |
| `unit` | no | | engineering unit, at most 16 characters, announced in the `birth` |
| `transforms` | no | | refused by this build |

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
`source_seq`); changed ones restart; removed ones stop (an output's directory is kept); added ones
start. While a changed output restarts, the records routed to it wait in memory (at most 10,000; beyond
that they become a `buffer_full` loss marker).

Outcomes, as reported to Laraplate:

- `rejected`: the document is invalid, older than the applied one, or asks for something this build
  does not provide. Nothing changed.
- `rolled_back`: a component did not start (an input has 30 seconds). What the apply started is stopped
  and what it stopped is started again.
- `applied`: the document runs and is kept as `data_dir/config/last-valid.json`, from which the agent
  starts next time.

## What this build refuses

`config validate` and every apply refuse these parts, valid in the schema, with
`<path>: not available in this build`:

| Part | Comes with |
|---|---|
| drivers other than `simulated` | the driver plans, in the design's order: `opcua` and `modbus`, then `mqtt`, `serial` and `file` |
| output types other than `laraplate-http` | the MQTT output plan (`laraplate-mqtt`) and the plugin plan (`plugin:<id>`) |
| `plugins`, `plugin:<id>` drivers and outputs | the plugin plan (design section 8) |
| `transforms` of points and of derived signals, `derived` signals | the transforms and CEL expressions plan (design section 7) |
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

- No custom CA bundle: HTTPS trusts the system roots only, so a network that inspects TLS cannot be
  used yet.
- A component left `failed` by a rollback is not retried until a new document is applied.
- The records held in memory for a `failed` output are discarded at shutdown, with a warning giving
  their count and `ts` range per priority.
- The records held in memory while a changed output reopens (at most 10,000) are lost without a loss
  marker if the agent crashes before the reopened buffer takes them, for instance during a slow reopen
  of a large buffer.
- YAML flow style at the top level of a document is read as JSON and refused; use block style.
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
