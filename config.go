package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that reads "30s", "5m", ... from YAML.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("duracao invalida %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// Behavior controls one scaling direction (same idea as the Kubernetes HPA "behavior").
type Behavior struct {
	// Stabilization: scale-up only follows a recommendation that held for this long;
	// scale-down only follows the highest recommendation seen in this window.
	Stabilization Duration `yaml:"stabilization"`
	// MaxStep: most replicas added/removed in a single adjustment.
	MaxStep int `yaml:"maxStep"`
}

// Policy is the per-app scaling policy. Values in `defaults` apply to every app
// and can be overridden inside each entry of `apps`.
type Policy struct {
	Min             int      `yaml:"min"`
	Max             int      `yaml:"max"`
	TargetRPS       float64  `yaml:"targetRPSPerReplica"` // requests/s one replica should handle
	Window          Duration `yaml:"window"`              // averaging window (complete minutes)
	Interval        Duration `yaml:"interval"`            // evaluation period
	Tolerance       float64  `yaml:"tolerance"`           // ignore deviations up to this ratio (0.1 = 10%)
	MaxP95Ms        float64  `yaml:"maxP95Ms"`            // 0 = off; p95 above this forces one extra replica
	MaxErrorPercent float64  `yaml:"maxErrorPercent"`     // 0 = off; 5xx rate at/above this blocks scale-down
	ScaleUp         Behavior `yaml:"scaleUp"`
	ScaleDown       Behavior `yaml:"scaleDown"`
	DryRun          bool     `yaml:"dryRun"`
}

type AppConfig struct {
	ID     int    `yaml:"id"`
	Name   string `yaml:"name"`
	Policy `yaml:",inline"`
}

func (a AppConfig) Label() string {
	if a.Name != "" {
		return a.Name
	}
	return fmt.Sprintf("app-%d", a.ID)
}

type MiabiConfig struct {
	URL       string `yaml:"url"`
	Workspace string `yaml:"workspace"`
}

type Config struct {
	Miabi    MiabiConfig
	DryRun   bool // global dry-run
	Defaults Policy
	Apps     []AppConfig
}

type fileConfig struct {
	Miabi    MiabiConfig `yaml:"miabi"`
	DryRun   bool        `yaml:"dryRun"`
	Defaults Policy      `yaml:"defaults"`
	Apps     []yaml.Node `yaml:"apps"`
}

func DefaultPolicy() Policy {
	return Policy{
		Min:       2,
		Max:       4,
		Window:    Duration{2 * time.Minute},
		Interval:  Duration{30 * time.Second},
		Tolerance: 0.1,
		ScaleUp:   Behavior{Stabilization: Duration{time.Minute}, MaxStep: 2},
		ScaleDown: Behavior{Stabilization: Duration{5 * time.Minute}, MaxStep: 1},
	}
}

func strictDecode(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(out)
}

// ParseConfig parses and validates the YAML configuration.
func ParseConfig(data []byte) (*Config, error) {
	fc := fileConfig{Defaults: DefaultPolicy()}
	if err := strictDecode(data, &fc); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	cfg := &Config{Miabi: fc.Miabi, DryRun: fc.DryRun, Defaults: fc.Defaults}
	for i := range fc.Apps {
		raw, err := yaml.Marshal(&fc.Apps[i])
		if err != nil {
			return nil, err
		}
		app := AppConfig{Policy: fc.Defaults}
		if err := strictDecode(raw, &app); err != nil {
			return nil, fmt.Errorf("apps[%d]: %w", i, err)
		}
		cfg.Apps = append(cfg.Apps, app)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func LoadConfig(path string) (*Config, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := ParseConfig(data)
	return cfg, data, err
}

func (c *Config) Validate() error {
	if len(c.Apps) == 0 {
		return fmt.Errorf("nenhum app em 'apps'")
	}
	seen := map[int]bool{}
	for _, a := range c.Apps {
		l := a.Label()
		switch {
		case a.ID <= 0:
			return fmt.Errorf("%s: 'id' obrigatorio e maior que zero", l)
		case seen[a.ID]:
			return fmt.Errorf("%s: id %d repetido", l, a.ID)
		case a.Min < 1 || a.Max < a.Min || a.Max > 100:
			return fmt.Errorf("%s: exige 1 <= min <= max <= 100 (min=%d max=%d)", l, a.Min, a.Max)
		case a.TargetRPS <= 0:
			return fmt.Errorf("%s: 'targetRPSPerReplica' obrigatorio e maior que zero", l)
		case a.Window.Duration < time.Minute || a.Window.Duration > time.Hour:
			return fmt.Errorf("%s: 'window' deve ficar entre 1m e 1h (o analytics tem granularidade de 1 minuto)", l)
		case a.Interval.Duration < 5*time.Second:
			return fmt.Errorf("%s: 'interval' minimo e 5s", l)
		case a.Tolerance < 0 || a.Tolerance >= 1:
			return fmt.Errorf("%s: 'tolerance' deve estar em [0, 1)", l)
		case a.ScaleUp.MaxStep < 1 || a.ScaleDown.MaxStep < 1:
			return fmt.Errorf("%s: 'maxStep' deve ser >= 1", l)
		case a.ScaleUp.Stabilization.Duration < 0 || a.ScaleDown.Stabilization.Duration < 0:
			return fmt.Errorf("%s: 'stabilization' nao pode ser negativa", l)
		case a.MaxP95Ms < 0 || a.MaxErrorPercent < 0:
			return fmt.Errorf("%s: 'maxP95Ms' e 'maxErrorPercent' nao podem ser negativos", l)
		}
		seen[a.ID] = true
	}
	return nil
}
