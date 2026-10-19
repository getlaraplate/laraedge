# laraplate-edge

Shop-floor edge agent for the Laraplate MES. It reads machines, probes, operator terminals and
measurement files, and delivers their samples to Laraplate over the `laraplate-machine/1` protocol
(HTTP or MQTT) without losing data across network outages, restarts or power loss.

Status: design approved, nothing built yet. The design lives in the Laraplate stack:
`docs/superpowers/specs/2026-10-12-laraplate-edge-agent-design.md`.

## Build and run

Requires Go 1.27 or newer. The agent is one static binary (no CGO).

```bash
go build -o laraplate-edge ./cmd/laraplate-edge
./laraplate-edge version
```

Set the reported version at build time with
`-ldflags "-X github.com/swolley/laraplate-edge/internal/version.version=1.2.3"`.
Commands: `run --bootstrap <path>`, `config validate <file>`, `status [--listen addr]`, `version`.
Only `version` works yet; the others are wired by later tasks of the agent core plan.

Tests:

```bash
go vet ./...
go test -race ./...
```

## License

laraplate-edge is open-source software licensed under the [GNU AGPL v3](https://www.gnu.org/licenses/agpl-3.0.html).
The plugin SDK and protobuf definitions are licensed separately under Apache-2.0 in `laraplate-edge-sdk`.

Copyright (c) 2026 Marco P. The full licence text is in [LICENSE](LICENSE).
