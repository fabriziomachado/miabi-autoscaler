package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Metrics is a tiny Prometheus text-format registry (no external dependency).
type Metrics struct {
	mu   sync.Mutex
	kind map[string]string
	help map[string]string
	vals map[string]map[string]float64
}

func NewMetrics(version string) *Metrics {
	m := &Metrics{kind: map[string]string{}, help: map[string]string{}, vals: map[string]map[string]float64{}}
	m.def("miabi_autoscaler_replicas", "gauge", "Replicas currently running.")
	m.def("miabi_autoscaler_recommended_replicas", "gauge", "Raw recommendation before stabilization.")
	m.def("miabi_autoscaler_desired_replicas", "gauge", "Replicas after stabilization and limits.")
	m.def("miabi_autoscaler_requests_per_second", "gauge", "Average requests/s over the window.")
	m.def("miabi_autoscaler_p95_latency_ms", "gauge", "Worst per-minute p95 latency in the window.")
	m.def("miabi_autoscaler_error_percent", "gauge", "5xx responses as percent of requests.")
	m.def("miabi_autoscaler_target_rps_per_replica", "gauge", "Configured target per replica.")
	m.def("miabi_autoscaler_last_evaluation_timestamp_seconds", "gauge", "Unix time of the last evaluation.")
	m.def("miabi_autoscaler_scale_events_total", "counter", "Scale operations by direction.")
	m.def("miabi_autoscaler_errors_total", "counter", "Failed evaluation steps by stage.")
	m.def("miabi_autoscaler_build_info", "gauge", "Build information.")
	m.Set("miabi_autoscaler_build_info", Labels("version", version), 1)
	return m
}

func (m *Metrics) def(name, kind, help string) {
	m.kind[name], m.help[name], m.vals[name] = kind, help, map[string]float64{}
}

func (m *Metrics) Set(name, labels string, v float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vals[name][labels] = v
}

func (m *Metrics) Add(name, labels string, v float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vals[name][labels] += v
}

// Forget drops every series carrying the given app label (app removed from the config).
func (m *Metrics) Forget(app string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	needle := `app="` + escape(app) + `"`
	for _, series := range m.vals {
		for l := range series {
			if strings.Contains(l, needle) {
				delete(series, l)
			}
		}
	}
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// Labels builds `{k="v",...}` from alternating keys and values.
func Labels(kv ...string) string {
	parts := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, kv[i], escape(kv[i+1])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func (m *Metrics) Write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.vals))
	for n := range m.vals {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", n, m.help[n], n, m.kind[n])
		keys := make([]string, 0, len(m.vals[n]))
		for l := range m.vals[n] {
			keys = append(keys, l)
		}
		sort.Strings(keys)
		for _, l := range keys {
			fmt.Fprintf(w, "%s%s %g\n", n, l, m.vals[n][l])
		}
	}
}
