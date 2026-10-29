package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/getlaraplate/laraedge-sdk/manifest"

	"github.com/getlaraplate/laraedge/internal/bootstrap"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/secrets"
)

// DriverPrefix starts the driver name of an input a plugin provides.
const DriverPrefix = "plugin:"

// Spec is a plugin entry of a document whose files were found and verified.
type Spec struct {
	// ID is the plugin id of the document.
	ID string
	// Path is the absolute or base-directory-joined path of the binary.
	Path string
	// Dir is the directory of the binary, where plugin.json lives.
	Dir string
	// Manifest is the decoded plugin.json.
	Manifest manifest.Manifest
	// ManifestRaw is the plugin.json bytes, as hashed.
	ManifestRaw []byte
	// SHA256 is the expected hex SHA-256 of the binary (either case).
	SHA256 string
	// ManifestSHA256 is the expected hex SHA-256 of plugin.json.
	ManifestSHA256 string
	// MaxMemory is the memory limit in bytes, 0 for none.
	MaxMemory int64
	// Raw is the plugin entry as compact canonical JSON.
	Raw json.RawMessage
}

// Paths holds the local inputs that decide where a plugin binary may live.
type Paths struct {
	// BaseDir is the directory of the configuration file, against which a
	// relative path resolves with the file provider; "" for a provider
	// without a file.
	BaseDir string
	// Provider is the bootstrap's provider name.
	Provider string
	// PluginsDir is the bootstrap's plugins_dir, absolute, "" when unset. It
	// is the local trust anchor: with the laraplate provider plugins are
	// disabled without it and every binary must lie inside it; with the file
	// provider it constrains the binaries only when set.
	PluginsDir string
}

// Resolve checks the plugin entries of doc against the files on disk: the
// path (see Paths for where it resolves and what contains it), the binary and
// manifest hashes, and the manifest itself. It returns a Spec for every entry
// that passed and one message per problem, prefixed "plugins.<i>". A missing
// binary or manifest is a problem. Messages name paths and fixed text, never
// a configured value.
func Resolve(doc config.Document, paths Paths) ([]Spec, []string) {
	var (
		specs []Spec
		msgs  []string
	)

	for i, p := range doc.Plugins {
		prefix := fmt.Sprintf("plugins.%d", i)
		spec, problems := resolveOne(p, prefix, paths)
		msgs = append(msgs, problems...)
		if len(problems) == 0 {
			specs = append(specs, spec)
		}
	}

	return specs, msgs
}

// contained reports whether binary lies inside dir once both have their
// symlinks resolved, and returns the resolved binary path: the host executes
// that path, so a symlink changed after the check cannot point it elsewhere.
// The error is the one of dir, or of binary when it does not exist; the caller
// decides what a missing file means.
func contained(dir, binary string) (string, bool, error) {
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false, fmt.Errorf("plugins_dir: %w", err)
	}
	realBinary, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return "", false, err
	}
	rel, err := filepath.Rel(realDir, realBinary)
	if err != nil {
		return "", false, nil
	}
	inside := rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)

	return realBinary, inside, nil
}

// resolveOne hashes the configured Path, not <dir>/<manifest name>: the binary
// named after the manifest is a suite rule only (SDK ruling R11).
func resolveOne(p config.Plugin, prefix string, paths Paths) (Spec, []string) {
	laraplate := paths.Provider == bootstrap.ProviderLaraplate
	if laraplate && paths.PluginsDir == "" {
		return Spec{}, []string{prefix + ".path: plugins are disabled: set plugins_dir in the bootstrap"}
	}

	binary := p.Path
	if !filepath.IsAbs(binary) {
		base := paths.BaseDir
		if laraplate {
			base = paths.PluginsDir
		}
		binary = filepath.Join(base, binary)
	}
	binary = filepath.Clean(binary)

	if paths.PluginsDir != "" {
		real, ok, err := contained(paths.PluginsDir, binary)
		switch {
		case err != nil && strings.HasPrefix(err.Error(), "plugins_dir: "):
			return Spec{}, []string{prefix + ".path: plugins_dir cannot be resolved"}
		case err == nil && !ok:
			return Spec{}, []string{prefix + ".path: must be inside plugins_dir"}
		case err == nil:
			binary = real
		}
		// A binary that does not exist is reported below, not masked here.
	}
	dir := filepath.Dir(binary)

	m, raw, err := manifest.Load(filepath.Join(dir, ManifestFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Spec{}, []string{prefix + ": plugin.json not found next to the binary"}
		}

		return Spec{}, []string{prefix + ": plugin.json cannot be read or is not a valid manifest"}
	}

	var msgs []string
	for _, problem := range m.Check() {
		msgs = append(msgs, prefix+".manifest: "+dottedMessage(problem))
	}

	binarySum, err := HashFile(binary)
	if err != nil {
		msgs = append(msgs, prefix+".path: the plugin binary cannot be read")
	} else if !strings.EqualFold(binarySum, p.SHA256) {
		msgs = append(msgs, prefix+".sha256: does not match the plugin binary")
	}
	if !strings.EqualFold(SumHex(raw), p.ManifestSHA256) {
		msgs = append(msgs, prefix+".manifest_sha256: does not match plugin.json")
	}
	if len(msgs) > 0 {
		return Spec{}, msgs
	}

	return Spec{
		ID: p.ID, Path: binary, Dir: dir, Manifest: m, ManifestRaw: raw,
		SHA256: p.SHA256, ManifestSHA256: p.ManifestSHA256, MaxMemory: p.MaxMemory, Raw: p.Raw,
	}, nil
}

// Validate checks the plugin inputs of doc: the plugin:<id> driver must name
// an entry of the document whose manifest offers the input kind, and the
// input section must satisfy the manifest. Plugins missing from specs (their
// files failed Resolve) are not checked against a manifest. Problems whose
// value is a secret reference or whose path ends in a secret key are dropped
// (decision H3): Configure validates the resolved values at start. Messages
// are prefixed "inputs.<i>".
func Validate(doc config.Document, specs []Spec) []string {
	byID := map[string]Spec{}
	for _, s := range specs {
		byID[s.ID] = s
	}
	declared := map[string]bool{}
	for _, p := range doc.Plugins {
		declared[p.ID] = true
	}

	var msgs []string
	for i, in := range doc.Inputs {
		id, ok := strings.CutPrefix(in.Driver, DriverPrefix)
		if !ok {
			continue
		}
		prefix := fmt.Sprintf("inputs.%d", i)

		if !declared[id] {
			msgs = append(msgs, prefix+".driver: no plugin entry with this id")
			continue
		}
		spec, found := byID[id]
		if !found {
			continue
		}
		if spec.Manifest.Kinds.Input == nil {
			msgs = append(msgs, prefix+".driver: the plugin does not offer the input kind")
			continue
		}

		var section any
		_ = json.Unmarshal(in.Raw, &section)
		for _, problem := range spec.Manifest.ValidateInput(in.Raw) {
			if dropped(section, problem.Path) {
				continue
			}
			if problem.Path == "" {
				msgs = append(msgs, prefix+": "+problem.Message)
			} else {
				msgs = append(msgs, prefix+"."+dotted(problem.Path)+": "+problem.Message)
			}
		}
	}

	return msgs
}

// dropped reports whether a problem at path of section is left to Configure:
// its path ends in a secret key, or the value there is a secret reference.
func dropped(section any, path string) bool {
	tokens := splitPath(path)
	if len(tokens) == 0 {
		return false
	}
	if last := tokens[len(tokens)-1]; last.key != "" && config.IsSecretKey(last.key) {
		return true
	}

	value := section
	for _, t := range tokens {
		switch node := value.(type) {
		case map[string]any:
			if t.key == "" {
				return false
			}
			value = node[t.key]
		case []any:
			if t.key != "" || t.index < 0 || t.index >= len(node) {
				return false
			}
			value = node[t.index]
		default:
			return false
		}
	}
	s, ok := value.(string)

	return ok && secrets.IsReference(s)
}

var indexPattern = regexp.MustCompile(`\[([0-9]+)\]`)

// dotted renders an SDK path in the dotted style of the other messages:
// devices[0].points[1].address becomes devices.0.points.1.address.
func dotted(path string) string { return indexPattern.ReplaceAllString(path, ".$1") }

// dottedMessage applies dotted to the path before the first ": " of a
// manifest.Check problem.
func dottedMessage(problem string) string {
	path, rest, ok := strings.Cut(problem, ": ")
	if !ok {
		return problem
	}

	return dotted(path) + ": " + rest
}

type pathToken struct {
	key   string
	index int
}

// splitPath splits a manifest problem path such as
// "devices[0].points[1].address" into keys and indexes.
func splitPath(path string) []pathToken {
	var tokens []pathToken
	for part := range strings.SplitSeq(path, ".") {
		if part == "" {
			continue
		}
		key, rest, _ := strings.Cut(part, "[")
		if key != "" {
			tokens = append(tokens, pathToken{key: key})
		}
		for rest != "" {
			num, after, _ := strings.Cut(rest, "]")
			n, err := strconv.Atoi(num)
			if err != nil {
				n = -1
			}
			tokens = append(tokens, pathToken{index: n})
			rest = strings.TrimPrefix(after, "[")
		}
	}

	return tokens
}
