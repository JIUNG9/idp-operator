package controller

import (
	"context"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
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

// The declaration is reported, and with the flag off the report must not claim
// a restriction that does not exist.
func TestEgressDeclaredConditionNamesTargetsAndAdmitsNoEnforcement(t *testing.T) {
	sd := valid()
	sd.Spec.Network.AllowEgressTo = []idpv1alpha1.EgressTarget{{Namespace: "payments"}, {Namespace: "ledger"}}
	cond := egressCondition(t, sd)
	if cond.Status != metav1.ConditionTrue || cond.Reason != "Declared" {
		t.Fatalf("want True/Declared, got %s/%s", cond.Status, cond.Reason)
	}
	for _, want := range []string{"payments, ledger", "without -enforce-egress"} {
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

// Without -enforce-egress, declaring egress adds no NetworkPolicy.
func TestEgressDeclarationAddsNoEgressPolicyWithoutFlag(t *testing.T) {
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
				t.Errorf("NetworkPolicy %s restricts egress while -enforce-egress is off", np.Name)
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

func enforcedFixture(t *testing.T, declared ...string) (client.Client, *ServiceDeploymentReconciler, *idpv1alpha1.ServiceDeployment) {
	t.Helper()
	sd := valid()
	for _, ns := range declared {
		sd.Spec.Network.AllowEgressTo = append(sd.Spec.Network.AllowEgressTo, idpv1alpha1.EgressTarget{Namespace: ns})
	}
	c, r, req := newFixtureOpts(t, sd, nil, func(r *ServiceDeploymentReconciler) { r.EnforceEgress = true })
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	return c, r, sd
}

func egressDestinations(np *networkingv1.NetworkPolicy) (namespaces []string, samePods bool) {
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			switch {
			case peer.PodSelector != nil && peer.NamespaceSelector == nil:
				samePods = true
			case peer.NamespaceSelector != nil:
				namespaces = append(namespaces, peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
			}
		}
	}
	return namespaces, samePods
}

// With the flag on, the policy selects every pod for Egress and lists exactly
// this namespace, DNS and the declared namespaces. Anything else is dropped by
// the Egress policy type, so an undeclared namespace must not appear.
func TestEnforceEgressAdmitsDeclaredNamespacesAndNothingElse(t *testing.T) {
	c, _, _ := enforcedFixture(t, "payments", "ledger")
	np := getNetworkPolicy(t, c, NetworkPolicyEgress)

	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Fatalf("policy types = %v, want [Egress] only", np.Spec.PolicyTypes)
	}
	if len(np.Spec.PodSelector.MatchLabels) != 0 || len(np.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("pod selector %v must select every pod", np.Spec.PodSelector)
	}
	got, samePods := egressDestinations(np)
	if !samePods {
		t.Error("same-namespace pods are not admitted")
	}
	want := []string{"kube-system", "payments", "ledger"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("admitted namespaces = %v, want %v", got, want)
	}
	for _, name := range got {
		if name == "billing" {
			t.Errorf("undeclared namespace %q is admitted", name)
		}
	}
}

// DNS is admitted on port 53 only. A rule to kube-system with no port list
// would open every kube-system pod, not just resolution.
func TestEnforceEgressLimitsKubeSystemToDNSPort(t *testing.T) {
	c, _, _ := enforcedFixture(t)
	np := getNetworkPolicy(t, c, NetworkPolicyEgress)

	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != DefaultDNSNamespace {
				continue
			}
			if len(rule.Ports) != 2 {
				t.Fatalf("DNS rule has %d ports, want UDP and TCP 53", len(rule.Ports))
			}
			for _, p := range rule.Ports {
				if p.Port == nil || p.Port.IntValue() != 53 {
					t.Errorf("DNS rule admits port %v, want 53", p.Port)
				}
			}
			return
		}
	}
	t.Fatal("no rule admits DNS")
}

// A cluster whose DNS is not in kube-system sets -dns-namespace. The DNS rule
// must follow it, and kube-system must not stay admitted: that would open a
// namespace the platform did not choose.
func TestEnforceEgressUsesConfiguredDNSNamespace(t *testing.T) {
	sd := valid()
	c, r, req := newFixtureOpts(t, sd, nil, func(r *ServiceDeploymentReconciler) {
		r.EnforceEgress = true
		r.Network.DNSNamespace = "openshift-dns"
	})
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	np := getNetworkPolicy(t, c, NetworkPolicyEgress)
	got, _ := egressDestinations(np)
	if strings.Join(got, ",") != "openshift-dns" {
		t.Errorf("admitted namespaces = %v, want only openshift-dns", got)
	}
	cond := apimeta.FindStatusCondition(getSD(t, c, sd.Name, sd.Namespace).Status.Conditions, CondEgressDeclared)
	if cond == nil || !strings.Contains(cond.Message, "openshift-dns") {
		t.Errorf("condition message should name openshift-dns, got %+v", cond)
	}
}

func TestEnforceEgressConditionStatesTheLimit(t *testing.T) {
	c, _, sd := enforcedFixture(t, "payments")
	cond := apimeta.FindStatusCondition(getSD(t, c, sd.Name, sd.Namespace).Status.Conditions, CondEgressDeclared)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "Enforced" {
		t.Fatalf("want True/Enforced, got %+v", cond)
	}
	if !strings.Contains(cond.Message, "payments") {
		t.Errorf("message %q does not name the declared namespace", cond.Message)
	}
}

// Turning the flag off again must remove the policy it created, or the
// service stays restricted by a setting nobody runs any more.
func TestEgressPolicyIsRemovedWhenFlagTurnsOff(t *testing.T) {
	c, r, sd := enforcedFixture(t, "payments")
	getNetworkPolicy(t, c, NetworkPolicyEgress) // exists while enforcing

	r.EnforceEgress = false
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sd.Name, Namespace: sd.Namespace}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var np networkingv1.NetworkPolicy
	err := c.Get(context.Background(), types.NamespacedName{Name: NetworkPolicyEgress, Namespace: sd.Name}, &np)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("egress policy still present after the flag was turned off: err=%v", err)
	}
}

// The API server must accept the generated policy: peer and port shapes are
// validated server-side, which the fake client does not do. envtest has no CNI,
// so this proves admission only, not that traffic is dropped.
func TestEnvtestAcceptsEgressPolicy(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set — run via `make test`")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	c, err := client.New(cfg, client.Options{Scheme: newScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orders-api"}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: NetworkPolicyEgress, Namespace: ns.Name},
		Spec: EgressSpec([]string{"payments", "ledger"}, []idpv1alpha1.EgressCIDRTarget{
			cidrTarget("203.0.113.0/24", idpv1alpha1.EgressPort{Port: 443}, idpv1alpha1.EgressPort{Port: 53, Protocol: "UDP"}),
			cidrTarget("2001:db8::/32", idpv1alpha1.EgressPort{Port: 5432}),
		}, DefaultDNSNamespace),
	}
	if err := c.Create(ctx, np); err != nil {
		t.Fatalf("API server rejected the egress policy: %v", err)
	}
}

func cidrTarget(cidr string, ports ...idpv1alpha1.EgressPort) idpv1alpha1.EgressCIDRTarget {
	return idpv1alpha1.EgressCIDRTarget{CIDR: cidr, Ports: ports}
}

// ipBlockRules returns the egress rules that name an ipBlock peer, by CIDR.
func ipBlockRules(np *networkingv1.NetworkPolicy) map[string]networkingv1.NetworkPolicyEgressRule {
	out := map[string]networkingv1.NetworkPolicyEgressRule{}
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				out[peer.IPBlock.CIDR] = rule
			}
		}
	}
	return out
}

// An external target becomes one ipBlock rule carrying exactly its ports. A rule
// with no port list would admit every port on the range.
func TestEnforceEgressRendersCIDRTargetsWithTheirPorts(t *testing.T) {
	sd := valid()
	sd.Spec.Network.AllowEgressTo = []idpv1alpha1.EgressTarget{{Namespace: "payments"}}
	sd.Spec.Network.AllowEgressToCIDRs = []idpv1alpha1.EgressCIDRTarget{
		cidrTarget("203.0.113.0/24", idpv1alpha1.EgressPort{Port: 443}, idpv1alpha1.EgressPort{Port: 53, Protocol: "UDP"}),
		cidrTarget("2001:db8::/32", idpv1alpha1.EgressPort{Port: 5432, Protocol: "TCP"}),
	}
	c, r, req := newFixtureOpts(t, sd, nil, func(r *ServiceDeploymentReconciler) { r.EnforceEgress = true })
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	np := getNetworkPolicy(t, c, NetworkPolicyEgress)
	rules := ipBlockRules(np)
	if len(rules) != 2 {
		t.Fatalf("want 2 ipBlock rules, got %d: %+v", len(rules), rules)
	}
	got := func(cidr string) string {
		var parts []string
		for _, p := range rules[cidr].Ports {
			parts = append(parts, string(*p.Protocol)+"/"+p.Port.String())
		}
		return strings.Join(parts, ",")
	}
	if g := got("203.0.113.0/24"); g != "TCP/443,UDP/53" {
		t.Errorf("203.0.113.0/24 ports = %q, want TCP/443,UDP/53 (protocol defaults to TCP)", g)
	}
	if g := got("2001:db8::/32"); g != "TCP/5432" {
		t.Errorf("2001:db8::/32 ports = %q, want TCP/5432", g)
	}
	// The namespace rules are untouched by the new field.
	ns, _ := egressDestinations(np)
	if strings.Join(ns, ",") != "kube-system,payments" {
		t.Errorf("admitted namespaces = %v, want kube-system,payments", ns)
	}
}

// With no CIDR declared the policy has no ipBlock rule at all.
func TestEnforceEgressWithoutCIDRsHasNoIPBlock(t *testing.T) {
	c, _, _ := enforcedFixture(t, "payments")
	if rules := ipBlockRules(getNetworkPolicy(t, c, NetworkPolicyEgress)); len(rules) != 0 {
		t.Errorf("unexpected ipBlock rules: %+v", rules)
	}
}

func TestEgressConditionNamesCIDRTargets(t *testing.T) {
	sd := valid()
	sd.Spec.Network.AllowEgressToCIDRs = []idpv1alpha1.EgressCIDRTarget{
		cidrTarget("203.0.113.0/24", idpv1alpha1.EgressPort{Port: 443}),
	}
	cond := egressCondition(t, sd)
	if cond.Reason != "Declared" {
		t.Fatalf("a CIDR-only declaration must read Declared, got %s", cond.Reason)
	}
	for _, want := range []string{"203.0.113.0/24:443/TCP", "without -enforce-egress"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message %q lacks %q", cond.Message, want)
		}
	}
}

func TestValidateEgressCIDRs(t *testing.T) {
	https := idpv1alpha1.EgressPort{Port: 443}
	cases := []struct {
		name    string
		targets []idpv1alpha1.EgressCIDRTarget
		wantErr string // empty: valid
	}{
		{"ipv4 range", []idpv1alpha1.EgressCIDRTarget{cidrTarget("10.20.0.0/16", https)}, ""},
		{"single host", []idpv1alpha1.EgressCIDRTarget{cidrTarget("203.0.113.7/32", https)}, ""},
		{"ipv6 range", []idpv1alpha1.EgressCIDRTarget{cidrTarget("2001:db8::/32", https)}, ""},
		{"not a cidr", []idpv1alpha1.EgressCIDRTarget{cidrTarget("example.com", https)}, "is not a CIDR"},
		{"bare address", []idpv1alpha1.EgressCIDRTarget{cidrTarget("203.0.113.7", https)}, "is not a CIDR"},
		{"host bits set", []idpv1alpha1.EgressCIDRTarget{cidrTarget("10.20.1.5/16", https)}, "write 10.20.0.0/16"},
		{"duplicate", []idpv1alpha1.EgressCIDRTarget{cidrTarget("10.20.0.0/16", https), cidrTarget("10.20.0.0/16", idpv1alpha1.EgressPort{Port: 80})}, "twice"},
		{"no ports", []idpv1alpha1.EgressCIDRTarget{cidrTarget("10.20.0.0/16")}, "at least one port"},
		{"port zero", []idpv1alpha1.EgressCIDRTarget{cidrTarget("10.20.0.0/16", idpv1alpha1.EgressPort{Port: 0})}, "between 1 and 65535"},
		{"port too high", []idpv1alpha1.EgressCIDRTarget{cidrTarget("10.20.0.0/16", idpv1alpha1.EgressPort{Port: 65536})}, "between 1 and 65535"},
		{"sctp", []idpv1alpha1.EgressCIDRTarget{cidrTarget("10.20.0.0/16", idpv1alpha1.EgressPort{Port: 80, Protocol: "SCTP"})}, "must be TCP or UDP"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sd := valid()
			sd.Spec.Network.AllowEgressToCIDRs = tc.targets
			err := Validate(sd)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// The CRD schema is the first line of validation for CIDR targets. It also
// defaults the protocol, which the policy builder then relies on.
func TestEnvtestEgressCIDRSchema(t *testing.T) {
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
	create := func(name string, targets ...idpv1alpha1.EgressCIDRTarget) (*idpv1alpha1.ServiceDeployment, error) {
		sd := valid()
		sd.Name, sd.Namespace = name, "default"
		sd.Spec.Network.AllowEgressToCIDRs = targets
		return sd, c.Create(context.Background(), sd)
	}
	sd, err := create("ok", cidrTarget("203.0.113.0/24", idpv1alpha1.EgressPort{Port: 443}))
	if err != nil {
		t.Fatalf("valid declaration rejected: %v", err)
	}
	if got := sd.Spec.Network.AllowEgressToCIDRs[0].Ports[0].Protocol; got != "TCP" {
		t.Errorf("protocol default = %q, want TCP", got)
	}
	if _, err := create("noports", cidrTarget("203.0.113.0/24")); err == nil {
		t.Error("a CIDR with no ports must be rejected by the schema")
	}
	if _, err := create("badport", cidrTarget("203.0.113.0/24", idpv1alpha1.EgressPort{Port: 70000})); err == nil {
		t.Error("port 70000 must be rejected by the schema")
	}
	if _, err := create("dup", cidrTarget("203.0.113.0/24", idpv1alpha1.EgressPort{Port: 443}), cidrTarget("203.0.113.0/24", idpv1alpha1.EgressPort{Port: 80})); err == nil {
		t.Error("a duplicate cidr must be rejected by the schema")
	}
}
