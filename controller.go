package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"
)

// Decision is the outcome of one evaluation of one app (also served at /status).
type Decision struct {
	Time         time.Time `json:"time"`
	App          string    `json:"app"`
	ID           int       `json:"id"`
	Action       string    `json:"action"` // none | scale-up | scale-down | bounds | skipped
	Current      int       `json:"current"`
	Recommended  int       `json:"recommended"`
	Desired      int       `json:"desired"`
	RPS          float64   `json:"rps"`
	P95Ms        float64   `json:"p95_ms"`
	ErrorPercent float64   `json:"error_percent"`
	Reason       string    `json:"reason"`
	DryRun       bool      `json:"dry_run"`
	Applied      bool      `json:"applied"`
}

type Controller struct {
	api     API
	metrics *Metrics
	log     *slog.Logger
	now     func() time.Time

	cfgPath string

	mu       sync.Mutex
	cfg      *Config
	cfgHash  [32]byte
	states   map[int]*State
	due      map[int]time.Time
	last     map[int]Decision
	lastTick time.Time
}

func NewController(api API, m *Metrics, log *slog.Logger, cfgPath string, cfg *Config, raw []byte) *Controller {
	return &Controller{
		api: api, metrics: m, log: log, now: time.Now, cfgPath: cfgPath,
		cfg: cfg, cfgHash: sha256.Sum256(raw),
		states: map[int]*State{}, due: map[int]time.Time{}, last: map[int]Decision{},
	}
}

// Run evaluates every app on its own interval until ctx is done, reloading the config when it changes.
func (c *Controller) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var reloadAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := c.now()
		if !now.Before(reloadAt) {
			c.reload()
			reloadAt = now.Add(10 * time.Second)
		}
		c.Tick(ctx, now)
	}
}

// Tick evaluates the apps that are due. Exposed for tests.
func (c *Controller) Tick(ctx context.Context, now time.Time) {
	c.mu.Lock()
	cfg := c.cfg
	c.lastTick = now
	c.mu.Unlock()
	for _, app := range cfg.Apps {
		c.mu.Lock()
		due := c.due[app.ID]
		c.mu.Unlock()
		if now.Before(due) {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		d := c.Evaluate(cctx, app, cfg.DryRun)
		cancel()
		c.mu.Lock()
		c.last[app.ID] = d
		c.due[app.ID] = c.now().Add(app.Interval.Duration)
		c.mu.Unlock()
	}
}

func (c *Controller) reload() {
	data, err := os.ReadFile(c.cfgPath)
	if err != nil {
		c.log.Warn("nao consegui reler a configuracao", "err", err)
		return
	}
	h := sha256.Sum256(data)
	c.mu.Lock()
	same := h == c.cfgHash
	c.mu.Unlock()
	if same {
		return
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		c.log.Error("configuracao nova invalida; mantendo a anterior", "err", err)
		c.mu.Lock()
		c.cfgHash = h // do not repeat the same error every 10s
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	keep := map[int]string{}
	for _, a := range cfg.Apps {
		keep[a.ID] = a.Label()
	}
	for _, old := range c.cfg.Apps {
		if _, ok := keep[old.ID]; !ok {
			delete(c.states, old.ID)
			delete(c.due, old.ID)
			delete(c.last, old.ID)
			c.metrics.Forget(old.Label())
		}
	}
	c.cfg, c.cfgHash = cfg, h
	c.mu.Unlock()
	c.log.Info("configuracao recarregada", "apps", len(cfg.Apps))
}

func (c *Controller) state(id int) *State {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.states[id]
	if !ok {
		s = &State{}
		c.states[id] = s
	}
	return s
}

// Healthy reports whether the control loop is alive.
func (c *Controller) Healthy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastTick.IsZero() || c.now().Sub(c.lastTick) < 2*time.Minute
}

// Snapshot returns the latest decision of every app, ordered by id.
func (c *Controller) Snapshot() []Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Decision, 0, len(c.last))
	for _, d := range c.last {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Evaluate runs one control cycle for one app.
func (c *Controller) Evaluate(ctx context.Context, app AppConfig, globalDry bool) Decision {
	now := c.now()
	label := app.Label()
	d := Decision{Time: now, App: label, ID: app.ID, Action: "skipped", DryRun: app.DryRun || globalDry}
	lbl := Labels("app", label)
	defer func() {
		c.metrics.Set("miabi_autoscaler_last_evaluation_timestamp_seconds", lbl, float64(now.Unix()))
		c.metrics.Set("miabi_autoscaler_target_rps_per_replica", lbl, app.TargetRPS)
		c.log.Info("avaliacao", "app", d.App, "action", d.Action, "current", d.Current, "recommended", d.Recommended,
			"desired", d.Desired, "rps", round(d.RPS), "p95_ms", round(d.P95Ms), "error_percent", round(d.ErrorPercent),
			"dry_run", d.DryRun, "applied", d.Applied, "reason", d.Reason)
	}()
	fail := func(stage string, err error) Decision {
		c.metrics.Add("miabi_autoscaler_errors_total", Labels("app", label, "stage", stage), 1)
		d.Reason = fmt.Sprintf("falha em %s: %v", stage, err)
		return d
	}

	info, err := c.api.App(ctx, app.ID)
	if err != nil {
		return fail("app", err)
	}
	if info.RuntimeKind != "service" {
		d.Reason = fmt.Sprintf("app nao esta em modo service/cluster (runtime=%q)", info.RuntimeKind)
		return d
	}
	st, err := c.api.AppStatus(ctx, app.ID)
	if err != nil {
		return fail("status", err)
	}
	current := st.ServiceReplicas
	d.Current = current
	c.metrics.Set("miabi_autoscaler_replicas", lbl, float64(current))
	if current < 1 {
		d.Reason = "app parado (0 replicas); nada a escalar"
		return d
	}
	state := c.state(app.ID)
	state.Seed(now, current)

	if current < app.Min || current > app.Max {
		d.Action, d.Desired, d.Reason = "bounds", clamp(current, app.Min, app.Max), "fora dos limites min/max"
		c.apply(ctx, app, &d)
		return d
	}
	if st.ServiceRunningTasks < current {
		d.Reason = fmt.Sprintf("aguardando as tasks estabilizarem (%d/%d)", st.ServiceRunningTasks, current)
		return d
	}

	sum, err := c.api.Analytics(ctx, app.ID, RangeFor(app.Window.Duration))
	if err != nil {
		return fail("analytics", err)
	}
	sig, err := ComputeSignal(sum.Series, now, app.Window.Duration)
	if err != nil {
		return fail("signal", err)
	}
	rec, why := Recommend(app.Policy, sig, current)
	desired := state.Decide(now, app.Policy, current, rec)
	d.RPS, d.P95Ms, d.ErrorPercent, d.Recommended, d.Desired, d.Reason = sig.RPS, sig.P95Ms, sig.ErrorPercent, rec, desired, why
	c.metrics.Set("miabi_autoscaler_requests_per_second", lbl, sig.RPS)
	c.metrics.Set("miabi_autoscaler_p95_latency_ms", lbl, sig.P95Ms)
	c.metrics.Set("miabi_autoscaler_error_percent", lbl, sig.ErrorPercent)
	c.metrics.Set("miabi_autoscaler_recommended_replicas", lbl, float64(rec))
	c.metrics.Set("miabi_autoscaler_desired_replicas", lbl, float64(desired))

	switch {
	case desired == current:
		d.Action = "none"
		if rec != current {
			d.Reason += fmt.Sprintf(" (recomendado %d, aguardando estabilizacao)", rec)
		}
	case desired > current:
		d.Action = "scale-up"
		c.apply(ctx, app, &d)
	default:
		d.Action = "scale-down"
		c.apply(ctx, app, &d)
	}
	return d
}

func (c *Controller) apply(ctx context.Context, app AppConfig, d *Decision) {
	if d.DryRun {
		return
	}
	if err := c.api.Scale(ctx, app.ID, d.Desired); err != nil {
		c.metrics.Add("miabi_autoscaler_errors_total", Labels("app", d.App, "stage", "scale"), 1)
		d.Reason += fmt.Sprintf(" | falha ao escalar: %v", err)
		return
	}
	d.Applied = true
	dir := "up"
	if d.Desired < d.Current {
		dir = "down"
	}
	c.metrics.Add("miabi_autoscaler_scale_events_total", Labels("app", d.App, "direction", dir), 1)
}

func round(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }
