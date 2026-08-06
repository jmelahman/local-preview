// Package manifest parses preview.toml, the contract a target repository
// declares so the orchestrator can build and run it. The file is always read
// from a committed tree (git show <sha>:preview.toml), never a working dir.
package manifest

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Defaults applied when the optional backend timeouts are omitted.
const (
	DefaultStartTimeout = 20 * time.Second
	DefaultIdleTimeout  = 30 * time.Minute
)

// Duration unmarshals from TOML strings like "20s" and round-trips through
// JSON as the same string form (run_config storage).
type Duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler for TOML decoding.
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON encodes as a duration string ("20s").
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts a duration string, or a raw nanosecond count for
// leniency.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		return d.UnmarshalText([]byte(s))
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("invalid duration %s", b)
	}
	*d = Duration(n)
	return nil
}

// Manifest is the parsed, validated preview.toml.
type Manifest struct {
	Frontend Frontend `toml:"frontend" json:"frontend"`
	Backend  Backend  `toml:"backend" json:"backend"`
}

// Frontend describes how to hash, build, and locate the static bundle.
// Build commands run with cwd <extracted-tree>/<Path>; Dist is relative to
// Path. Path doubles as the hash-partition root.
type Frontend struct {
	Path  string     `toml:"path" json:"path"`
	Build [][]string `toml:"build" json:"build"`
	Dist  string     `toml:"dist" json:"dist"`
}

// Backend describes how to hash, build, and run the backend. The hash covers
// entries under Path, minus the frontend's Path, minus Exclude patterns.
// Run is templated with {port} and {state_dir} at process start.
type Backend struct {
	Path         string     `toml:"path" json:"path"`
	Exclude      []string   `toml:"exclude" json:"exclude,omitempty"`
	Build        [][]string `toml:"build" json:"build"`
	Run          []string   `toml:"run" json:"run"`
	HealthPath   string     `toml:"health_path" json:"health_path"`
	StartTimeout Duration   `toml:"start_timeout" json:"start_timeout,omitempty"`
	IdleTimeout  Duration   `toml:"idle_timeout" json:"idle_timeout,omitempty"`
}

// Parse decodes and validates preview.toml content. Unknown keys are a hard
// error so typos surface at deploy time instead of silently changing nothing.
func Parse(data []byte) (Manifest, error) {
	var m Manifest
	md, err := toml.Decode(string(data), &m)
	if err != nil {
		return Manifest{}, fmt.Errorf("parse preview.toml: %w", err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, len(un))
		for i, k := range un {
			keys[i] = k.String()
		}
		return Manifest{}, fmt.Errorf("preview.toml: unknown keys: %s", strings.Join(keys, ", "))
	}
	if err := m.normalize(); err != nil {
		return Manifest{}, fmt.Errorf("preview.toml: %w", err)
	}
	return m, nil
}

func (m *Manifest) normalize() error {
	var err error
	if m.Frontend.Path, err = cleanRel("frontend.path", m.Frontend.Path); err != nil {
		return err
	}
	if err := validateSteps("frontend.build", m.Frontend.Build); err != nil {
		return err
	}
	if m.Frontend.Dist, err = cleanRel("frontend.dist", m.Frontend.Dist); err != nil {
		return err
	}
	if m.Backend.Path, err = cleanRel("backend.path", m.Backend.Path); err != nil {
		return err
	}
	if err := validateSteps("backend.build", m.Backend.Build); err != nil {
		return err
	}
	if len(m.Backend.Run) == 0 || m.Backend.Run[0] == "" {
		return fmt.Errorf("backend.run is required")
	}
	if !strings.HasPrefix(m.Backend.HealthPath, "/") {
		return fmt.Errorf("backend.health_path must start with %q", "/")
	}
	if m.Backend.StartTimeout <= 0 {
		m.Backend.StartTimeout = Duration(DefaultStartTimeout)
	}
	if m.Backend.IdleTimeout <= 0 {
		m.Backend.IdleTimeout = Duration(DefaultIdleTimeout)
	}
	return nil
}

// cleanRel normalizes a repo-relative slash path and rejects anything that
// escapes the repo root. "." is allowed (the whole tree).
func cleanRel(field, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	cp := path.Clean(strings.ReplaceAll(p, "\\", "/"))
	if strings.HasPrefix(cp, "/") || cp == ".." || strings.HasPrefix(cp, "../") {
		return "", fmt.Errorf("%s must be a relative path inside the repo", field)
	}
	return cp, nil
}

func validateSteps(field string, steps [][]string) error {
	if len(steps) == 0 {
		return fmt.Errorf("%s is required", field)
	}
	for i, step := range steps {
		if len(step) == 0 || step[0] == "" {
			return fmt.Errorf("%s[%d] must be a non-empty argv array", field, i)
		}
	}
	return nil
}
