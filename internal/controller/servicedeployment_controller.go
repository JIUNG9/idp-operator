package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	idpv1alpha1 "github.com/JIUNG9/idp-operator/api/v1alpha1"
)

const (
	// ArgoCDNamespace is where Application objects are written.
	ArgoCDNamespace = "argocd"

	// Finalizer guards garbage collection of the children ownerReferences
	// can't reach: the cluster-scoped namespace and the Application over in
	// the argocd namespace. OwnerRefs are illegal across those boundaries.
	Finalizer = "idp.jiung9.github.io/cleanup"

	// Phases. Provisioned means the golden-path objects exist and match
	// spec — it deliberately does NOT claim the application is healthy;
	// delivery health is ArgoCD's job.
	PhaseProvisioned = "Provisioned"
	PhaseInvalid     = "Invalid"
	PhaseError       = "Error"
	PhaseDeleting    = "Deleting"

	// Condition types.
	CondProvisioned = "Provisioned"
	CondPDBReady    = "PDBReady"
)

// ApplicationGVK identifies the ArgoCD Application resource; it is handled
// as unstructured so the operator has no ArgoCD Go dependency.
var ApplicationGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}

// ServiceDeploymentReconciler provisions the golden path for one manifest:
// namespace -> SLO baseline -> PDB -> ArgoCD Application.
type ServiceDeploymentReconciler struct {
	client.Client
	// Reader, when set, is an uncached reader (mgr.GetAPIReader()). The
	// namespace-ownership guard needs it because the manager cache is
	// label-filtered to managed objects and would hide foreign namespaces.
	Reader client.Reader
}

// reader returns the uncached reader, falling back to the client (tests use
// an unfiltered fake client where the distinction doesn't exist).
func (r *ServiceDeploymentReconciler) reader() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}

// +kubebuilder:rbac:groups=idp.jiung9.github.io,resources=servicedeployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=idp.jiung9.github.io,resources=servicedeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=idp.jiung9.github.io,resources=servicedeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list;watch;create;update;patch;delete

func (r *ServiceDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sd idpv1alpha1.ServiceDeployment
	if err := r.Get(ctx, req.NamespacedName, &sd); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !sd.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &sd)
	}

	if !controllerutil.ContainsFinalizer(&sd, Finalizer) {
		controllerutil.AddFinalizer(&sd, Finalizer)
		if err := r.Update(ctx, &sd); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := Validate(&sd); err != nil {
		return ctrl.Result{}, r.setStatus(ctx, &sd, PhaseInvalid,
			newCondition(&sd, CondProvisioned, metav1.ConditionFalse, PhaseInvalid, err.Error()))
	}

	// The CRD schema is the authoritative defaulter; this repeats it for
	// clients that bypass API-server defaulting. Never persisted to spec.
	sd.Spec.SLO = ApplySLODefaults(sd.Spec.SLO)
	sd.Spec.GitOps = ApplyGitOpsDefaults(sd.Spec.GitOps)
	labels := Labels(&sd)

	// 1. Namespace — the service's home, stamped with ownership. Refuse to
	// adopt one this operator doesn't own: without the guard, two
	// ServiceDeployments with the same name in different namespaces would
	// silently fight over one cluster namespace.
	var existing corev1.Namespace
	switch err := r.reader().Get(ctx, types.NamespacedName{Name: sd.Name}, &existing); {
	case err == nil:
		if existing.Labels[LabelManagedBy] != ManagedByValue {
			return r.fail(ctx, &sd, "namespace",
				fmt.Errorf("namespace %q exists and is not managed by idp-operator", sd.Name))
		}
		if owner := existing.Labels[LabelOwnerNamespace]; owner != "" && owner != sd.Namespace {
			return r.fail(ctx, &sd, "namespace",
				fmt.Errorf("namespace %q is owned by a ServiceDeployment in %q", sd.Name, owner))
		}
	case !apierrors.IsNotFound(err):
		return r.fail(ctx, &sd, "namespace", err)
	}
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
		return r.fail(ctx, &sd, "namespace", err)
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
		return r.fail(ctx, &sd, "slo baseline", err)
	}

	// 3. PDB — derived from declared replicas. Below two replicas a PDB
	// can't protect anything, so provision none and say so, instead of
	// shipping the classic drain-blocking minAvailable=1 footgun.
	var pdbCond metav1.Condition
	if PDBWanted(sd.Spec.Replicas) {
		pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: sd.Name, Namespace: sd.Name}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
			pdb.Labels = labels
			maxUnavailable := intstr.FromInt32(1)
			pdb.Spec = policyv1.PodDisruptionBudgetSpec{
				MaxUnavailable: &maxUnavailable,
				Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{LabelName: sd.Name}},
			}
			return nil
		}); err != nil {
			return r.fail(ctx, &sd, "pdb", err)
		}
		pdbCond = newCondition(&sd, CondPDBReady, metav1.ConditionTrue, "Protected",
			fmt.Sprintf("maxUnavailable=1 across %d replicas", sd.Spec.Replicas))
	} else {
		stale := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: sd.Name, Namespace: sd.Name}}
		if err := r.Delete(ctx, stale); err != nil && !apierrors.IsNotFound(err) {
			return r.fail(ctx, &sd, "pdb", err)
		}
		reason, message := PDBSkipReason(sd.Spec.Replicas)
		pdbCond = newCondition(&sd, CondPDBReady, metav1.ConditionFalse, reason, message)
	}

	// 4. ArgoCD Application — delivery is Git-to-production from day one.
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(ApplicationGVK)
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
		return r.fail(ctx, &sd, "argocd application", err)
	}

	logger.Info("golden path reconciled", "service", sd.Name, "team", sd.Spec.Team)
	return ctrl.Result{}, r.setStatus(ctx, &sd, PhaseProvisioned,
		newCondition(&sd, CondProvisioned, metav1.ConditionTrue, "Reconciled", "golden path provisioned"),
		pdbCond)
}

// finalize tears down what the golden path provisioned. Deletion order:
// Application first (it lives outside the namespace cascade), then the
// namespace, which cascades to the ConfigMap/PDB inside it. Every delete is
// idempotent — names are derived, never stored — so unwinding a partial
// provision is safe. The namespace's actual termination is the API server's
// job; this operator only requires deletion to be initiated.
func (r *ServiceDeploymentReconciler) finalize(ctx context.Context, sd *idpv1alpha1.ServiceDeployment) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(sd, Finalizer) {
		return ctrl.Result{}, nil
	}
	// Best-effort phase marker; teardown proceeds even if this write loses.
	_ = r.setStatus(ctx, sd, PhaseDeleting,
		newCondition(sd, CondProvisioned, metav1.ConditionFalse, PhaseDeleting, "golden path tearing down"))

	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(ApplicationGVK)
	app.SetName(sd.Name)
	app.SetNamespace(ArgoCDNamespace)
	if err := r.Delete(ctx, app); err != nil &&
		!apierrors.IsNotFound(err) && !apimeta.IsNoMatchError(err) {
		return ctrl.Result{}, err
	}

	// Read through the uncached reader: the manager cache is label-filtered,
	// so a namespace whose labels were mutated (or not yet synced) would look
	// absent and the finalizer would be removed without deleting it — a leak.
	var ns corev1.Namespace
	switch err := r.reader().Get(ctx, types.NamespacedName{Name: sd.Name}, &ns); {
	case apierrors.IsNotFound(err):
		// already gone
	case err != nil:
		return ctrl.Result{}, err
	case ns.Labels[LabelManagedBy] == ManagedByValue &&
		(ns.Labels[LabelOwnerNamespace] == "" || ns.Labels[LabelOwnerNamespace] == sd.Namespace):
		if ns.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, &ns); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	default:
		// Not ours (unmanaged, or owned by another ServiceDeployment after a
		// collision) — leave it alone.
		log.FromContext(ctx).Info("skipping namespace on teardown: not owned by this ServiceDeployment",
			"namespace", sd.Name)
	}

	// Remove the finalizer from the freshest copy — the status write above
	// already bumped resourceVersion, so the in-hand object is stale.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest idpv1alpha1.ServiceDeployment
		if err := r.Get(ctx, client.ObjectKeyFromObject(sd), &latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !controllerutil.ContainsFinalizer(&latest, Finalizer) {
			return nil
		}
		controllerutil.RemoveFinalizer(&latest, Finalizer)
		return r.Update(ctx, &latest)
	})
	return ctrl.Result{}, err
}

// fail records an Error status and returns the error so controller-runtime
// requeues with backoff.
func (r *ServiceDeploymentReconciler) fail(ctx context.Context, sd *idpv1alpha1.ServiceDeployment, component string, err error) (ctrl.Result, error) {
	msg := fmt.Sprintf("%s: %v", component, err)
	if serr := r.setStatus(ctx, sd, PhaseError,
		newCondition(sd, CondProvisioned, metav1.ConditionFalse, PhaseError, msg)); serr != nil {
		return ctrl.Result{}, serr
	}
	return ctrl.Result{}, fmt.Errorf("%s", msg)
}

// setStatus persists phase + conditions against the freshest copy, retrying
// on resourceVersion conflicts (the fake client hides them; envtest and real
// API servers do not). meta.SetStatusCondition keeps per-type history and
// only bumps LastTransitionTime on a real transition.
func (r *ServiceDeploymentReconciler) setStatus(ctx context.Context, sd *idpv1alpha1.ServiceDeployment, phase string, conditions ...metav1.Condition) error {
	// ObservedGeneration reports the generation this reconcile actually
	// processed — sd's, not whatever is freshest at write time. A racing
	// spec edit bumps the generation and gets its own reconcile; claiming
	// it here would mark spec we never looked at as observed.
	observed := sd.Generation
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest idpv1alpha1.ServiceDeployment
		if err := r.Get(ctx, client.ObjectKeyFromObject(sd), &latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		latest.Status.Phase = phase
		latest.Status.ObservedGeneration = observed
		for _, c := range conditions {
			apimeta.SetStatusCondition(&latest.Status.Conditions, c)
		}
		return r.Status().Update(ctx, &latest)
	})
}

func newCondition(sd *idpv1alpha1.ServiceDeployment, condType string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: sd.Generation,
	}
}

// mapProvisioned routes drift on a provisioned object back to the owning
// ServiceDeployment via the labels every provisioned object carries. This is
// the watch path for children ownerReferences can't cover (the cluster-scoped
// namespace, the cross-namespace ConfigMap/PDB/Application).
func mapProvisioned(_ context.Context, obj client.Object) []ctrl.Request {
	l := obj.GetLabels()
	if l[LabelManagedBy] != ManagedByValue {
		return nil
	}
	name, owner := l[LabelName], l[LabelOwnerNamespace]
	if name == "" || owner == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: name, Namespace: owner}}}
}

// SetupWithManager wires the reconciler into the manager.
func (r *ServiceDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&idpv1alpha1.ServiceDeployment{}).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(mapProvisioned)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(mapProvisioned)).
		Watches(&policyv1.PodDisruptionBudget{}, handler.EnqueueRequestsFromMapFunc(mapProvisioned))

	// The Application watch needs the ArgoCD CRD. On clusters without it,
	// skip the watch (drift on Applications won't re-reconcile) instead of
	// failing manager start; creating Applications will surface its own
	// error in status until the CRD arrives.
	if _, err := mgr.GetRESTMapper().RESTMapping(ApplicationGVK.GroupKind(), ApplicationGVK.Version); err == nil {
		app := &unstructured.Unstructured{}
		app.SetGroupVersionKind(ApplicationGVK)
		b = b.Watches(app, handler.EnqueueRequestsFromMapFunc(mapProvisioned))
	} else {
		ctrl.Log.WithName("setup").Info(
			"argoproj.io Application CRD not found; Application drift will not trigger reconciles")
	}
	return b.Complete(r)
}
