package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestWorkloadLabelsChangedIgnoresEverythingButTemplateLabels(t *testing.T) {
	p := workloadLabelsChanged()
	old := deploymentWithPodLabels("api", map[string]string{LabelName: "orders-api"})

	scaled := old.DeepCopy()
	scaled.Spec.Replicas = ptr(int32(5))
	scaled.Status.ReadyReplicas = 5
	scaled.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app", Image: "app:2"}}
	if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: scaled}) {
		t.Fatal("scaling, an image bump or a status write must not queue a reconcile")
	}

	relabeled := old.DeepCopy()
	relabeled.Spec.Template.Labels = map[string]string{"app": "orders"}
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: relabeled}) {
		t.Fatal("a change to the pod-template labels must queue a reconcile")
	}

	dropped := old.DeepCopy()
	dropped.Spec.Template.Labels = nil
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: dropped}) {
		t.Fatal("removing the pod-template labels must queue a reconcile")
	}

	if !p.Create(event.CreateEvent{Object: old}) || !p.Delete(event.DeleteEvent{Object: old}) {
		t.Fatal("a workload appearing or going away must queue a reconcile")
	}
}

func TestWorkloadLabelsChangedCoversStatefulSets(t *testing.T) {
	p := workloadLabelsChanged()
	old := &appsv1.StatefulSet{Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"a": "1"}}}}}
	next := old.DeepCopy()
	next.Spec.Template.Labels["a"] = "2"
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: next}) {
		t.Fatal("a StatefulSet template label change must queue a reconcile")
	}
}

func TestMapWorkloadFindsTheServiceDeploymentByNamespaceName(t *testing.T) {
	sd := valid() // name orders-api
	other := valid()
	other.Name = "payments"
	_, r, _ := newFixture(t, sd, other)

	got := r.mapWorkload(context.Background(), deploymentWithPodLabels("api", nil))
	if len(got) != 1 || got[0].Name != sd.Name || got[0].Namespace != sd.Namespace {
		t.Fatalf("want one request for %s/%s, got %+v", sd.Namespace, sd.Name, got)
	}

	stray := deploymentWithPodLabels("api", nil)
	stray.Namespace = "kube-system"
	if got := r.mapWorkload(context.Background(), stray); len(got) != 0 {
		t.Fatalf("a workload outside any service namespace must map to nothing, got %+v", got)
	}
}

func TestTrimWorkloadKeepsWhatTheWatchReads(t *testing.T) {
	full := deploymentWithPodLabels("api", map[string]string{LabelName: "orders-api"})
	full.UID, full.ResourceVersion, full.Generation = "u1", "7", 3
	full.Labels = map[string]string{"team": "x"}
	full.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "argocd"}}
	full.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app", Image: "app:1"}}
	full.Status.ReadyReplicas = 2

	out, err := TrimWorkload(full)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(*appsv1.Deployment)
	if got.Name != "api" || got.Namespace != "orders-api" || got.UID != "u1" || got.ResourceVersion != "7" {
		t.Fatalf("identity fields lost: %+v", got.ObjectMeta)
	}
	if got.Spec.Template.Labels[LabelName] != "orders-api" {
		t.Fatalf("template labels lost: %+v", got.Spec.Template.Labels)
	}
	if len(got.ManagedFields) != 0 || len(got.Spec.Template.Spec.Containers) != 0 || got.Status.ReadyReplicas != 0 {
		t.Fatalf("expected managedFields, containers and status dropped: %+v", got)
	}

	if out, _ := TrimWorkload(&corev1.ConfigMap{}); out == nil {
		t.Fatal("other kinds must pass through unchanged")
	}
}
