package prometheustest_test

import (
	"errors"
	"fmt"
	"math"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/henvic/prometheustest"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

func TestExpect(t *testing.T) {
	t.Parallel()

	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests.",
	}, []string{"code", "method"})
	requests.WithLabelValues("200", "GET").Add(42)
	requests.WithLabelValues("201", "POST").Inc()
	requests.WithLabelValues("404", "GET").Add(3)
	requests.WithLabelValues("500", "GET") // Create a sample with value zero.
	requests.WithLabelValues("503", "GET") // Unlisted zero samples are allowed.

	cpu := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "process_cpu_seconds_total",
		Help: "Total CPU time spent in seconds.",
	})
	cpu.Add(1.234)

	temperature := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "temperature",
		Help: "Current temperature in kelvins.",
	})
	temperature.Set(270.65)
	other := prometheus.NewGauge(prometheus.GaugeOpts{Name: "unrelated_metric"})
	other.Set(99)

	registry := prometheus.NewRegistry()
	registry.MustRegister(requests, cpu, temperature, other)

	// Omit metadata and change metric and label order in the expectation.
	want := `
process_cpu_seconds_total *
http_requests_total{method="POST",code="201"} 1
http_requests_total{method="GET",code="200"} +
http_requests_total{method="GET",code="404"} 3
http_requests_total{method="GET",code="500"} 0
http_requests_total{method="GET",code="405"} -
temperature 270.65
`
	prometheustest.Expect(t, registry, want)
}

func TestExpectErrors(t *testing.T) {
	requests := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "requests_total",
		Help: "Total requests.",
	})
	requests.Add(3)
	registry := prometheus.NewRegistry()
	registry.MustRegister(requests)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	broken := "broken"
	for _, tt := range []struct {
		name     string
		gatherer prometheus.Gatherer
		metrics  string
		want     string
	}{
		{
			name:     "match with comments",
			gatherer: registry,
			metrics: `# HELP requests_total A different description.
requests_total 3`,
		},
		{
			name:     "empty registry",
			gatherer: prometheus.NewRegistry(),
		},
		{
			name:     "mismatch",
			gatherer: registry,
			metrics:  "requests_total 0",
			want:     "Prometheus metrics mismatch (-want +got):",
		},
		{
			name:     "missing metric",
			gatherer: prometheus.NewRegistry(),
			metrics:  "requests_total *",
			want:     "Prometheus metrics mismatch (-want +got):",
		},
		{
			name:     "empty expectation ignores metrics",
			gatherer: registry,
		},
		{name: "nil gatherer", metrics: "requests_total *", want: "nil gatherer"},
		{name: "nil registry", gatherer: (*prometheus.Registry)(nil), metrics: "requests_total *", want: "nil gatherer"},
		{name: "nil gatherer function", gatherer: prometheus.GathererFunc(nil), metrics: "requests_total *", want: "nil gatherer"},
		{name: "empty gatherer list", gatherer: prometheus.Gatherers(nil), metrics: "requests_total -"},
		{name: "nil gatherer with comments", metrics: "# nothing to gather"},
		{
			name:     "invalid expectation",
			gatherer: registry,
			metrics:  "requests_total invalid",
			want:     "invalid want metrics:",
		},
		{
			name: "gather failure",
			gatherer: prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
				return nil, errors.New("collection failed")
			}),
			metrics: "requests_total *",
			want:    "cannot gather Prometheus metrics: collection failed",
		},
		{
			name: "partial gather failure",
			gatherer: prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
				return families, errors.New("incomplete collection")
			}),
			metrics: "requests_total 3",
			want:    "cannot gather Prometheus metrics: incomplete collection",
		},
		{
			name: "encoding failure",
			gatherer: prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
				return []*dto.MetricFamily{{Name: &broken}}, nil
			}),
			metrics: "broken *",
			want:    `cannot encode Prometheus metric family "broken":`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checkExpectation(t, tt.gatherer, tt.metrics, tt.want)
		})
	}
}

func TestExpectScope(t *testing.T) {
	t.Parallel()
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "requests_total"}, []string{"code", "method"})
	requests.WithLabelValues("200", "GET").Add(3)
	requests.WithLabelValues("500", "GET")
	temperature := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "temperature"}, []string{"city"})
	temperature.WithLabelValues("north").Set(-2.5)
	temperature.WithLabelValues("south")
	registry := prometheus.NewRegistry()
	registry.MustRegister(requests, temperature)

	for _, tt := range []struct {
		name    string
		metrics string
		error   string
	}{
		{"ignore other families and zero series", `requests_total{method="GET",code="200"} +`, ""},
		{"label whitespace", `requests_total { method = "GET", code = "200" } 3`, ""},
		{"omitted nonzero counter", `requests_total{code="500",method="GET"} 0`, `unlisted Prometheus samples must be zero:
requests_total{code="200",method="GET"} 3`},
		{"omitted nonzero gauge", `temperature{city="south"} 0`, `temperature{city="north"} -2.5`},
		{"omitted zero gauge", `temperature{city="north"} -2.5`, ""},
		{"missing exact zero", `missing 0`, "metrics mismatch"},
		{"missing any value", `missing *`, "metrics mismatch"},
		{"missing positive sample", `missing +`, "metrics mismatch"},
		{"missing labeled zero", `requests_total{code="200",method="GET"} 3
requests_total{code="404",method="GET"} 0`, "metrics mismatch"},
		{"missing labeled wildcard", `requests_total{code="200",method="GET"} 3
requests_total{code="404",method="GET"} *`, "metrics mismatch"},
		{"absent family", `missing -`, ""},
		{"present family", `requests_total -`, "want absent"},
		{"absent labeled series", `requests_total{method="GET",code="200"} 3
requests_total{method="GET",code="404"} -`, ""},
		{"present zero labeled series", `requests_total{code="200",method="GET"} 3
requests_total{code="500",method="GET"} -`, "want absent"},
		{"absence selects family", `requests_total{code="404",method="GET"} -`, "unlisted Prometheus samples must be zero"},
		{"partial labels identify a different series", `requests_total{code="200",method="GET"} 3
requests_total{code="500"} -`, ""},
		{"conflicting duplicates", `temperature{city="north"} 0
temperature{city="north"} -2.5`, "duplicate sample"},
		{"reordered duplicates", `requests_total{code="200",method="GET"} *
requests_total{method="GET",code="200"} +`, "duplicate sample"},
		{"conflicting absence", `missing -
missing 0`, "duplicate sample"},
		{"invalid absence labels", `missing{code=200} -`, "invalid want metrics"},
		{"absence with timestamp", `missing - 123`, "unexpected timestamp after '-'"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checkExpectation(t, registry, tt.metrics, tt.error)
		})
	}

	// A zero-valued series is still present until it is deleted.
	requests.DeleteLabelValues("500", "GET")
	prometheustest.Expect(t, registry, `requests_total{code="200",method="GET"} 3
requests_total{code="500",method="GET"} -`)

	requests.DeleteLabelValues("200", "GET")
	prometheustest.Expect(t, registry, "requests_total -")
}

func TestExpectMultipleFailures(t *testing.T) {
	registry := prometheus.NewRegistry()
	temperature := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "temperature"}, []string{"city"})
	registry.MustRegister(temperature)
	temperature.WithLabelValues("west").Set(-2)
	temperature.WithLabelValues("south").Set(0)
	temperature.WithLabelValues("north").Set(math.NaN())
	temperature.WithLabelValues("east").Set(0)
	temperature.WithLabelValues("central").Set(3)

	tb := &testTB{}
	prometheustest.Expect(tb, registry, `temperature{city="south"} -
temperature{city="central"} 5
temperature{city="east"} -`)
	want := []string{
		`Prometheus metric temperature{city="east"} is present, want absent (-)`,
		`Prometheus metric temperature{city="south"} is present, want absent (-)`,
		`unlisted Prometheus samples must be zero:
temperature{city="north"} NaN
temperature{city="west"} -2`,
		`Prometheus metrics mismatch (-want +got):
- temperature{city="central"} 5
+ temperature{city="central"} 3`,
	}
	if diff := cmp.Diff(want, tb.errors); diff != "" {
		t.Errorf("Expect diagnostics (-want +got):\n%s", diff)
	}
}

func TestExpectValues(t *testing.T) {
	t.Parallel()
	temperature := prometheus.NewGauge(prometheus.GaugeOpts{Name: "temperature"})
	registry := prometheus.NewRegistry()
	registry.MustRegister(temperature)

	for _, tt := range []struct {
		name  string
		value float64
		want  string
		error string
	}{
		{"above one", 1.01, "+", ""},
		{"one", 1, "+", ""},
		{"fraction", 0.5, "+", ""},
		{"negative", -1, "+", "metrics mismatch"},
		{"zero", 0, "+", "metrics mismatch"},
		{"nan is not positive", math.NaN(), "+", "metrics mismatch"},
		{"positive infinity", math.Inf(1), "+", ""},
		{"negative infinity", math.Inf(-1), "+", "metrics mismatch"},
		{"any zero", 0, "*", ""},
		{"any negative", -1, "*", ""},
		{"any nan", math.NaN(), "*", ""},
		{"equal nan", math.NaN(), "NaN", ""},
		{"negative zero", math.Copysign(0, -1), "0", ""},
		{"scientific notation", 3, "3e0", ""},
		{"zero is present", 0, "-", "want absent"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			temperature.Set(tt.value)
			checkExpectation(t, registry, "temperature "+tt.want, tt.error)
		})
	}
}

func TestExpectAbsentFamilyReportsOnce(t *testing.T) {
	for _, tt := range []struct {
		name   string
		values []float64
	}{
		{"zero series", []float64{0, 0}},
		{"nonzero series", []float64{2, 3}},
		{"mixed series", []float64{0, 3}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			amount := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "amount"}, []string{"id"})
			for i, value := range tt.values {
				amount.WithLabelValues(strconv.Itoa(i)).Set(value)
			}
			other := prometheus.NewGauge(prometheus.GaugeOpts{Name: "other"})
			other.Set(4)
			registry := prometheus.NewRegistry()
			registry.MustRegister(amount, other)
			checkExpectation(t, registry, "amount -", "amount is present, want absent")
			// An independent value mismatch still produces its own diagnostic.
			checkExpectation(t, registry, `amount -
other 0`, "amount is present, want absent", "metrics mismatch")
		})
	}
}

func TestExpectHistogramAndSummary(t *testing.T) {
	t.Parallel()
	histogram := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "request_seconds", Help: "Request duration.", Buckets: []float64{1, 5},
	}, []string{"method"})
	summary := prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Name: "rpc_seconds", Help: "RPC duration.", Objectives: map[float64]float64{0.5: 0.05},
	}, []string{"method"})
	registry := prometheus.NewRegistry()
	registry.MustRegister(histogram, summary)
	prometheustest.Expect(t, registry, `request_seconds -
rpc_seconds -`)

	histogram.WithLabelValues("GET").Observe(1) // Exactly on the first bucket's upper bound.
	histogram.WithLabelValues("GET").Observe(6) // Above the largest finite bucket.
	histogram.WithLabelValues("POST")
	summary.WithLabelValues("GET").Observe(1)
	summary.WithLabelValues("GET").Observe(3)

	for _, tt := range []struct {
		name    string
		metrics string
		error   string
	}{
		{"histogram count only", `request_seconds_count{method="GET"} 2`, ""},
		{"histogram sum only", `request_seconds_sum{method="GET"} 7`, ""},
		{"histogram selected bucket", `request_seconds_bucket{method="GET", le="1"} 1`, ""},
		{"bucket label spelling", `request_seconds_bucket{method="GET",le="1.0"} 1`, "metrics mismatch"},
		{"histogram infinite bucket", `request_seconds_bucket{le="+Inf",method="GET"} 2`, ""},
		{"histogram wrong count", `request_seconds_count{method="GET"} 3`, "metrics mismatch"},
		{"histogram missing bucket", `request_seconds_bucket{method="GET",le="2"} *`, "metrics mismatch"},
		{"histogram missing series", `request_seconds_count{method="DELETE"} 0`, "metrics mismatch"},
		{"histogram family present", `request_seconds -`, "want absent"},
		{"histogram series present", `request_seconds{method="GET"} -`, "want absent"},
		{"histogram zero series present", `request_seconds{method="POST"} -`, "want absent"},
		{"histogram series absent", `request_seconds{method="DELETE"} -`, ""},
		{"histogram bucket present", `request_seconds_bucket{method="GET",le="1"} -`, "want absent"},
		{"bucket absence matches the full label set", `request_seconds_bucket{method="GET"} -`, ""},
		{"histogram bucket absent", `request_seconds_bucket{method="GET",le="2"} -`, ""},
		{"histogram derived name present", `request_seconds_count -`, "want absent"},
		{"summary count only", `rpc_seconds_count{method="GET"} 2`, ""},
		{"summary sum only", `rpc_seconds_sum{method="GET"} 4`, ""},
		{"summary quantile only", `rpc_seconds{quantile="0.5",method="GET"} *`, ""},
		{"quantile label spelling", `rpc_seconds{quantile="0.50",method="GET"} *`, "metrics mismatch"},
		{"summary is not a scalar zero", `rpc_seconds 0`, "metrics mismatch"},
		{"summary family present", `rpc_seconds -`, "want absent"},
		{"summary series present", `rpc_seconds{method="GET"} -`, "want absent"},
		{"summary series absent", `rpc_seconds{method="DELETE"} -`, ""},
		{"summary quantile present", `rpc_seconds{method="GET",quantile="0.5"} -`, "want absent"},
		{"summary quantile absent", `rpc_seconds{method="GET",quantile="0.99"} -`, ""},
		{"summary derived name present", `rpc_seconds_sum -`, "want absent"},
		{"mixed families", `request_seconds_count{method="GET"} 2
rpc_seconds_sum{method="GET"} *`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checkExpectation(t, registry, tt.metrics, tt.error)
		})
	}
}

func TestExpectGaugeHistogram(t *testing.T) {
	name := "queue_age_seconds"
	count := uint64(3)
	sum := 7.5
	gatherer := prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
		return []*dto.MetricFamily{{
			Name: &name, Type: dto.MetricType_GAUGE_HISTOGRAM.Enum(),
			Metric: []*dto.Metric{{Histogram: &dto.Histogram{SampleCount: &count, SampleSum: &sum}}},
		}}, nil
	})
	// Selecting the count leaves the nonzero sum and buckets unchecked.
	prometheustest.Expect(t, gatherer, "queue_age_seconds_count 3")
}

func TestExpectFamilyNames(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	for _, name := range []string{"jobs", "jobs_count", "jobs_sum", "jobs_bucket", "other_count"} {
		counter := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name}, []string{"status"})
		counter.WithLabelValues("success").Add(3)
		counter.WithLabelValues("failure").Inc()
		registry.MustRegister(counter)
	}
	for _, name := range []string{"jobs_count", "jobs_sum", "jobs_bucket"} {
		t.Run(name, func(t *testing.T) {
			checkExpectation(t, registry, name+`{status="success"} 3`, name+`{status="failure"} 1`)
			want := fmt.Sprintf(`%s{status="success"} 3
%s{status="failure"} 1`, name, name)
			prometheustest.Expect(t, registry, want)
		})
	}
	// A scalar suffix must not make an unrelated, absent base family appear present.
	prometheustest.Expect(t, registry, "other -")
}

func TestExpectNativeHistogramTextSamples(t *testing.T) {
	size := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "payload_bytes", NativeHistogramBucketFactor: 1.1,
	})
	size.Observe(512)
	registry := prometheus.NewRegistry()
	registry.MustRegister(size)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	h := families[0].GetMetric()[0].GetHistogram()
	if len(h.GetPositiveSpan()) == 0 || len(h.GetBucket()) != 0 {
		t.Fatal("expected native histogram buckets without classic buckets")
	}
	// Text exposition retains count, sum and the implicit +Inf bucket.
	prometheustest.Expect(t, registry, `
payload_bytes_count 1
payload_bytes_sum 512
payload_bytes_bucket{le="+Inf"} 1
`)
	// Native bucket spans cannot be checked through classic text bucket samples.
	checkExpectation(t, registry, `payload_bytes_bucket{le="512"} *`, "metrics mismatch")
}

func TestExpectEscaping(t *testing.T) {
	t.Parallel()
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "requests_total"}, []string{"path", "method"})
	path := "C:\\tmp\\file \"quoted\"\n # { } + * -"
	requests.WithLabelValues(path, "GET").Add(3)
	registry := prometheus.NewRegistry()
	registry.MustRegister(requests)
	metrics := fmt.Sprintf("requests_total{method=\"GET\", path=%q} +", path)
	prometheustest.Expect(t, registry, metrics)
	checkExpectation(t, registry, strings.TrimSuffix(metrics, "+")+"-", "want absent")

	gauge := prometheus.NewGauge(prometheus.GaugeOpts{Name: "café.temperature{peak}", ConstLabels: prometheus.Labels{"site.name": "north"}})
	gauge.Set(3)
	registry.MustRegister(gauge)
	prometheustest.Expect(t, registry, `{"café.temperature{peak}","site.name"="north"} +`)
	checkExpectation(t, registry, `{"café.temperature{peak}"} -`, "want absent")
}

func TestPrometheusCommonIssue1045(t *testing.T) {
	registry := prometheus.NewRegistry()
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "service:requests"}, []string{"server:region", "routing.rule"})
	requests.WithLabelValues("west", `server:region="east"`).Inc()
	registry.MustRegister(requests, prometheus.NewGauge(prometheus.GaugeOpts{Name: "queue_depth"}))

	prometheustest.Expect(t, registry, `service:requests{"server:region"="west","routing.rule"="server:region=\"east\""} +`)
	prometheustest.Expect(t, registry, "queue_depth 0")
	checkExpectation(t, registry, `service:requests{server:region="west"} 1`, "invalid want metrics")

	response := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), "GET", "/metrics", nil))
	// The default HTTP representation escapes the dot in routing.rule.
	diff, err := prometheustest.Diff(`queue_depth 0
service:requests{"server:region"="west",routing_rule="server:region=\"east\""} +`, response.Body.String())
	if err != nil {
		t.Fatal(err)
	}
	if diff != "" {
		t.Errorf("scraped metrics mismatch (-want +got):\n%s", diff)
	}
	if _, err := prometheustest.Diff(`service:requests{server:region="west"} 1`, response.Body.String()); err == nil || !strings.HasPrefix(err.Error(), "invalid want metrics:") {
		t.Errorf("expected invalid want diagnostic, got %v", err)
	}
}

func TestExpectEmptyLabel(t *testing.T) {
	registry := prometheus.NewRegistry()
	queue := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "queue_depth"}, []string{"region"})
	queue.WithLabelValues("").Set(0)
	registry.MustRegister(queue)

	prometheustest.Expect(t, registry, `queue_depth{region=""} 0`)
	checkExpectation(t, registry, "queue_depth 0", "- queue_depth 0")
	checkExpectation(t, registry, `queue_depth{region=""} -`, `queue_depth{region=""} is present`)

	unlabeled := prometheus.NewRegistry()
	unlabeled.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{Name: "queue_depth"}))
	prometheustest.Expect(t, unlabeled, `queue_depth{region=""} -`)
}

func TestExpectSkipsGathering(t *testing.T) {
	for _, tt := range []struct{ name, metrics, wantError string }{
		{"empty", "", ""},
		{"comments", " \n# comments only\n", ""},
		{"missing value", "invalid", "invalid want metrics"},
		{"duplicate sample", `amount 0
amount 1`, "duplicate sample"},
		{"timestamp on absence", "amount - 123", "unexpected timestamp after '-'"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gatherer := prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
				t.Fatal("unexpected call to Gather")
				return nil, nil
			})
			checkExpectation(t, gatherer, tt.metrics, tt.wantError)
		})
	}
}

func TestExpectTimestampsAndUntyped(t *testing.T) {
	for _, tt := range []struct {
		name      string
		kind      dto.MetricType
		valueType prometheus.ValueType
	}{
		{"gauge", dto.MetricType_GAUGE, prometheus.GaugeValue},
		{"untyped", dto.MetricType_UNTYPED, prometheus.UntypedValue},
	} {
		t.Run(tt.name, func(t *testing.T) {
			name := "amount"
			family := &dto.MetricFamily{Name: &name, Type: &tt.kind}
			desc := prometheus.NewDesc(name, "Amount.", []string{"status"}, nil)
			for _, sample := range []struct {
				status    string
				value     float64
				timestamp int64
			}{
				{"ready", 3, 123},
				{"idle", 0, 456},
			} {
				m := prometheus.MustNewConstMetric(desc, tt.valueType, sample.value, sample.status)
				m = prometheus.NewMetricWithTimestamp(time.UnixMilli(sample.timestamp), m)
				var out dto.Metric
				if err := m.Write(&out); err != nil {
					t.Fatal(err)
				}
				family.Metric = append(family.Metric, &out)
			}
			gatherer := prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
				return []*dto.MetricFamily{family}, nil
			})
			// Unlisted zero samples do not require a timestamp.
			prometheustest.Expect(t, gatherer, `amount{status="ready"} + 123`)
			checkExpectation(t, gatherer, `amount{status="ready"} * 456`, "metrics mismatch")
			checkExpectation(t, gatherer, `amount{status="ready"} 3`, "metrics mismatch")
			checkExpectation(t, gatherer, `amount{status="idle"} 0 456`, `amount{status="ready"} 3 123`)
			// Both checks fail: idle is present, and the unlisted ready series is nonzero.
			checkExpectation(t, gatherer, `amount{status="idle"} -`, "want absent", `amount{status="ready"} 3 123`)
		})
	}
}

func TestExpectInvalidGathering(t *testing.T) {
	histogramRegistry := prometheus.NewRegistry()
	histogram := prometheus.NewHistogram(prometheus.HistogramOpts{Name: "latency", Buckets: []float64{1}})
	histogram.Observe(2)
	histogramRegistry.MustRegister(histogram)
	families, err := histogramRegistry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	scalarRegistry := prometheus.NewRegistry()
	scalarRegistry.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{Name: "latency_count"}))
	scalars, err := scalarRegistry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	// This summary emits latency_count_sum and latency_count_count, but no quantiles.
	summaryRegistry := prometheus.NewRegistry()
	summaryRegistry.MustRegister(prometheus.NewSummary(prometheus.SummaryOpts{Name: "latency_count"}))
	summaries, err := summaryRegistry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	const collision = `ambiguous Prometheus sample name "latency_count" in families "latency" and "latency_count"`
	duplicateSamples := &dto.MetricFamily{
		Name: scalars[0].Name, Type: scalars[0].Type,
		Metric: []*dto.Metric{scalars[0].Metric[0], scalars[0].Metric[0]},
	}
	for _, tt := range []struct {
		name     string
		families []*dto.MetricFamily
		error    string
	}{
		{"nil family", []*dto.MetricFamily{nil}, "nil metric family"},
		{"nil metric", []*dto.MetricFamily{{Name: families[0].Name, Type: families[0].Type, Metric: []*dto.Metric{nil}}}, `family "latency": nil metric`},
		{"duplicate family", []*dto.MetricFamily{families[0], families[0]}, "duplicate family"},
		{"duplicate sample within family", []*dto.MetricFamily{duplicateSamples}, `cannot parse Prometheus metric family "latency_count": line 3: duplicate sample latency_count`},
		{"ambiguous sample name", []*dto.MetricFamily{families[0], scalars[0]}, "ambiguous Prometheus sample name"},
		{"derived name before family", []*dto.MetricFamily{families[0], summaries[0]}, collision},
		{"family before derived name", []*dto.MetricFamily{summaries[0], families[0]}, collision},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gatherer := prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
				return tt.families, nil
			})
			checkExpectation(t, gatherer, "latency_count *", tt.error)
		})
	}
}

func checkExpectation(t *testing.T, gatherer prometheus.Gatherer, metrics string, wantErrors ...string) {
	t.Helper()
	if len(wantErrors) == 0 || (len(wantErrors) == 1 && wantErrors[0] == "") {
		prometheustest.Expect(t, gatherer, metrics)
		return
	}
	tb := &testTB{}
	prometheustest.Expect(tb, gatherer, metrics)
	if tb.helpers == 0 {
		t.Error("Expect() did not mark itself as a test helper")
	}
	if len(tb.errors) != len(wantErrors) {
		t.Fatalf("Expect() reported %d errors, want %d: %v", len(tb.errors), len(wantErrors), tb.errors)
	}
	for i, want := range wantErrors {
		if !strings.Contains(tb.errors[i], want) {
			t.Errorf("Expect() error %d = %q, want substring %q", i+1, tb.errors[i], want)
		}
	}
}

func TestDiffEqual(t *testing.T) {
	tests := []struct {
		name string
		want string
		got  string
	}{
		{"empty", "", ""},
		{"comments only", "# expected\n", " \t# HELP ignored\n# TYPE ignored\n\n"},
		{
			name: "comments on both sides",
			want: `# want
requests_total 3`,
			got: `# HELP requests_total Requests
# TYPE requests_total counter
requests_total 3
# after`,
		},
		{
			name: "different types ignored",
			want: `# TYPE requests_total gauge
requests_total 3`,
			got: `# TYPE requests_total counter
requests_total 3`,
		},
		{"whitespace", "\nrequests_total\t3\n\n", " \trequests_total    3 \r\n"},
		{
			name: "order",
			want: "a 1\nb 2",
			got:  "b 2\na 1",
		},
		{"label order", `requests_total{method="GET",code="200"} 3`, `requests_total{code="200",method="GET"} 3`},
		{
			name: "sample order",
			want: `requests_total{code="200"} 3
requests_total{code="404"} 1`,
			got: `requests_total{code="404"} 1
requests_total{code="200"} 3`,
		},
		{"label whitespace", `requests_total { method = "GET", code = "200", } 3`, `requests_total{code="200",method="GET"} 3`},
		{"empty labels", "requests_total{} 3", "requests_total 3"},
		{"integer and decimal", "requests_total 1", "requests_total 1.0"},
		{"scientific notation", "requests_total 1000", "requests_total 1e3"},
		{"negative values", "temperature -2.5", "temperature -25e-1"},
		{"negative zero", "temperature 0", "temperature -0"},
		{"nan", "temperature NaN", "temperature NaN"},
		{"positive infinity", "temperature +Inf", "temperature +Inf"},
		{"negative infinity", "temperature -Inf", "temperature -Inf"},
		{"plus", "requests_total +", "requests_total 2"},
		{"plus decimal", "requests_total +", "requests_total 1.01"},
		{"plus one", "requests_total +", "requests_total 1"},
		{"plus fraction", "requests_total +", "requests_total 0.004"},
		{"plus infinity", "requests_total +", "requests_total +Inf"},
		{"star zero", "temperature *", "temperature 0"},
		{"star positive", "temperature *", "temperature 3.5"},
		{"star negative", "temperature *", "temperature -3.5"},
		{"star nan", "temperature *", "temperature NaN"},
		{"star infinity", "temperature *", "temperature -Inf"},
		{"wildcard with labels", `requests_total{code="200"} +`, `requests_total{code="200"} 3`},
		{"wildcard label whitespace", `requests_total { code = "200" } *`, `requests_total{code="200"} 3`},
		{"wildcard compact labels", `requests_total{code="200"}+`, `requests_total{code="200"}3`},
		{"literal wildcard labels", `requests_total{path="*",method="+"} 3`, `requests_total{method="+",path="*"} 3`},
		{"hash and spaces in labels", `requests_total{path="/a # b",code="200"} *`, `requests_total{path="/a # b",code="200"} 3`},
		{"escaped labels", `requests_total{path="a\\b\n\" } + *"} +`, `requests_total{path="a\\b\n\" } + *"} 3`},
		{"timestamp", "requests_total 3 12345", "requests_total 3.0 12345"},
		{"timestamp formatting", "requests_total 3 00123", "requests_total 3 123"},
		{"wildcard with timestamp", "requests_total * 12345", "requests_total 3 12345"},
		{"quoted metric name", `{"requests.total","status.code"="200"} *`, `{"requests.total","status.code"="200"} 3`},
		{"quoted legacy name", `{"requests_total","code"="200"} 3`, `requests_total{code="200"} 3`},
		{"utf8 names", `{"café.temperature","café.location"="Montréal"} +`, `{"café.temperature","café.location"="Montréal"} 3`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diff, err := prometheustest.Diff(tt.want, tt.got)
			if err != nil {
				t.Fatal(err)
			}
			if diff != "" {
				t.Errorf("Diff() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDiffMismatches(t *testing.T) {
	for _, tt := range []struct {
		name    string
		pattern string
		samples string
		want    string
	}{
		{
			name:    "close values",
			pattern: "v 1.000000000000001",
			samples: "v 1.000000000000002",
			want:    "- v 1.000000000000001\n+ v 1.000000000000002",
		},
		{
			name:    "missing wildcard",
			pattern: "v *",
			samples: "",
			want:    "- v *",
		},
		{
			name:    "missing positive",
			pattern: "v +",
			samples: "",
			want:    "- v +",
		},
		{
			name:    "extra zero",
			pattern: "a 1",
			samples: "a 1\nb 0",
			want:    "+ b 0",
		},
		{
			name:    "positive rejects zero",
			pattern: "v +",
			samples: "v -0",
			want:    "- v +\n+ v 0",
		},
		{
			name:    "positive rejects negative",
			pattern: "v +",
			samples: "v -2",
			want:    "- v +\n+ v -2",
		},
		{
			name:    "positive rejects NaN",
			pattern: "v +",
			samples: "v NaN",
			want:    "- v +\n+ v NaN",
		},
		{
			name:    "positive rejects negative infinity",
			pattern: "v +",
			samples: "v -Inf",
			want:    "- v +\n+ v -Inf",
		},
		{
			name:    "infinity sign",
			pattern: "v +Inf",
			samples: "v -Inf",
			want:    "- v +Inf\n+ v -Inf",
		},
		{
			name:    "NaN and number",
			pattern: "v NaN",
			samples: "v 3",
			want:    "- v NaN\n+ v 3",
		},
		{
			name:    "missing timestamp",
			pattern: "v 3 123",
			samples: "v 3",
			want:    "- v 3 123\n+ v 3",
		},
		{
			name:    "extra timestamp",
			pattern: "v 3",
			samples: "v 3 0",
			want:    "- v 3\n+ v 3 0",
		},
		{
			name:    "large timestamp",
			pattern: "v 3 9007199254740992",
			samples: "v 3 9007199254740993",
			want:    "- v 3 9007199254740992\n+ v 3 9007199254740993",
		},
		{
			name:    "empty label is present",
			pattern: `v{region=""} 0`,
			samples: "v 0",
			want:    "+ v 0\n- v{region=\"\"} 0",
		},
		{
			name:    "labels are literal",
			pattern: `v{code="*"} *`,
			samples: `v{code="200"} 3`,
			want:    "- v{code=\"*\"} *\n+ v{code=\"200\"} 3",
		},
		{
			name:    "numeric labels are text",
			pattern: `v{code="200"} 3`,
			samples: `v{code="200.0"} 3`,
			want:    "- v{code=\"200\"} 3\n+ v{code=\"200.0\"} 3",
		},
		{
			name:    "metric name containing braces",
			pattern: `{"v{code=\"200\"}"} 3`,
			samples: `v{code="200"} 3`,
			want:    "+ v{code=\"200\"} 3\n- {\"v{code=\\\"200\\\"}\"} 3",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			diff, err := prometheustest.Diff(tt.pattern, tt.samples)
			if err != nil {
				t.Fatal(err)
			}
			if diff != tt.want {
				t.Errorf("Diff(%q, %q) returned:\n%s\nwant:\n%s", tt.pattern, tt.samples, diff, tt.want)
			}
		})
	}
}

func TestDiffInvalid(t *testing.T) {
	// Valid samples before an error must not produce a partial diff.
	for _, tt := range []struct {
		name, want, got, errorPrefix string
	}{
		{"malformed expectation", `a 1
# comment

b bad`, "a 2", "invalid want metrics: line 4:"},
		{"malformed scrape", "a 1", `a 2
b bad`, "invalid got metrics: line 2:"},
		{"identical malformed inputs", "a", "a", "invalid want metrics: line 1:"},
		{"value overflow", "a *", "a 1e1000", "invalid got metrics: line 1:"},
		{"invalid escape", "", `a{path="\q"} 3`, "invalid got metrics: line 1:"},
		{"invalid colon label", "", `a{1http:method="GET"} 1`, "invalid got metrics: line 1:"},
		{"duplicate labels", `a{code="200",code="404"} 3`, "", "invalid want metrics: line 1:"},
		{"duplicate series", `a{x="1",y="2"} +
a{y="2",x="1"} *`, "", `invalid want metrics: line 2: duplicate sample a{x="1",y="2"}`},
		{"duplicate with new timestamp", "", `a 3 123
a 4 456`, "invalid got metrics: line 2: duplicate sample a"},
		{"timestamp overflow", "", "a 3 9223372036854775808", "invalid got metrics: line 1:"},
		{"timestamp wildcard", "a 3 *", "", "invalid want metrics: line 1:"},
		{"absence expectation", `# old counter
a -`, "", "invalid want metrics: line 2: '-' is only supported by Expect; omit this series from want to require its absence"},
		{"star in scrape", "a *", "a *", "invalid got metrics: line 1:"},
		{"plus in scrape", "a +", "a +", "invalid got metrics: line 1:"},
		{"absence in scrape", "", "a -", "invalid got metrics: line 1:"},
		{"OpenMetrics exemplar", "a 1", `a 1 # {trace_id="abc"} 1`, "invalid got metrics: line 1:"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			report, err := prometheustest.Diff(tt.want, tt.got)
			if report != "" {
				t.Errorf("Diff returned a report for invalid input: %q", report)
			}
			if err == nil || !strings.HasPrefix(err.Error(), tt.errorPrefix) {
				t.Errorf("Diff error = %v, want prefix %q", err, tt.errorPrefix)
			}
		})
	}
}

func TestDiffScrapedHistogramAndSummary(t *testing.T) {
	registry := prometheus.NewRegistry()
	duration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "request_seconds", Help: "Request duration.", Buckets: []float64{1},
	})
	size := prometheus.NewSummary(prometheus.SummaryOpts{
		Name: "response_bytes", Help: "Response size.", Objectives: map[float64]float64{0.5: 0.01},
	})
	registry.MustRegister(duration, size)
	duration.Observe(0.5)
	size.Observe(256)

	const want = `request_seconds_bucket{le="1"} 1
request_seconds_bucket{le="+Inf"} +
request_seconds_sum *
request_seconds_count 1
response_bytes{quantile="0.5"} 256
response_bytes_sum 256
response_bytes_count 1`

	handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	checkScrape := func(wantReport string) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), "GET", "/metrics", nil))
		report, err := prometheustest.Diff(want, response.Body.String())
		if err != nil {
			t.Fatal(err)
		}
		if report != wantReport {
			t.Errorf("Diff returned %q, want %q", report, wantReport)
		}
	}
	checkScrape("")
	duration.Observe(0.25)
	checkScrape(`- request_seconds_bucket{le="1"} 1
+ request_seconds_bucket{le="1"} 2
- request_seconds_count 1
+ request_seconds_count 2`)
}

func TestDiffLongLine(t *testing.T) {
	labels := `requests_total{path="` + strings.Repeat("a", 100000) + `"}`
	diff, err := prometheustest.Diff(labels+" *", labels+" 3")
	if err != nil {
		t.Fatal(err)
	}
	if diff != "" {
		t.Errorf("Diff() failed for a long line: %s", diff)
	}
}

func TestDiffReport(t *testing.T) {
	const want = `requests_total{code="200",method="GET"} + 123
retired_total *
requests_total{code="404",method="GET"} 0
requests_total{code="500",method="GET"} 0
{"café.temperature{peak}","site.name"="north"} * 123`
	const got = `{"café.temperature{peak}","site.name"="north"} 2 456
requests_total{code="500",method="GET"} 1
requests_total{code="404",method="GET"} 0
requests_total{code="200",method="GET"} 3 456
connections{"site.name"="west",port0="admin",port="http"} 7`
	const report = `+ connections{port="http",port0="admin","site.name"="west"} 7
- requests_total{code="200",method="GET"} + 123
+ requests_total{code="200",method="GET"} 3 456
- requests_total{code="500",method="GET"} 0
+ requests_total{code="500",method="GET"} 1
- retired_total *
- {"café.temperature{peak}","site.name"="north"} * 123
+ {"café.temperature{peak}","site.name"="north"} 2 456`
	actual, err := prometheustest.Diff(want, got)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(report, actual); diff != "" {
		t.Errorf("Diff report mismatch (-want +got):\n%s", diff)
	}
	// Removing the diff prefix yields a sample that parses as written.
	for _, sample := range []string{
		`{"café.temperature"} 1`,
		"{\"café.temperature\",\"site name\"=\"a\tb\"} 2",
	} {
		diff, err := prometheustest.Diff(sample, "")
		if err != nil {
			t.Fatal(err)
		}
		if diff != "- "+sample {
			t.Errorf("Diff(%q, empty) = %q", sample, diff)
		}
	}
}

type testTB struct {
	testing.TB
	helpers int
	errors  []string
}

func (t *testTB) Helper() {
	t.helpers++
}

func (t *testTB) Errorf(format string, args ...any) {
	t.errors = append(t.errors, fmt.Sprintf(format, args...))
}
