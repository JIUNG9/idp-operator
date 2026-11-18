package controller

import (
	"context"
	"testing"

	"github.com/JIUNG9/idp-operator/internal/budget"
)

func TestNamespaceMappingOverridesSelfDeclaredEnvironment(t *testing.T) {
	sd := valid() // lives in idp-system
	sd.Spec.Environment = "dev"
	c, r, req := newFixtureOpts(t, sd, nil, func(r *ServiceDeploymentReconciler) {
		withBudget(budget.StaticSource{Value: 0.1})(r)
		r.NamespaceEnvironments = map[string]string{"idp-system": "prod"}
	})

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if automatedSync(t, c) {
		t.Fatal("a ServiceDeployment in a namespace mapped to prod must be gated as prod, whatever spec.environment says")
	}
	got := getSD(t, c, sd.Name, sd.Namespace)
	if got.Status.Budget == nil || got.Status.Budget.Environment != "prod" {
		t.Fatalf("status must record the environment the gate used (prod), got %+v", got.Status.Budget)
	}
}

func TestUnmappedNamespaceKeepsSpecEnvironment(t *testing.T) {
	sd := valid()
	sd.Spec.Environment = "dev"
	c, r, req := newFixtureOpts(t, sd, nil, func(r *ServiceDeploymentReconciler) {
		withBudget(budget.StaticSource{Value: 0.1})(r)
		r.NamespaceEnvironments = map[string]string{"some-other-namespace": "prod"}
	})

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !automatedSync(t, c) {
		t.Fatal("an unmapped namespace must keep today's behaviour: dev at 10% budget keeps automated delivery")
	}
	got := getSD(t, c, sd.Name, sd.Namespace)
	if got.Status.Budget == nil || got.Status.Budget.Environment != "dev" {
		t.Fatalf("status must record dev, got %+v", got.Status.Budget)
	}
}

func TestParseNamespaceEnvironments(t *testing.T) {
	got, err := ParseNamespaceEnvironments(" team-a = prod, team-b=dev ,")
	if err != nil || got["team-a"] != "prod" || got["team-b"] != "dev" || len(got) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := ParseNamespaceEnvironments(""); err != nil || len(got) != 0 {
		t.Fatalf("empty must parse to an empty map, got %v, %v", got, err)
	}
	for _, bad := range []string{"team-a", "team-a=production", "=prod", "team-a=prod,team-a=dev", "team-a="} {
		if _, err := ParseNamespaceEnvironments(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
