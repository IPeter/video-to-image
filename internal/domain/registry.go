package domain

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-playground/validator/v10"
	"gopkg.in/yaml.v3"
)

// Registry is the immutable set of domains loaded at startup.
type Registry struct {
	byKey map[string]*Config
	keys  []string
}

// NewRegistry builds a registry from already-validated configs (used by tests).
func NewRegistry(cfgs ...*Config) *Registry {
	r := &Registry{byKey: make(map[string]*Config, len(cfgs))}
	for _, c := range cfgs {
		r.byKey[c.Key] = c
		r.keys = append(r.keys, c.Key)
	}
	sort.Strings(r.keys)
	return r
}

// Get returns the domain for key.
func (r *Registry) Get(key string) (*Config, bool) {
	c, ok := r.byKey[key]
	return c, ok
}

// Keys returns all domain keys, sorted.
func (r *Registry) Keys() []string { return append([]string(nil), r.keys...) }

// LoadDir parses and validates every *.yaml / *.yml file in dir. Any invalid file is an
// error: a bad domain config must crash startup rather than serve a wrong prompt.
func LoadDir(dir string) (*Registry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("domain: read dir %q: %w", dir, err)
	}
	var cfgs []*Config
	var errs []error
	for _, e := range entries {
		name := e.Name()
		ext := filepath.Ext(name)
		// Kubernetes ConfigMap mounts contain ..data symlinks and hidden dirs; skip them.
		if e.IsDir() || strings.HasPrefix(name, ".") || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		c, err := LoadFile(filepath.Join(dir, name))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		cfgs = append(cfgs, c)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if len(cfgs) == 0 {
		return nil, fmt.Errorf("domain: no domain files in %q", dir)
	}
	seen := map[string]bool{}
	for _, c := range cfgs {
		if seen[c.Key] {
			return nil, fmt.Errorf("domain: duplicate key %q", c.Key)
		}
		seen[c.Key] = true
	}
	return NewRegistry(cfgs...), nil
}

// LoadFile parses and validates one domain file. The key must equal the file name.
func LoadFile(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("domain: read %q: %w", path, err)
	}
	c, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("domain: %s: %w", filepath.Base(path), err)
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if c.Key != base {
		return nil, fmt.Errorf("domain: %s: key %q must equal the file name %q", filepath.Base(path), c.Key, base)
	}
	return c, nil
}

var validate = validator.New(validator.WithRequiredStructEnabled())

// Parse decodes and validates a domain config. Unknown YAML fields are rejected so a
// typo in a key is caught at startup instead of silently using a zero value.
func Parse(raw []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if err := Validate(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate runs tag validation plus the cross-field rules from the plan: unique shot
// IDs, weights summing to 1 ± 0.01 and at least one required shot.
func Validate(c *Config) error {
	if err := validate.Struct(c); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	var errs []error
	ids := map[string]bool{}
	required := 0
	for _, s := range c.Shots {
		if s.ID == OtherShotID {
			errs = append(errs, fmt.Errorf("shot id %q is reserved", OtherShotID))
		}
		if ids[s.ID] {
			errs = append(errs, fmt.Errorf("duplicate shot id %q", s.ID))
		}
		ids[s.ID] = true
		if s.Required {
			required++
		}
	}
	if required == 0 {
		errs = append(errs, errors.New("at least one shot must be required"))
	}
	if c.Selection.ImageCount < required {
		// The coverage pass stops at imageCount, so a visible required shot could be
		// reported missing.
		errs = append(errs, fmt.Errorf("selection.imageCount (%d) must be at least the number of required shots (%d)", c.Selection.ImageCount, required))
	}
	w := c.Selection.Weights
	if sum := w.Relevance + w.Sharpness + w.Exposure; math.Abs(sum-1) > 0.01 {
		errs = append(errs, fmt.Errorf("selection.weights must sum to 1 (got %.3f)", sum))
	}
	if c.Model.ThinkingBudget != nil && c.Model.ThinkingLevel != "" {
		errs = append(errs, errors.New("model: set thinkingBudget or thinkingLevel, not both"))
	}
	if c.Limits.MinDurationSec > c.Limits.MaxDurationSec {
		errs = append(errs, errors.New("limits.minDurationSec must not exceed limits.maxDurationSec"))
	}
	for _, o := range c.AllowedOrigins {
		if NormalizeOrigin(o) == "" {
			errs = append(errs, fmt.Errorf("allowedOrigins: %q is not an origin", o))
		}
	}
	return errors.Join(errs...)
}
