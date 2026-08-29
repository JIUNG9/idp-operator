package controller

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	idpv1alpha1 "github.com/JIUNG9/idp-operator/api/v1alpha1"
)

// TestEnvtestLifecycle is the create → verify → delete round-trip against a
// real API server: CRD schema defaulting, resourceVersion conflicts, and
// deletion timestamps — everything the fake client fakes away.
func TestEnvtestLifecycle(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set — run via `make test`")
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../config/crd", "../../test/crds"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := newScheme(t)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &ServiceDeploymentReconciler{
		Client: mgr.GetClient(),
		Reader: mgr.GetAPIReader(),
		Network: NetworkConfig{
			AllowIngressFromNamespaces: []string{"ingress-nginx"},
			AllowIngressFromCIDRs:      []string{"10.0.0.0/16"},
		},
		Policy: PolicyConfig{CosignPublicKey: "-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----"},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"idp-system", ArgoCDNamespace} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}

	// ---- create: slo and replicas deliberately omitted ----
	sd := &idpv1alpha1.ServiceDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-api", Namespace: "idp-system"},
		Spec: idpv1alpha1.ServiceDeploymentSpec{
			Team: "checkout",
			GitOps: idpv1alpha1.GitOps{
				RepoURL: "https://github.com/acme-corp/orders-api-deploy",
				Path:    "overlays/prod",
			},
		},
	}
	if err := c.Create(ctx, sd); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Name: sd.Name, Namespace: sd.Namespace}

	// The CRD schema (not the controller) must have defaulted the spec.
	var created idpv1alpha1.ServiceDeployment
	if err := c.Get(ctx, key, &created); err != nil {
		t.Fatal(err)
	}
	if created.Spec.SLO.AvailabilityTarget != "99.9" || created.Spec.SLO.LatencyP99Ms != 300 {
		t.Fatalf("CRD schema did not default the SLO: %+v", created.Spec.SLO)
	}
	if created.Spec.Replicas != 1 || created.Spec.GitOps.TargetRevision != "HEAD" {
		t.Fatalf("CRD schema defaults missing: replicas=%d gitops=%+v", created.Spec.Replicas, created.Spec.GitOps)
	}

	// ---- verify: provisioned, one replica → no PDB, condition explains ----
	eventually(t, 30*time.Second, func() error {
		var got idpv1alpha1.ServiceDeployment
		if err := c.Get(ctx, key, &got); err != nil {
			return err
		}
		if got.Status.Phase != PhaseProvisioned {
			return fmt.Errorf("phase %q, conditions %+v", got.Status.Phase, got.Status.Conditions)
		}
		pc := apimeta.FindStatusCondition(got.Status.Conditions, CondPDBReady)
		if pc == nil || pc.Status != metav1.ConditionFalse || pc.Reason != "SingleReplicaUnprotected" {
			return fmt.Errorf("PDBReady condition wrong: %+v", pc)
		}
		return nil
	})
	var ns corev1.Namespace
	if err := c.Get(ctx, types.NamespacedName{Name: "orders-api"}, &ns); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	if ns.Labels[LabelOwnerNamespace] != "idp-system" {
		t.Fatalf("namespace labels: %v", ns.Labels)
	}
	var pdb policyv1.PodDisruptionBudget
	if err := c.Get(ctx, types.NamespacedName{Name: "orders-api", Namespace: "orders-api"}, &pdb); !apierrors.IsNotFound(err) {
		t.Fatalf("single replica must have no PDB, got err=%v", err)
	}

	// ---- policy: both planes, validated by a real API server ----
	for _, name := range []string{NetworkPolicyDenyIngress, NetworkPolicyAllowSameNS, NetworkPolicyAllowPlatform} {
		var np networkingv1.NetworkPolicy
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "orders-api"}, &np); err != nil {
			t.Fatalf("network policy %q: %v", name, err)
		}
		if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
			t.Fatalf("%s must not touch egress: %v", name, np.Spec.PolicyTypes)
		}
	}

	// The Kyverno CRD is installed (test/crds), so the baseline is provisioned.
	// envtest runs no admission webhooks, so this asserts the policy exists and
	// says what it should — the M4 kind e2e is what proves it rejects a pod.
	pol := &unstructured.Unstructured{}
	pol.SetGroupVersionKind(KyvernoPolicyGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: KyvernoPolicyName, Namespace: "orders-api"}, pol); err != nil {
		t.Fatalf("kyverno policy: %v", err)
	}
	rules, found, err := unstructured.NestedSlice(pol.Object, "spec", "rules")
	if err != nil || !found || len(rules) != 3 {
		t.Fatalf("expected 3 rules through a real API server, got %d (%v)", len(rules), err)
	}
	verify := rules[2].(map[string]any)["verifyImages"].([]any)[0].(map[string]any)
	if verify["mutateDigest"] != false || verify["verifyDigest"] != false {
		t.Fatalf("digest flags must survive a real round-trip as false: %+v", verify)
	}

	// ---- scale up: PDB appears with maxUnavailable=1 ----
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest idpv1alpha1.ServiceDeployment
		if err := c.Get(ctx, key, &latest); err != nil {
			return err
		}
		latest.Spec.Replicas = 3
		return c.Update(ctx, &latest)
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, func() error {
		var got policyv1.PodDisruptionBudget
		if err := c.Get(ctx, types.NamespacedName{Name: "orders-api", Namespace: "orders-api"}, &got); err != nil {
			return err
		}
		if got.Spec.MaxUnavailable == nil || got.Spec.MaxUnavailable.IntValue() != 1 {
			return fmt.Errorf("pdb spec: %+v", got.Spec)
		}
		return nil
	})

	// ---- delete: Application gone, namespace terminating, CR released ----
	if err := c.Delete(ctx, &created); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, func() error {
		var got idpv1alpha1.ServiceDeployment
		if err := c.Get(ctx, key, &got); err == nil {
			return fmt.Errorf("ServiceDeployment still present (finalizers %v)", got.Finalizers)
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	})
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(ApplicationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "orders-api", Namespace: ArgoCDNamespace}, app); !apierrors.IsNotFound(err) {
		t.Fatalf("application must be gone, got err=%v", err)
	}
	// envtest runs no namespace controller, so termination never completes —
	// the operator's contract is that deletion was initiated.
	if err := c.Get(ctx, types.NamespacedName{Name: "orders-api"}, &ns); err == nil {
		if ns.DeletionTimestamp.IsZero() {
			t.Fatal("namespace deletion must be initiated")
		}
	} else if !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, timeout time.Duration, f func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = f(); last == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %v", timeout, last)
}
