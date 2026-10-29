# Backend fixtures (decision C2)

Copied from the Laraplate MES module so the agent schema is proven to accept every
document the backend's structural schema accepts in its own tests.

- `laraplate-edge-config-1.structural.schema.json`: verbatim copy of
  `laraplate/Modules/MES/resources/edge/laraplate-edge-config-1.schema.json`
  (MES commit `dd02c71`).
- The documents are the valid ones of the backend test "finds no error in a valid document,
  with or without plugins and version" in
  `laraplate/Modules/MES/tests/Feature/Machine/MachineAgentConfigTest.php` (MES commit `2db00db`),
  translated from the PHP arrays of `agentConfigFields()` / `agentConfigDocument()`:
  - `document.json`: `agentConfigDocument()`;
  - `without-plugins.json`: the fields without `plugins`;
  - `with-version.json`: `agentConfigDocument(['version' => 12])`;
  - `empty-lists.json`: `agentConfigDocument(['inputs' => [], 'outputs' => []])`.
  The fourth assertion of that test, `json_encode(agentConfigDocument())`, is the same document
  as `document.json` passed as text, so it has no file of its own.

Replace these files when the backend test or its schema changes. The agent test validates each
document against both schemas (layer 1 only): this build still refuses the opcua driver, the
keystore token and the plugin entries in its build check.

Exception: spec section 5 makes `manifest_sha256` required in a plugin entry, which the backend's
arrays lack, so the agent test expects exactly `plugins.<i>.manifest_sha256: is required` for them.
The backend must add `manifest_sha256` to its test arrays before it publishes plugin documents.
