// Package observe provides a dependency-free Prometheus metrics registry
// (design/60 6.1, design/80 B-09). It implements the subset ADC needs —
// gauges, counters and fixed-bucket histograms — rendered by hand in the
// Prometheus text exposition format 0.0.4 with the standard library only.
// prometheus/client_golang is deliberately avoided so the ce module gains
// zero new dependencies; swap in the full client if its feature set (pull
// collectors, OpenMetrics) becomes necessary.
package observe

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// contentHeader is the Prometheus text exposition content type (format 0.0.4).
const contentHeader = "text/plain; version=0.0.4; charset=utf-8"

// labelSep joins label values inside vec keys. A NUL byte cannot occur in the
// controlled label values ADC emits (tenant ids, route patterns, status
// codes), and the vec itself is the only producer of keys.
const labelSep = "\x00"

// escapeLabelValue renders a label value in Prometheus quoted-string syntax:
// backslash, double quote and newline are escaped, everything else passes
// through verbatim.
func escapeLabelValue(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	var b strings.Builder
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// renderLabels writes {name="value",...} in declaration order ("" when
// there are no labels).
func renderLabels(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(values[i]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// formatFloat renders a float the way strconv does with 'g' precision -1,
// so integral values stay compact ("3" not "3.000000").
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Gauge is a float64 value with atomic set/add. The zero value is usable.
type Gauge struct {
	bits atomic.Uint64
}

// Set stores v unconditionally.
func (g *Gauge) Set(v float64) { g.bits.Store(math.Float64bits(v)) }

// Add atomically adds delta to the current value.
func (g *Gauge) Add(delta float64) {
	for {
		old := g.bits.Load()
		if g.bits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+delta)) {
			return
		}
	}
}

// Get returns the current value.
func (g *Gauge) Get() float64 { return math.Float64frombits(g.bits.Load()) }

// noopGauge absorbs writes routed through nil vecs (instrumentation is
// optional: nil seam = metrics disabled, FR-016).
var noopGauge = &Gauge{}

// Counter is a monotonically increasing uint64. The zero value is usable.
type Counter struct {
	n atomic.Uint64
}

// Inc increments by one.
func (c *Counter) Inc() { c.n.Add(1) }

// Add increments by delta.
func (c *Counter) Add(delta uint64) { c.n.Add(delta) }

// Get returns the current count.
func (c *Counter) Get() uint64 { return c.n.Load() }

// noopCounter absorbs increments routed through nil vecs.
var noopCounter = &Counter{}

// Histogram is a fixed-bucket histogram with hand-rolled buckets (design/80
// B-09). Bucket counts are stored cumulatively (counts[i] = observations <=
// buckets[i]); the +Inf bucket is implied by the total count.
type Histogram struct {
	buckets []float64
	counts  []atomic.Uint64
	count   atomic.Uint64
	sumBits atomic.Uint64
}

func newHistogram(buckets []float64) *Histogram {
	return &Histogram{buckets: buckets, counts: make([]atomic.Uint64, len(buckets))}
}

// Observe records one observation (duration in seconds for the ADC set).
func (h *Histogram) Observe(v float64) {
	h.count.Add(1)
	for {
		old := h.sumBits.Load()
		if h.sumBits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+v)) {
			break
		}
	}
	// Cumulative: walk from the largest bucket downwards and bump every
	// bucket whose upper bound is >= v.
	for i := len(h.buckets) - 1; i >= 0; i-- {
		if v <= h.buckets[i] {
			h.counts[i].Add(1)
		} else {
			break
		}
	}
}

// noopHistogram absorbs observations routed through nil vecs.
var noopHistogram = newHistogram(nil)

// gaugeEntry is one label-value combination of a GaugeVec.
type gaugeEntry struct {
	values []string
	g      *Gauge
}

// GaugeVec is a family of gauges keyed by label values. A nil *GaugeVec is a
// valid no-op (all writes are dropped), so callers can pass a nil metrics
// seam without branching.
type GaugeVec struct {
	mu sync.Mutex
	m  map[string]*gaugeEntry
}

// With returns the gauge for the given label values (declaration order).
func (v *GaugeVec) With(values ...string) *Gauge {
	if v == nil {
		return noopGauge
	}
	key := strings.Join(values, labelSep)
	v.mu.Lock()
	defer v.mu.Unlock()
	if e, ok := v.m[key]; ok {
		return e.g
	}
	e := &gaugeEntry{values: append([]string(nil), values...), g: &Gauge{}}
	v.m[key] = e
	return e.g
}

// Get returns the current value of one label combination (0 when absent).
// Test helper; the exporter uses snapshot.
func (v *GaugeVec) Get(values ...string) float64 {
	if v == nil {
		return 0
	}
	key := strings.Join(values, labelSep)
	v.mu.Lock()
	defer v.mu.Unlock()
	if e, ok := v.m[key]; ok {
		return e.g.Get()
	}
	return 0
}

// gaugeSample is a snapshot of one label combination.
type gaugeSample struct {
	values []string
	v      float64
}

// snapshot returns the current label combinations sorted by key for stable
// text output.
func (v *GaugeVec) snapshot() []gaugeSample {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]gaugeSample, 0, len(v.m))
	for _, e := range v.m {
		out = append(out, gaugeSample{values: e.values, v: e.g.Get()})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.Join(out[i].values, labelSep) < strings.Join(out[j].values, labelSep)
	})
	return out
}

// counterEntry is one label-value combination of a CounterVec.
type counterEntry struct {
	values []string
	c      *Counter
}

// CounterVec is a family of counters keyed by label values. A nil *CounterVec
// is a valid no-op.
type CounterVec struct {
	mu sync.Mutex
	m  map[string]*counterEntry
}

// With returns the counter for the given label values (declaration order).
func (v *CounterVec) With(values ...string) *Counter {
	if v == nil {
		return noopCounter
	}
	key := strings.Join(values, labelSep)
	v.mu.Lock()
	defer v.mu.Unlock()
	if e, ok := v.m[key]; ok {
		return e.c
	}
	e := &counterEntry{values: append([]string(nil), values...), c: &Counter{}}
	v.m[key] = e
	return e.c
}

// Get returns the current count of one label combination (0 when absent).
// Test helper; the exporter uses snapshot.
func (v *CounterVec) Get(values ...string) uint64 {
	if v == nil {
		return 0
	}
	key := strings.Join(values, labelSep)
	v.mu.Lock()
	defer v.mu.Unlock()
	if e, ok := v.m[key]; ok {
		return e.c.Get()
	}
	return 0
}

// counterSample is a snapshot of one label combination.
type counterSample struct {
	values []string
	n      uint64
}

// snapshot returns the current label combinations sorted by key for stable
// text output.
func (v *CounterVec) snapshot() []counterSample {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]counterSample, 0, len(v.m))
	for _, e := range v.m {
		out = append(out, counterSample{values: e.values, n: e.c.Get()})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.Join(out[i].values, labelSep) < strings.Join(out[j].values, labelSep)
	})
	return out
}

// histEntry is one label-value combination of a HistogramVec.
type histEntry struct {
	values []string
	h      *Histogram
}

// HistogramVec is a family of histograms keyed by label values. A nil
// *HistogramVec is a valid no-op.
type HistogramVec struct {
	mu      sync.Mutex
	buckets []float64
	m       map[string]*histEntry
}

// With returns the histogram for the given label values (declaration order).
func (v *HistogramVec) With(values ...string) *Histogram {
	if v == nil {
		return noopHistogram
	}
	key := strings.Join(values, labelSep)
	v.mu.Lock()
	defer v.mu.Unlock()
	if e, ok := v.m[key]; ok {
		return e.h
	}
	e := &histEntry{values: append([]string(nil), values...), h: newHistogram(v.buckets)}
	v.m[key] = e
	return e.h
}

// histSample is a snapshot of one label combination.
type histSample struct {
	values []string
	counts []uint64
	sum    float64
	count  uint64
}

// snapshot returns the current label combinations sorted by key for stable
// text output.
func (v *HistogramVec) snapshot() []histSample {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]histSample, 0, len(v.m))
	for _, e := range v.m {
		counts := make([]uint64, len(v.buckets))
		for i := range v.buckets {
			counts[i] = e.h.counts[i].Load()
		}
		out = append(out, histSample{
			values: e.values,
			counts: counts,
			sum:    math.Float64frombits(e.h.sumBits.Load()),
			count:  e.h.count.Load(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.Join(out[i].values, labelSep) < strings.Join(out[j].values, labelSep)
	})
	return out
}

// entry is one registered metric family. Exactly one vec field is non-nil.
type entry struct {
	name       string
	help       string
	typ        string // "gauge" | "counter" | "histogram"
	labelNames []string
	gauge      *GaugeVec
	counter    *CounterVec
	histogram  *HistogramVec
}

// write renders the HELP/TYPE header plus every sample of the family.
func (e *entry) write(buf *bytes.Buffer) {
	fmt.Fprintf(buf, "# HELP %s %s\n", e.name, e.help)
	fmt.Fprintf(buf, "# TYPE %s %s\n", e.name, e.typ)
	switch e.typ {
	case "gauge":
		for _, s := range e.gauge.snapshot() {
			fmt.Fprintf(buf, "%s%s %s\n", e.name, renderLabels(e.labelNames, s.values), formatFloat(s.v))
		}
	case "counter":
		for _, s := range e.counter.snapshot() {
			fmt.Fprintf(buf, "%s%s %d\n", e.name, renderLabels(e.labelNames, s.values), s.n)
		}
	case "histogram":
		e.writeHistogram(buf)
	}
}

// writeHistogram renders _bucket/_sum/_count series; the le label is
// appended after the family's own labels (Prometheus convention).
func (e *entry) writeHistogram(buf *bytes.Buffer) {
	v := e.histogram
	for _, s := range v.snapshot() {
		for i, ub := range v.buckets {
			names := append(append([]string(nil), e.labelNames...), "le")
			vals := append(append([]string(nil), s.values...), formatFloat(ub))
			fmt.Fprintf(buf, "%s_bucket%s %d\n", e.name, renderLabels(names, vals), s.counts[i])
		}
		infNames := append(append([]string(nil), e.labelNames...), "le")
		infVals := append(append([]string(nil), s.values...), "+Inf")
		fmt.Fprintf(buf, "%s_bucket%s %d\n", e.name, renderLabels(infNames, infVals), s.count)
		fmt.Fprintf(buf, "%s_sum%s %s\n", e.name, renderLabels(e.labelNames, s.values), formatFloat(s.sum))
		fmt.Fprintf(buf, "%s_count%s %d\n", e.name, renderLabels(e.labelNames, s.values), s.count)
	}
}

// Registry holds metric families and exposes them in text format.
type Registry struct {
	mu      sync.Mutex
	entries map[string]*entry
	order   []string
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]*entry)}
}

// NewGauge registers a gauge family with the given label names (0 = plain).
func (r *Registry) NewGauge(name, help string, labelNames ...string) *GaugeVec {
	v := &GaugeVec{m: make(map[string]*gaugeEntry)}
	r.register(&entry{name: name, help: help, typ: "gauge", labelNames: labelNames, gauge: v})
	return v
}

// NewCounter registers a counter family with the given label names.
func (r *Registry) NewCounter(name, help string, labelNames ...string) *CounterVec {
	v := &CounterVec{m: make(map[string]*counterEntry)}
	r.register(&entry{name: name, help: help, typ: "counter", labelNames: labelNames, counter: v})
	return v
}

// NewHistogram registers a histogram family with the given buckets and label
// names.
func (r *Registry) NewHistogram(name, help string, buckets []float64, labelNames ...string) *HistogramVec {
	v := &HistogramVec{buckets: buckets, m: make(map[string]*histEntry)}
	r.register(&entry{name: name, help: help, typ: "histogram", labelNames: labelNames, histogram: v})
	return v
}

// register fails fast on duplicate names: duplicate registration is a
// programming error, not a runtime condition.
func (r *Registry) register(e *entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.entries[e.name]; dup {
		panic("observe: duplicate metric name " + e.name)
	}
	r.entries[e.name] = e
	r.order = append(r.order, e.name)
}

// Handler serves the exposition text of every registered family, sorted by
// metric name for deterministic output.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentHeader)
		var buf bytes.Buffer
		r.writeTo(&buf)
		_, _ = w.Write(buf.Bytes())
	})
}

func (r *Registry) writeTo(buf *bytes.Buffer) {
	r.mu.Lock()
	names := append([]string(nil), r.order...)
	entries := make(map[string]*entry, len(names))
	for _, n := range names {
		entries[n] = r.entries[n]
	}
	r.mu.Unlock()
	sort.Strings(names)
	for _, n := range names {
		entries[n].write(buf)
	}
}
