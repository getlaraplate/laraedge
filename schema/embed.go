// Package schema embeds the published JSON Schema of the configuration
// document, laraplate-edge-config/1.
package schema

import (
	"bytes"
	_ "embed"
)

// ConfigV1ID is the $id of the configuration schema.
const ConfigV1ID = "https://laraplate.dev/schemas/laraplate-edge-config-1.schema.json"

//go:embed laraplate-edge-config-1.schema.json
var configV1 []byte

// ConfigV1 returns a copy of the JSON Schema (draft 2020-12) of
// laraplate-edge-config/1.
func ConfigV1() []byte {
	return bytes.Clone(configV1)
}
