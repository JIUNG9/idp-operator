package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	idpv1alpha1 "github.com/JIUNG9/idp-operator/api/v1alpha1"
)

// foreignLabels is what an object somebody else created looks like to us.
var foreignLabels = map[string]string{"owner": "someone-else"}

func foreignPolicy() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(KyvernoPolicyGVK)
	u.SetName(KyvernoPolicyName)
	u.SetNamespace("orders-api")
	u.SetLabels(foreignLabels)
	return u
}

func foreignApplication() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ApplicationGVK)
	u.SetName("orders-api")
	u.SetNamespace(ArgoCDNamespace)
	u.SetLabels(foreignLabels)
	return u
}

// TestReconcileRefusesObjectsItDoesNotOwn pre-creates, per kind the operator
// writes, an object under the name the golden path wants but without our
// labels. The reconcile must fail with the NotOwned reason on that kind's
// condition and leave the object exactly as it was.
func TestReconcileRefusesObjectsItDoesNotOwn(t *testing.T) {
	cases := []struct {
		name string
		cond string
		obj  client.Object
		key  types.NamespacedName
	}{
		{"ConfigMap", CondSLOReady, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "slo-baseline", Namespace: "orders-api", Labels: foreignLabels},
			Data:       map[string]string{"availability_target": "1"},
		}, types.NamespacedName{Name: "slo-baseline", Namespace: "orders-api"}},
		{"PodDisruptionBudget", CondPDBReady, &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "orders-api", Namespace: "orders-api", Labels: foreignLabels},
		}, types.NamespacedName{Name: "orders-api", Namespace: "orders-api"}},
		{"NetworkPolicy", CondNetworkPolicyReady, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: NetworkPolicyDenyIngress, Namespace: "orders-api", Labels: foreignLabels},
		}, types.NamespacedName{Name: NetworkPolicyDenyIngress, Namespace: "orders-api"}},
		{"Kyverno Policy", CondAdmissionPolicyReady, foreignPolicy(),
			types.NamespacedName{Name: KyvernoPolicyName, Namespace: "orders-api"}},
		{"ArgoCD Application", CondDeliveryReady, foreignApplication(),
			types.NamespacedName{Name: "orders-api", Namespace: ArgoCDNamespace}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sd := valid()
			c, r, req := newFixtureOpts(t, sd, kyvernoMapper(), nil, tc.obj)

			_, err := r.Reconcile(context.Background(), req)
			var notOwned *NotOwnedError
			if !errors.As(err, &notOwned) {
				t.Fatalf("want a NotOwnedError, got %v", err)
			}
			got := getSD(t, c, sd.Name, sd.Namespace)
			cond := apimeta.FindStatusCondition(got.Status.Conditions, tc.cond)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonNotOwned {
				t.Fatalf("%s condition = %+v, want False/%s", tc.cond, cond, ReasonNotOwned)
			}
			if got.Status.Phase != PhaseError {
				t.Fatalf("phase = %q, want %q", got.Status.Phase, PhaseError)
			}

			live := tc.obj.DeepCopyObject().(client.Object)
			if err := c.Get(context.Background(), tc.key, live); err != nil {
				t.Fatal(err)
			}
			if live.GetLabels()["owner"] != "someone-else" || live.GetLabels()[LabelManagedBy] != "" {
				t.Fatalf("the foreign object was modified: labels = %v", live.GetLabels())
			}
		})
	}
}

func TestReconcileRefusesObjectOwnedByAnotherNamespace(t *testing.T) {
	sd := valid()
	other := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "slo-baseline", Namespace: "orders-api",
		Labels: map[string]string{LabelManagedBy: ManagedByValue, LabelOwnerNamespace: "somewhere-else"},
	}}
	_, r, req := newFixture(t, sd, other)

	var notOwned *NotOwnedError
	if _, err := r.Reconcile(context.Background(), req); !errors.As(err, &notOwned) {
		t.Fatalf("want a NotOwnedError for an object owned in another namespace, got %v", err)
	}
}

// The cache the operator runs with is filtered to its own labels, so the
// ownership check must read uncached. Here the cached client cannot see the
// squatter at all; the reader can.
func TestOwnershipCheckReadsThroughTheUncachedReader(t *testing.T) {
	sd := valid()
	squatter := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: NetworkPolicyDenyIngress, Namespace: "orders-api", Labels: foreignLabels,
	}}
	base := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(sd, squatter).WithStatusSubresource(sd).Build()
	hidden := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*networkingv1.NetworkPolicy); ok && key.Name == NetworkPolicyDenyIngress {
				return apierrors.NewNotFound(networkingv1.Resource("networkpolicies"), key.Name)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	r := &ServiceDeploymentReconciler{Client: hidden, Reader: base}

	err := r.applyManaged(context.Background(), sd,
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: NetworkPolicyDenyIngress, Namespace: "orders-api"}},
		func() error { return nil })
	var notOwned *NotOwnedError
	if !errors.As(err, &notOwned) {
		t.Fatalf("want NotOwnedError, got %v", err)
	}
}

func TestOwnedObjectIsStillUpdated(t *testing.T) {
	sd := valid()
	c, r, req := newFixture(t, sd)
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	key := types.NamespacedName{Name: "slo-baseline", Namespace: "orders-api"}
	if err := c.Get(ctx, key, &cm); err != nil {
		t.Fatal(err)
	}
	cm.Data["availability_target"] = "drifted"
	if err := c.Update(ctx, &cm); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, &cm); err != nil || cm.Data["availability_target"] == "drifted" {
		t.Fatalf("our own ConfigMap must be reconciled back: %v, %v", err, cm.Data)
	}
}

func TestStalePDBCleanupLeavesForeignPDB(t *testing.T) {
	sd := valid()
	sd.Spec.Replicas = ptr(int32(1)) // no PDB wanted
	foreign := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{
		Name: "orders-api", Namespace: "orders-api", Labels: foreignLabels}}
	c, r, req := newFixture(t, sd, foreign)

	var notOwned *NotOwnedError
	if _, err := r.Reconcile(context.Background(), req); !errors.As(err, &notOwned) {
		t.Fatalf("want NotOwnedError when the stale-PDB name belongs to someone else, got %v", err)
	}
	var live policyv1.PodDisruptionBudget
	if err := c.Get(context.Background(), types.NamespacedName{Name: "orders-api", Namespace: "orders-api"}, &live); err != nil {
		t.Fatalf("foreign PDB must survive: %v", err)
	}
}

func TestStalePlatformPolicyCleanupLeavesForeignPolicy(t *testing.T) {
	sd := valid() // no platform ingress configured, so allow-platform is "stale"
	foreign := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: NetworkPolicyAllowPlatform, Namespace: "orders-api", Labels: foreignLabels}}
	c, r, req := newFixture(t, sd, foreign)

	var notOwned *NotOwnedError
	if _, err := r.Reconcile(context.Background(), req); !errors.As(err, &notOwned) {
		t.Fatalf("want NotOwnedError, got %v", err)
	}
	if np := getNetworkPolicy(t, c, NetworkPolicyAllowPlatform); np.Labels["owner"] != "someone-else" {
		t.Fatalf("foreign policy must survive: %v", np.Labels)
	}
}

func TestTeardownLeavesForeignApplication(t *testing.T) {
	sd := valid()
	sd.Finalizers = []string{Finalizer}
	app := foreignApplication()
	c, r, req := newFixtureOpts(t, sd, nil, nil, app)
	ctx := context.Background()

	if err := c.Delete(ctx, getSD(t, c, sd.Name, sd.Namespace)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("teardown must finish even when the Application is not ours: %v", err)
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(ApplicationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "orders-api", Namespace: ArgoCDNamespace}, live); err != nil {
		t.Fatalf("foreign Application must survive teardown: %v", err)
	}
	var gone idpv1alpha1.ServiceDeployment
	if err := c.Get(ctx, types.NamespacedName{Name: sd.Name, Namespace: sd.Namespace}, &gone); err == nil {
		t.Fatal("the finalizer must still be removed")
	}
}
