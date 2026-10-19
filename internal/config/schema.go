package config

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/swolley/laraplate-edge/internal/docrules"
	"github.com/swolley/laraplate-edge/schema"
)

// compiledSchema compiles the embedded schema once; the result is never
// modified afterwards.
var compiledSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema.ConfigV1()))
	if err != nil {
		return nil, fmt.Errorf("config: parse the embedded schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource(schema.ConfigV1ID, doc); err != nil {
		return nil, fmt.Errorf("config: load the embedded schema: %w", err)
	}
	s, err := c.Compile(schema.ConfigV1ID)
	if err != nil {
		return nil, fmt.Errorf("config: compile the embedded schema: %w", err)
	}
	return s, nil
})

// defMessages describes the values a pattern of the schema accepts, by the
// name of the $defs entry holding the pattern.
var defMessages = map[string]string{
	"id":         "must be 1 to 64 characters of a-z, 0-9, '.', '_' or '-', starting with a letter or digit",
	"driver":     "unknown driver (want opcua, modbus, mqtt, serial, file, simulated or plugin:<id>)",
	"outputType": "unknown output type (want laraplate-http, laraplate-mqtt or plugin:<id>)",
	"duration":   "must be a duration such as 500ms, 2s or 1m30s",
	"size":       "must be a size such as 512KiB, 16MiB or 1GiB",
	"secretRef":  secretMessage,
	"httpURL":    "must be an http or https URL",
	"deviceName": "must not contain /",
	"signalName": "must not contain /",
}

const secretMessage = "must be a secret:// reference (secret://env/NAME, secret://file/PATH or secret://keystore/NAME), not a plain value"

// validateSchema validates the tree against the embedded schema. The messages
// are built from the instance location and the schema only: the library's
// own texts quote instance values and are never used.
func validateSchema(tree any) ([]string, error) {
	s, err := compiledSchema()
	if err != nil {
		return nil, err
	}
	err = s.Validate(tree)
	if err == nil {
		return nil, nil
	}
	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		return nil, fmt.Errorf("config: validate: %w", err)
	}
	var msgs []string
	collectSchemaMessages(verr, nil, &msgs)
	if len(msgs) == 0 {
		msgs = append(msgs, path{}.msg("is invalid"))
	}
	slices.Sort(msgs)
	return slices.Compact(msgs), nil
}

// collectSchemaMessages renders the leaves of e. parent is the location of
// the enclosing error: the library reports a propertyNames failure with a
// location slice it keeps modifying, so that one takes its parent's.
func collectSchemaMessages(e *jsonschema.ValidationError, parent path, msgs *[]string) {
	p := slices.Clone(path(e.InstanceLocation))
	if _, ok := e.ErrorKind.(*kind.PropertyNames); ok && parent != nil {
		p = parent
	}
	add := func(text string) { *msgs = append(*msgs, p.msg(text)) }

	switch k := e.ErrorKind.(type) {
	case *kind.Required:
		for _, name := range k.Missing {
			*msgs = append(*msgs, p.key(name).msg("is required"))
		}
	case *kind.AdditionalProperties:
		for _, name := range k.Properties {
			*msgs = append(*msgs, p.key(name).msg("unknown key"))
		}
	case *kind.PropertyNames:
		text := "unknown key"
		if strings.HasSuffix(e.SchemaURL, "/$defs/transform/propertyNames") {
			text = "unknown transform"
		}
		if docrules.IsSafeKey(k.Property) {
			text += " " + k.Property
		}
		add(text)
		return
	case *kind.Type:
		add("must be " + strings.Join(k.Want, " or ") + " (got " + k.Got + ")")
	case *kind.Enum:
		add("must be one of " + joinValues(k.Want))
	case *kind.Const:
		add("must be " + joinValues([]any{k.Want}))
	case *kind.Pattern:
		add(describePattern(e.SchemaURL))
	case *kind.MinLength:
		add(fmt.Sprintf("must be at least %d characters", k.Want))
	case *kind.MaxLength:
		add(fmt.Sprintf("must be at most %d characters", k.Want))
	case *kind.Minimum:
		add("must be at least " + ratText(k.Want))
	case *kind.Maximum:
		add("must be at most " + ratText(k.Want))
	case *kind.ExclusiveMinimum:
		add("must be greater than " + ratText(k.Want))
	case *kind.ExclusiveMaximum:
		add("must be less than " + ratText(k.Want))
	case *kind.MinItems:
		add(fmt.Sprintf("must have at least %d items", k.Want))
	case *kind.MaxItems:
		add(fmt.Sprintf("must have at most %d items", k.Want))
	case *kind.MinProperties:
		add(fmt.Sprintf("must have at least %d keys", k.Want))
	case *kind.MaxProperties:
		add(fmt.Sprintf("must have at most %d keys", k.Want))
	case *kind.FalseSchema:
		add("is not allowed")
	default:
		if len(e.Causes) == 0 {
			add("is invalid")
		}
	}
	for _, cause := range e.Causes {
		collectSchemaMessages(cause, p, msgs)
	}
}

// describePattern names what the pattern at schemaURL accepts.
func describePattern(schemaURL string) string {
	_, def, ok := strings.Cut(schemaURL, "#/$defs/")
	if ok {
		def, _, _ = strings.Cut(def, "/")
		if text, known := defMessages[def]; known {
			return text
		}
	}
	return "does not match the expected format"
}

// joinValues renders schema values (never instance values).
func joinValues(values []any) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ", ")
}

func ratText(r *big.Rat) string {
	if r == nil {
		return "the limit"
	}
	if r.IsInt() {
		return r.Num().String()
	}
	f, _ := r.Float64()
	return strconv.FormatFloat(f, 'g', -1, 64)
}
