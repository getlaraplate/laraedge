# laraplate-edge

Shop-floor edge agent for the Laraplate MES. It reads machines, probes, operator terminals and
measurement files, and delivers their samples to Laraplate over the `laraplate-machine/1` protocol
(HTTP or MQTT) without losing data across network outages, restarts or power loss.

The agent core is built: the `simulated` input driver, the `laraplate-http` output, a durable disk
buffer per output with loss markers, versioned configuration from a local file or from Laraplate, a
local status endpoint with Prometheus metrics, JSON logs and the systemd watchdog. Real drivers,
transforms, the MQTT output, plugins and packages come with later plans. The design lives in the
Laraplate stack: `docs/superpowers/specs/2026-10-12-laraplate-edge-agent-design.md`.

- [docs/USAGE.md](docs/USAGE.md): build, run, bootstrap file, signals, status endpoint and metrics,
  logs, systemd, troubleshooting.
- [docs/CONFIGURATION.md](docs/CONFIGURATION.md): the configuration document, the buffer and the known
  limits of this build.

## Build and run

Requires Go 1.27 or newer. The agent is one static binary (no CGO).

```bash
go build -o laraplate-edge ./cmd/laraplate-edge
./laraplate-edge version
cd examples/simulated && LARAPLATE_EDGE_TOKEN=... ../../laraplate-edge run --bootstrap bootstrap.yaml
```

Commands: `run --bootstrap <path>`, `config validate <file>`, `status [--listen addr]`, `version`.

Tests:

```bash
go vet ./...
go test -race ./...
go test -tags e2e -race ./internal/e2e   # end to end with the built binary (Unix)
```

## License

laraplate-edge is open-source software licensed under the [GNU AGPL v3](https://www.gnu.org/licenses/agpl-3.0.html).
The plugin SDK and protobuf definitions are licensed separately under Apache-2.0 in `laraplate-edge-sdk`.

Copyright (c) 2026 Marco P. The full licence text is in [LICENSE](LICENSE).
