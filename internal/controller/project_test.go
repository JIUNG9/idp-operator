package controller

import (
	"context"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func applicationExists(t *testing.T, c client.Client, namespace string) bool {
	t.Helper()
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(ApplicationGVK)
	err := c.Get(context.Background(), types.NamespacedName{Name: "orders-api", Namespace: namespace}, app)
	return err == nil
}

func TestProjectOutsideAllowListIsRejected(t *testing.T) {
	cases := []struct {
		name    string
		project string
		allowed []string
		reject  bool
	}{
		{"omitted project defaults to default, which is not listed", "", []string{"teams"}, true},
		{"explicit default is rejected unless listed", "default", []string{"teams"}, true},
		{"privileged project is rejected", "platform-admin", []string{"teams"}, true},
		{"empty allow-list rejects everything", "default", nil, true},
		{"project match is exact, not a prefix", "teams-admin", []string{"teams"}, true},
		{"listed project is accepted", "teams", []string{"teams", "default"}, false},
		{"default is accepted when listed", "", []string{"default"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sd := valid()
			sd.Spec.GitOps.Project = tc.project
			c, r, req := newFixtureOpts(t, sd, nil, func(r *ServiceDeploymentReconciler) {
				r.AllowedProjects = tc.allowed
			})
			if _, err := r.Reconcile(context.Background(), req); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			got := getSD(t, c, sd.Name, sd.Namespace)
			cond := apimeta.FindStatusCondition(got.Status.Conditions, CondProvisioned)
			if tc.reject {
				if got.Status.Phase != PhaseInvalid {
					t.Fatalf("phase = %q, want %q", got.Status.Phase, PhaseInvalid)
				}
				if cond == nil || cond.Reason != ReasonProjectNotAllowed || !strings.Contains(cond.Message, "spec.gitops.project") {
					t.Fatalf("condition = %+v, want reason %q naming spec.gitops.project", cond, ReasonProjectNotAllowed)
				}
				if applicationExists(t, c, ArgoCDNamespace) {
					t.Fatal("a rejected project must not produce an Application")
				}
				return
			}
			if got.Status.Phase != PhaseProvisioned {
				t.Fatalf("phase = %q, want %q (conditions: %+v)", got.Status.Phase, PhaseProvisioned, got.Status.Conditions)
			}
			if !applicationExists(t, c, ArgoCDNamespace) {
				t.Fatal("an allowed project must produce an Application")
			}
		})
	}
}

func TestRejectedProjectNamesTheAllowedOnes(t *testing.T) {
	r := &ServiceDeploymentReconciler{AllowedProjects: []string{"teams", "default"}}
	err := r.checkProject("platform-admin")
	if err == nil || !strings.Contains(err.Error(), "teams, default") {
		t.Fatalf("error = %v, want the allowed projects listed", err)
	}
}

func TestApplicationIsWrittenToTheConfiguredNamespace(t *testing.T) {
	sd := valid()
	c, r, req := newFixtureOpts(t, sd, nil, func(r *ServiceDeploymentReconciler) {
		r.ArgoCDNamespace = "gitops"
	})
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !applicationExists(t, c, "gitops") {
		t.Fatal("application not found in the configured namespace")
	}
	if applicationExists(t, c, ArgoCDNamespace) {
		t.Fatal("application must not be written to the default argocd namespace")
	}

	if err := c.Delete(ctx, getSD(t, c, sd.Name, sd.Namespace)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if applicationExists(t, c, "gitops") {
		t.Fatal("teardown must delete the application from the configured namespace")
	}
}
