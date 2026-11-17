package controller

import (
	"context"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// reconcileWithKyvernoStatus reconciles once so the Policy exists, writes the
// given status the way Kyverno would, reconciles again and returns the
// AdmissionPolicyReady and Ready conditions.
func reconcileWithKyvernoStatus(t *testing.T, status map[string]any) (admission, ready *metav1.Condition) {
	t.Helper()
	sd := valid()
	c, r, req := newFixtureOpts(t, sd, kyvernoMapper(), nil, deploymentWithPodLabels("api", map[string]string{LabelName: "orders-api"}))
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if status != nil {
		pol := &unstructured.Unstructured{}
		pol.SetGroupVersionKind(KyvernoPolicyGVK)
		if err := c.Get(ctx, types.NamespacedName{Name: KyvernoPolicyName, Namespace: "orders-api"}, pol); err != nil {
			t.Fatalf("policy missing: %v", err)
		}
		if err := unstructured.SetNestedMap(pol.Object, status, "status"); err != nil {
			t.Fatal(err)
		}
		if err := c.Update(ctx, pol); err != nil {
			t.Fatalf("write policy status: %v", err)
		}
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	got := getSD(t, c, sd.Name, sd.Namespace)
	return apimeta.FindStatusCondition(got.Status.Conditions, CondAdmissionPolicyReady),
		apimeta.FindStatusCondition(got.Status.Conditions, CondReady)
}

func TestAdmissionPolicyDegradesWhenKyvernoReportsNotReady(t *testing.T) {
	admission, ready := reconcileWithKyvernoStatus(t, map[string]any{
		"conditions": []any{map[string]any{
			"type": "Ready", "status": "False", "reason": "Failed", "message": "variable x is not defined",
		}},
	})
	if admission == nil || admission.Status != metav1.ConditionFalse || admission.Reason != "KyvernoPolicyNotReady" {
		t.Fatalf("AdmissionPolicyReady must be False/KyvernoPolicyNotReady: %+v", admission)
	}
	if !strings.Contains(admission.Message, "variable x is not defined") {
		t.Fatalf("Kyverno's own message must reach the condition: %q", admission.Message)
	}
	// A declared but inactive guardrail is degraded, so kubectl wait on Ready must not pass.
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready must be False while the policy is not loaded: %+v", ready)
	}
}

func TestAdmissionPolicyReadyWhenKyvernoReportsReady(t *testing.T) {
	admission, ready := reconcileWithKyvernoStatus(t, map[string]any{
		"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "Succeeded"}},
	})
	if admission == nil || admission.Status != metav1.ConditionTrue || !strings.Contains(admission.Message, "Kyverno reports the policy Ready") {
		t.Fatalf("expected True with Kyverno's readiness in the message: %+v", admission)
	}
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("Ready must stay True: %+v", ready)
	}
}

func TestAdmissionPolicyUnreportedStatusIsNotAFailure(t *testing.T) {
	admission, ready := reconcileWithKyvernoStatus(t, nil)
	if admission == nil || admission.Status != metav1.ConditionTrue || !strings.Contains(admission.Message, "has not reported a status") {
		t.Fatalf("no status yet must read True and say so: %+v", admission)
	}
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("Ready must not flap while Kyverno catches up: %+v", ready)
	}
}

func TestKyvernoReadinessLegacyBool(t *testing.T) {
	cases := []struct {
		name   string
		status map[string]any
		want   kyvernoState
	}{
		{"ready bool true", map[string]any{"ready": true}, kyvernoReady},
		{"ready bool false", map[string]any{"ready": false}, kyvernoNotReady},
		{"condition beats bool", map[string]any{
			"ready":      false,
			"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
		}, kyvernoReady},
		{"unrelated condition only", map[string]any{
			"conditions": []any{map[string]any{"type": "Other", "status": "False"}},
		}, kyvernoUnreported},
		{"condition Unknown", map[string]any{
			"conditions": []any{map[string]any{"type": "Ready", "status": "Unknown"}},
		}, kyvernoUnreported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pol := &unstructured.Unstructured{Object: map[string]any{"status": tc.status}}
			if got, _ := kyvernoReadiness(pol); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
