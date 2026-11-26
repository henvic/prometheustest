// Package prometheustest lets Go tests check Prometheus metrics.
package prometheustest

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// Expect gathers metrics and reports mismatches.
// Write each expected sample in Prometheus text format, with the following
// additional markers:
//
//   - '*' for samples with any value, including zero.
//   - '+' for samples with a value greater than zero.
//   - '-' to require absence of a metric.
//
// For counters, gauges, and untyped metrics, list every nonzero series in
// each family you check.
//
// Blank lines and comments are ignored. Sample and label order don’t matter.
//
// For example:
//
//	Expect(t, registry, `
//	http_requests_total{code="200"} +
//	http_requests_total{code="201"} 5
//	http_requests_total{code="500"} -
//	`)
func Expect(tb testing.TB, gatherer prometheus.Gatherer, metrics string) {
	tb.Helper()
	expected, err := parse(metrics, allMarkers)
	if err != nil {
		tb.Errorf("invalid want metrics: %v", err)
		return
	}
	if len(expected) == 0 {
		return
	}
	actual, err := collect(gatherer)
	if err != nil {
		tb.Errorf("%v", err)
		return
	}
	selected := make(map[string]bool)
	want := make(map[string]metric)
	for _, key := range slices.Sorted(maps.Keys(expected)) {
		m := expected[key]
		if m.match == absent {
			if actual.contains(key, m) {
				tb.Errorf("Prometheus metric %s is present, want absent (-)", key)
			}
			if !m.labeled {
				continue
			}
		} else {
			want[key] = m
		}
		if _, ok := actual.families[m.name]; ok {
			selected[m.name] = true
		} else if owner, ok := actual.owners[m.name]; ok {
			selected[owner] = true
		}
	}
	got := make(map[string]metric)
	var unlisted []string
	for key, m := range actual.samples {
		if !selected[m.family] {
			continue
		}
		if expectation, listed := expected[key]; listed {
			if expectation.match == absent {
				continue
			}
		} else {
			switch actual.families[m.family].kind {
			case dto.MetricType_HISTOGRAM, dto.MetricType_GAUGE_HISTOGRAM, dto.MetricType_SUMMARY:
				continue
			}
			if m.value != 0 {
				unlisted = append(unlisted, m.line(key))
			}
			continue
		}
		got[key] = m
	}
	if len(unlisted) > 0 {
		slices.Sort(unlisted)
		tb.Errorf("unlisted Prometheus samples must be zero:\n%s", strings.Join(unlisted, "\n"))
	}
	if diff := diffSamples(want, got); diff != "" {
		tb.Errorf("Prometheus metrics mismatch (-want +got):\n%s", diff)
	}
}

// Diff returns a human-readable report of differences between the expectations
// in want and the complete set of samples in got.
// If either input is invalid, Diff returns an error.
// If the metrics match, the report is empty.
//
// In want, you can use:
//
//   - '*' for samples with any value, including zero.
//   - '+' for samples with a value greater than zero.
//
// Only [Expect] accepts '-' as an absence marker. Diff reports every sample
// in got that is missing from want, even if its value is zero.
//
// Blank lines, comments, and the order of samples and labels are ignored.
//
// For example:
//
//	report, err := Diff("requests_total +", "requests_total 0")
//	if err != nil {
//		log.Fatal(err)
//	}
//	fmt.Println(report)
//	// Output:
//	// - requests_total +
//	// + requests_total 0
func Diff(want, got string) (diff string, err error) {
	expected, err := parse(want, valueMarkers)
	if err != nil {
		return "", fmt.Errorf("invalid want metrics: %w", err)
	}
	actual, err := parse(quoteColonLabels(got), noMarkers)
	if err != nil {
		return "", fmt.Errorf("invalid got metrics: %w", err)
	}
	return diffSamples(expected, actual), nil
}

type matcher string

const (
	exact    matcher = ""
	anyValue matcher = "*"
	positive matcher = "+"
	absent   matcher = "-"
)

type metric struct {
	name      string
	labeled   bool
	value     float64
	timestamp *int64
	match     matcher
	family    string // Originating gathered family, including for derived samples.
}

// line formats m with key as a Prometheus text sample line.
func (m metric) line(key string) string {
	value := string(m.match)
	if m.match == exact {
		value = strconv.FormatFloat(m.value, 'g', -1, 64)
	}
	line := key + " " + value
	if m.timestamp != nil {
		line += " " + strconv.FormatInt(*m.timestamp, 10)
	}
	return line
}

// matches reports whether got satisfies m's timestamp and value matcher.
func (m metric) matches(got metric) bool {
	if (m.timestamp == nil) != (got.timestamp == nil) {
		return false
	}
	if m.timestamp != nil && *m.timestamp != *got.timestamp {
		return false
	}
	switch m.match {
	case anyValue:
		return true
	case positive:
		return got.value > 0
	default:
		return m.value == got.value || (math.IsNaN(m.value) && math.IsNaN(got.value))
	}
}

// diffSamples returns a sorted -want/+got diff of mismatched samples.
func diffSamples(expected, actual map[string]metric) string {
	keys := slices.Collect(maps.Keys(expected))
	for key := range actual {
		if _, listed := expected[key]; !listed {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	var lines []string
	for _, key := range keys {
		want, inWant := expected[key]
		got, inGot := actual[key]
		if inWant && inGot && want.matches(got) {
			continue
		}
		if inWant {
			lines = append(lines, "- "+want.line(key))
		}
		if inGot {
			lines = append(lines, "+ "+got.line(key))
		}
	}
	return strings.Join(lines, "\n")
}

type family struct {
	kind   dto.MetricType
	series map[string]bool
}

type collection struct {
	samples  map[string]metric
	families map[string]family
	owners   map[string]string // Exposed sample name to originating family name.
}

// nilGatherer checks for a nil interface, a nil *prometheus.Registry, or a nil prometheus.GathererFunc.
func nilGatherer(gatherer prometheus.Gatherer) bool {
	switch g := gatherer.(type) {
	case *prometheus.Registry:
		return g == nil
	case prometheus.GathererFunc:
		return g == nil
	default:
		return gatherer == nil
	}
}

// collect gathers metrics and records which family each sample and series belongs to
func collect(gatherer prometheus.Gatherer) (collection, error) {
	if nilGatherer(gatherer) {
		return collection{}, errors.New("cannot gather Prometheus metrics: nil gatherer")
	}
	families, err := gatherer.Gather()
	if err != nil {
		return collection{}, fmt.Errorf("cannot gather Prometheus metrics: %w", err)
	}
	result := collection{
		samples:  make(map[string]metric),
		families: make(map[string]family),
		owners:   make(map[string]string),
	}
	for _, f := range families {
		if f == nil {
			return collection{}, errors.New("cannot gather Prometheus metrics: nil metric family")
		}
		name := f.GetName()
		if _, exists := result.families[name]; exists {
			return collection{}, fmt.Errorf("cannot gather Prometheus metrics: duplicate family %q", name)
		}
		series := make(map[string]bool)
		for _, m := range f.GetMetric() {
			if m == nil {
				return collection{}, fmt.Errorf("cannot gather Prometheus metric family %q: nil metric", name)
			}
			series[metricKey(name, m.GetLabel())] = true
		}
		var text strings.Builder
		if _, err := expfmt.MetricFamilyToText(&text, f); err != nil {
			return collection{}, fmt.Errorf("cannot encode Prometheus metric family %q: %w", name, err)
		}
		samples, err := parse(quoteColonLabels(text.String()), noMarkers)
		if err != nil {
			return collection{}, fmt.Errorf("cannot parse Prometheus metric family %q: %w", name, err)
		}
		result.families[name] = family{kind: f.GetType(), series: series}
		for key, m := range samples {
			if owner, exists := result.owners[m.name]; exists && owner != name {
				return collection{}, fmt.Errorf("ambiguous Prometheus sample name %q in families %q and %q", m.name, owner, name)
			}
			m.family = name
			result.samples[key] = m
			result.owners[m.name] = name
		}
	}
	// Reject any family whose name matches a sample from another family.
	// For example, a histogram named "latency" already emits "latency_count".
	for _, f := range families {
		name := f.GetName()
		if owner, exists := result.owners[name]; exists && owner != name {
			return collection{}, fmt.Errorf("ambiguous Prometheus sample name %q in families %q and %q", name, owner, name)
		}
	}
	return result, nil
}

// contains reports whether the metric is present in the collection.
func (c collection) contains(key string, m metric) bool {
	if m.labeled {
		_, sampleExists := c.samples[key]
		return sampleExists || c.families[m.name].series[key]
	}
	_, nameExists := c.owners[m.name]
	return nameExists || len(c.families[m.name].series) != 0
}

// expositionToken matches comments and quoted strings whole so their contents
// pass through quoteColonLabels untouched, alongside label-name assignments.
var expositionToken = regexp.MustCompile(`(?m)^#[^\n]*|"(?:[^"\\\n]|\\.)*"|[\w:]+=`)

// quoteColonLabels works around an expfmt bug: prometheus/common through v0.74.0
// omits quotes around label names that contain colons. See TestPrometheusCommonIssue1045.
// Upstream fix: https://github.com/prometheus/common/pull/1045.
// TODO: Drop the Expect workaround after upgrading. Diff still needs it to
// read scrapes from exporters using an affected version.
func quoteColonLabels(text string) string {
	if !strings.Contains(text, ":") {
		return text
	}
	return expositionToken.ReplaceAllStringFunc(text, func(token string) string {
		if token[0] == '#' || token[0] == '"' || !strings.Contains(token, ":") {
			return token
		}
		name := strings.TrimSuffix(token, "=")
		if !model.LegacyValidation.IsValidMetricName(name) {
			return token
		}
		return quoteText(name) + "="
	})
}

type markerMode uint8

const (
	noMarkers    markerMode = iota // Numeric values only.
	valueMarkers                   // Numeric values, * and +.
	allMarkers                     // Numeric values, *, + and -.
)

// parse decodes Prometheus text samples with the permitted markers.
func parse(text string, markers markerMode) (map[string]metric, error) {
	metrics := make(map[string]metric)
	parser := expfmt.NewTextParser(model.UTF8Validation)
	lineNumber := 0
	for line := range strings.SplitSeq(text, "\n") {
		lineNumber++
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		match := exact
		if markers != noMarkers {
			line, match = replaceMarker(line)
			if markers == valueMarkers && match == absent {
				return nil, fmt.Errorf("line %d: '-' is only supported by Expect; omit this series from want to require its absence", lineNumber)
			}
		}
		// Parse samples individually without metadata so histogram buckets and
		// summary quantiles are compared just like other samples. Adding a newline
		// also permits test fixtures without a final newline.
		families, err := parser.TextToMetricFamilies(strings.NewReader(line + "\n"))
		if err != nil {
			if parseError, ok := errors.AsType[expfmt.ParseError](err); ok {
				return nil, fmt.Errorf("line %d: %s", lineNumber, parseError.Msg)
			}
			return nil, fmt.Errorf("line %d: %w", lineNumber, err)
		}
		for name, family := range families {
			for _, m := range family.GetMetric() {
				key := metricKey(name, m.GetLabel())
				if _, exists := metrics[key]; exists {
					return nil, fmt.Errorf("line %d: duplicate sample %s", lineNumber, key)
				}
				if match == absent && m.TimestampMs != nil {
					return nil, fmt.Errorf("line %d: unexpected timestamp after '-'", lineNumber)
				}
				value := m.GetUntyped().GetValue()
				if value == 0 {
					value = 0 // Normalize negative zero.
				}
				metrics[key] = metric{
					name: name, labeled: len(m.GetLabel()) != 0,
					value: value, timestamp: m.TimestampMs, match: match,
				}
			}
		}
	}
	return metrics, nil
}

var textEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)

// quoteText returns s as a double-quoted Prometheus text string literal.
func quoteText(s string) string { return `"` + textEscaper.Replace(s) + `"` }

// metricKey returns the canonical Prometheus text identity for a sample, with
// labels sorted by name and non-legacy names quoted.
func metricKey(name string, labels []*dto.LabelPair) string {
	legacy := model.LegacyValidation.IsValidMetricName(name)
	if legacy && len(labels) == 0 {
		return name
	}
	parts := make([]string, 0, len(labels))
	ordered := slices.Clone(labels)
	slices.SortFunc(ordered, func(a, b *dto.LabelPair) int {
		return strings.Compare(a.GetName(), b.GetName())
	})
	for _, label := range ordered {
		labelName := label.GetName()
		if !model.LegacyValidation.IsValidLabelName(labelName) {
			labelName = quoteText(labelName)
		}
		parts = append(parts, labelName+"="+quoteText(label.GetValue()))
	}
	if !legacy {
		parts = slices.Insert(parts, 0, quoteText(name))
		return "{" + strings.Join(parts, ",") + "}"
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

// replaceMarker substitutes a numeric placeholder for an expected matcher,
// leaving syntax validation to the Prometheus parser. Quoted strings and label
// braces must be skipped because label values can contain spaces and markers.
func replaceMarker(line string) (string, matcher) {
	quoted, labels := false, false
	for i := 0; i < len(line); i++ {
		if quoted {
			switch line[i] {
			case '\\':
				i++
			case '"':
				quoted = false
			}
			continue
		}
		switch line[i] {
		case '"':
			quoted = true
			continue
		case '{':
			labels = true
			continue
		case '}':
			labels = false
		case ' ', '\t':
			if labels {
				continue
			}
		default:
			continue
		}
		// Whitespace between the metric name and its label set is allowed.
		tail := strings.TrimLeft(line[i+1:], " \t")
		if strings.HasPrefix(tail, "{") {
			continue
		}
		end := strings.IndexAny(tail, " \t")
		if end == -1 {
			end = len(tail)
		}
		value := tail[:end]
		if len(value) == 1 && strings.Contains("+*-", value) {
			return line[:len(line)-len(tail)] + "0" + tail[end:], matcher(value)
		}
		return line, exact
	}
	return line, exact
}
