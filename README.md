# laraplate-edge

Shop-floor edge agent for the Laraplate MES. It reads machines, probes, operator terminals and
measurement files, and delivers their samples to Laraplate over the `laraplate-machine/1` protocol
(HTTP or MQTT) without losing data across network outages, restarts or power loss.

Status: design approved, nothing built yet. The design lives in the Laraplate stack:
`docs/superpowers/specs/2026-10-12-laraplate-edge-agent-design.md`.

## License

laraplate-edge is open-source software licensed under the [GNU AGPL v3](https://www.gnu.org/licenses/agpl-3.0.html).
The plugin SDK and protobuf definitions are licensed separately under Apache-2.0 in `laraplate-edge-sdk`.

Copyright (c) 2026 Marco P. The full licence text is in [LICENSE](LICENSE).
