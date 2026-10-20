// Package bootstrap loads the small local file the agent needs before it can
// fetch any configuration: its identity, where the configuration comes from,
// and where it keeps its data. Secrets appear only as secret:// references and
// are never resolved here.
package bootstrap

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/getlaraplate/laraplate-edge/internal/docrules"
	"github.com/getlaraplate/laraplate-edge/internal/secrets"
)

const (
	// ProviderFile reads the configuration document from a local file.
	ProviderFile = "file"
	// ProviderLaraplate polls the configuration document from Laraplate.
	ProviderLaraplate = "laraplate"

	// DefaultStatusListen is where the local status endpoint binds by default.
	DefaultStatusListen = "127.0.0.1:9180"
	// DefaultPollSeconds is the default configuration polling interval.
	DefaultPollSeconds = 60
	// MinPollSeconds is the shortest accepted polling interval.
	MinPollSeconds = 5

	hostGOOS = runtime.GOOS
)

// File is the parsed bootstrap file, with defaults applied.
type File struct {
	// AgentID identifies this agent to Laraplate.
	AgentID string `yaml:"agent_id"`
	// Config says where the configuration document comes from.
	Config Config `yaml:"config"`
	// DataDir holds the disk buffer and the applied configuration.
	DataDir string `yaml:"data_dir"`
	// StatusListen is the host:port of the local status endpoint.
	StatusListen string `yaml:"status_listen"`
	// StatusToken is an optional secret:// reference protecting the status endpoint.
	StatusToken string `yaml:"status_token"`
}

// Config selects the configuration provider.
type Config struct {
	// Provider is ProviderFile or ProviderLaraplate.
	Provider string `yaml:"provider"`
	// File is the configuration document path for ProviderFile.
	File string `yaml:"file"`
	// Laraplate holds the settings for ProviderLaraplate.
	Laraplate Laraplate `yaml:"laraplate"`
}

// Laraplate holds how to reach the Laraplate backend.
type Laraplate struct {
	// URL is the backend base URL (https, or http on a loopback host).
	URL string `yaml:"url"`
	// Token is a secret:// reference to the agent token.
	Token string `yaml:"token"`
	// PollSeconds is the configuration polling interval, at least MinPollSeconds.
	PollSeconds int `yaml:"poll_seconds"`
}

// DefaultDataDir returns the default data directory for goos. getenv is used
// on Windows to read ProgramData and may be nil elsewhere.
func DefaultDataDir(goos string, getenv func(string) string) string {
	if goos != "windows" {
		return "/var/lib/laraplate-edge"
	}
	base := ""
	if getenv != nil {
		base = getenv("ProgramData")
	}
	if base == "" {
		base = `C:\ProgramData`
	}
	return strings.TrimRight(base, `\`) + `\laraplate-edge`
}

// Load reads, defaults and validates the bootstrap file at path. Unknown keys
// are an error. Every validation error names the offending key and never
// echoes a secret value. Secret references are checked for syntax only.
func Load(path string) (File, error) {
	f, err := os.Open(path)
	if err != nil {
		return File{}, fmt.Errorf("bootstrap: open %s: %w", path, err)
	}
	defer f.Close()

	file := File{
		Config:       Config{Laraplate: Laraplate{PollSeconds: DefaultPollSeconds}},
		DataDir:      DefaultDataDir(hostGOOS, os.Getenv),
		StatusListen: DefaultStatusListen,
	}
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return File{}, fmt.Errorf("bootstrap: parse %s: %s", path, docrules.DescribeYAMLError(err))
	}
	if err := dec.Decode(&yaml.Node{}); !errors.Is(err, io.EOF) {
		return File{}, fmt.Errorf("bootstrap: parse %s: the file must hold exactly one YAML document", path)
	}
	if err := file.validate(); err != nil {
		return File{}, fmt.Errorf("bootstrap: %s: %w", path, err)
	}
	return file, nil
}

func (f File) validate() error {
	if strings.TrimSpace(f.AgentID) == "" {
		return errors.New("agent_id is required")
	}
	if err := f.Config.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(f.DataDir) == "" {
		return errors.New("data_dir must not be empty")
	}
	if err := validateListen(f.StatusListen); err != nil {
		return fmt.Errorf("status_listen: %w", err)
	}
	if f.StatusToken != "" {
		if err := validateReference("status_token", f.StatusToken); err != nil {
			return err
		}
	}
	return nil
}

func (c Config) validate() error {
	switch c.Provider {
	case "":
		return errors.New("config.provider is required (file or laraplate)")
	case ProviderFile:
		if strings.TrimSpace(c.File) == "" {
			return errors.New("config.file is required when config.provider is file")
		}
		return nil
	case ProviderLaraplate:
		return c.Laraplate.validate()
	default:
		return errors.New(`config.provider must be "file" or "laraplate"`)
	}
}

func (l Laraplate) validate() error {
	if strings.TrimSpace(l.URL) == "" {
		return errors.New("config.laraplate.url is required when config.provider is laraplate")
	}
	if err := docrules.CheckURL(l.URL); err != nil {
		return fmt.Errorf("config.laraplate.url: %w", err)
	}
	if l.Token == "" {
		return errors.New("config.laraplate.token is required when config.provider is laraplate")
	}
	if err := validateReference("config.laraplate.token", l.Token); err != nil {
		return err
	}
	if l.PollSeconds < MinPollSeconds {
		return fmt.Errorf("config.laraplate.poll_seconds must be at least %d", MinPollSeconds)
	}
	return nil
}

// validateReference checks that value is a well-formed, supported secret
// reference. The message never includes value unless it is a reference.
func validateReference(key, value string) error {
	if !secrets.IsReference(value) {
		return fmt.Errorf("%s must be a secret:// reference (secret://env/NAME or secret://file/PATH), not a plain value", key)
	}
	if err := secrets.Check(value); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

func validateListen(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("must be host:port, for example 127.0.0.1:9180")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return errors.New("port must be a number between 0 and 65535")
	}
	return nil
}
