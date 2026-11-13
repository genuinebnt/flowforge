package platform

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Registry is a minimal Prometheus registry: counters, gauges and
// histograms with label values, exposed in the text format (version 0.0.4).
// There is no client library in this module, so this is hand-written. It is
// safe for concurrent use.
type Registry struct {
	mu      sync.Mutex
	metrics map[string]*metric
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{metrics: map[string]*metric{}} }

type metricKind string

const (
	kindCounter   metricKind = "counter"
	kindGauge     metricKind = "gauge"
	kindHistogram metricKind = "histogram"
)

type metric struct {
	name, help string
	kind       metricKind
	labels     []string
	buckets    []float64
	mu         sync.Mutex
	series     map[string]*series
}

type series struct {
	values []string
	value  float64
	counts []uint64 // histogram buckets (cumulative at render)
	sum    float64
	count  uint64
}

func (r *Registry) register(name, help string, kind metricKind, labels []string, buckets []float64) *metric {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.metrics[name]; ok {
		return m
	}
	m := &metric{name: name, help: help, kind: kind, labels: labels, buckets: buckets, series: map[string]*series{}}
	r.metrics[name] = m
	return m
}

func (m *metric) get(values []string) *series {
	if len(values) != len(m.labels) {
		panic(fmt.Sprintf("platform: metric %s wants %d label values, got %d", m.name, len(m.labels), len(values)))
	}
	k := strings.Join(values, "\x00")
	s, ok := m.series[k]
	if !ok {
		s = &series{values: slices.Clone(values)}
		if m.kind == kindHistogram {
			s.counts = make([]uint64, len(m.buckets))
		}
		m.series[k] = s
	}
	return s
}

// Counter is a monotonically increasing metric.
type Counter struct{ m *metric }

// Counter registers (or returns) a counter.
func (r *Registry) Counter(name, help string, labels ...string) Counter {
	return Counter{r.register(name, help, kindCounter, labels, nil)}
}

// Add adds v (>= 0) to the series with the given label values.
func (c Counter) Add(v float64, values ...string) {
	if v < 0 {
		return
	}
	c.m.mu.Lock()
	c.m.get(values).value += v
	c.m.mu.Unlock()
}

// Inc adds one.
func (c Counter) Inc(values ...string) { c.Add(1, values...) }

// Gauge is a metric that can go up and down.
type Gauge struct{ m *metric }

// Gauge registers (or returns) a gauge.
func (r *Registry) Gauge(name, help string, labels ...string) Gauge {
	return Gauge{r.register(name, help, kindGauge, labels, nil)}
}

// Set sets the series value.
func (g Gauge) Set(v float64, values ...string) {
	g.m.mu.Lock()
	g.m.get(values).value = v
	g.m.mu.Unlock()
}

// Add adds v (may be negative).
func (g Gauge) Add(v float64, values ...string) {
	g.m.mu.Lock()
	g.m.get(values).value += v
	g.m.mu.Unlock()
}

// Histogram counts observations into buckets.
type Histogram struct{ m *metric }

// DefaultBuckets are latency buckets in seconds.
var DefaultBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300}

// Histogram registers (or returns) a histogram; nil buckets means
// DefaultBuckets.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) Histogram {
	if buckets == nil {
		buckets = DefaultBuckets
	}
	return Histogram{r.register(name, help, kindHistogram, labels, buckets)}
}

// Observe records v.
func (h Histogram) Observe(v float64, values ...string) {
	h.m.mu.Lock()
	s := h.m.get(values)
	for i, b := range h.m.buckets {
		if v <= b {
			s.counts[i]++
			break
		}
	}
	s.sum += v
	s.count++
	h.m.mu.Unlock()
}

// Since observes the seconds elapsed since start.
func (h Histogram) Since(start time.Time, values ...string) {
	h.Observe(time.Since(start).Seconds(), values...)
}

// Value returns the current value of a counter or gauge series (tests).
func (r *Registry) Value(name string, values ...string) float64 {
	r.mu.Lock()
	m := r.metrics[name]
	r.mu.Unlock()
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.series[strings.Join(values, "\x00")]; ok {
		if m.kind == kindHistogram {
			return float64(s.count)
		}
		return s.value
	}
	return 0
}

// WriteText writes every metric in the Prometheus text format, sorted by
// name and label values.
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	names := make([]string, 0, len(r.metrics))
	for n := range r.metrics {
		names = append(names, n)
	}
	r.mu.Unlock()
	slices.Sort(names)
	var b strings.Builder
	for _, n := range names {
		r.mu.Lock()
		m := r.metrics[n]
		r.mu.Unlock()
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", m.name, escapeHelp(m.help), m.name, m.kind)
		m.mu.Lock()
		keys := make([]string, 0, len(m.series))
		for k := range m.series {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			s := m.series[k]
			if m.kind != kindHistogram {
				fmt.Fprintf(&b, "%s%s %s\n", m.name, labelText(m.labels, s.values, "", ""), formatFloat(s.value))
				continue
			}
			var cum uint64
			for i, ub := range m.buckets {
				cum += s.counts[i]
				fmt.Fprintf(&b, "%s_bucket%s %d\n", m.name, labelText(m.labels, s.values, "le", formatFloat(ub)), cum)
			}
			fmt.Fprintf(&b, "%s_bucket%s %d\n", m.name, labelText(m.labels, s.values, "le", "+Inf"), s.count)
			fmt.Fprintf(&b, "%s_sum%s %s\n", m.name, labelText(m.labels, s.values, "", ""), formatFloat(s.sum))
			fmt.Fprintf(&b, "%s_count%s %d\n", m.name, labelText(m.labels, s.values, "", ""), s.count)
		}
		m.mu.Unlock()
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Handler serves /metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = r.WriteText(w)
	})
}

func labelText(names, values []string, extraName, extraValue string) string {
	if len(names) == 0 && extraName == "" {
		return ""
	}
	parts := make([]string, 0, len(names)+1)
	for i, n := range names {
		parts = append(parts, n+`="`+escapeLabel(values[i])+`"`)
	}
	if extraName != "" {
		parts = append(parts, extraName+`="`+extraValue+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(s)
}

func escapeHelp(s string) string { return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s) }

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Metrics are the named metrics of spec 18.1 plus the RPC metrics every
// service records. Services use the fields that apply to them.
type Metrics struct {
	Registry             *Registry
	RunsTotal            Counter   // ff_runs_total{status}
	RunDuration          Histogram // ff_run_duration_seconds
	EffectsTotal         Counter   // ff_effects_total{kind,outcome}
	ScopeDecisions       Counter   // ff_scope_decisions_total{decision,reason}
	LimiterDenied        Counter   // ff_limiter_denied_total{level}
	ReplayNondeterminism Counter   // ff_replay_nondeterminism_total
	AgentTokens          Counter   // ff_agent_tokens_total
	AgentPlanRejected    Counter   // ff_agent_plan_rejected_total
	OutboxLag            Histogram // ff_outbox_lag_seconds{topic}
	GraphIngest          Histogram // ff_graph_ingest_seconds
	ApprovalsPending     Gauge     // ff_approvals_pending
	EgressRefused        Counter   // ff_egress_refused_total{reason}
	AlertsTotal          Counter   // ff_alerts_total{status}
	DeliveriesTotal      Counter   // ff_deliveries_total{kind,outcome}
	BusDeadLetters       Counter   // ff_bus_dead_letters_total{topic}
	RPCTotal             Counter   // ff_rpc_requests_total{method,code}
	RPCDuration          Histogram // ff_rpc_duration_seconds{method}
	// ScopeCheck is the latency of one ScopeService.Check (the 100 ms p99
	// SLO of infra/modules/monitoring). CheckBatch is not recorded here:
	// its latency grows with the batch (ff_rpc_duration_seconds has it).
	ScopeCheck Histogram // ff_scope_check_seconds
	// RunSubmitToStart is the time from a run's run_submitted event to its
	// run_started event (both event times), the 5 s p95 SLO.
	RunSubmitToStart Histogram // ff_run_submit_to_start_seconds
	// GraphPending counts observations waiting for referenced entity ids
	// (G-CORE-03) by event: buffered, released, expired, rejected (the
	// per-tenant bound was full).
	GraphPending Counter // ff_graph_pending_observations_total{event}
}

// NewMetrics registers the standard metrics in r (a new registry when nil).
func NewMetrics(r *Registry) *Metrics {
	if r == nil {
		r = NewRegistry()
	}
	return &Metrics{
		Registry:             r,
		RunsTotal:            r.Counter("ff_runs_total", "Runs that reached a terminal status.", "status"),
		RunDuration:          r.Histogram("ff_run_duration_seconds", "Run duration from start to finish.", nil),
		EffectsTotal:         r.Counter("ff_effects_total", "Effects by kind and outcome.", "kind", "outcome"),
		ScopeDecisions:       r.Counter("ff_scope_decisions_total", "Scope decisions by result and reason.", "decision", "reason"),
		LimiterDenied:        r.Counter("ff_limiter_denied_total", "Rate limiter denials.", "level"),
		ReplayNondeterminism: r.Counter("ff_replay_nondeterminism_total", "Runs failed with nondeterminism_detected."),
		AgentTokens:          r.Counter("ff_agent_tokens_total", "LLM tokens used by agent steps."),
		AgentPlanRejected:    r.Counter("ff_agent_plan_rejected_total", "Agent plans rejected."),
		OutboxLag:            r.Histogram("ff_outbox_lag_seconds", "Delay between an outbox write and its publication.", nil, "topic"),
		GraphIngest:          r.Histogram("ff_graph_ingest_seconds", "Graph ingest latency per observation.", nil),
		ApprovalsPending:     r.Gauge("ff_approvals_pending", "Approvals waiting for a decision (approximate, from events)."),
		EgressRefused:        r.Counter("ff_egress_refused_total", "Egress proxy refusals.", "reason"),
		AlertsTotal:          r.Counter("ff_alerts_total", "Alert events created.", "status"),
		DeliveriesTotal:      r.Counter("ff_deliveries_total", "Notification delivery attempts.", "kind", "outcome"),
		BusDeadLetters:       r.Counter("ff_bus_dead_letters_total", "Messages moved to a dead-letter topic.", "topic"),
		RPCTotal:             r.Counter("ff_rpc_requests_total", "RPCs handled.", "method", "code"),
		RPCDuration:          r.Histogram("ff_rpc_duration_seconds", "RPC handling time.", nil, "method"),
		ScopeCheck:           r.Histogram("ff_scope_check_seconds", "Latency of one scope check (ScopeService.Check).", nil),
		RunSubmitToStart:     r.Histogram("ff_run_submit_to_start_seconds", "Time from run_submitted to run_started.", nil),
		GraphPending:         r.Counter("ff_graph_pending_observations_total", "Observations waiting for referenced entities, by event.", "event"),
	}
}
