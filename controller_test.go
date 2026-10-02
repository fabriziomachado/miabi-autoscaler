package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type fakeAPI struct {
	app            App
	status         Status
	series         []Bucket
	analytics      error
	scaleErr       error
	scaleCalls     []int
	analyticsCalls int
}

func (f *fakeAPI) App(context.Context, int) (App, error)          { return f.app, nil }
func (f *fakeAPI) AppStatus(context.Context, int) (Status, error) { return f.status, nil }
func (f *fakeAPI) Analytics(context.Context, int, string) (Summary, error) {
	f.analyticsCalls++
	return Summary{Series: f.series}, f.analytics
}
func (f *fakeAPI) Scale(_ context.Context, _ int, n int) error {
	f.scaleCalls = append(f.scaleCalls, n)
	return f.scaleErr
}

func newTestController(f *fakeAPI) (*Controller, AppConfig) {
	cfg := AppConfig{ID: 1, Name: "hello", Policy: DefaultPolicy()}
	cfg.Min, cfg.Max, cfg.TargetRPS = 1, 5, 10
	cfg.Window.Duration = time.Minute
	cfg.AnalyticsDelay.Duration = 0 // fake series have no publication lag
	cfg.ScaleUp.Stabilization.Duration, cfg.ScaleDown.Stabilization.Duration = 0, 0
	c := NewController(f, NewMetrics("test"), slog.New(slog.NewTextHandler(io.Discard, nil)), "", &Config{Apps: []AppConfig{cfg}}, nil)
	c.now = func() time.Time { return t0 }
	c.state(1).seeded = true // no startup grace in these tests
	return c, cfg
}

func healthy(replicas int, minuteRequests float64) *fakeAPI {
	return &fakeAPI{
		app:    App{ID: 1, RuntimeKind: "service", Replicas: replicas},
		status: Status{Running: true, ServiceReplicas: replicas, ServiceRunningTasks: replicas},
		series: buildSeries([]float64{minuteRequests}, nil, nil),
	}
}

func TestEvaluateScalesUp(t *testing.T) {
	f := healthy(2, 1800) // 30 req/s with a target of 10 -> 3 replicas
	c, app := newTestController(f)
	d := c.Evaluate(context.Background(), app, false)
	if d.Action != "scale-up" || !d.Applied || len(f.scaleCalls) != 1 || f.scaleCalls[0] != 3 {
		t.Fatalf("decision %+v, calls %v", d, f.scaleCalls)
	}
}

func TestEvaluateScalesDown(t *testing.T) {
	f := healthy(4, 60) // 1 req/s -> 1 replica, but one step at a time
	c, app := newTestController(f)
	d := c.Evaluate(context.Background(), app, false)
	if d.Action != "scale-down" || len(f.scaleCalls) != 1 || f.scaleCalls[0] != 3 {
		t.Fatalf("decision %+v, calls %v", d, f.scaleCalls)
	}
}

func TestEvaluateDryRunDoesNotScale(t *testing.T) {
	f := healthy(2, 1800)
	c, app := newTestController(f)
	d := c.Evaluate(context.Background(), app, true)
	if d.Action != "scale-up" || d.Applied || len(f.scaleCalls) != 0 || !d.DryRun {
		t.Fatalf("decision %+v, calls %v", d, f.scaleCalls)
	}
}

func TestEvaluateHoldsWithinTolerance(t *testing.T) {
	f := healthy(3, 1800) // exactly 10 req/s per replica
	c, app := newTestController(f)
	if d := c.Evaluate(context.Background(), app, false); d.Action != "none" || len(f.scaleCalls) != 0 {
		t.Fatalf("decision %+v", d)
	}
}

func TestEvaluateWaitsForTasksToSettle(t *testing.T) {
	f := healthy(3, 1800)
	f.status.ServiceRunningTasks = 2
	c, app := newTestController(f)
	d := c.Evaluate(context.Background(), app, false)
	if d.Action != "skipped" || f.analyticsCalls != 0 || len(f.scaleCalls) != 0 || !strings.Contains(d.Reason, "2/3") {
		t.Fatalf("decision %+v, analytics calls %d", d, f.analyticsCalls)
	}
}

func TestEvaluateSkipsNonServiceAndStoppedApps(t *testing.T) {
	f := healthy(2, 1800)
	f.app.RuntimeKind = "container"
	c, app := newTestController(f)
	if d := c.Evaluate(context.Background(), app, false); d.Action != "skipped" || len(f.scaleCalls) != 0 {
		t.Fatalf("container app: %+v", d)
	}
	f = healthy(2, 1800)
	f.status.ServiceReplicas = 0
	c, app = newTestController(f)
	if d := c.Evaluate(context.Background(), app, false); d.Action != "skipped" || len(f.scaleCalls) != 0 {
		t.Fatalf("stopped app: %+v", d)
	}
}

func TestEvaluateFailsSafeWithoutAnalytics(t *testing.T) {
	f := healthy(4, 0)
	f.analytics = errors.New("boom")
	c, app := newTestController(f)
	d := c.Evaluate(context.Background(), app, false)
	if d.Action != "skipped" || len(f.scaleCalls) != 0 {
		t.Fatalf("must not scale blind: %+v calls=%v", d, f.scaleCalls)
	}
	var out bytes.Buffer
	c.metrics.Write(&out)
	if !strings.Contains(out.String(), `miabi_autoscaler_errors_total{app="hello",stage="analytics"} 1`) {
		t.Fatalf("error not counted:\n%s", out.String())
	}
}

func TestEvaluateFixesOutOfBoundsWithoutAnalytics(t *testing.T) {
	f := healthy(8, 0) // max is 5
	c, app := newTestController(f)
	d := c.Evaluate(context.Background(), app, false)
	if d.Action != "bounds" || len(f.scaleCalls) != 1 || f.scaleCalls[0] != 5 || f.analyticsCalls != 0 {
		t.Fatalf("decision %+v calls=%v", d, f.scaleCalls)
	}
}

func TestEvaluateReportsScaleFailure(t *testing.T) {
	f := healthy(2, 1800)
	f.scaleErr = errors.New("403")
	c, app := newTestController(f)
	d := c.Evaluate(context.Background(), app, false)
	if d.Applied || !strings.Contains(d.Reason, "falha ao escalar") {
		t.Fatalf("decision %+v", d)
	}
}

func TestStartupGraceDelaysScaleDown(t *testing.T) {
	f := healthy(4, 0)
	c, app := newTestController(f)
	app.ScaleDown.Stabilization.Duration = 5 * time.Minute
	c.states[1] = &State{} // as after a restart
	if d := c.Evaluate(context.Background(), app, false); d.Action != "none" || len(f.scaleCalls) != 0 {
		t.Fatalf("a fresh start must not scale down immediately: %+v", d)
	}
}

func TestTickHonoursInterval(t *testing.T) {
	f := healthy(3, 1800)
	c, _ := newTestController(f)
	c.Tick(context.Background(), t0)
	c.Tick(context.Background(), t0.Add(10*time.Second)) // before the 30s interval
	if f.analyticsCalls != 1 {
		t.Fatalf("analytics calls = %d, want 1", f.analyticsCalls)
	}
	c.now = func() time.Time { return t0.Add(31 * time.Second) }
	c.Tick(context.Background(), t0.Add(31*time.Second))
	if f.analyticsCalls != 2 || len(c.Snapshot()) != 1 {
		t.Fatalf("analytics calls = %d snapshot=%d", f.analyticsCalls, len(c.Snapshot()))
	}
}

func TestMetricsForget(t *testing.T) {
	m := NewMetrics("v")
	m.Set("miabi_autoscaler_replicas", Labels("app", "a"), 2)
	m.Set("miabi_autoscaler_replicas", Labels("app", "b"), 3)
	m.Forget("a")
	var out bytes.Buffer
	m.Write(&out)
	if strings.Contains(out.String(), `app="a"`) || !strings.Contains(out.String(), `miabi_autoscaler_replicas{app="b"} 3`) {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
}
