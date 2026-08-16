package alerts

// MetricReader seam of the evaluator. The B2 slice self-scrapes the local
// /metrics exposition (observe.Handler) instead of querying Prometheus:
// the exposition is dependency-free text, the parser below is ~60 lines,
// and the evaluator sees exactly the series the console dashboard renders.
// Production cluster alerting stays with deploy/prometheus/alert-rules.yml
// (Prometheus query semantics: rate(), for, offset); this package covers
// tenant-configurable absolute thresholds only.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Series is one label-value combination of a metric family as parsed from
// the exposition text. Labels is empty for plain metrics.
type Series struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// MetricReader returns the current series snapshot. Implementations must
// be safe for concurrent use by the evaluator loop.
type MetricReader interface {
	Read(ctx context.Context) ([]Series, error)
}

// StaticReader serves a fixed snapshot (tests, offline assemblies).
type StaticReader struct {
	Series []Series
	Err    error
}

// Read implements MetricReader.
func (s *StaticReader) Read(context.Context) ([]Series, error) { return s.Series, s.Err }

// defaultScrapeClient is shared by HTTPReaders without an injected client;
// a 5s timeout keeps one slow scrape from wedging the 30s evaluation loop.
var defaultScrapeClient = &http.Client{Timeout: 5 * time.Second}

// HTTPReader pulls the exposition text from one endpoint (the local
// internal port's /metrics in the adc assembly).
type HTTPReader struct {
	URL    string
	Client *http.Client
}

// NewHTTPReader builds a reader for the given metrics URL.
func NewHTTPReader(metricsURL string) *HTTPReader {
	return &HTTPReader{URL: metricsURL}
}

// Read implements MetricReader: GET the URL and parse the exposition.
func (r *HTTPReader) Read(ctx context.Context) ([]Series, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("alerts: build scrape request: %w", err)
	}
	req.Header.Set("Accept", "text/plain")
	c := r.Client
	if c == nil {
		c = defaultScrapeClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("alerts: scrape %s: %w", r.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("alerts: scrape %s rejected with HTTP %d", r.URL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("alerts: read scrape body: %w", err)
	}
	return ParseExposition(body)
}

// ParseExposition parses Prometheus text exposition format 0.0.4 lines
// into series (HELP/TYPE/comment lines and EOF markers are skipped).
// Values that are not finite are dropped: a NaN histogram or gauge is not
// a number a threshold can compare.
func ParseExposition(text []byte) ([]Series, error) {
	var out []Series
	for _, line := range bytes.Split(text, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		if bytes.Equal(line, []byte("# EOF")) {
			continue
		}
		s, ok := parseSeriesLine(string(line))
		if !ok {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// parseSeriesLine handles "name{label="value"} 1.5" (and the bare
// "name 1.5" form). Malformed lines yield ok=false rather than an error:
// the exposition is advisory data and one corrupt line must not kill the
// evaluation cycle.
func parseSeriesLine(line string) (Series, bool) {
	var s Series
	brace := strings.IndexByte(line, '{')
	nameEnd := len(line)
	if brace >= 0 {
		nameEnd = brace
	} else if sp := strings.IndexByte(line, ' '); sp > 0 {
		nameEnd = sp
	}
	name := line[:nameEnd]
	if name == "" {
		return s, false
	}
	s.Name = name
	rest := line[nameEnd:]
	if brace >= 0 {
		closeBrace := strings.IndexByte(rest, '}')
		if closeBrace < 0 {
			return s, false
		}
		if labels, ok := parseLabels(rest[1:closeBrace]); ok {
			s.Labels = labels
		} else {
			return s, false
		}
		rest = rest[closeBrace+1:]
	}
	valueText := strings.TrimSpace(rest)
	if valueText == "" {
		return s, false
	}
	switch valueText {
	case "NaN", "+Inf", "-Inf":
		return s, false
	}
	v, err := parseFloat(valueText)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return s, false
	}
	s.Value = v
	return s, true
}

// parseLabels splits the inside of {...} into a label map. The observe
// exporter quotes values only when they contain \ " or newline, so both
// quoted and bare values are accepted.
func parseLabels(raw string) (map[string]string, bool) {
	labels := map[string]string{}
	if raw == "" {
		return labels, true
	}
	var parts []string
	var cur strings.Builder
	inQuote := false
	escaped := false
	for _, ch := range raw {
		switch {
		case escaped:
			cur.WriteRune(ch)
			escaped = false
		case ch == '\\' && inQuote:
			escaped = true
		case ch == '"':
			inQuote = !inQuote
		case ch == ',' && !inQuote:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(ch)
		}
	}
	parts = append(parts, cur.String())
	for _, part := range parts {
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			return nil, false
		}
		key := strings.TrimSpace(part[:eq])
		value := strings.TrimSpace(part[eq+1:])
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		if key == "" {
			return nil, false
		}
		labels[key] = value
	}
	return labels, true
}

// parseFloat is strconv.ParseFloat restricted to the exponent-free values
// observe emits ('g' precision rendering keeps integers compact).
func parseFloat(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

// sumMatching returns the sum of the family's series that satisfy the
// filters. Tenant semantics: when any series of the family carries a
// tenant label, only the rule's tenant series count (tenant-scoped rule
// on a tenant-labeled family); otherwise every series counts (platform
// metrics like the PG pool watermark have no tenant dimension).
func sumMatching(family []Series, tenantID string, filters map[string]string) (float64, bool) {
	hasTenant := false
	for _, s := range family {
		if _, ok := s.Labels["tenant"]; ok {
			hasTenant = true
			break
		}
	}
	var sum float64
	matched := 0
	for _, s := range family {
		if hasTenant && s.Labels["tenant"] != tenantID {
			continue
		}
		ok := true
		for k, v := range filters {
			if s.Labels[k] != v {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		sum += s.Value
		matched++
	}
	return sum, matched > 0
}

// seriesByName indexes a snapshot by family name for the evaluator.
func seriesByName(all []Series) map[string][]Series {
	idx := make(map[string][]Series, len(all))
	for _, s := range all {
		idx[s.Name] = append(idx[s.Name], s)
	}
	return idx
}
