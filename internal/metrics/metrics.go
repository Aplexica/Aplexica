// Package metrics implements the BRD-03 FR-03.14 / FR-10.4 Prometheus-
// format metrics endpoint. The daemon exposes /metrics on a loopback
// listener (configurable via metrics.listen) when metrics.enabled is
// true; off by default.
//
// We render the Prometheus text exposition format by hand rather than
// pulling github.com/prometheus/client_golang as a dependency. The
// metric surface is small (≤ 20 series) and the format is stable:
// see https://prometheus.io/docs/instrumenting/exposition_formats/.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// decimalBase and float64BitSize name the stdlib strconv formatter
// arguments used throughout the renderer (base-10 integer formatting and
// 64-bit float formatting). They are §10.4 algorithmic constants — the
// fixed numeric encoding of the Prometheus text format, not tunables —
// named here so the exposition code carries no bare 10 / 64 literals.
const (
	decimalBase    = 10
	float64BitSize = 64
)

// SyncLatencyBuckets is the upper-bound ladder (seconds) for the
// sync_latency_seconds histogram NFR-10 §5.2 mandates. It is a fixed,
// well-known observability bucket set (the same ladder Prometheus ships
// as DefBuckets), so per NFR-10 §10.4 it is an algorithmic constant that
// lives in code rather than config. The ladder brackets the §3 SLO of a
// sub-2-second local sync 95th percentile: it has fine resolution around
// the 1-5 ms canonical-encode floor and around the 1-2 s target, and a
// long tail out to 10 s so a stall is still bucketed below +Inf.
//
// Bounds MUST be sorted ascending and MUST NOT contain +Inf — the
// renderer appends the implicit +Inf bucket. Buckets are cumulative
// (a value counts in every bound it is <=).
var SyncLatencyBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// histogram accumulates observations into a fixed bucket ladder plus a
// running sum and count, the three series Prometheus renders as
// <name>_bucket / <name>_sum / <name>_count.
type histogram struct {
	bounds []float64 // ascending upper bounds, shared (do not mutate)
	counts []uint64  // cumulative-at-render is computed in Render; stored per-bucket here
	sum    float64
	count  uint64
}

// Registry holds the metric instances. One Registry per daemon; the
// HTTP handler renders its content on each /metrics scrape.
type Registry struct {
	startedAt time.Time

	mu sync.RWMutex

	// counters: label-set → value. Each counter is keyed by name only
	// (no labels) for the simple no-label case, or by name+sorted-
	// label-string for labeled counters.
	counters map[string]map[string]*uint64

	// gauges: same shape as counters but they can be set absolutely.
	gauges map[string]map[string]*int64

	// histograms: name → labelKey → *histogram. The sync_latency_seconds
	// family NFR-10 §5.2 mandates lives here. Each label set gets its own
	// bucket ladder. Mutated under mu (atomics don't compose across the
	// three correlated fields), so observations take the write lock.
	histograms map[string]map[string]*histogram

	// adapterStates: name → state-string. Rendered as the labeled
	// gauge aplexica_adapter_state{name=…,state=…} = 1 per row.
	adapterStates map[string]string
}

// NewRegistry returns a fresh, empty Registry stamped with the
// daemon's start time.
func NewRegistry(now time.Time) *Registry {
	return &Registry{
		startedAt:     now,
		counters:      map[string]map[string]*uint64{},
		gauges:        map[string]map[string]*int64{},
		histograms:    map[string]map[string]*histogram{},
		adapterStates: map[string]string{},
	}
}

// counterPtr returns the *uint64 for (name, labelKey), creating it
// lazily. labelKey is the sorted "k=v,k=v" rendering of the label set
// (empty string for unlabeled).
func (r *Registry) counterPtr(name, labelKey string) *uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.counters[name]
	if !ok {
		m = map[string]*uint64{}
		r.counters[name] = m
	}
	p, ok := m[labelKey]
	if !ok {
		var v uint64
		p = &v
		m[labelKey] = p
	}
	return p
}

func (r *Registry) gaugePtr(name, labelKey string) *int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.gauges[name]
	if !ok {
		m = map[string]*int64{}
		r.gauges[name] = m
	}
	p, ok := m[labelKey]
	if !ok {
		var v int64
		p = &v
		m[labelKey] = p
	}
	return p
}

// IncCounter increments the named counter (with labels) by 1.
func (r *Registry) IncCounter(name string, labels map[string]string) {
	atomic.AddUint64(r.counterPtr(name, encodeLabels(labels)), 1)
}

// AddCounter increments by delta.
func (r *Registry) AddCounter(name string, delta uint64, labels map[string]string) {
	atomic.AddUint64(r.counterPtr(name, encodeLabels(labels)), delta)
}

// SetGauge sets a labeled gauge to v.
func (r *Registry) SetGauge(name string, v int64, labels map[string]string) {
	atomic.StoreInt64(r.gaugePtr(name, encodeLabels(labels)), v)
}

// ObserveHistogram records one observation v into the named histogram
// for the given label set, using SyncLatencyBuckets as the bucket
// ladder. Used for sync_latency_seconds (NFR-10 §5.2). The three
// correlated fields (per-bucket counts, sum, count) are updated under
// the registry write lock so a concurrent Render never sees a torn
// triplet. Negative observations are still recorded (they count in
// every bucket); callers measuring wall-clock latency pass a
// non-negative seconds value.
func (r *Registry) ObserveHistogram(name string, v float64, labels map[string]string) {
	labelKey := encodeLabels(labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.histograms[name]
	if !ok {
		m = map[string]*histogram{}
		r.histograms[name] = m
	}
	h, ok := m[labelKey]
	if !ok {
		h = &histogram{
			bounds: SyncLatencyBuckets,
			counts: make([]uint64, len(SyncLatencyBuckets)),
		}
		m[labelKey] = h
	}
	// Store NON-cumulatively: bump only the single narrowest bucket the
	// value falls into (the first bound it is <=). Render walks the bounds
	// ascending and accumulates to produce the cumulative counts Prometheus
	// requires. A value above every finite bound bumps no finite bucket; it
	// is still counted in h.count, which is what the +Inf bucket renders.
	for i, ub := range h.bounds {
		if v <= ub {
			h.counts[i]++
			break
		}
	}
	h.sum += v
	h.count++
}

// SetAdapterState records the state string for an adapter (used by
// the daemon to report active/quarantined/etc). Replaces any prior
// value.
func (r *Registry) SetAdapterState(name, state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapterStates[name] = state
}

// RegisterCounter / RegisterGauge / RegisterHistogram pre-declare a metric
// family so Render emits its # TYPE / # HELP header even before any
// observation lands. They create the family's (empty) series map without
// adding a child series, so a scrape advertises the full NFR-10 §5.2
// mandated set with no spurious zero-valued rows for unknown label sets.
// Idempotent: registering an already-touched family leaves its data intact.
func (r *Registry) RegisterCounter(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.counters[name]; !ok {
		r.counters[name] = map[string]*uint64{}
	}
}

func (r *Registry) RegisterGauge(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.gauges[name]; !ok {
		r.gauges[name] = map[string]*int64{}
	}
}

func (r *Registry) RegisterHistogram(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.histograms[name]; !ok {
		r.histograms[name] = map[string]*histogram{}
	}
}

// encodeLabels renders a label set as a sorted "k=\"v\",k=\"v\""
// fragment for the Prometheus exposition format. Empty map → "".
func encodeLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteByte('"')
		b.WriteString(escapeLabelValue(labels[k]))
		b.WriteByte('"')
	}
	return b.String()
}

func escapeLabelValue(s string) string {
	// Per Prometheus text format: backslash, double-quote, and newline
	// must be escaped.
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

// formatFloat renders a float for the Prometheus text format: the
// shortest decimal that round-trips ('g' with -1 precision), so 0.1
// stays "0.1" and 1.55 stays "1.55" rather than acquiring binary-float
// noise. Used for histogram bucket bounds, _sum values, and the
// reserved +Inf sentinel is handled by the caller.
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, float64BitSize)
}

// Render produces the Prometheus text-exposition-format snapshot.
// Called from the /metrics HTTP handler on every scrape.
func (r *Registry) Render(now time.Time) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var b strings.Builder
	// Synthesized: uptime gauge. Rendered under BOTH the spec-mandated
	// NFR-10 §5.2 name (daemon_uptime_seconds) and the historical
	// aplexica_-prefixed name kept for back-compat with existing
	// scrape configs. Both carry the same value.
	upSec := int64(now.Sub(r.startedAt).Seconds())
	b.WriteString("# HELP daemon_uptime_seconds ")
	b.WriteString(helpFor("daemon_uptime_seconds"))
	b.WriteByte('\n')
	b.WriteString("# TYPE daemon_uptime_seconds gauge\n")
	b.WriteString("daemon_uptime_seconds ")
	b.WriteString(strconv.FormatInt(upSec, decimalBase))
	b.WriteByte('\n')
	b.WriteString("# HELP aplexica_uptime_seconds Time since the daemon started.\n")
	b.WriteString("# TYPE aplexica_uptime_seconds gauge\n")
	b.WriteString("aplexica_uptime_seconds ")
	b.WriteString(strconv.FormatInt(upSec, decimalBase))
	b.WriteByte('\n')

	// Counters.
	for _, name := range sortedKeys(r.counters) {
		b.WriteString("# HELP ")
		b.WriteString(name)
		b.WriteByte(' ')
		b.WriteString(helpFor(name))
		b.WriteByte('\n')
		b.WriteString("# TYPE ")
		b.WriteString(name)
		b.WriteString(" counter\n")
		for _, labelKey := range sortedKeys(r.counters[name]) {
			b.WriteString(name)
			if labelKey != "" {
				b.WriteByte('{')
				b.WriteString(labelKey)
				b.WriteByte('}')
			}
			b.WriteByte(' ')
			b.WriteString(strconv.FormatUint(atomic.LoadUint64(r.counters[name][labelKey]), decimalBase))
			b.WriteByte('\n')
		}
	}

	// Gauges.
	for _, name := range sortedKeys(r.gauges) {
		b.WriteString("# HELP ")
		b.WriteString(name)
		b.WriteByte(' ')
		b.WriteString(helpFor(name))
		b.WriteByte('\n')
		b.WriteString("# TYPE ")
		b.WriteString(name)
		b.WriteString(" gauge\n")
		for _, labelKey := range sortedKeys(r.gauges[name]) {
			b.WriteString(name)
			if labelKey != "" {
				b.WriteByte('{')
				b.WriteString(labelKey)
				b.WriteByte('}')
			}
			b.WriteByte(' ')
			b.WriteString(strconv.FormatInt(atomic.LoadInt64(r.gauges[name][labelKey]), decimalBase))
			b.WriteByte('\n')
		}
	}

	// Histograms — one _bucket ladder + _sum + _count per label set.
	// Bucket counts are stored per-bucket (non-cumulative); Prometheus
	// requires them cumulative and ending in an +Inf bucket, so we
	// accumulate as we walk the ascending bounds and emit +Inf last.
	for _, name := range sortedKeys(r.histograms) {
		b.WriteString("# HELP ")
		b.WriteString(name)
		b.WriteByte(' ')
		b.WriteString(helpFor(name))
		b.WriteByte('\n')
		b.WriteString("# TYPE ")
		b.WriteString(name)
		b.WriteString(" histogram\n")
		for _, labelKey := range sortedKeys(r.histograms[name]) {
			h := r.histograms[name][labelKey]
			var cumulative uint64
			for i, ub := range h.bounds {
				cumulative += h.counts[i]
				b.WriteString(name)
				b.WriteString("_bucket{")
				if labelKey != "" {
					b.WriteString(labelKey)
					b.WriteByte(',')
				}
				b.WriteString(`le="`)
				b.WriteString(formatFloat(ub))
				b.WriteString(`"} `)
				b.WriteString(strconv.FormatUint(cumulative, decimalBase))
				b.WriteByte('\n')
			}
			// Mandatory +Inf bucket counts every observation.
			b.WriteString(name)
			b.WriteString("_bucket{")
			if labelKey != "" {
				b.WriteString(labelKey)
				b.WriteByte(',')
			}
			b.WriteString(`le="+Inf"} `)
			b.WriteString(strconv.FormatUint(h.count, decimalBase))
			b.WriteByte('\n')
			// _sum and _count carry only the histogram's own labels.
			b.WriteString(name)
			b.WriteString("_sum")
			if labelKey != "" {
				b.WriteByte('{')
				b.WriteString(labelKey)
				b.WriteByte('}')
			}
			b.WriteByte(' ')
			b.WriteString(formatFloat(h.sum))
			b.WriteByte('\n')
			b.WriteString(name)
			b.WriteString("_count")
			if labelKey != "" {
				b.WriteByte('{')
				b.WriteString(labelKey)
				b.WriteByte('}')
			}
			b.WriteByte(' ')
			b.WriteString(strconv.FormatUint(h.count, decimalBase))
			b.WriteByte('\n')
		}
	}

	// Adapter state — synthesized gauge per (name, state).
	if len(r.adapterStates) > 0 {
		b.WriteString("# HELP aplexica_adapter_state Adapter state (1 = the labeled state is currently active).\n")
		b.WriteString("# TYPE aplexica_adapter_state gauge\n")
		names := make([]string, 0, len(r.adapterStates))
		for n := range r.adapterStates {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(&b, "aplexica_adapter_state{name=%q,state=%q} 1\n",
				n, r.adapterStates[n])
		}
	}

	return b.String()
}

// sortedKeys returns the keys of any map[string]V as a sorted slice.
// Using generic sortedKeys keeps Render simple.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// helpFor returns the documentation string for a known metric name.
// Unknown names fall back to a generic "Aplexica metric." so the
// renderer always produces a valid # HELP line.
func helpFor(name string) string {
	switch name {
	case "aplexica_events_total":
		return "Total ACF events processed by the daemon, by kind and direction."
	case "aplexica_conflicts_total":
		return "Total conflicts detected since daemon start."
	case "aplexica_conflicts_unresolved":
		return "Currently unresolved conflict count."
	case "aplexica_pending_projects":
		return "Number of staged project-scope artifacts whose project ID isn't linked locally."
	case "aplexica_paused":
		return "1 if sync is paused (global or per-adapter), 0 otherwise."

	// NFR-10 §5.2 mandated metric families.
	case "daemon_uptime_seconds":
		return "Time since the daemon started, in seconds."
	case "adapter_events_inbound_total":
		return "Total ACF events imported from agents into the canonical store, by adapter."
	case "adapter_events_outbound_total":
		return "Total ACF events materialized out to agents (fan-out), by adapter."
	case "adapter_errors_total":
		return "Total adapter operation errors since daemon start, by adapter."
	case "store_size_bytes":
		return "Approximate on-disk size of the canonical store, in bytes."
	case "store_events_total":
		return "Total number of events held in the canonical store."
	case "conflict_count":
		return "Currently unresolved conflict count."
	case "queue_depth":
		return "Number of artifacts pending import."
	case "sync_latency_seconds":
		return "Import-to-fan-out materialization latency per synced artifact, in seconds."
	}
	return "Aplexica metric."
}

// Handler returns an http.Handler that renders the registry on every
// request. Suitable for serving /metrics via http.ServeMux.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(r.Render(time.Now())))
	})
}
