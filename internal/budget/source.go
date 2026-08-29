package budget

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// Service identifies what to measure and against which target.
type Service struct {
	Namespace string
	Name      string
	// AvailabilityTarget is the SLO as a percentage, e.g. 99.9.
	AvailabilityTarget float64
	// Window is the rolling window the budget is computed over, e.g. "28d".
	Window string
}

// Source reads how much error budget a service has left.
//
// Implementations return a fraction in [0,1]. They must return an error
// rather than a guess: the caller fails closed on an error, and a plausible
// number invented on missing data would hand out unattended production
// delivery on no evidence at all.
type Source interface {
	Remaining(ctx context.Context, svc Service) (float64, error)
	// Describe names the source for status messages, so a reader can tell
	// where a number came from.
	Describe() string
}

// StaticSource returns a fixed budget. For tests, demos, and clusters that
// have not wired metrics up yet.
type StaticSource struct {
	Value float64
}

func (s StaticSource) Remaining(context.Context, Service) (float64, error) {
	if s.Value < 0 || s.Value > 1 {
		return 0, fmt.Errorf("static budget %v is outside [0,1]", s.Value)
	}
	return s.Value, nil
}

func (s StaticSource) Describe() string { return fmt.Sprintf("static(%.2f)", s.Value) }

// DefaultBurnQuery is the error ratio of a service over the SLO window,
// assuming the common http_requests_total convention. Metric names differ
// everywhere, which is why this is a template the platform team replaces
// rather than something baked in.
const DefaultBurnQuery = `sum(rate(http_requests_total{namespace="{{.Namespace}}",status=~"5.."}[{{.Window}}])) / sum(rate(http_requests_total{namespace="{{.Namespace}}"}[{{.Window}}]))`

// PrometheusSource computes remaining budget from an observed error ratio.
//
// The query must return a single instant value: the fraction of requests
// that failed over the window. Remaining budget is then
//
//	1 - observed_error_ratio / allowed_error_ratio
//
// where the allowed ratio comes from the service's SLO target — 99.9% allows
// 0.001. A service exactly at its target has none of its budget left.
type PrometheusSource struct {
	BaseURL string
	Query   *template.Template
	Client  *http.Client
}

// NewPrometheusSource parses the query template up front, so a malformed one
// fails at startup instead of at the first reconcile.
func NewPrometheusSource(baseURL, query string, timeout time.Duration) (*PrometheusSource, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("prometheus base URL is empty")
	}
	if query == "" {
		query = DefaultBurnQuery
	}
	tmpl, err := template.New("burn").Parse(query)
	if err != nil {
		return nil, fmt.Errorf("burn query template: %w", err)
	}
	return &PrometheusSource{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Query:   tmpl,
		Client:  &http.Client{Timeout: timeout},
	}, nil
}

func (p *PrometheusSource) Describe() string { return "prometheus(" + p.BaseURL + ")" }

func (p *PrometheusSource) Remaining(ctx context.Context, svc Service) (float64, error) {
	if svc.AvailabilityTarget <= 0 || svc.AvailabilityTarget >= 100 {
		return 0, fmt.Errorf("availability target %v must be in (0,100)", svc.AvailabilityTarget)
	}
	allowed := (100 - svc.AvailabilityTarget) / 100

	var q strings.Builder
	if err := p.Query.Execute(&q, svc); err != nil {
		return 0, fmt.Errorf("rendering burn query: %w", err)
	}

	observed, err := p.instantQuery(ctx, q.String())
	if err != nil {
		return 0, err
	}

	remaining := 1 - observed/allowed
	// Burning past the budget is normal; reporting a negative fraction is
	// not. Exhausted is exhausted.
	return math.Max(0, math.Min(1, remaining)), nil
}

// instantQuery runs a Prometheus instant query and returns the single scalar
// it produced.
func (p *PrometheusSource) instantQuery(ctx context.Context, query string) (float64, error) {
	endpoint := p.BaseURL + "/api/v1/query?" + url.Values{"query": {query}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("querying prometheus: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("prometheus returned %s", resp.Status)
	}

	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("decoding prometheus response: %w", err)
	}
	if body.Status != "success" {
		return 0, fmt.Errorf("prometheus query failed: %s", body.Error)
	}
	// An empty vector means the query matched nothing. That is not zero
	// errors — it is no evidence, and the two must not be confused.
	if len(body.Data.Result) == 0 {
		return 0, fmt.Errorf("burn query returned no data; cannot judge the budget")
	}
	if len(body.Data.Result) > 1 {
		return 0, fmt.Errorf("burn query returned %d series; it must reduce to one", len(body.Data.Result))
	}
	if len(body.Data.Result[0].Value) != 2 {
		return 0, fmt.Errorf("unexpected sample shape in prometheus response")
	}
	raw, ok := body.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("unexpected sample value in prometheus response")
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing sample %q: %w", raw, err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		// Prometheus returns NaN for 0/0 — a service with no traffic at all.
		// Treating that as a healthy budget would hand out unattended
		// delivery to a service nobody can measure.
		return 0, fmt.Errorf("burn query returned %v; cannot judge the budget", v)
	}
	return v, nil
}
