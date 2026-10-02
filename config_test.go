package main

import (
	"strings"
	"testing"
	"time"
)

const goodYAML = `
miabi: {url: "https://painel.example.com", workspace: "1"}
defaults:
  min: 1
  max: 6
  targetRPSPerReplica: 20
  scaleUp: {maxStep: 3}
apps:
  - id: 1
    name: hello
    max: 3
    scaleDown: {stabilization: 10m}
  - id: 2
    targetRPSPerReplica: 5
    dryRun: true
`

func TestParseConfigMergesDefaultsAndOverrides(t *testing.T) {
	cfg, err := ParseConfig([]byte(goodYAML))
	if err != nil {
		t.Fatal(err)
	}
	a, b := cfg.Apps[0], cfg.Apps[1]
	if a.Min != 1 || a.Max != 3 || a.TargetRPS != 20 {
		t.Errorf("app1 overrides wrong: %+v", a.Policy)
	}
	if a.ScaleUp.MaxStep != 3 || a.ScaleUp.Stabilization.Duration != time.Minute {
		t.Errorf("a partial nested override must keep the other defaults: %+v", a.ScaleUp)
	}
	if a.ScaleDown.Stabilization.Duration != 10*time.Minute || a.ScaleDown.MaxStep != 1 {
		t.Errorf("scaleDown wrong: %+v", a.ScaleDown)
	}
	if b.Max != 6 || b.TargetRPS != 5 || !b.DryRun || a.DryRun {
		t.Errorf("app2 wrong or leaked into app1: %+v / %+v", b.Policy, a.Policy)
	}
	if a.Label() != "hello" || b.Label() != "app-2" {
		t.Errorf("labels: %q %q", a.Label(), b.Label())
	}
}

func TestParseConfigRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"no apps":        "defaults: {targetRPSPerReplica: 10}\n",
		"no target":      "apps:\n  - id: 1\n",
		"missing id":     "defaults: {targetRPSPerReplica: 10}\napps:\n  - name: x\n",
		"duplicate id":   "defaults: {targetRPSPerReplica: 10}\napps:\n  - id: 1\n  - id: 1\n",
		"min > max":      "defaults: {targetRPSPerReplica: 10}\napps:\n  - id: 1\n    min: 5\n    max: 2\n",
		"max over 100":   "defaults: {targetRPSPerReplica: 10}\napps:\n  - id: 1\n    max: 101\n",
		"window < 1m":    "defaults: {targetRPSPerReplica: 10, window: 30s}\napps:\n  - id: 1\n",
		"interval < 5s":  "defaults: {targetRPSPerReplica: 10, interval: 1s}\napps:\n  - id: 1\n",
		"bad duration":   "defaults: {targetRPSPerReplica: 10, interval: soon}\napps:\n  - id: 1\n",
		"unknown field":  "defaults: {targetRPSPerReplica: 10, targetRps: 3}\napps:\n  - id: 1\n",
		"unknown in app": "defaults: {targetRPSPerReplica: 10}\napps:\n  - id: 1\n    colour: red\n",
		"tolerance 1":    "defaults: {targetRPSPerReplica: 10, tolerance: 1}\napps:\n  - id: 1\n",
	}
	for name, y := range cases {
		if _, err := ParseConfig([]byte(y)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseConfigErrorMentionsTheApp(t *testing.T) {
	_, err := ParseConfig([]byte("apps:\n  - id: 7\n    name: shop\n"))
	if err == nil || !strings.Contains(err.Error(), "shop") {
		t.Fatalf("error should name the app, got %v", err)
	}
}
