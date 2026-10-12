# Agent Rules (caveman)

## What this repo is

- `laraplate-edge`: shop-floor edge agent. Reads machines, probes, terminals, measurement files.
- Sends `laraplate-machine/1` to Laraplate (HTTP, MQTT) and to plugin outputs.
- Go. One static binary. Built-in open drivers; proprietary drivers as out-of-process gRPC plugins.
- Design: `../docs/superpowers/specs/2026-10-12-laraplate-edge-agent-design.md` (stack root).
- Protocol source of truth: `../laraplate/Modules/MES/resources/protocol/laraplate-machine-1.schema.json`.
- Stack context: `../AGENTS.md`, `../CLAUDE.md`.

## Language

- Chat with user: Italian.
- Code, comments, docs content: English.

## Licence (hard)

- This repo: AGPL-3.0. `LICENSE` is the verbatim FSF text. Never edit it.
- Plugin SDK and protobuf definitions: Apache-2.0, separate repo `laraplate-edge-sdk`. Never put them here.
- No proprietary code here: vendor drivers (FOCAS, S7, EtherNet/IP, Heidenhain), profile packs, fleet management.
- No vendor libraries with own licences (e.g. FANUC FOCAS).
- Dependencies: permissive only (MIT, BSD, Apache). Copyleft blocks the commercial dual licence.
- Check a library's licence before adopting it. New dependencies need user approval.
- Contributions to the core need a signed CLA.
- Never copy code from `laraplate-ui` or `laraplate-importers` (proprietary).
- This repo is a submodule of the stack only, never of `laraplate`.

## Specs and plans

- Agent plans live here: `docs/superpowers/plans/`, with an `INDEX.md`. Create the folder with the first plan.
- Agent-only specs live here: `docs/superpowers/specs/`. Specs spanning backend and agent stay at the stack root.
- Backend changes for the agent go in a `laraplate` plan, not here.
- Tick a task's checkbox when the task finishes. Close a plan with `## Delivery status (date)`.

## Go

- Format: `gofmt` (via `go fmt ./...`). Imports grouped: stdlib, third party, local.
- Static checks: `go vet ./...` before done. Use a linter only once configured in the repo.
- Errors: return them, wrap with `%w` and context. No `panic` outside `main` and impossible states.
- Pass `context.Context` as first parameter to anything that blocks, reads I/O or can be cancelled.
- No global mutable state. Inject dependencies through interfaces where tests need fakes.
- Goroutines: every one has an owner that stops it. No leaks on shutdown.
- Exported identifiers have doc comments. Other comments rare and useful.
- No CGO in the agent binary.

## Domain rules (from the spec)

- Drivers are read-only. Never write to a machine.
- Respect the PLC: rate limit and max in-flight requests per input.
- The agent produces values, never meanings. Meaning lives in the backend profile.
- Nothing confirmed is lost: buffer before send, sealed batches, resend identical on retry.
- Every data drop leaves a loss marker. No silent loss.
- No telemetry to the vendor. Talk only to configured destinations.
- Secrets only as `secret://` references. Never log secrets. Never log sample values at `info`.
- No option to disable TLS certificate verification.

## Testing

- Every code change needs automated test proof.
- Table-driven tests. Fakes over mocks. Fuzz decoders.
- Run: `go test ./...`; with `-race` for concurrent code.
- Narrowest relevant package first, then the whole module before done.
- No timing-based assertions; use fake clocks.

## Git

- Commit only your own hunks. Never stash, reset or discard others' work.
- Never push. The user pushes.
- After commits here, the stack pointer needs a bump: `git add laraplate-edge` in the stack root.

## Before final

- Summarize changed files.
- State `go fmt`, `go vet`, `go test` run. If not run, say why.

@RTK.md
