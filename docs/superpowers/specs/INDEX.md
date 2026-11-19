# Specs Index

Navigation aid only. Agent-only specs live here; specs spanning the backend and the agent stay at the stack root
(`../../../../docs/superpowers/specs/`, the parent being `2026-10-12-laraplate-edge-agent-design.md`).

- `2026-10-19-transforms-modbus-opcua-design.md` (approved 2026-10-19; transforms built 2026-10-20, `modbus` built 2026-10-21, `opcua` built 2026-10-22): the level-1 transform catalogue, the `modbus` and `opcua` drivers, the driver kit, `probe` and `config test`; delivered by three plans: transforms (`../plans/2026-10-19-transforms.md`, closed), `modbus` (`../plans/2026-10-20-modbus.md`, closed), `opcua` (`../plans/2026-10-21-opcua.md`, closed).
- `2026-11-06-mqtt-driver-design.md` (approved 2026-11-06; plan 1 delivered 2026-11-06, plan 2 delivered 2026-11-17): the `mqtt` input driver, with JSON and plain payloads and Sparkplug B; client library, addresses, availability and death, `probe` and `browse`; delivered by two plans, JSON and plain first (`../plans/2026-11-06-mqtt-json.md`, closed), then Sparkplug B (`../plans/2026-11-17-mqtt-sparkplug-b.md`, closed).
- `2026-11-18-file-driver-design.md` (approved 2026-11-18; plan 1 and plan 2 delivered 2026-11-18): the `file` input driver, with CSV and Q-DAS reports; polling detection and why file system events were not chosen, file lifecycle and ledger, deferred commit, sample context in the core, `probe` and `browse`; delivered by two plans, the sample context and CSV first (`../plans/2026-11-18-file-csv.md`, closed), then Q-DAS (`../plans/2026-11-18-file-qdas.md`, closed).
