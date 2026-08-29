package budget

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEvaluate(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want Verdict
	}{
		// Exhaustion is categorical: it stops delivery everywhere, which is
		// the one rule inherited from error-budget-gate unchanged.
		{"exhausted in prod", Input{Tier: 2, Production: true, Remaining: 0}, Block},
		{"exhausted in dev", Input{Tier: 2, Production: false, Remaining: 0}, Block},
		{"burnt past the budget", Input{Tier: 2, Production: true, Remaining: -0.5}, Block},

		{"tier-0 prod, low budget", Input{Tier: 0, Production: true, Remaining: 0.1}, Block},
		// A tier-0 service with a healthy budget still ships automatically.
		// error-budget-gate would require a human here; that rule is about
		// robot-invented actions, not about delivering a reviewed commit.
		{"tier-0 prod, healthy", Input{Tier: 0, Production: true, Remaining: 0.9}, Allow},
		{"tier-0 dev, low budget", Input{Tier: 0, Production: false, Remaining: 0.1}, Allow},

		{"prod, low budget", Input{Tier: 2, Production: true, Remaining: 0.1}, RequireHuman},
		{"prod, just under the line", Input{Tier: 2, Production: true, Remaining: 0.249}, RequireHuman},
		{"prod, exactly at the line", Input{Tier: 2, Production: true, Remaining: 0.25}, Allow},
		{"prod, healthy", Input{Tier: 2, Production: true, Remaining: 0.8}, Allow},

		// The other deliberate divergence: a dev service losing automated
		// sync is friction with no risk story behind it.
		{"dev, low budget", Input{Tier: 2, Production: false, Remaining: 0.1}, Allow},
		{"dev, healthy", Input{Tier: 3, Production: false, Remaining: 1}, Allow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.in)
			if got.Verdict != tc.want {
				t.Fatalf("got %q (%s), want %q", got.Verdict, got.Reason, tc.want)
			}
			if got.Reason == "" {
				t.Fatal("a verdict nobody can explain is not actionable")
			}
		})
	}
}

func TestSeverityOrdersVerdicts(t *testing.T) {
	if !(Severity(Block) > Severity(RequireHuman) && Severity(RequireHuman) > Severity(Allow)) {
		t.Fatal("tightening must be detectable by comparing severities")
	}
}

func TestIsProductionFailsSafe(t *testing.T) {
	for _, env := range []string{"dev", "development", "staging", "stage", "test"} {
		if IsProduction(env) {
			t.Fatalf("%q should not be production", env)
		}
	}
	for _, env := range []string{"prod", "production", "", "prd", "anything-else"} {
		if !IsProduction(env) {
			t.Fatalf("%q must count as production: an unrecognised environment is the one most likely mislabelled", env)
		}
	}
}

func TestStaticSource(t *testing.T) {
	v, err := StaticSource{Value: 0.4}.Remaining(context.Background(), Service{})
	if err != nil || v != 0.4 {
		t.Fatalf("got %v, %v", v, err)
	}
	if _, err := (StaticSource{Value: 2}).Remaining(context.Background(), Service{}); err == nil {
		t.Fatal("a budget outside [0,1] is a bug, not a reading")
	}
}

// promStub serves one canned Prometheus instant-query response.
func promStub(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.URL.Query().Get("query") == "" {
			t.Error("query parameter missing")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func vector(value string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"` + value + `"]}]}}`
}

func TestPrometheusSourceComputesRemaining(t *testing.T) {
	// 99.9% target allows a 0.001 error ratio. An observed 0.0005 is half
	// the budget spent, so half remains.
	srv := promStub(t, vector("0.0005"), http.StatusOK)
	defer srv.Close()

	src, err := NewPrometheusSource(srv.URL, "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, err := src.Remaining(context.Background(), Service{
		Namespace: "orders-api", AvailabilityTarget: 99.9, Window: "28d",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got < 0.49 || got > 0.51 {
		t.Fatalf("remaining = %v, want ~0.5", got)
	}
}

func TestPrometheusSourceClampsOverspend(t *testing.T) {
	srv := promStub(t, vector("0.05"), http.StatusOK) // 50x the allowance
	defer srv.Close()
	src, _ := NewPrometheusSource(srv.URL, "", 5*time.Second)
	got, err := src.Remaining(context.Background(), Service{AvailabilityTarget: 99.9, Window: "28d"})
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("remaining = %v; burning past the budget is exhausted, not negative", got)
	}
}

func TestPrometheusSourceRefusesToGuess(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		wantErr    string
	}{
		{"no data", `{"status":"success","data":{"resultType":"vector","result":[]}}`, http.StatusOK, "no data"},
		{"NaN from no traffic", vector("NaN"), http.StatusOK, "cannot judge"},
		{"many series", `{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"0.1"]},{"value":[1,"0.2"]}]}}`, http.StatusOK, "must reduce to one"},
		{"query error", `{"status":"error","error":"parse error"}`, http.StatusOK, "parse error"},
		{"server error", `{}`, http.StatusInternalServerError, "500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := promStub(t, tc.body, tc.status)
			defer srv.Close()
			src, _ := NewPrometheusSource(srv.URL, "", 5*time.Second)
			_, err := src.Remaining(context.Background(), Service{AvailabilityTarget: 99.9, Window: "28d"})
			if err == nil {
				t.Fatal("missing data must be an error, never a number the caller trusts")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q should mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestPrometheusSourceRendersTheServiceIntoTheQuery(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query().Get("query")
		_, _ = w.Write([]byte(vector("0")))
	}))
	defer srv.Close()

	src, _ := NewPrometheusSource(srv.URL, `errors{ns="{{.Namespace}}",svc="{{.Name}}"}[{{.Window}}]`, 5*time.Second)
	if _, err := src.Remaining(context.Background(), Service{
		Namespace: "orders-api", Name: "orders-api", AvailabilityTarget: 99.9, Window: "28d",
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`ns="orders-api"`, `svc="orders-api"`, `[28d]`} {
		if !strings.Contains(seen, want) {
			t.Fatalf("query %q missing %q", seen, want)
		}
	}
}

func TestNewPrometheusSourceValidatesUpFront(t *testing.T) {
	if _, err := NewPrometheusSource("", "", time.Second); err == nil {
		t.Fatal("an empty base URL must fail at startup")
	}
	if _, err := NewPrometheusSource("http://prom", "{{.Nope", time.Second); err == nil {
		t.Fatal("a malformed template must fail at startup, not at the first reconcile")
	}
}

func TestPrometheusSourceRejectsImpossibleTarget(t *testing.T) {
	src, _ := NewPrometheusSource("http://prom", "", time.Second)
	if _, err := src.Remaining(context.Background(), Service{AvailabilityTarget: 100}); err == nil {
		t.Fatal("a 100% target allows no errors at all; dividing by that allowance is meaningless")
	}
}
