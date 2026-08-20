package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	idpv1alpha1 "github.com/JIUNG9/idp-operator/api/v1alpha1"
)

// ArgoCDNamespace is where Application objects are written.
const ArgoCDNamespace = "argocd"

// ServiceDeploymentReconciler provisions the golden path for one manifest:
// namespace -> SLO baseline -> PDB -> ArgoCD Application.
type ServiceDeploymentReconciler struct {
	client.Client
}

func (r *ServiceDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sd idpv1alpha1.ServiceDeployment
	if err := r.Get(ctx, req.NamespacedName, &sd); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := Validate(&sd); err != nil {
		return r.setStatus(ctx, &sd, "Invalid", err.Error())
	}

	sd.Spec.SLO = ApplySLODefaults(sd.Spec.SLO)
	sd.Spec.GitOps = ApplyGitOpsDefaults(sd.Spec.GitOps)
	labels := Labels(&sd)

	// 1. Namespace — the service's home, stamped with ownership.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: sd.Name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ns, func() error {
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		for k, v := range labels {
			ns.Labels[k] = v
		}
		return nil
	}); err != nil {
		return r.setStatus(ctx, &sd, "Error", fmt.Sprintf("namespace: %v", err))
	}

	// 2. SLO baseline — machine-readable reliability contract alerting reads.
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "slo-baseline", Namespace: sd.Name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labels
		cm.Data = map[string]string{
			"availability_target": sd.Spec.SLO.AvailabilityTarget,
			"latency_p99_ms":      fmt.Sprintf("%d", sd.Spec.SLO.LatencyP99Ms),
			"error_budget_window": ErrorBudgetWindow,
		}
		return nil
	}); err != nil {
		return r.setStatus(ctx, &sd, "Error", fmt.Sprintf("slo baseline: %v", err))
	}

	// 3. PDB — disruption safety by default, not by request.
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: sd.Name, Namespace: sd.Name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Labels = labels
		minAvailable := intstr.FromInt32(1)
		pdb.Spec = policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": sd.Name}},
		}
		return nil
	}); err != nil {
		return r.setStatus(ctx, &sd, "Error", fmt.Sprintf("pdb: %v", err))
	}

	// 4. ArgoCD Application — delivery is Git-to-production from day one.
	app := &unstructured.Unstructured{}
	app.SetAPIVersion("argoproj.io/v1alpha1")
	app.SetKind("Application")
	app.SetName(sd.Name)
	app.SetNamespace(ArgoCDNamespace)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, app, func() error {
		app.SetLabels(labels)
		return unstructured.SetNestedMap(app.Object, map[string]any{
			"project": sd.Spec.GitOps.Project,
			"source": map[string]any{
				"repoURL":        sd.Spec.GitOps.RepoURL,
				"path":           sd.Spec.GitOps.Path,
				"targetRevision": sd.Spec.GitOps.TargetRevision,
			},
			"destination": map[string]any{
				"server":    "https://kubernetes.default.svc",
				"namespace": sd.Name,
			},
			"syncPolicy": map[string]any{
				"automated": map[string]any{"prune": true, "selfHeal": true},
			},
		}, "spec")
	}); err != nil {
		return r.setStatus(ctx, &sd, "Error", fmt.Sprintf("argocd application: %v", err))
	}

	logger.Info("golden path reconciled", "service", sd.Name, "team", sd.Spec.Team)
	return r.setStatus(ctx, &sd, "Ready", "")
}

func (r *ServiceDeploymentReconciler) setStatus(ctx context.Context, sd *idpv1alpha1.ServiceDeployment, phase, msg string) (ctrl.Result, error) {
	sd.Status.Phase = phase
	sd.Status.ObservedGeneration = sd.Generation
	cond := metav1.Condition{
		Type:               "Provisioned",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            "golden path provisioned",
		LastTransitionTime: metav1.Now(),
	}
	if phase != "Ready" {
		cond.Status = metav1.ConditionFalse
		cond.Reason = phase
		cond.Message = msg
	}
	meta := &sd.Status.Conditions
	*meta = []metav1.Condition{cond}
	if err := r.Status().Update(ctx, sd); err != nil {
		return ctrl.Result{}, err
	}
	if phase == "Error" {
		return ctrl.Result{}, fmt.Errorf("%s", msg)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager wires the reconciler into the manager.
func (r *ServiceDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&idpv1alpha1.ServiceDeployment{}).
		Complete(r)
}
