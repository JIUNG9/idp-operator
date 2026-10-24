package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	idpv1alpha1 "github.com/JIUNG9/idp-operator/api/v1alpha1"
)

func newScheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := idpv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newFixture(t *testing.T, sd *idpv1alpha1.ServiceDeployment, extra ...client.Object) (client.Client, *ServiceDeploymentReconciler, ctrl.Request) {
	t.Helper()
	return newFixtureOpts(t, sd, nil, nil, extra...)
}

// newFixtureOpts builds a fixture with control over what the cluster knows
// (mapper) and how the operator is configured (tune).
func newFixtureOpts(t *testing.T, sd *idpv1alpha1.ServiceDeployment, mapper apimeta.RESTMapper, tune func(*ServiceDeploymentReconciler), extra ...client.Object) (client.Client, *ServiceDeploymentReconciler, ctrl.Request) {
	t.Helper()
	objs := append([]client.Object{sd}, extra...)
	b := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).WithStatusSubresource(sd)
	if mapper != nil {
		b = b.WithRESTMapper(mapper)
	}
	c := b.Build()
	r := &ServiceDeploymentReconciler{Client: c}
	if tune != nil {
		tune(r)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sd.Name, Namespace: sd.Namespace}}
	return c, r, req
}

// kyvernoMapper simulates a cluster with Kyverno's CRDs installed.
func kyvernoMapper() apimeta.RESTMapper {
	m := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{KyvernoPolicyGVK.GroupVersion()})
	m.Add(KyvernoPolicyGVK, apimeta.RESTScopeNamespace)
	return m
}

func getNetworkPolicy(t *testing.T, c client.Client, name string) *networkingv1.NetworkPolicy {
	t.Helper()
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "orders-api"}, &np); err != nil {
		t.Fatalf("network policy %q: %v", name, err)
	}
	return &np
}

func getSD(t *testing.T, c client.Client, name, namespace string) *idpv1alpha1.ServiceDeployment {
	t.Helper()
	var got idpv1alpha1.ServiceDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func TestReconcileProvisionsGoldenPath(t *testing.T) {
	sd := valid()
	c, r, req := newFixture(t, sd)
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var ns corev1.Namespace
	if err := c.Get(ctx, types.NamespacedName{Name: "orders-api"}, &ns); err != nil {
		t.Fatalf("namespace not created: %v", err)
	}
	if ns.Labels[LabelTeam] != "checkout" {
		t.Fatalf("namespace missing team label: %v", ns.Labels)
	}
	if ns.Labels[LabelOwnerNamespace] != "idp-system" {
		t.Fatalf("namespace missing owner-namespace label: %v", ns.Labels)
	}

	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: "slo-baseline", Namespace: "orders-api"}, &cm); err != nil {
		t.Fatalf("slo baseline not created: %v", err)
	}
	if cm.Data["availability_target"] != DefaultAvailabilityTarget {
		t.Fatalf("SLO default not applied: %v", cm.Data)
	}

	var pdb policyv1.PodDisruptionBudget
	if err := c.Get(ctx, types.NamespacedName{Name: "orders-api", Namespace: "orders-api"}, &pdb); err != nil {
		t.Fatalf("pdb not created for %d replicas: %v", sd.Spec.Replicas, err)
	}
	if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.IntValue() != 1 {
		t.Fatalf("pdb must use maxUnavailable=1, got %+v", pdb.Spec)
	}
	if pdb.Spec.MinAvailable != nil {
		t.Fatalf("minAvailable must not be set: %+v", pdb.Spec)
	}

	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(ApplicationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "orders-api", Namespace: ArgoCDNamespace}, app); err != nil {
		t.Fatalf("argocd application not created: %v", err)
	}
	repo, _, _ := unstructured.NestedString(app.Object, "spec", "source", "repoURL")
	if repo != sd.Spec.GitOps.RepoURL {
		t.Fatalf("application repoURL = %q", repo)
	}

	got := getSD(t, c, sd.Name, sd.Namespace)
	if got.Status.Phase != PhaseProvisioned {
		t.Fatalf("phase = %q, want %q (conditions: %+v)", got.Status.Phase, PhaseProvisioned, got.Status.Conditions)
	}
	if len(got.Finalizers) != 1 || got.Finalizers[0] != Finalizer {
		t.Fatalf("finalizer not added: %v", got.Finalizers)
	}
	if pc := apimeta.FindStatusCondition(got.Status.Conditions, CondPDBReady); pc == nil || pc.Status != metav1.ConditionTrue {
		t.Fatalf("PDBReady condition wrong: %+v", got.Status.Conditions)
	}
}

func TestReconcileRejectsInvalidSpec(t *testing.T) {
	sd := valid()
	sd.Spec.Team = ""
	c, r, req := newFixture(t, sd)

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("invalid spec must not requeue-error: %v", err)
	}

	got := getSD(t, c, sd.Name, sd.Namespace)
	if got.Status.Phase != PhaseInvalid {
		t.Fatalf("phase = %q, want %q", got.Status.Phase, PhaseInvalid)
	}
	var ns corev1.Namespace
	if err := c.Get(context.Background(), types.NamespacedName{Name: "orders-api"}, &ns); err == nil {
		t.Fatal("namespace must NOT be created for an invalid spec")
	}
}

func TestReconcileSingleReplicaSkipsPDB(t *testing.T) {
	sd := valid()
	sd.Spec.Replicas = ptr(int32(1))
	c, r, req := newFixture(t, sd)

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var pdb policyv1.PodDisruptionBudget
	if err := c.Get(context.Background(), types.NamespacedName{Name: "orders-api", Namespace: "orders-api"}, &pdb); err == nil {
		t.Fatal("a single-replica service must not get a PDB")
	}
	got := getSD(t, c, sd.Name, sd.Namespace)
	if got.Status.Phase != PhaseProvisioned {
		t.Fatalf("phase = %q, want %q — no PDB is a documented state, not a failure", got.Status.Phase, PhaseProvisioned)
	}
	pc := apimeta.FindStatusCondition(got.Status.Conditions, CondPDBReady)
	if pc == nil || pc.Status != metav1.ConditionFalse || pc.Reason != "SingleReplicaUnprotected" {
		t.Fatalf("PDBReady condition must explain the skip, got %+v", pc)
	}
}

func TestReconcileScaleDownRemovesPDB(t *testing.T) {
	sd := valid() // 3 replicas
	c, r, req := newFixture(t, sd)
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	var pdb policyv1.PodDisruptionBudget
	if err := c.Get(ctx, types.NamespacedName{Name: "orders-api", Namespace: "orders-api"}, &pdb); err != nil {
		t.Fatalf("pdb should exist at 3 replicas: %v", err)
	}

	fresh := getSD(t, c, sd.Name, sd.Namespace)
	fresh.Spec.Replicas = ptr(int32(1))
	if err := c.Update(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	if err := c.Get(ctx, types.NamespacedName{Name: "orders-api", Namespace: "orders-api"}, &pdb); err == nil {
		t.Fatal("stale pdb must be deleted after scale-down below 2")
	}
	got := getSD(t, c, sd.Name, sd.Namespace)
	if pc := apimeta.FindStatusCondition(got.Status.Conditions, CondPDBReady); pc == nil || pc.Status != metav1.ConditionFalse {
		t.Fatalf("PDBReady must flip false after scale-down: %+v", pc)
	}
}

func TestReconcileDeleteCleansUp(t *testing.T) {
	sd := valid()
	c, r, req := newFixture(t, sd)
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := c.Delete(ctx, getSD(t, c, sd.Name, sd.Namespace)); err != nil {
		t.Fatal(err)
	}
	// The finalizer holds the object; this reconcile is the teardown pass.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("teardown reconcile: %v", err)
	}

	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(ApplicationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "orders-api", Namespace: ArgoCDNamespace}, app); err == nil {
		t.Fatal("application must be deleted on teardown")
	}
	var ns corev1.Namespace
	if err := c.Get(ctx, types.NamespacedName{Name: "orders-api"}, &ns); err == nil && ns.DeletionTimestamp.IsZero() {
		t.Fatal("namespace deletion must be initiated on teardown")
	}
	var gone idpv1alpha1.ServiceDeployment
	if err := c.Get(ctx, types.NamespacedName{Name: sd.Name, Namespace: sd.Namespace}, &gone); err == nil {
		t.Fatal("finalizer must be removed so the ServiceDeployment goes away")
	}
}

func TestReconcileDeleteBeforeProvisionIsClean(t *testing.T) {
	// Deleting a CR whose children never got created must unwind cleanly:
	// names are derived, deletes are idempotent, nothing to trip over.
	sd := valid()
	sd.Finalizers = []string{Finalizer}
	c, r, req := newFixture(t, sd)
	ctx := context.Background()

	if err := c.Delete(ctx, getSD(t, c, sd.Name, sd.Namespace)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("teardown of a never-provisioned CR must not error: %v", err)
	}
	var gone idpv1alpha1.ServiceDeployment
	if err := c.Get(ctx, types.NamespacedName{Name: sd.Name, Namespace: sd.Namespace}, &gone); err == nil {
		t.Fatal("finalizer must be removed even when nothing was provisioned")
	}
}

func TestReconcileRefusesForeignNamespace(t *testing.T) {
	sd := valid()
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orders-api"}} // no labels: not ours
	c, r, req := newFixture(t, sd, foreign)

	_, err := r.Reconcile(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "not managed") {
		t.Fatalf("must refuse to adopt an unmanaged namespace, got err=%v", err)
	}
	got := getSD(t, c, sd.Name, sd.Namespace)
	if got.Status.Phase != PhaseError {
		t.Fatalf("phase = %q, want %q", got.Status.Phase, PhaseError)
	}
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Name: "slo-baseline", Namespace: "orders-api"}, &cm); err == nil {
		t.Fatal("nothing may be provisioned into a namespace we don't own")
	}
}

func TestReconcileRefusesCollidingOwner(t *testing.T) {
	sd := valid()
	// Same cluster namespace, but owned by a ServiceDeployment elsewhere.
	other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "orders-api",
		Labels: map[string]string{
			LabelManagedBy:      ManagedByValue,
			LabelName:           "orders-api",
			LabelOwnerNamespace: "somewhere-else",
		},
	}}
	c, r, req := newFixture(t, sd, other)

	_, err := r.Reconcile(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "owned by") {
		t.Fatalf("must refuse a namespace owned by another ServiceDeployment, got err=%v", err)
	}
	if got := getSD(t, c, sd.Name, sd.Namespace); got.Status.Phase != PhaseError {
		t.Fatalf("phase = %q, want %q", got.Status.Phase, PhaseError)
	}
}

func TestReconcileProvisionsNetworkPolicies(t *testing.T) {
	sd := valid()
	c, r, req := newFixture(t, sd)

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	deny := getNetworkPolicy(t, c, NetworkPolicyDenyIngress)
	if len(deny.Spec.Ingress) != 0 || len(deny.Spec.PolicyTypes) != 1 {
		t.Fatalf("deny policy wrong: %+v", deny.Spec)
	}
	if deny.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Fatalf("egress must be left alone until dependencies can be declared, got %v", deny.Spec.PolicyTypes)
	}
	same := getNetworkPolicy(t, c, NetworkPolicyAllowSameNS)
	if len(same.Spec.Ingress) != 1 {
		t.Fatalf("same-namespace policy wrong: %+v", same.Spec)
	}

	// Nothing configured on the operator, so no platform policy exists.
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), types.NamespacedName{Name: NetworkPolicyAllowPlatform, Namespace: "orders-api"}, &np); err == nil {
		t.Fatal("platform ingress policy must not exist when nothing is configured")
	}

	got := getSD(t, c, sd.Name, sd.Namespace)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, CondNetworkPolicyReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("NetworkPolicyReady wrong: %+v", cond)
	}
	// An exposed service with no configured ingress source is about to look
	// broken; the condition has to say so rather than read as healthy.
	if !strings.Contains(cond.Message, "no platform ingress sources are configured") {
		t.Fatalf("condition must warn that nothing outside the namespace can reach the service: %q", cond.Message)
	}
}

func TestReconcilePlatformIngressPolicy(t *testing.T) {
	sd := valid()
	c, r, req := newFixtureOpts(t, sd, nil, func(r *ServiceDeploymentReconciler) {
		r.Network = NetworkConfig{
			AllowIngressFromNamespaces: []string{"ingress-nginx"},
			AllowIngressFromCIDRs:      []string{"10.0.0.0/16"},
		}
	})

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	platform := getNetworkPolicy(t, c, NetworkPolicyAllowPlatform)
	if len(platform.Spec.Ingress[0].From) != 2 {
		t.Fatalf("expected namespace + cidr peers, got %+v", platform.Spec.Ingress[0].From)
	}
	got := getSD(t, c, sd.Name, sd.Namespace)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, CondNetworkPolicyReady)
	if cond == nil || !strings.Contains(cond.Message, "ingress-nginx") {
		t.Fatalf("condition should name the allowed sources: %+v", cond)
	}
}

func TestReconcileRemovesStalePlatformPolicy(t *testing.T) {
	sd := valid()
	c, r, req := newFixtureOpts(t, sd, nil, func(r *ServiceDeploymentReconciler) {
		r.Network = NetworkConfig{AllowIngressFromNamespaces: []string{"ingress-nginx"}}
	})
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	getNetworkPolicy(t, c, NetworkPolicyAllowPlatform) // exists

	// The platform team removed the configuration; the policy must go with it.
	r.Network = NetworkConfig{}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	var np networkingv1.NetworkPolicy
	if err := c.Get(ctx, types.NamespacedName{Name: NetworkPolicyAllowPlatform, Namespace: "orders-api"}, &np); err == nil {
		t.Fatal("stale platform policy must be deleted when configuration is removed")
	}
}

func TestReconcileSkipsAdmissionPolicyWithoutKyverno(t *testing.T) {
	sd := valid()
	c, r, req := newFixture(t, sd) // no Kyverno in the RESTMapper

	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("a cluster without Kyverno must not fail the golden path: %v", err)
	}
	// Nothing watches a CRD that has not been installed yet, so the only way
	// back is to ask for one.
	if res.RequeueAfter != EnvironmentalRetryInterval {
		t.Fatalf("expected a requeue so Kyverno arriving later is noticed, got %v", res.RequeueAfter)
	}

	got := getSD(t, c, sd.Name, sd.Namespace)
	if got.Status.Phase != PhaseProvisioned {
		t.Fatalf("phase = %q; a missing guardrail is degraded, not broken", got.Status.Phase)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, CondAdmissionPolicyReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "KyvernoNotInstalled" {
		t.Fatalf("AdmissionPolicyReady wrong: %+v", cond)
	}
	agg := apimeta.FindStatusCondition(got.Status.Conditions, CondProvisioned)
	if agg == nil || agg.Status != metav1.ConditionTrue || agg.Reason != "ReconciledWithSkips" {
		t.Fatalf("aggregate wrong: %+v", agg)
	}
	if !strings.Contains(agg.Message, CondAdmissionPolicyReady) {
		t.Fatalf("aggregate must name what was skipped: %q", agg.Message)
	}
}

func TestReconcileProvisionsAdmissionPolicy(t *testing.T) {
	sd := valid()
	c, r, req := newFixtureOpts(t, sd, kyvernoMapper(), nil, deploymentWithPodLabels("api", map[string]string{LabelName: "orders-api"}))

	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("nothing environmental is missing; no requeue expected, got %v", res.RequeueAfter)
	}

	pol := &unstructured.Unstructured{}
	pol.SetGroupVersionKind(KyvernoPolicyGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: KyvernoPolicyName, Namespace: "orders-api"}, pol); err != nil {
		t.Fatalf("kyverno policy not created: %v", err)
	}
	rules, found, err := unstructured.NestedSlice(pol.Object, "spec", "rules")
	if err != nil || !found || len(rules) != 2 {
		t.Fatalf("expected the 2 workload rules without a signing identity, got %v (%v)", len(rules), err)
	}
	if pol.GetLabels()[LabelManagedBy] != ManagedByValue {
		t.Fatalf("policy must carry the drift-watch labels: %v", pol.GetLabels())
	}

	got := getSD(t, c, sd.Name, sd.Namespace)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, CondAdmissionPolicyReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("AdmissionPolicyReady wrong: %+v", cond)
	}
	// Silence here would read as "images are verified". They are not.
	if !strings.Contains(cond.Message, "NOT verified") {
		t.Fatalf("condition must admit signatures are unverified: %q", cond.Message)
	}
}

func TestReconcileAdmissionPolicyVerifiesImagesWhenConfigured(t *testing.T) {
	sd := valid()
	c, r, req := newFixtureOpts(t, sd, kyvernoMapper(), func(r *ServiceDeploymentReconciler) {
		r.Policy = PolicyConfig{CosignPublicKey: "-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----"}
	})

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	pol := &unstructured.Unstructured{}
	pol.SetGroupVersionKind(KyvernoPolicyGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: KyvernoPolicyName, Namespace: "orders-api"}, pol); err != nil {
		t.Fatal(err)
	}
	rules, _, _ := unstructured.NestedSlice(pol.Object, "spec", "rules")
	if len(rules) != 3 {
		t.Fatalf("expected the verify rule to be added, got %d rules", len(rules))
	}
	verify := rules[2].(map[string]any)["verifyImages"].([]any)[0].(map[string]any)
	if verify["mutateDigest"] != false {
		t.Fatal("mutateDigest must survive the round-trip as false — otherwise Kyverno and ArgoCD selfHeal fight over the image")
	}

	got := getSD(t, c, sd.Name, sd.Namespace)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, CondAdmissionPolicyReady)
	if cond == nil || !strings.Contains(cond.Message, "verify-image-signatures") {
		t.Fatalf("condition should enumerate the shipped rules: %+v", cond)
	}
	if strings.Contains(cond.Message, "NOT verified") {
		t.Fatalf("verification is configured; condition must not claim otherwise: %q", cond.Message)
	}
}

func TestReconcileReportsEveryComponent(t *testing.T) {
	sd := valid()
	c, r, req := newFixtureOpts(t, sd, kyvernoMapper(), nil)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getSD(t, c, sd.Name, sd.Namespace)
	for _, condType := range []string{
		CondNamespaceReady, CondSLOReady, CondPDBReady,
		CondNetworkPolicyReady, CondAdmissionPolicyReady, CondDeliveryReady, CondProvisioned,
	} {
		cond := apimeta.FindStatusCondition(got.Status.Conditions, condType)
		if cond == nil {
			t.Fatalf("%s condition missing", condType)
		}
		if cond.Message == "" {
			t.Fatalf("%s has no message; a condition nobody can act on is noise", condType)
		}
	}
}

func TestReconcileFailureNamesTheBrokenComponent(t *testing.T) {
	sd := valid()
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orders-api"}}
	c, r, req := newFixture(t, sd, foreign)

	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("expected an error")
	}
	got := getSD(t, c, sd.Name, sd.Namespace)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, CondNamespaceReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "not managed") {
		t.Fatalf("the failing component must carry the reason: %+v", cond)
	}
	// Steps after the failure never ran, so they must not claim readiness.
	if apimeta.FindStatusCondition(got.Status.Conditions, CondDeliveryReady) != nil {
		t.Fatal("a step that never ran must not report a condition")
	}
}

func TestReconcileAdoptsAnObjectTheCacheCannotSee(t *testing.T) {
	// The manager cache is filtered to this operator's labels, so an
	// existing object with different labels reads as absent. Without the
	// uncached fallback the create loses with AlreadyExists and the service
	// never gets provisioned again.
	sd := valid()
	squatter := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      NetworkPolicyDenyIngress,
			Namespace: "orders-api",
			Labels:    map[string]string{"owner": "someone-else"},
		},
	}
	base := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(sd, squatter).WithStatusSubresource(sd).Build()

	// Client behaves like the filtered cache: it cannot see the squatter.
	hidden := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*networkingv1.NetworkPolicy); ok && key.Name == NetworkPolicyDenyIngress {
				return apierrors.NewNotFound(networkingv1.Resource("networkpolicies"), key.Name)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	r := &ServiceDeploymentReconciler{Client: hidden, Reader: base}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sd.Name, Namespace: sd.Namespace}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("a stray same-named policy must not wedge the reconcile: %v", err)
	}

	// The operator owns its guardrails: the object is taken over, not left.
	np := getNetworkPolicy(t, base, NetworkPolicyDenyIngress)
	if np.Labels[LabelManagedBy] != ManagedByValue {
		t.Fatalf("policy should have been adopted, labels = %v", np.Labels)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Fatalf("adopted policy must carry our spec: %+v", np.Spec)
	}
}

func TestSetStatusRetriesOnConflict(t *testing.T) {
	// A status write can lose to a concurrent update; losing once must not
	// lose the status. Forced deterministically rather than raced.
	sd := valid()
	base := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(sd).WithStatusSubresource(sd).Build()

	var conflicts int
	flaky := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if conflicts == 0 {
				conflicts++
				return apierrors.NewConflict(
					schema.GroupResource{Group: "idp.jiung9.github.io", Resource: "servicedeployments"},
					obj.GetName(), errors.New("object was modified"))
			}
			return cl.Status().Update(ctx, obj, opts...)
		},
	})
	r := &ServiceDeploymentReconciler{Client: flaky, Reader: base}

	if err := r.setStatus(context.Background(), sd, PhaseProvisioned,
		newCondition(sd, CondProvisioned, metav1.ConditionTrue, "Reconciled", "ok")); err != nil {
		t.Fatalf("a single conflict must be retried, not surfaced: %v", err)
	}
	if conflicts != 1 {
		t.Fatalf("expected the conflict path to be exercised, got %d", conflicts)
	}
	got := getSD(t, base, sd.Name, sd.Namespace)
	if got.Status.Phase != PhaseProvisioned {
		t.Fatalf("status lost to the conflict: %+v", got.Status)
	}
}

func TestFailWrapsTheUnderlyingError(t *testing.T) {
	sd := valid()
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orders-api"}}
	_, r, req := newFixture(t, sd, foreign)

	_, err := r.Reconcile(context.Background(), req)
	if err == nil {
		t.Fatal("expected an error")
	}
	// The component name must survive for the logs, and so must the cause.
	if !strings.HasPrefix(err.Error(), "namespace: ") {
		t.Fatalf("error should name the component: %v", err)
	}
	if errors.Unwrap(err) == nil {
		t.Fatal("the cause must stay wrapped so errors.Is/As keep working")
	}
}

func deploymentWithPodLabels(name string, labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "orders-api"},
		Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}}},
	}
}

func TestWorkloadsSelectedCondition(t *testing.T) {
	cases := []struct {
		name     string
		objs     []client.Object
		replicas int32
		status   metav1.ConditionStatus
		reason   string
		message  string
		requeue  bool
	}{
		{"labelled deployment", []client.Object{deploymentWithPodLabels("api", map[string]string{LabelName: "orders-api"})},
			3, metav1.ConditionTrue, "Selected", "1 workload(s)", false},
		{"label missing from the pod template", []client.Object{deploymentWithPodLabels("api", map[string]string{"app": "orders-api"})},
			3, metav1.ConditionFalse, "LabelMissing", "Deployment/api", true},
		{"one of two is unlabelled", []client.Object{
			deploymentWithPodLabels("api", map[string]string{LabelName: "orders-api"}),
			&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "orders-api"}},
		}, 3, metav1.ConditionFalse, "LabelMissing", "StatefulSet/cache", true},
		{"nothing delivered yet", nil, 3, metav1.ConditionFalse, "NoWorkloads", "no Deployment", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sd := valid()
			sd.Spec.Replicas = ptr(tc.replicas)
			c, r, req := newFixture(t, sd, tc.objs...)
			res, err := r.Reconcile(context.Background(), req)
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			got := getSD(t, c, sd.Name, sd.Namespace)
			cond := apimeta.FindStatusCondition(got.Status.Conditions, CondWorkloadsSelected)
			if cond == nil || cond.Status != tc.status || cond.Reason != tc.reason || !strings.Contains(cond.Message, tc.message) {
				t.Fatalf("WorkloadsSelected wrong: %+v", cond)
			}
			if got.Status.Phase != PhaseProvisioned {
				t.Fatalf("phase = %q; a mislabelled workload is degraded, not broken", got.Status.Phase)
			}
			if tc.requeue && res.RequeueAfter == 0 {
				t.Fatal("a fixed label or a late sync is not watched; the reconcile must come back")
			}
		})
	}
}

func TestWorkloadsSelectedIsOmittedWithoutAPDB(t *testing.T) {
	sd := valid()
	sd.Spec.Replicas = ptr(int32(1))
	c, r, req := newFixture(t, sd, deploymentWithPodLabels("api", nil))
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getSD(t, c, sd.Name, sd.Namespace)
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, CondWorkloadsSelected); cond != nil {
		t.Fatalf("no PDB, nothing to select; condition should be absent: %+v", cond)
	}
}
