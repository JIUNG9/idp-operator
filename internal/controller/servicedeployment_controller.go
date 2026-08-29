package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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

	// EnvironmentalRetryInterval is how long the reconciler waits before
	// looking again when the cluster could not yet provide something the
	// golden path wants. Nothing in the watch set fires when a CRD is
	// installed, so this is the only way back.
	EnvironmentalRetryInterval = 5 * time.Minute

	// Phases. Provisioned means the golden-path objects exist and match
	// spec — it deliberately does NOT claim the application is healthy;
	// delivery health is ArgoCD's job.
	PhaseProvisioned = "Provisioned"
	PhaseInvalid     = "Invalid"
	PhaseError       = "Error"
	PhaseDeleting    = "Deleting"

	// Condition types. One per provisioned component, plus the aggregate.
	CondProvisioned          = "Provisioned"
	CondNamespaceReady       = "NamespaceReady"
	CondSLOReady             = "SLOReady"
	CondPDBReady             = "PDBReady"
	CondNetworkPolicyReady   = "NetworkPolicyReady"
	CondAdmissionPolicyReady = "AdmissionPolicyReady"
	CondDeliveryReady        = "DeliveryReady"
)

// ApplicationGVK identifies the ArgoCD Application resource; it is handled
// as unstructured so the operator has no ArgoCD Go dependency.
var ApplicationGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}

// ServiceDeploymentReconciler provisions the golden path for one manifest:
// namespace -> SLO baseline -> PDB -> policy -> ArgoCD Application.
type ServiceDeploymentReconciler struct {
	client.Client
	// Reader, when set, is an uncached reader (mgr.GetAPIReader()). The
	// namespace-ownership guard needs it because the manager cache is
	// label-filtered to managed objects and would hide foreign namespaces.
	Reader client.Reader
	// Network and Policy are cluster-level configuration: the ingress
	// topology and the signing identity are properties of the platform,
	// not of any single service.
	Network NetworkConfig
	Policy  PolicyConfig
}

// reader returns the uncached reader, falling back to the client (tests use
// an unfiltered fake client where the distinction doesn't exist).
func (r *ServiceDeploymentReconciler) reader() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}

// components records what each part of the golden path did, so the aggregate
// condition can tell a deliberate skip from a failure. A single-replica
// service has no PDB and a cluster without Kyverno has no admission policy;
// neither is a broken golden path, and neither should read as one.
type components struct {
	sd        *idpv1alpha1.ServiceDeployment
	conds     []metav1.Condition
	skipped   []string
	retryable bool
}

func (c *components) ready(condType, reason, message string) {
	c.conds = append(c.conds, newCondition(c.sd, condType, metav1.ConditionTrue, reason, message))
}

// skip records something the spec asked us not to provision. A spec change
// starts its own reconcile, so there is nothing to come back for.
func (c *components) skip(condType, reason, message string) {
	c.conds = append(c.conds, newCondition(c.sd, condType, metav1.ConditionFalse, reason, message))
	c.skipped = append(c.skipped, condType)
}

// skipEnvironmental records something the cluster could not provide yet.
// Unlike a spec-driven skip, nothing will tell us when that changes.
func (c *components) skipEnvironmental(condType, reason, message string) {
	c.skip(condType, reason, message)
	c.retryable = true
}

func (c *components) aggregate() metav1.Condition {
	if len(c.skipped) == 0 {
		return newCondition(c.sd, CondProvisioned, metav1.ConditionTrue,
			"Reconciled", "golden path provisioned")
	}
	// Still True: everything this spec and this cluster allow was provisioned.
	// The reason names what was not, so "degraded" is never silent.
	return newCondition(c.sd, CondProvisioned, metav1.ConditionTrue, "ReconciledWithSkips",
		"golden path provisioned; not provisioned: "+strings.Join(c.skipped, ", "))
}

func (c *components) all() []metav1.Condition {
	return append(c.conds, c.aggregate())
}

// +kubebuilder:rbac:groups=idp.jiung9.github.io,resources=servicedeployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=idp.jiung9.github.io,resources=servicedeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=idp.jiung9.github.io,resources=servicedeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kyverno.io,resources=policies,verbs=get;list;watch;create;update;patch;delete
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
	c := &components{sd: &sd}

	// The golden path, in order. Each step records its own condition; the
	// first failure stops the path, because everything after it would be
	// provisioned into a namespace that may not exist.
	steps := []struct {
		cond string
		name string
		run  func() error
	}{
		{CondNamespaceReady, "namespace", func() error { return r.reconcileNamespace(ctx, &sd, labels, c) }},
		{CondSLOReady, "slo baseline", func() error { return r.reconcileSLOBaseline(ctx, &sd, labels, c) }},
		{CondPDBReady, "pdb", func() error { return r.reconcilePDB(ctx, &sd, labels, c) }},
		{CondNetworkPolicyReady, "network policy", func() error { return r.reconcileNetworkPolicies(ctx, &sd, labels, c) }},
		{CondAdmissionPolicyReady, "admission policy", func() error { return r.reconcileAdmissionPolicy(ctx, &sd, labels, c) }},
		{CondDeliveryReady, "argocd application", func() error { return r.reconcileApplication(ctx, &sd, labels, c) }},
	}
	for _, s := range steps {
		if err := s.run(); err != nil {
			return r.fail(ctx, &sd, c, s.cond, s.name, err)
		}
	}

	logger.Info("golden path reconciled", "service", sd.Name, "team", sd.Spec.Team,
		"skipped", strings.Join(c.skipped, ","))

	res := ctrl.Result{}
	if c.retryable {
		res.RequeueAfter = EnvironmentalRetryInterval
	}
	return res, r.setStatus(ctx, &sd, PhaseProvisioned, c.all()...)
}

// reconcileNamespace creates the service's home, refusing to adopt a
// namespace this operator didn't create or one another ServiceDeployment
// already owns — without that guard, two ServiceDeployments of the same name
// in different namespaces would silently fight over one cluster namespace.
func (r *ServiceDeploymentReconciler) reconcileNamespace(ctx context.Context, sd *idpv1alpha1.ServiceDeployment, labels map[string]string, c *components) error {
	var existing corev1.Namespace
	switch err := r.reader().Get(ctx, types.NamespacedName{Name: sd.Name}, &existing); {
	case err == nil:
		if existing.Labels[LabelManagedBy] != ManagedByValue {
			return fmt.Errorf("namespace %q exists and is not managed by idp-operator", sd.Name)
		}
		if owner := existing.Labels[LabelOwnerNamespace]; owner != "" && owner != sd.Namespace {
			return fmt.Errorf("namespace %q is owned by a ServiceDeployment in %q", sd.Name, owner)
		}
	case !apierrors.IsNotFound(err):
		return err
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
		return err
	}
	c.ready(CondNamespaceReady, "Provisioned", fmt.Sprintf("namespace %q owned by team %q", sd.Name, sd.Spec.Team))
	return nil
}

// reconcileSLOBaseline writes the reliability contract other tooling reads.
func (r *ServiceDeploymentReconciler) reconcileSLOBaseline(ctx context.Context, sd *idpv1alpha1.ServiceDeployment, labels map[string]string, c *components) error {
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
		return err
	}
	c.ready(CondSLOReady, "Provisioned", fmt.Sprintf("%s%% availability, %dms p99 over %s",
		sd.Spec.SLO.AvailabilityTarget, sd.Spec.SLO.LatencyP99Ms, ErrorBudgetWindow))
	return nil
}

// reconcilePDB derives disruption policy from the declared replicas. Below
// two replicas a PDB cannot protect anything, so none is provisioned and the
// condition says which case this is.
func (r *ServiceDeploymentReconciler) reconcilePDB(ctx context.Context, sd *idpv1alpha1.ServiceDeployment, labels map[string]string, c *components) error {
	if !PDBWanted(sd.Spec.Replicas) {
		stale := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: sd.Name, Namespace: sd.Name}}
		if err := r.Delete(ctx, stale); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		reason, message := PDBSkipReason(sd.Spec.Replicas)
		c.skip(CondPDBReady, reason, message)
		return nil
	}

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
		return err
	}
	c.ready(CondPDBReady, "Protected", fmt.Sprintf("maxUnavailable=1 across %d replicas", sd.Spec.Replicas))
	return nil
}

// reconcileNetworkPolicies denies ingress by default and re-admits the two
// sources that are always legitimate: the namespace itself, and whatever the
// platform team declared as its ingress path.
func (r *ServiceDeploymentReconciler) reconcileNetworkPolicies(ctx context.Context, sd *idpv1alpha1.ServiceDeployment, labels map[string]string, c *components) error {
	apply := func(name string, spec networkingv1.NetworkPolicySpec) error {
		np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sd.Name}}
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
			np.Labels = labels
			np.Spec = spec
			return nil
		})
		return err
	}

	if err := apply(NetworkPolicyDenyIngress, DenyIngressSpec()); err != nil {
		return err
	}
	if err := apply(NetworkPolicyAllowSameNS, SameNamespaceIngressSpec()); err != nil {
		return err
	}

	platform, configured := r.Network.PlatformIngressSpec()
	if !configured {
		// Drop a policy left over from an earlier configuration.
		stale := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: NetworkPolicyAllowPlatform, Namespace: sd.Name}}
		if err := r.Delete(ctx, stale); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		// Not a skip: the deny is in force and correct. It does mean nothing
		// outside the namespace can reach this service, which is worth saying
		// out loud, because for an externally exposed service it is an outage
		// waiting to be reported as a bug.
		c.ready(CondNetworkPolicyReady, "DefaultDenyIngress",
			"ingress denied except from within the namespace; no platform ingress sources are configured on the operator")
		return nil
	}
	if err := apply(NetworkPolicyAllowPlatform, platform); err != nil {
		return err
	}
	sources := append(append([]string{}, r.Network.AllowIngressFromNamespaces...), r.Network.AllowIngressFromCIDRs...)
	c.ready(CondNetworkPolicyReady, "DefaultDenyIngress",
		"ingress denied except from within the namespace and "+strings.Join(sources, ", "))
	return nil
}

// reconcileAdmissionPolicy installs the Kyverno baseline. Kyverno is not a
// dependency of this operator: without it the rest of the golden path still
// provisions and the condition says the guardrail is missing.
func (r *ServiceDeploymentReconciler) reconcileAdmissionPolicy(ctx context.Context, sd *idpv1alpha1.ServiceDeployment, labels map[string]string, c *components) error {
	if !r.kyvernoAvailable() {
		// Kyverno's CRDs can be installed after this operator starts, and
		// nothing in the watch set fires when they are — hence the requeue.
		// (The signing identity needs no such treatment: it arrives as
		// operator flags, and the restart carrying it reconciles everything.)
		c.skipEnvironmental(CondAdmissionPolicyReady, "KyvernoNotInstalled",
			"kyverno.io/v1 Policy is not registered in this cluster; no admission guardrails were installed")
		return nil
	}

	pol := &unstructured.Unstructured{}
	pol.SetGroupVersionKind(KyvernoPolicyGVK)
	pol.SetName(KyvernoPolicyName)
	pol.SetNamespace(sd.Name)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, pol, func() error {
		pol.SetLabels(labels)
		return unstructured.SetNestedMap(pol.Object, KyvernoPolicySpec(r.Policy), "spec")
	}); err != nil {
		return err
	}

	message := "enforcing " + strings.Join(KyvernoRuleNames(r.Policy), ", ")
	if !r.Policy.ImageVerificationConfigured() {
		message += "; image signatures are NOT verified (no signing identity configured on the operator)"
	}
	c.ready(CondAdmissionPolicyReady, "Provisioned", message)
	return nil
}

// reconcileApplication hands the service to ArgoCD.
func (r *ServiceDeploymentReconciler) reconcileApplication(ctx context.Context, sd *idpv1alpha1.ServiceDeployment, labels map[string]string, c *components) error {
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
		return err
	}
	c.ready(CondDeliveryReady, "Provisioned",
		fmt.Sprintf("argocd syncing %s (%s) from %s", sd.Spec.GitOps.Path, sd.Spec.GitOps.TargetRevision, sd.Spec.GitOps.RepoURL))
	return nil
}

// kyvernoAvailable reports whether this cluster knows what a Kyverno Policy is.
func (r *ServiceDeploymentReconciler) kyvernoAvailable() bool {
	m := r.RESTMapper()
	if m == nil {
		return false
	}
	_, err := m.RESTMapping(KyvernoPolicyGVK.GroupKind(), KyvernoPolicyGVK.Version)
	return err == nil
}

// finalize tears down what the golden path provisioned. Deletion order:
// Application first (it lives outside the namespace cascade), then the
// namespace, which cascades to everything inside it. Every delete is
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

// fail records an Error status against the component that broke and returns
// the error so controller-runtime requeues with backoff.
func (r *ServiceDeploymentReconciler) fail(ctx context.Context, sd *idpv1alpha1.ServiceDeployment, c *components, condType, component string, err error) (ctrl.Result, error) {
	msg := fmt.Sprintf("%s: %v", component, err)
	conds := append(c.conds,
		newCondition(sd, condType, metav1.ConditionFalse, PhaseError, msg),
		newCondition(sd, CondProvisioned, metav1.ConditionFalse, PhaseError, msg))
	if serr := r.setStatus(ctx, sd, PhaseError, conds...); serr != nil {
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
// namespace, the cross-namespace ConfigMap/PDB/policies/Application).
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
		Watches(&policyv1.PodDisruptionBudget{}, handler.EnqueueRequestsFromMapFunc(mapProvisioned)).
		Watches(&networkingv1.NetworkPolicy{}, handler.EnqueueRequestsFromMapFunc(mapProvisioned))

	// The Application and Kyverno watches need their CRDs. On clusters
	// without them, skip the watch instead of failing manager start — drift
	// on those objects won't re-reconcile, and the reconciler already
	// reports the missing guardrail in status.
	for _, gvk := range []schema.GroupVersionKind{ApplicationGVK, KyvernoPolicyGVK} {
		if _, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
			ctrl.Log.WithName("setup").Info(
				"CRD not found; drift on this kind will not trigger reconciles", "kind", gvk.String())
			continue
		}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		b = b.Watches(u, handler.EnqueueRequestsFromMapFunc(mapProvisioned))
	}
	return b.Complete(r)
}
