package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
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

func TestReconcileProvisionsGoldenPath(t *testing.T) {
	s := newScheme(t)
	sd := valid()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sd).WithStatusSubresource(sd).Build()
	r := &ServiceDeploymentReconciler{Client: c}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: sd.Name, Namespace: sd.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ctx := context.Background()

	var ns corev1.Namespace
	if err := c.Get(ctx, types.NamespacedName{Name: "orders-api"}, &ns); err != nil {
		t.Fatalf("namespace not created: %v", err)
	}
	if ns.Labels["idp.jiung9.github.io/team"] != "checkout" {
		t.Fatalf("namespace missing team label: %v", ns.Labels)
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
		t.Fatalf("pdb not created: %v", err)
	}

	app := &unstructured.Unstructured{}
	app.SetAPIVersion("argoproj.io/v1alpha1")
	app.SetKind("Application")
	if err := c.Get(ctx, types.NamespacedName{Name: "orders-api", Namespace: ArgoCDNamespace}, app); err != nil {
		t.Fatalf("argocd application not created: %v", err)
	}
	repo, _, _ := unstructured.NestedString(app.Object, "spec", "source", "repoURL")
	if repo != sd.Spec.GitOps.RepoURL {
		t.Fatalf("application repoURL = %q", repo)
	}

	var got idpv1alpha1.ServiceDeployment
	if err := c.Get(ctx, types.NamespacedName{Name: sd.Name, Namespace: sd.Namespace}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != "Ready" {
		t.Fatalf("phase = %q, want Ready (conditions: %+v)", got.Status.Phase, got.Status.Conditions)
	}
}

func TestReconcileRejectsInvalidSpec(t *testing.T) {
	s := newScheme(t)
	sd := valid()
	sd.Spec.Team = ""
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sd).WithStatusSubresource(sd).Build()
	r := &ServiceDeploymentReconciler{Client: c}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: sd.Name, Namespace: sd.Namespace},
	}); err != nil {
		t.Fatalf("invalid spec must not requeue-error: %v", err)
	}

	var got idpv1alpha1.ServiceDeployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: sd.Name, Namespace: sd.Namespace}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != "Invalid" {
		t.Fatalf("phase = %q, want Invalid", got.Status.Phase)
	}
	var ns corev1.Namespace
	if err := c.Get(context.Background(), types.NamespacedName{Name: "orders-api"}, &ns); err == nil {
		t.Fatal("namespace must NOT be created for an invalid spec")
	}
}
