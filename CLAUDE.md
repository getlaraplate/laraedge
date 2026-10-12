# laraplate-edge: Claude Code

Shop-floor edge agent for Laraplate MES machine data (Go). **Open source, AGPL-3.0.**
Stack context: `../CLAUDE.md`, `../AGENTS.md`. Design: `../docs/superpowers/specs/2026-10-12-laraplate-edge-agent-design.md`.
Canonical repo rules: `AGENTS.md` (imported below).

@AGENTS.md

## Boundaries
- Never a submodule of the AGPL `laraplate` repo; a submodule of the stack only.
- Plugin SDK and protobuf definitions belong to `laraplate-edge-sdk` (Apache-2.0), not here.
- Proprietary drivers, profile packs and fleet management never enter this repo.
- Laraplate stays generic: the backend learns nothing about drivers; it stores the agent configuration as an opaque document.

## Language
- Chat with the user: **Italian**. Code, comments, docs: **English**.
