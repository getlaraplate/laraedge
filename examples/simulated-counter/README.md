# simulated-counter plugin demo

Runs the plugin SDK's `simulated-counter` example as a plugin: a machine whose points are counters.
It needs no hardware. Documentation: `docs/USAGE.md` ("Running plugins") and `docs/CONFIGURATION.md`
("Plugins").

Build the plugin from the SDK module the agent depends on, next to its manifest, and pin the files:

```bash
cd examples/simulated-counter
mkdir -p plugins/simulated-counter
go build -o plugins/simulated-counter/simulated-counter github.com/getlaraplate/laraedge-sdk/examples/simulated-counter
plugins/simulated-counter/simulated-counter --manifest > plugins/simulated-counter/plugin.json
../../laraplate-edge plugin hash plugins/simulated-counter
```

Copy the two values printed into `plugins[0].sha256` and `plugins[0].manifest_sha256` of `config.yaml`
(repeat after every rebuild), then:

```bash
../../laraplate-edge config validate config.yaml
../../laraplate-edge probe counter-line --config config.yaml
export LARAPLATE_EDGE_TOKEN=...
../../laraplate-edge run --bootstrap bootstrap.yaml
```
