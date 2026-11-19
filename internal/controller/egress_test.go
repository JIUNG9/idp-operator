package controller

import (
	"context"
	"os"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	idpv1alpha1 "github.com/JIUNG9/idp-operator/api/v1alpha1"
)

func egressCondition(t *testing.T, sd *idpv1alpha1.ServiceDeployment) *metav1.Condition {
	t.Helper()
	c, r, req := newFixture(t, sd)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	got := getSD(t, c, sd.Name, sd.Namespace)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, CondEgressDeclared)
	if cond == nil {
		t.Fatalf("no %s condition: %+v", CondEgressDeclared, got.Status.Conditions)
	}
	return cond
}

// The declaration is reported, and the report must not claim a restriction
// that does not exist: slice 1 applies no egress policy.
func TestEgressDeclaredConditionNamesTargetsAndAdmitsNoEnforcement(t *testing.T) {
	sd := valid()
	sd.Spec.Network.AllowEgressTo = []idpv1alpha1.EgressTarget{{Namespace: "payments"}, {Namespace: "ledger"}}
	cond := egressCondition(t, sd)
	if cond.Status != metav1.ConditionTrue || cond.Reason != "Declared" {
		t.Fatalf("want True/Declared, got %s/%s", cond.Status, cond.Reason)
	}
	for _, want := range []string{"payments, ledger", "no egress policy is applied yet"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message %q lacks %q", cond.Message, want)
		}
	}
}

func TestEgressNoneDeclared(t *testing.T) {
	cond := egressCondition(t, valid())
	if cond.Status != metav1.ConditionTrue || cond.Reason != "NoneDeclared" {
		t.Fatalf("want True/NoneDeclared, got %s/%s", cond.Status, cond.Reason)
	}
}

// Declaring egress must not add a NetworkPolicy: slice 1 is the field and the
// status only.
func TestEgressDeclarationAddsNoEgressPolicy(t *testing.T) {
	sd := valid()
	sd.Spec.Network.AllowEgressTo = []idpv1alpha1.EgressTarget{{Namespace: "payments"}}
	c, r, req := newFixture(t, sd)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var list networkingv1.NetworkPolicyList
	if err := c.List(context.Background(), &list, client.InNamespace(sd.Name)); err != nil {
		t.Fatal(err)
	}
	for _, np := range list.Items {
		for _, pt := range np.Spec.PolicyTypes {
			if pt == "Egress" {
				t.Errorf("NetworkPolicy %s restricts egress; slice 1 must not", np.Name)
			}
		}
	}
}

// The CRD schema is the first line of validation; the Go Validate only
// repeats it for clients that bypass the API server.
func TestEnvtestEgressSchemaRejectsBadNamespaces(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set — run via `make test`")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../config/crd"}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	c, err := client.New(cfg, client.Options{Scheme: newScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	create := func(name string, targets ...string) error {
		sd := valid()
		sd.Name, sd.Namespace = name, "default"
		for _, n := range targets {
			sd.Spec.Network.AllowEgressTo = append(sd.Spec.Network.AllowEgressTo, idpv1alpha1.EgressTarget{Namespace: n})
		}
		return c.Create(context.Background(), sd)
	}
	if err := create("ok", "payments", "ledger"); err != nil {
		t.Fatalf("valid declaration rejected: %v", err)
	}
	if err := create("upper", "Payments"); err == nil {
		t.Error("an uppercase namespace must be rejected by the schema")
	}
	if err := create("dup", "payments", "payments"); err == nil {
		t.Error("a duplicate namespace must be rejected by the schema")
	}
}
