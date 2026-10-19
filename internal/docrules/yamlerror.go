package docrules

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

var decodeLine = regexp.MustCompile(`^line (\d+): (.*)$`)

// DescribeYAMLError renders a YAML decode error without any text taken from
// the document values or keys, which could carry a secret written in the wrong
// place. yaml.v3 embeds document text in several messages:
//
//   - TypeError entries: scalars in type mismatches, keys in duplicate and
//     unknown key entries (see describeTypeError);
//   - plain errors: anchor names ("unknown anchor", "anchor ... contains
//     itself"), the offending key ("invalid map key") and the scalar of a
//     failed explicit tag ("cannot decode ... as a ..."). These are reduced
//     to fixed texts.
//
// The remaining plain errors come from the scanner, parser and reader, whose
// messages are constant strings, and are passed through.
func DescribeYAMLError(err error) string {
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) {
		msgs := make([]string, 0, len(typeErr.Errors))
		for _, m := range typeErr.Errors {
			msgs = append(msgs, describeTypeError(m))
		}
		return strings.Join(msgs, "; ")
	}
	text := strings.TrimPrefix(err.Error(), "yaml: ")
	line := ""
	if m := decodeLine.FindStringSubmatch(text); m != nil {
		line, text = "line "+m[1]+": ", m[2]
	}
	switch {
	case strings.HasPrefix(text, "unknown anchor "):
		return line + "unknown alias"
	case strings.HasPrefix(text, "anchor "):
		return line + "recursive anchor"
	case strings.HasPrefix(text, "invalid map key"):
		return line + "invalid map key"
	case strings.HasPrefix(text, "cannot decode "):
		return line + "value does not match its explicit tag"
	}
	return err.Error()
}

func describeTypeError(entry string) string {
	match := decodeLine.FindStringSubmatch(entry)
	if match == nil {
		return "invalid YAML"
	}
	line, text := "line "+match[1]+": ", match[2]
	switch {
	case strings.HasPrefix(text, "cannot unmarshal "):
		rest := strings.TrimPrefix(text, "cannot unmarshal ")
		tag, _, _ := strings.Cut(rest, " ")
		target := rest
		if i := strings.LastIndex(rest, " into "); i >= 0 {
			target = rest[i+len(" into "):]
		}
		return line + "cannot unmarshal " + tag + " into " + target
	case strings.HasPrefix(text, "mapping key "):
		rest := strings.TrimPrefix(text, "mapping key ")
		if i := strings.LastIndex(rest, " already defined at line "); i >= 0 {
			if key, err := strconv.Unquote(rest[:i]); err == nil && IsSafeKey(key) {
				return line + "duplicate key " + key
			}
		}
		return line + "duplicate key"
	case strings.HasPrefix(text, "field "):
		rest := strings.TrimPrefix(text, "field ")
		if i := strings.LastIndex(rest, " not found in type "); i >= 0 && IsSafeKey(rest[:i]) {
			return line + "unknown key " + rest[:i] + " in " + rest[i+len(" not found in type "):]
		}
		if i := strings.LastIndex(rest, " already set in type "); i >= 0 && IsSafeKey(rest[:i]) {
			return line + "duplicate key " + rest[:i]
		}
		if strings.Contains(rest, " already set in type ") {
			return line + "duplicate key"
		}
		return line + "unknown key"
	default:
		return line + "invalid YAML"
	}
}
