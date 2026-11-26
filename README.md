# prometheustest

[![Go Reference](https://pkg.go.dev/badge/github.com/henvic/prometheustest.svg)](https://pkg.go.dev/github.com/henvic/prometheustest)
[![Tests](https://github.com/henvic/prometheustest/actions/workflows/go.yml/badge.svg)](https://github.com/henvic/prometheustest/actions/workflows/go.yml)
[![Coverage Status](https://coveralls.io/repos/henvic/prometheustest/badge.svg)](https://coveralls.io/github/henvic/prometheustest)

Use [`Expect`](https://pkg.go.dev/github.com/henvic/prometheustest#Expect) to check selected Prometheus metric families in a Go test. Use [`Diff`](https://pkg.go.dev/github.com/henvic/prometheustest#Diff) to compare samples in Prometheus text format.

`Expect` is usually the simpler choice for checking instrumentation metrics. It verifies only the metric families you list, leaving unrelated metrics out of the test as long as you give each test its own Prometheus registry.

Testing metrics alongside your application helps you verify that as your code changes, it counts events accurately, records the values you expect, and uses the right labels.

## Quickstart with prometheustest.Expect

For example, in a test for an HTTP handler we cna add a prometheustest.Expect call like this:

```go
func TestOrdersHandler(t *testing.T) {
	metrics := prometheus.NewRegistry()
	handler := newOrdersHandler(metrics) // your handler under test

	// Simulating accessing /orders
	req := httptest.NewRequestWithContext(t.Context(), "GET", "/orders", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Using prometheustest to check if metrics are being recorded as intended:
	prometheustest.Expect(t, metrics, `
http_requests_total{code="200"} 1
http_request_duration_seconds_count 1
http_request_timeouts_total -
`)
}
```

If the handler counts the request twice, the failure shows both values.

```diff
- http_requests_total{code="200"} 1
+ http_requests_total{code="200"} 2
```

## Matching rules

Write each expected sample in Prometheus text format. Use a number to match the value exactly.

- `*` for samples with any value, including zero.
- `+` for samples with a value greater than zero.
- `-` to require absence of a metric. Only available in `Expect`.

Blank lines and comments are ignored. Sample and label order don’t matter.

For values, `1` matches `1.0` and `NaN` matches `NaN`. Labels match as text, including empty values.

For counters, gauges, and untyped metrics, list every nonzero series in each family you check with `Expect`. A series you omit may be absent or zero. For histograms and summaries, you can check a single bucket, quantile, count, or sum. In Prometheus text format, native histograms expose their count, sum, and `+Inf` bucket, not their native bucket spans.

Without labels, `-` checks that the whole family is absent; with labels, only that exact label set.

## Comparing metrics with prometheustest.Diff

To check what the `/metrics` endpoint serves, pass the response body as `got` to `Diff(want, got)`. Unlike `Expect`, `Diff` compares the complete set of samples in `got` and reports every unlisted sample, even if its value is zero—so omitting a sample from `want` already requires its absence.

```go
const body = `jobs_total 3
queue_depth 0`

report, err := prometheustest.Diff("jobs_total 3", body)
if err != nil {
	log.Fatal(err)
}
fmt.Println(report)
// Output: + queue_depth 0
```

A match produces an empty report. Malformed input returns an error.

When using UTF-8 metric or label names, match the names as encoded in the response: by default, `promhttp` underscore-escapes names such as `site.name` to `site_name`.

See [the package tests](prometheustest_test.go) for more examples.

> [!NOTE]
> Most of the code here is AI-generated
> and has been thoroughly modified and verified by humans.
