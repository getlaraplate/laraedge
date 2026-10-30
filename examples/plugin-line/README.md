# Plugin pipeline demo

Runs the three example plugins of the plugin SDK, one of each kind: `simulated-counter` reads two
presses (input), `line-state` derives the state of their line (processor) and `jsonl` writes every
batch to a file (output). It needs no hardware and no Laraplate. Documentation: `docs/USAGE.md`
("Running plugins") and `docs/CONFIGURATION.md` ("Plugins", "Processors", "Plugin outputs").

Build the plugins from the SDK module the agent depends on, each next to its manifest, and pin them:

```bash
cd examples/plugin-line
for p in simulated-counter line-state jsonl; do
  mkdir -p plugins/$p
  go build -o plugins/$p/$p github.com/getlaraplate/laraedge-sdk/examples/$p
  plugins/$p/$p --manifest > plugins/$p/plugin.json
  ../../laraplate-edge plugin hash plugins/$p
done
```

Copy the two values printed for each plugin into its entry of `plugins` in `config.yaml` (repeat after
every rebuild), then:

```bash
../../laraplate-edge config validate config.yaml
../../laraplate-edge run --bootstrap bootstrap.yaml
```

The batches land in `plugins/jsonl/machine.jsonl`: the samples of `press-1` and `press-2`, the samples
of the derived device `line-1`, and the births of all three. `../../laraplate-edge status` shows the
three plugins, the processor `line` and the output `file`.

Two things to try while it runs:

- `touch plugins/jsonl/machine.jsonl.suspend`: the jsonl plugin answers `SUSPEND_AUTH`, the output goes
  `suspended_auth` and the records keep buffering. Remove the file and send `SIGHUP` to the agent
  (or wait 5 minutes): delivery resumes and nothing is missing.
- `pkill -KILL -x line-state`: the presses keep flowing, `line-1/state` is sent `bad` once, the agent
  restarts the plugin within seconds and the derived data comes back. Keep it down longer than
  `death_tolerance` (rename its binary away, kill it, wait, put it back) and `line-1` is reported dead,
  then born again.

The plugins listen on a Unix socket under `data_dir/run/plugins/<id>/`, whose path must fit 107 bytes
on Linux. If the repository lives in a deep directory and the plugins fail with `socket path too long`,
set `data_dir` in `bootstrap.yaml` to a short absolute path.
