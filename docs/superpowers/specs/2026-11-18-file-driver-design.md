# File driver: design

**Status:** Approved (2026-11-18). Plan 1 (sample context and `file` with CSV, `../plans/2026-11-18-file-csv.md`) delivered 2026-11-18; plan 2 (Q-DAS) is open.

**Date:** 2026-11-18

**Parent spec:** `../../../../docs/superpowers/specs/2026-10-12-laraplate-edge-agent-design.md` (stack root),
sections 4, 9.1 and 9.2 (`file` row). This document settles what the parent leaves open for the `file` input
driver; where the two disagree, this one is newer and wins for this subject.

**Builds on:** `2026-10-19-transforms-modbus-opcua-design.md` (driver kit, D6 non-blocking start, `probe`,
`death_tolerance` wiring) and `2026-11-06-mqtt-driver-design.md` (the latest driver, whose conventions this one
follows: read error kinds, `probe` and `browse` output, state kept in `data_dir`).

---

## 1. Purpose and success

Coordinate measuring machines (CMM) and other measuring stations write one report per inspected part into a
directory: Zeiss Calypso, Hexagon PC-DMIS and Mitutoyo MCOSMOS all export CSV and the Q-DAS ASCII transfer format
(AQDEF). The `file` driver watches such a directory, reads each finished report once, and turns it into typed
samples, with the part's serial and order in the sample context. In Laraplate MES each measured characteristic is a
`measurement` signal pointing at a quality plan characteristic, so a report fills the quality check of the
operation that produced the part (`Modules/MES/docs/MACHINE_CONNECTIVITY.md`, "Probe measurements and quality
checks").

The design is generic: no plant and no real export files are available. Success is measured against files written
by the tests into temporary directories (unit and integration tests on the fake clock, one end-to-end test per
format with the real binary), in CI, with no hardware.

**Goals:** complete files dropped in a local or network directory; CSV and Q-DAS; each file read once, also across
restarts; the part's identity carried to the backend; a technician can see with `probe` and `browse` what the
driver would read before turning it on.

**Non-goals:** files that grow (log files a machine appends to: test benches, ovens, data loggers). They need a
read position kept across restarts, rotation and truncation handling, and none of the stability, `done/` and hash
rules below apply to them. They are a separate design when a plant needs them (section 10).

**Anti-goals:** reading a file before the machine finished writing it; sending one part's measurements twice
with different timestamps (the backend drops a measurement whose signal and timestamp are already stored,
`ProbeMeasurementRecorder::alreadyStored`, but a duplicate with another timestamp counts towards a quality check's
required samples); losing a report silently.

## 2. Approaches

How the driver notices new files was the one open choice of how to build it.

| | Approach | Trade-off |
|---|---|---|
| **A (chosen)** | **Polling only.** Every `interval` the driver lists the directory and compares each file's size and modification time with the previous listing. | One code path for local disks and network shares; no new dependency; testable on the fake clock, as the configuration provider `file` already is (`internal/provider/file.go`). Costs up to one `interval` of latency. |
| B | File system events (`fsnotify`, BSD-3-Clause) with polling as the fallback for network shares, as the parent spec's `file` row says. | Lower latency on a local disk, nothing else. |
| C | An external tool (inotify scripts, rsync, a sync agent) moves the files somewhere the agent reads. | Rejected at once: one more component for the plant to install and keep running, for nothing the driver cannot do itself. |

**Why not B.** The parent spec chose B; this design reverses it, for these reasons:

- **Events do not arrive from network shares.** SMB and NFS mounts deliver no inotify events for changes made by
  another host, which is exactly how a CMM PC writes to a share the agent's gateway mounts. ReadDirectoryChangesW
  on Windows works on SMB only when the server supports change notifications, and drops them silently when its
  buffer overflows. B would need polling anyway, and the plant cannot tell in advance which of the two paths a
  given directory takes.
- **Two paths, twice the tests, a switch between them.** B needs a rule for when to fall back to polling (on an
  error, on a mount type, on a timer) and tests for each path and for the switch. A failure that shows only on one
  path, in one plant, is the hardest kind to diagnose remotely.
- **An event does not mean a file is finished.** A file is read only after it has stayed unchanged for
  `stable_for` (section 5), so the driver must look at it again after an event in any case. With the default
  `stable_for` of 5 s the latency B saves (up to `interval`, 2 s by default) is small next to the wait it cannot
  avoid, and a CMM report is not urgent: it feeds a quality check, not a machine state.
- **Events overflow.** inotify queues are bounded (`max_queued_events`); on overflow the watcher must rescan the
  whole directory, which is the polling path again.
- **A new dependency** with platform-specific code for Linux, Windows and the BSDs, against the parent's rule to
  keep built-in drivers on pure Go and few dependencies (section 3).

A costs one directory listing every `interval`. For the expected directories (tens to hundreds of files, since
read files move to `done/`) that is negligible; `max_files_per_scan` and the listing of the top level only bound
it (section 5).

## 3. Decisions

| # | Topic | Decision |
|---|---|---|
| F1 | Scope and delivery | One spec, two plans in order: the sample context in the core plus `file` with `csv`, then `qdas` on the same base. |
| F2 | Detection | Polling only (section 2, approach A). |
| F3 | What a file is | A complete report, read whole, once. Growing files are out of scope (section 1). |
| F4 | Identity of a file | The SHA-256 of its content together with its modification time, kept in a ledger in `data_dir`. A file whose pair is in the ledger is never read again, whatever its name. The modification time is part of it because it is the timestamp of a report without one (F8): two byte-identical reports written at different times are two parts. A copy that keeps the modification time (a rename, a move) is the same report. |
| F5 | After reading | `after_read: move` (default) moves the file to `done/` or `error/` under the watched directory; `keep` leaves it in place and relies on the ledger alone, for read-only shares. |
| F6 | Sample context | New in the agent: an optional context per sample with the four keys the backend accepts (`serial`, `order_ref`, `operation_ref`, `lot`), mapped from the file. Built in plan 1, before the parser. |
| F7 | Death | The directory unreachable beyond `death_tolerance`. Silence never kills a device: a CMM may write nothing for hours. |
| F8 | Timestamp | A column or field of the file when configured, else the file's modification time, never the time of reading. |
| F9 | Delivery guarantee | At least once, without a change to the core: a file is committed (ledger, then move) only at a later scan, after its samples have had time to reach the disk; until then a crash or a reload reads it again, and the backend drops the repeated samples since their signal and timestamp are already stored (section 5). |
| F10 | Dependencies | CSV with the standard library `encoding/csv`; character sets with `golang.org/x/text/encoding/charmap` (BSD-3-Clause), already in the module graph as an indirect dependency, made direct. No other new dependency. |

## 4. Connection

`connection` of the input. An unknown field is an error named with its path, as for `modbus` and `mqtt`.

| Field | Default | Meaning |
|---|---|---|
| `directory` | | required: an absolute path, local or a mounted network share; read at the top level only, subdirectories are never entered |
| `pattern` | `*` | a glob on the file name (Go `path.Match`), e.g. `*.csv`; matched case-insensitively. Hidden files (leading `.`) and names ending in `.tmp`, `.part` or `~` are always skipped |
| `format` | | required: `csv` or `qdas` (plan 2). One input, one format |
| `interval` | `2s` | polling period, at least `500ms` |
| `stable_for` | the larger of `5s` and `interval` | how long size and modification time must stay unchanged before a file is read; at least `interval` (default amended 2026-11-18 by `../plans/2026-11-18-file-csv.md`: `5s` alone would be below an `interval` set above it) |
| `max_file_size` | `16MiB` | a larger file is not read: moved to `error/` (or left, with `keep`), kind `file_too_large` |
| `max_files_per_scan` | `100` | files read in one scan, oldest modification time first; the rest wait for the next scan, so a backlog never floods the buffers |
| `after_read` | `move` | `move` or `keep` (F5) |
| `seen_retention` | `30d` | how long a hash stays in the ledger; older entries are dropped at each scan |
| `encoding` | `utf-8` | `utf-8`, `windows-1252` or `iso-8859-1`; a UTF-8 byte order mark is skipped |
| `timezone` | the agent's local zone | IANA name used for timestamps written without an offset, which is how most CMM software writes them |
| `timestamp` | modification time | where the part's measuring time is (sections 6 and 7) |
| `context` | | `{serial?, order_ref?, operation_ref?, lot?}`, each naming where the value is in the file (sections 6 and 7) |
| `csv` | | the CSV settings (section 6); refused with `qdas` |

## 5. Lifecycle of a file

**Scan.** Every `interval` the driver lists `directory` (top level, `pattern`, skips as in section 4). For each
file it keeps the size and modification time it saw last.

**Scan order** (amended 2026-11-18 by `../plans/2026-11-18-file-csv.md`). The directory is listed before pending files are committed: while the listing fails nothing is committed, moved or written, the ledger included.

**Stable.** A file is ready when its size and modification time have not changed for `stable_for`. A file that
changes meanwhile restarts its wait. A file that cannot be opened (another process holds it, as Windows does while
the CMM writes) is tried again at the next scan.

**Read.** A ready file is read whole (up to `max_file_size`), its hash computed, and the ledger checked:

1. Hash and modification time already in the ledger: no samples; with `move` the file goes to `done/` (a copy
   dropped twice), with `keep` it is left; not counted as an error.
2. Otherwise the parser turns it into samples (sections 6 and 7) and the driver emits them. The file is now
   *pending*, held in memory only.
3. **Commit, at a later scan** at least `interval` after the emit (and never sooner than one second, well past the
   buffer's 200 ms group fsync): the pair is written to the ledger (`data_dir/file/seen/{input_id}.json`, written
   atomically, outside the input's state directory that the supervisor deletes on a rollback, as `mqtt` keeps its
   topics), then, with `move`, the file is renamed into `done/` or, when the parser refused the whole file,
   `error/`. Both are created on first use. A name already taken there gets `-1`, `-2`, ... before the extension.

**Why the commit waits** (decided after the builder check of 2026-11-18). `emit` gives no acknowledgement: the
records may still be in flight when it returns (the buffer fsyncs in groups every 200 ms; a retired instance
drops what it is handed during a reload). Recording the file at once would lose a report on a power cut or a
reload in that window. Waiting one scan closes the power-cut window, and a file still pending when the input
stops is simply read again at the next start. The repeat is harmless: its samples carry the same timestamps (from
the file, or from the modification time a rename preserves) and the backend drops a measurement whose signal and
timestamp are already stored. What stays possible is shared by every driver and is never silent: an output
restarting beyond its hold limit records a loss marker, a refused buffer append is logged. A core change making
`emit` acknowledge durability was considered and left out: it would touch supervisor, router and buffer for every
driver, to close windows the deferred commit already closes for this one.

**A corrupt ledger** (amended 2026-11-18 by `../plans/2026-11-18-file-csv.md`): a ledger that does not parse is renamed `{input_id}.json.corrupt-<unix seconds>`, a warning is logged and the input starts with an empty one; the reports still present are read again and the backend drops their repeated samples. `probe` and `browse` read the ledger without ever renaming it.

**Moving fails** (read-only share, missing permission): logged at warn once per file, kind `move_failed`; the
ledger still stops it from being read again. A share the agent may not write to should use `keep`.

**Order.** Files are read oldest modification time first, so the samples of a backlog arrive in measuring order.
Only files not yet committed count towards `max_files_per_scan`.

**With `keep`.** Files stay in the directory, so the driver remembers name, size and modification time of every
committed file it still sees and does not read or hash it again while those three are unchanged (a change makes it
a new candidate, checked against the ledger). A ledger entry whose file is still present never expires:
`seen_retention` counts from the scan that no longer finds it. Seen files do not count towards
`max_files_per_scan`, so a directory full of old reports never starves new ones.

**Devices.** An input may have several devices, as every input; each file is offered to every point of every
device of the input. The usual layout is one input per directory and one device per measuring machine.

**Start (D6).** `Start` checks that `directory` is absolute and returns at once; scanning runs in the background.
A directory that does not exist or cannot be listed is death after `death_tolerance`, never a failing `Start`,
since a network share may mount after the agent starts.

**Death and birth.** A scan whose listing succeeds renews every device of the input, so silence never kills one.
A listing that fails (share unmounted, permission lost) is counted with kind `listing`; beyond `death_tolerance`
without a successful listing every device of the input dies, and the next successful listing is their birth.
`laraplate_edge_input_connected{input}` follows the last listing.

**Configuration changes.** The ledger is kept per `input_id` and survives reloads and restarts, so changing the
points of an input never re-reads old files.

## 6. CSV (plan 1)

**Settings** (`connection.csv`):

| Field | Default | Meaning |
|---|---|---|
| `delimiter` | `,` | one character; `;` is common in European exports, `\t` for tab |
| `decimal` | `.` | `.` or `,`; with `,` the delimiter may not be `,` |
| `header_row` | `1` | the 1-based row holding the column names; rows before it are skipped. It counts physical lines, blank ones included (amended 2026-11-18 by `../plans/2026-11-18-file-csv.md`) |
| `layout` | `wide` | `wide` or `long` |
| `key_column` | | `long` only, required: the column naming the characteristic |
| `value_column` | | `long` only, required: the column holding the measured value |

**Layouts.**

- `wide`: one row per part, one column per characteristic (typical of loggers and of Calypso's table export).
  Each data row gives one sample per point, with the row's timestamp and context.
- `long`: one row per characteristic, the whole file is one part (typical of per-part reports). Each row whose
  `key_column` matches a point gives that point's sample; the timestamp and context are read from the first data
  row that has them.

**Address.** `wide`: the column name, exactly as in the header after trimming spaces. `long`: the value of
`key_column`, trimmed. Two columns with the same name in `wide`, or a key repeated in `long`, make the file
`malformed_file` when a point addresses it (the driver cannot tell which value is meant).

**Read:** `event` only; `poll` and `subscribe` refuse the document. `type` is required.

**Timestamp:** `timestamp: {column, format}`, `format` one of `rfc3339`, `unix_s`, `unix_ms`, or a layout written
with the tokens `YYYY`, `MM`, `DD`, `hh`, `mm`, `ss` and literal separators (e.g. `DD.MM.YYYY hh:mm:ss`); `YYYY`, `MM` and `DD` are required, each token appears at most once and the literals hold no letter or digit (amended 2026-11-18 by `../plans/2026-11-18-file-csv.md`). A value
without an offset is read in `timezone`. Without `timestamp`, every sample of the file takes its modification time;
a `wide` file with more than one data row and no `timestamp` is then `malformed_file`, since its parts would share one
timestamp and the backend would keep only the first of them (decided after the builder check of 2026-11-18).

**Context:** each key names a column (`context: {serial: "Serial No", order_ref: "Order"}`). An empty cell gives no
key; a missing column makes the file `malformed_file`, since the mapping is wrong for every row; so does a configured `timestamp.column` missing from the header (amended 2026-11-18 by `../plans/2026-11-18-file-csv.md`).

**Conversion** to the point's `type`, lossless or `bad`, with the rules of the `mqtt` plain payloads (number
grammar, booleans `true`/`false`, `1`/`0`, `on`/`off`), after replacing `decimal` `,` with `.`. Thousands separators
are not accepted: under `decimal: ","` a numeric value that contains a `.` is a `conversion` error (amended 2026-11-18 by `../plans/2026-11-18-file-csv.md`).

**Outcomes:**

| Case | Result |
|---|---|
| A column or key no point addresses | ignored, no count (reports carry many more fields than are wanted) |
| A point's column missing from the header, or its key absent in `long` | no sample for that point; kind `missing_field` once per file |
| An empty cell | no sample for that point (a characteristic not measured on this part) |
| A value that does not convert | `bad` sample; kind `conversion` |
| An unreadable timestamp | `bad` samples for that row; kind `bad_timestamp` |
| A row with a different number of fields than the header | its samples are `bad`; kind `malformed_row` |
| Undecodable text in `encoding`, no header row, a context column missing, a duplicate addressed name | no samples; the file goes to `error/`; kind `malformed_file` |

## 7. Q-DAS (plan 2)

The Q-DAS ASCII transfer format is described publicly in the AQDEF document (Advanced Quality Data Exchange Format);
plan 2 takes field numbers and separators from that document, read when the plan is written, and from nowhere
else. What follows fixes the behaviour; the plan fixes the details.

- Files: `.dfq` (description and values in one file) and the pair `.dfd` + `.dfx` (description, values). Plan 2
  reads `.dfq`; a pair is read when both are stable, as one report, the `.dfd` named by the `.dfx`'s base name.
- **Address:** the characteristic number (K2001) as written, trimmed; `#<n>` addresses the n-th characteristic when
  K2001 is missing.
- Each value line is one part: one sample per point, with that line's date and time (K0004) as timestamp, read in
  `timezone`. A file with more than one value line, one of them without date and time, is `malformed_file`, for the
  reason given for `wide` CSV in section 6.
- **Context:** each key names a K-field of the value line (e.g. `serial: K0014`, `order_ref: K0053`); without a
  `context` mapping the driver sets none.
- A value marked as excluded by its attribute (K0002) gives no sample; the attribute values follow AQDEF.
- The descriptive fields (nominal, limits, unit) are shown by `browse` to help the technician, and never sent: the
  limits of record are the quality plan's in Laraplate.

## 8. Commands and metrics

**`probe <input>`:** parses the newest file the driver would read now (stable, not in the ledger)
without emitting, moving or recording it, and shows each point's value, type, quality and timestamp, or why it has
none (`no file`, `missing_field`, the read error kind), then a `source: <file>` line. It never touches the directory or the ledger. (amended 2026-11-18 by `../plans/2026-11-18-file-csv.md`, P10): it does not list the pending files, since `browse` does, with their states. The read error kind is attributed per file, not per point.)

**`browse <input> [--limit 20]`:** lists the newest files of the directory with size, age and state (`waiting`,
`ready`, `seen`), then, for the newest readable one, the addresses it offers in the form to copy into the document:
`wide` column names or `long` keys with a value preview; under `qdas` the characteristics with K2001, K2002 and the
nominal and limits, and the K-fields of the first value line usable as context.

**Metrics:** those of the driver kit, `laraplate_edge_input_connected{input}` and
`laraplate_edge_input_read_errors_total{input,kind}` with the kinds `listing`, `file_too_large`, `malformed_file`,
`malformed_row`, `missing_field`, `conversion`, `bad_timestamp` and `move_failed`; each file read, duplicate or refused is logged at `info` with its name, the number of samples and its outcome (`read`, `duplicate`, `refused`). (amended 2026-11-18 by `../plans/2026-11-18-file-csv.md`, P9): the metric `laraplate_edge_file_files_total{input,outcome}` is not built, since the status endpoint takes input metrics only from the driver kit's health and a per-driver counter would widen that interface for every driver.)

## 9. Sample context in the core (plan 1, first task)

The agent today has no sample context: `model.Sample` has no such field, the envelope does not write one, and the
plugin SDK's `Sample` message has none. The backend accepts it already (`laraplate-machine-1.schema.json`:
`context` with `order_ref`, `operation_ref`, `serial`, `lot`, no other key).

- `model.Sample` gains an optional context with those four keys; an empty one is absent.
- The buffer records are JSON (`model.Sample` has no tags today, so the new field gets an explicit `omitempty`
  tag): a field left out when empty keeps every record written before readable, so no new record format version
  is needed.
- The envelope writes `context` on a sample that has one.
- Transforms keep the context of the sample they transform. A transform that drops samples (`deadband`,
  `min_interval`, `debounce`) drops their context with them; the documentation advises no such transform on
  measurement points. Derived signals have no context.
- Plugins: the SDK's `Sample` is unchanged in this work, so a plugin input cannot set a context and a plugin output
  receives samples without it. Adding the field to the SDK is a separate, compatible SDK release (section 10).

## 10. Tests, plans and what is left out

**Tests:**

- Unit tests on the fake clock and a temporary directory: stability (a file still growing is not read, a file
  changing restarts its wait), order, `max_files_per_scan`, the ledger (a duplicate copy, retention, survival
  across a restart), the move and its name collisions, `keep` (no re-hashing, retention while present, no
  starvation), the deferred commit (an input stopped before the commit reads the file again with the same
  timestamps), a multi-row `wide` file without `timestamp` refused.
- Parser tests from files written by the tests: both layouts, delimiters, decimal comma, encodings with a byte
  order mark, every outcome of section 6; for Q-DAS, files built from the AQDEF description.
- The directory removed and restored: death after `death_tolerance`, birth on the next listing.
- One end-to-end test per format with the real binary.
- A test runs the polling against a directory on which writes happen in two steps (write, then rename), as the
  common "write `.tmp` then rename" pattern of exporters.

**Plans, in order:**

1. **Sample context and `file` with `csv`:** section 9, then the connection, the lifecycle of section 5, the CSV
   parser, `probe`, `browse`, the factory validating `connection`, `CheckBuild` no longer refusing `file`.
2. **Q-DAS:** the `qdas` format, `.dfq` and `.dfd` + `.dfx`, `browse` for Q-DAS.

Each plan closes with its documentation in `docs/CONFIGURATION.md` and `docs/USAGE.md` and an example in
`examples/file`. Plan 1 also amends the parent spec's `file` row to point here, recording the reversal of section 2.

**Known limits of plan 1** (amended 2026-11-18 by `../plans/2026-11-18-file-csv.md`): a file above `max_file_size` under `keep` is never hashed, so it is refused again after each restart; plugin inputs and outputs do not carry the sample context (section 9).

**Out of scope:** growing files and tailing; recursion into subdirectories; file system events (section 2); the
`context` field in the plugin SDK; Excel and other binary formats; the descriptive Q-DAS fields sent as signals;
writing anything but `done/`, `error/` and the agent's own ledger.
