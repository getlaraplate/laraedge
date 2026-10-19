package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/swolley/laraplate-edge/internal/docrules"
)

const (
	// maxDepth bounds the nesting of a document.
	maxDepth = 64
	// maxValues bounds the values of a document, aliases expanded.
	maxValues = 250_000
)

// errAbort stops a decode after a message that makes the rest meaningless.
var errAbort = errors.New("abort")

// path locates a value in the document, rendered as dotted keys and indexes.
type path []string

func (p path) key(k string) path { return append(slices.Clip(p), k) }

func (p path) index(i int) path { return p.key(strconv.Itoa(i)) }

// String renders the path. A key that does not have the shape of a document
// key (it could be a secret written in the wrong place) is rendered as *.
func (p path) String() string {
	if len(p) == 0 {
		return "document"
	}
	parts := make([]string, len(p))
	for i, token := range p {
		parts[i] = safeToken(token)
	}
	return strings.Join(parts, ".")
}

func (p path) msg(text string) string { return p.String() + ": " + text }

func safeToken(token string) string {
	if token != "" && strings.Trim(token, "0123456789") == "" {
		return token
	}
	if docrules.IsSafeKey(token) {
		return token
	}
	return "*"
}

// decodeTree turns a JSON or YAML document into a tree of map[string]any,
// []any, string, json.Number, bool and nil, with numbers in canonical form.
// A document whose first significant byte is { or [ is JSON, any other YAML.
// No message quotes the document.
func decodeTree(data []byte) (any, []string) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return nil, []string{path{}.msg("is not valid UTF-8")}
	}
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		return decodeJSON(data)
	}
	return decodeYAML(data)
}

type treeBuilder struct {
	values int
	msgs   []string
}

func (b *treeBuilder) count(depth int) error {
	if depth > maxDepth {
		b.msgs = append(b.msgs, path{}.msg(fmt.Sprintf("nesting deeper than %d levels", maxDepth)))
		return errAbort
	}
	b.values++
	if b.values > maxValues {
		b.msgs = append(b.msgs, path{}.msg(fmt.Sprintf("more than %d values", maxValues)))
		return errAbort
	}
	return nil
}

// number canonicalises a number literal: integers in plain decimal, other
// values in the shortest float64 form. ok is false when the value is not a
// finite float64.
func number(lit string) (json.Number, bool) {
	if i, err := strconv.ParseInt(lit, 10, 64); err == nil {
		return json.Number(strconv.FormatInt(i, 10)), true
	}
	if u, err := strconv.ParseUint(lit, 10, 64); err == nil {
		return json.Number(strconv.FormatUint(u, 10)), true
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return "", false
	}
	if f == math.Trunc(f) && f >= math.MinInt64 && f < math.MaxInt64 {
		return json.Number(strconv.FormatInt(int64(f), 10)), true
	}
	return json.Number(strconv.FormatFloat(f, 'g', -1, 64)), true
}

// --- YAML ---

func decodeYAML(data []byte) (any, []string) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, []string{path{}.msg("is empty")}
		}
		return nil, []string{strings.TrimPrefix(docrules.DescribeYAMLError(err), "yaml: ")}
	}
	if err := dec.Decode(&yaml.Node{}); !errors.Is(err, io.EOF) {
		return nil, []string{path{}.msg("must hold exactly one YAML document")}
	}
	if root.Kind == yaml.DocumentNode && len(root.Content) == 0 {
		return nil, []string{path{}.msg("is empty")}
	}

	b := &yamlBuilder{active: map[*yaml.Node]bool{}}
	v, err := b.value(&root, path{}, 0)
	if err != nil || len(b.msgs) > 0 {
		return nil, b.msgs
	}
	return v, nil
}

type yamlBuilder struct {
	treeBuilder
	active map[*yaml.Node]bool
}

func (b *yamlBuilder) at(n *yaml.Node, text string) {
	b.msgs = append(b.msgs, fmt.Sprintf("line %d: %s", n.Line, text))
}

func (b *yamlBuilder) value(n *yaml.Node, p path, depth int) (any, error) {
	if err := b.count(depth); err != nil {
		return nil, err
	}
	switch n.Kind {
	case yaml.DocumentNode:
		return b.value(n.Content[0], p, depth)
	case yaml.AliasNode:
		if b.active[n.Alias] {
			b.at(n, "recursive alias")
			return nil, nil
		}
		b.active[n.Alias] = true
		defer delete(b.active, n.Alias)
		return b.value(n.Alias, p, depth)
	case yaml.MappingNode:
		return b.mapping(n, p, depth)
	case yaml.SequenceNode:
		list := make([]any, 0, len(n.Content))
		for i, item := range n.Content {
			v, err := b.value(item, p.index(i), depth+1)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		return list, nil
	case yaml.ScalarNode:
		return b.scalar(n, p), nil
	default:
		b.at(n, "unsupported node")
		return nil, nil
	}
}

func (b *yamlBuilder) mapping(n *yaml.Node, p path, depth int) (any, error) {
	m := make(map[string]any, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		if k.Kind == yaml.AliasNode && k.Alias != nil {
			k = k.Alias
		}
		switch {
		case k.Kind == yaml.ScalarNode && k.ShortTag() == "!!merge":
			b.at(n.Content[i], "merge keys are not supported")
			continue
		case k.Kind != yaml.ScalarNode || k.ShortTag() != "!!str":
			b.at(n.Content[i], "mapping keys must be strings")
			continue
		}
		if _, dup := m[k.Value]; dup {
			if docrules.IsSafeKey(k.Value) {
				b.at(n.Content[i], "duplicate key "+k.Value)
			} else {
				b.at(n.Content[i], "duplicate key")
			}
			continue
		}
		v, err := b.value(n.Content[i+1], p.key(k.Value), depth+1)
		if err != nil {
			return nil, err
		}
		m[k.Value] = v
	}
	return m, nil
}

func (b *yamlBuilder) scalar(n *yaml.Node, p path) any {
	switch n.ShortTag() {
	case "!!str", "!!timestamp":
		return n.Value
	case "!!null":
		return nil
	case "!!bool":
		var v bool
		if err := n.Decode(&v); err != nil {
			b.at(n, "invalid boolean")
			return nil
		}
		return v
	case "!!int", "!!float":
		var v any
		if err := n.Decode(&v); err != nil {
			b.at(n, "invalid number")
			return nil
		}
		var lit string
		switch x := v.(type) {
		case int:
			lit = strconv.Itoa(x)
		case int64:
			lit = strconv.FormatInt(x, 10)
		case uint64:
			lit = strconv.FormatUint(x, 10)
		case float64:
			lit = strconv.FormatFloat(x, 'g', -1, 64)
		default:
			b.at(n, "invalid number")
			return nil
		}
		num, ok := number(lit)
		if !ok {
			b.msgs = append(b.msgs, p.msg("number out of range"))
			return nil
		}
		return num
	default:
		b.at(n, "unsupported tag")
		return nil
	}
}

// --- JSON ---

func decodeJSON(data []byte) (any, []string) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	b := &jsonBuilder{data: data, dec: dec}

	v, err := b.value(path{}, 0)
	if err != nil {
		return nil, b.msgs
	}
	if rest := skipJSONSpace(data, int(dec.InputOffset()), false); rest < len(data) {
		b.msgs = append(b.msgs, b.position(rest)+": unexpected data after the JSON document")
	}
	if len(b.msgs) > 0 {
		return nil, b.msgs
	}
	return v, nil
}

type jsonBuilder struct {
	treeBuilder
	data []byte
	dec  *json.Decoder
}

// position renders the byte at offset as line and column, both from 1.
func (b *jsonBuilder) position(offset int) string {
	offset = min(max(offset, 0), len(b.data))
	before := b.data[:offset]
	line := bytes.Count(before, []byte("\n")) + 1
	col := utf8.RuneCount(before[bytes.LastIndexByte(before, '\n')+1:]) + 1
	return fmt.Sprintf("line %d, column %d", line, col)
}

// token reads the next token. Syntax errors keep only their position: their
// text can quote a character of the document.
func (b *jsonBuilder) token() (json.Token, error) {
	tok, err := b.dec.Token()
	if err == nil {
		return tok, nil
	}
	var syntax *json.SyntaxError
	switch {
	case errors.As(err, &syntax):
		b.msgs = append(b.msgs, b.position(int(syntax.Offset))+": invalid JSON")
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		b.msgs = append(b.msgs, path{}.msg("unexpected end of JSON"))
	default:
		b.msgs = append(b.msgs, path{}.msg("invalid JSON"))
	}
	return nil, errAbort
}

func (b *jsonBuilder) value(p path, depth int) (any, error) {
	if err := b.count(depth); err != nil {
		return nil, err
	}
	tok, err := b.token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '{' {
			return b.object(p, depth)
		}
		return b.array(p, depth)
	case json.Number:
		num, ok := number(string(t))
		if !ok {
			b.msgs = append(b.msgs, p.msg("number out of range"))
			return nil, nil
		}
		return num, nil
	default:
		return t, nil
	}
}

func (b *jsonBuilder) object(p path, depth int) (any, error) {
	m := map[string]any{}
	for b.dec.More() {
		start := skipJSONSpace(b.data, int(b.dec.InputOffset()), true)
		tok, err := b.token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		_, dup := m[key]
		if dup {
			text := "duplicate key"
			if docrules.IsSafeKey(key) {
				text += " " + key
			}
			b.msgs = append(b.msgs, b.position(start)+": "+text)
		}
		v, err := b.value(p.key(key), depth+1)
		if err != nil {
			return nil, err
		}
		if !dup {
			m[key] = v
		}
	}
	if _, err := b.token(); err != nil {
		return nil, err
	}
	return m, nil
}

func (b *jsonBuilder) array(p path, depth int) (any, error) {
	list := []any{}
	for i := 0; b.dec.More(); i++ {
		v, err := b.value(p.index(i), depth+1)
		if err != nil {
			return nil, err
		}
		list = append(list, v)
	}
	if _, err := b.token(); err != nil {
		return nil, err
	}
	return list, nil
}

// skipJSONSpace returns the offset of the first byte at or after offset that
// is not JSON whitespace, nor a separator when separators is true.
func skipJSONSpace(data []byte, offset int, separators bool) int {
	for ; offset < len(data); offset++ {
		switch c := data[offset]; {
		case c == ' ', c == '\t', c == '\r', c == '\n':
		case separators && (c == ',' || c == ':'):
		default:
			return offset
		}
	}
	return offset
}

// errCanonical is a fixed text: the encoder's error could quote a value.
var errCanonical = errors.New("config: internal error: the document has no canonical JSON form")

// canonical renders v as compact JSON with sorted keys and no HTML escaping.
func canonical(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, errCanonical
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
