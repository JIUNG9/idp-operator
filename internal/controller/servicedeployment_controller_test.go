package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
	objs := append([]client.Object{sd}, extra...)
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).WithStatusSubresource(sd).Build()
	r := &ServiceDeploymentReconciler{Client: c}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sd.Name, Namespace: sd.Namespace}}
	return c, r, req
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
	sd.Spec.Replicas = 1
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
	fresh.Spec.Replicas = 1
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
