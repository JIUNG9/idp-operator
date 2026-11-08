package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
	"github.com/JIUNG9/idp-operator/internal/budget"
)

const (
	// ArgoCDNamespace is where Application objects are written unless the
	// reconciler is configured with another one (-argocd-namespace).
	ArgoCDNamespace = "argocd"

	// ReasonProjectNotAllowed is the condition reason when spec.gitops.project
	// is not on the operator's allow-list.
	ReasonProjectNotAllowed = "ProjectNotAllowed"

	// Finalizer guards garbage collection of the children ownerReferences
	// can't reach: the cluster-scoped namespace and the Application over in
	// the argocd namespace. OwnerRefs are illegal across those boundaries.
	Finalizer = "idp.jiung9.github.io/cleanup"

	// EnvironmentalRetryInterval is how long the reconciler waits before
	// looking again when the cluster could not yet provide something the
	// golden path wants. Nothing in the watch set fires when a CRD is
	// installed, so this is the only way back.
	EnvironmentalRetryInterval = 5 * time.Minute

	// BudgetReevaluationInterval is how often a budget-gated service is
	// looked at again. A budget reading has a shelf life; nothing else in
	// the watch set changes when a service starts burning.
	BudgetReevaluationInterval = 5 * time.Minute

	// LooseningStreak is how many consecutive readings must agree before the
	// gate relaxes. Tightening never waits.
	LooseningStreak int32 = 3

	// StaleVerdictGraceMultiple bounds how long a verdict may stand on a
	// reading the operator can no longer refresh, as a multiple of the
	// re-evaluation interval.
	//
	// Holding the last verdict through a brief outage is right: a Prometheus
	// restart is not a reliability event, and turning every scrape blip into
	// a delivery freeze would teach people to rip the gate out. Holding it
	// indefinitely is wrong for a reason that matters more: monitoring tends
	// to fail *with* the things it monitors, so an unbounded hold keeps
	// unattended delivery running exactly through the incident where nobody
	// can see the budget. After the grace period, an unreadable budget is
	// treated as what it is — no evidence — and the gate tightens.
	StaleVerdictGraceMultiple = 3

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
	CondWorkloadsSelected    = "WorkloadsSelected"
	CondNetworkPolicyReady   = "NetworkPolicyReady"
	CondAdmissionPolicyReady = "AdmissionPolicyReady"
	CondDeliveryReady        = "DeliveryReady"
	CondBudgetGateReady      = "BudgetGateReady"
)

// ApplicationGVK identifies the ArgoCD Application resource; it is handled
// as unstructured so the operator has no ArgoCD Go dependency.
var ApplicationGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}

// RolloutListGVK is the Argo Rollouts list kind, read as unstructured for the
// same reason. The CRD is optional.
var RolloutListGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "RolloutList"}

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
	// BurnSource reads how much error budget a service has left. Nil means
	// delivery is not budget-gated at all.
	BurnSource budget.Source
	// ReevaluationInterval is how stale a budget reading may get before it
	// is taken again. Zero means BudgetReevaluationInterval.
	ReevaluationInterval time.Duration
	// AllowedProjects lists the ArgoCD AppProjects a ServiceDeployment may
	// name. The operator writes Applications with its own privileges, so a
	// team that could name any project could deploy with the permissions of
	// the most privileged one. Empty means no project is allowed.
	AllowedProjects []string
	// ArgoCDNamespace is where Applications are written. Empty means the
	// ArgoCDNamespace constant.
	ArgoCDNamespace string
}

func (r *ServiceDeploymentReconciler) argocdNamespace() string {
	if r.ArgoCDNamespace != "" {
		return r.ArgoCDNamespace
	}
	return ArgoCDNamespace
}

// checkProject rejects a project that is not on the allow-list. The project
// is compared after defaulting, so an omitted project ("default") is held to
// the same rule as one written out.
func (r *ServiceDeploymentReconciler) checkProject(project string) error {
	for _, p := range r.AllowedProjects {
		if p == project {
			return nil
		}
	}
	if len(r.AllowedProjects) == 0 {
		return fmt.Errorf("spec.gitops.project %q is not allowed: the operator has no allowed projects (set -allowed-projects)", project)
	}
	return fmt.Errorf("spec.gitops.project %q is not allowed; allowed projects: %s", project, strings.Join(r.AllowedProjects, ", "))
}

func (r *ServiceDeploymentReconciler) reevalInterval() time.Duration {
	if r.ReevaluationInterval > 0 {
		return r.ReevaluationInterval
	}
	return BudgetReevaluationInterval
}

// staleVerdictGrace is how long a verdict may go on standing after the burn
// source stopped answering. Past it, the verdict is no longer evidence of
// anything and the gate tightens.
func (r *ServiceDeploymentReconciler) staleVerdictGrace() time.Duration {
	return StaleVerdictGraceMultiple * r.reevalInterval()
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
	sd           *idpv1alpha1.ServiceDeployment
	conds        []metav1.Condition
	skipped      []string
	degraded     []string
	requeueAfter time.Duration
}

// requeue asks for another look, keeping the soonest request.
func (c *components) requeue(d time.Duration) {
	if c.requeueAfter == 0 || d < c.requeueAfter {
		c.requeueAfter = d
	}
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
	c.requeue(EnvironmentalRetryInterval)
}

// degrade records something that was provisioned but is not fully operative
// — the budget gate holding delivery back, say. Distinct from a skip, which
// means nothing was provisioned at all, and worth distinguishing: "you have
// no PDB" and "your error budget stopped your deploys" are not the same news.
func (c *components) degrade(condType, reason, message string) {
	c.conds = append(c.conds, newCondition(c.sd, condType, metav1.ConditionFalse, reason, message))
	c.degraded = append(c.degraded, condType)
}

func (c *components) aggregate() metav1.Condition {
	var parts []string
	if len(c.skipped) > 0 {
		parts = append(parts, "not provisioned: "+strings.Join(c.skipped, ", "))
	}
	if len(c.degraded) > 0 {
		parts = append(parts, "degraded: "+strings.Join(c.degraded, ", "))
	}
	if len(parts) == 0 {
		return newCondition(c.sd, CondProvisioned, metav1.ConditionTrue,
			"Reconciled", "golden path provisioned")
	}
	// Still True: everything this spec and this cluster allow was provisioned.
	// The reason names what was not, so degradation is never silent.
	reason := "ReconciledWithSkips"
	if len(c.degraded) > 0 {
		reason = "Degraded"
	}
	return newCondition(c.sd, CondProvisioned, metav1.ConditionTrue, reason,
		"golden path provisioned; "+strings.Join(parts, "; "))
}

func (c *components) all() []metav1.Condition {
	return append(c.conds, c.aggregate())
}

// +kubebuilder:rbac:groups=idp.jiung9.github.io,resources=servicedeployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=idp.jiung9.github.io,resources=servicedeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=idp.jiung9.github.io,resources=servicedeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list
// +kubebuilder:rbac:groups=argoproj.io,resources=rollouts,verbs=get;list
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
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
	if err := r.checkProject(sd.Spec.GitOps.Project); err != nil {
		return ctrl.Result{}, r.setStatus(ctx, &sd, PhaseInvalid,
			newCondition(&sd, CondProvisioned, metav1.ConditionFalse, ReasonProjectNotAllowed, err.Error()))
	}
	labels := Labels(&sd)
	c := &components{sd: &sd}

	// The golden path, in order. Each step records its own condition; the
	// first failure stops the path, because everything after it would be
	// provisioned into a namespace that may not exist.
	// The gate's verdict decides the Application's sync policy, so it is
	// evaluated before delivery is written.
	verdict := budget.Allow
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
		{CondBudgetGateReady, "budget gate", func() error {
			v, err := r.reconcileBudgetGate(ctx, &sd, c)
			verdict = v
			return err
		}},
		{CondDeliveryReady, "argocd application", func() error { return r.reconcileApplication(ctx, &sd, labels, c, verdict) }},
		{CondWorkloadsSelected, "workload labels", func() error { return r.reconcileWorkloadSelection(ctx, &sd, c) }},
	}
	for _, s := range steps {
		if err := s.run(); err != nil {
			return r.fail(ctx, &sd, c, s.cond, s.name, err)
		}
	}

	logger.Info("golden path reconciled", "service", sd.Name, "team", sd.Spec.Team,
		"skipped", strings.Join(c.skipped, ","))

	return ctrl.Result{RequeueAfter: c.requeueAfter}, r.setStatus(ctx, &sd, PhaseProvisioned, c.all()...)
}

// ReasonNotOwned is the condition reason when the operator refuses to touch
// an object it did not create.
const ReasonNotOwned = "NotOwned"

// NotOwnedError reports that an object already exists under a name the golden
// path wants, but does not carry this ServiceDeployment's ownership labels.
type NotOwnedError struct{ msg string }

func (e *NotOwnedError) Error() string { return e.msg }

// checkOwned returns a NotOwnedError unless obj carries managed-by=idp-operator
// and an owner-namespace that is empty (written before the label existed) or
// equal to the ServiceDeployment's namespace. This is the one takeover rule:
// the operator never adopts, overwrites or deletes anything outside it.
func checkOwned(sd *idpv1alpha1.ServiceDeployment, obj client.Object) error {
	l := obj.GetLabels()
	what := fmt.Sprintf("%s %s/%s", objectKind(obj), obj.GetNamespace(), obj.GetName())
	if l[LabelManagedBy] != ManagedByValue {
		return &NotOwnedError{fmt.Sprintf("%s exists and is not managed by idp-operator; refusing to overwrite it", what)}
	}
	if owner := l[LabelOwnerNamespace]; owner != "" && owner != sd.Namespace {
		return &NotOwnedError{fmt.Sprintf("%s is owned by a ServiceDeployment in %q; refusing to overwrite it", what, owner)}
	}
	return nil
}

func objectKind(obj client.Object) string {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return u.GetKind()
	}
	switch obj.(type) {
	case *corev1.ConfigMap:
		return "ConfigMap"
	case *policyv1.PodDisruptionBudget:
		return "PodDisruptionBudget"
	case *networkingv1.NetworkPolicy:
		return "NetworkPolicy"
	}
	return fmt.Sprintf("%T", obj)
}

// getExisting reads the live object through the uncached reader. The manager
// cache is filtered to this operator's labels, so it can never show an object
// somebody else created, which is exactly the case that has to be detected.
// A missing object, or a kind the cluster does not serve, reads as absent.
func (r *ServiceDeploymentReconciler) getExisting(ctx context.Context, obj client.Object) (bool, error) {
	err := r.reader().Get(ctx, client.ObjectKeyFromObject(obj), obj)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err):
		return false, nil
	}
	return false, err
}

// applyManaged creates or updates an object the golden path owns, and refuses
// when the name is already taken by an object it does not own.
//
// controllerutil.CreateOrUpdate reads through the filtered manager cache, so
// an object with other labels reads as absent there and the create would lose
// to it with AlreadyExists. The uncached read happens first and decides: an
// existing object without our labels is reported as NotOwnedError, never
// overwritten.
func (r *ServiceDeploymentReconciler) applyManaged(ctx context.Context, sd *idpv1alpha1.ServiceDeployment, obj client.Object, mutate controllerutil.MutateFn) error {
	probe := obj.DeepCopyObject().(client.Object)
	if found, err := r.getExisting(ctx, probe); err != nil {
		return err
	} else if found {
		if err := checkOwned(sd, probe); err != nil {
			return err
		}
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, mutate)
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	// Ours by label but not in the cache, or created between the read and the
	// create. Re-check before writing.
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return err
	}
	if err := checkOwned(sd, obj); err != nil {
		return err
	}
	if err := mutate(); err != nil {
		return err
	}
	return r.Update(ctx, obj)
}

// deleteOwned deletes obj only when it exists and carries our ownership
// labels. An object that is absent is already done; one we do not own is left
// alone and reported, so a stale-object cleanup can never remove something a
// team created under the same name.
func (r *ServiceDeploymentReconciler) deleteOwned(ctx context.Context, sd *idpv1alpha1.ServiceDeployment, obj client.Object) error {
	found, err := r.getExisting(ctx, obj)
	if err != nil || !found {
		return err
	}
	if err := checkOwned(sd, obj); err != nil {
		return err
	}
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
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
			return &NotOwnedError{fmt.Sprintf("namespace %q exists and is not managed by idp-operator", sd.Name)}
		}
		if owner := existing.Labels[LabelOwnerNamespace]; owner != "" && owner != sd.Namespace {
			return &NotOwnedError{fmt.Sprintf("namespace %q is owned by a ServiceDeployment in %q", sd.Name, owner)}
		}
	case !apierrors.IsNotFound(err):
		return err
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: sd.Name}}
	if err := r.applyManaged(ctx, sd, ns, func() error {
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
	if err := r.applyManaged(ctx, sd, cm, func() error {
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
	replicas := sd.Spec.ReplicasOrDefault()
	if !PDBWanted(replicas) {
		stale := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: sd.Name, Namespace: sd.Name}}
		if err := r.deleteOwned(ctx, sd, stale); err != nil {
			return err
		}
		reason, message := PDBSkipReason(replicas)
		c.skip(CondPDBReady, reason, message)
		return nil
	}

	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: sd.Name, Namespace: sd.Name}}
	if err := r.applyManaged(ctx, sd, pdb, func() error {
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
	c.ready(CondPDBReady, "Protected", fmt.Sprintf("maxUnavailable=1 across %d replicas", replicas))
	return nil
}

// reconcileWorkloadSelection checks that the workloads delivered into the
// service namespace carry the label the PDB selects on. The workload comes
// from the team's GitOps repo, so a missing label leaves a PDB that selects
// nothing. Argo Rollouts are read when their CRD is installed. Bare Pods (no controller owner) are checked on their own labels;
// pods a controller owns are covered through its template. It only reads: it never edits a workload it does not own. With no
// PDB there is nothing to select, so no condition is reported.
func (r *ServiceDeploymentReconciler) reconcileWorkloadSelection(ctx context.Context, sd *idpv1alpha1.ServiceDeployment, c *components) error {
	if !PDBWanted(sd.Spec.ReplicasOrDefault()) {
		return nil
	}
	// Uncached: the manager cache is filtered to this operator's own labels,
	// which a team's workloads do not carry.
	var deployments appsv1.DeploymentList
	if err := r.reader().List(ctx, &deployments, client.InNamespace(sd.Name)); err != nil {
		return err
	}
	var statefulSets appsv1.StatefulSetList
	if err := r.reader().List(ctx, &statefulSets, client.InNamespace(sd.Name)); err != nil {
		return err
	}
	var pods corev1.PodList
	if err := r.reader().List(ctx, &pods, client.InNamespace(sd.Name)); err != nil {
		return err
	}
	rollouts, err := r.listRollouts(ctx, sd.Name)
	if err != nil {
		return err
	}
	var workloads []Workload
	workloads = append(workloads, rollouts...)
	for _, d := range deployments.Items {
		workloads = append(workloads, Workload{"Deployment", d.Name, d.Spec.Template.Labels})
	}
	for _, s := range statefulSets.Items {
		workloads = append(workloads, Workload{"StatefulSet", s.Name, s.Spec.Template.Labels})
	}
	for _, p := range pods.Items {
		// A pod with a controller is covered through its owner's template.
		if metav1.GetControllerOf(&p) == nil {
			workloads = append(workloads, Workload{"Pod", p.Name, p.Labels})
		}
	}
	if len(workloads) == 0 {
		// ArgoCD may not have synced yet, and no watch fires when it does.
		c.skipEnvironmental(CondWorkloadsSelected, "NoWorkloads",
			"no Deployment, StatefulSet, Rollout or bare Pod in the namespace yet; the PDB selects nothing until one is delivered")
		return nil
	}
	if bad := UnselectedWorkloads(sd.Name, workloads); len(bad) > 0 {
		c.degrade(CondWorkloadsSelected, "LabelMissing", fmt.Sprintf(
			"pod template lacks %s=%s, so the PDB does not select: %s", LabelName, sd.Name, strings.Join(bad, ", ")))
		c.requeue(EnvironmentalRetryInterval)
		return nil
	}
	c.ready(CondWorkloadsSelected, "Selected", fmt.Sprintf("%d workload(s) carry %s=%s", len(workloads), LabelName, sd.Name))
	return nil
}

// listRollouts returns the Argo Rollouts in the namespace as workloads, read
// from spec.template.metadata.labels. The Rollout CRD is optional: when it is
// not installed the API server answers NotFound (or the mapper reports no
// match), and that means no Rollouts, not a failure.
func (r *ServiceDeploymentReconciler) listRollouts(ctx context.Context, namespace string) ([]Workload, error) {
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(RolloutListGVK)
	if err := r.reader().List(ctx, &list, client.InNamespace(namespace)); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Workload
	for _, item := range list.Items {
		labels, _, err := unstructured.NestedStringMap(item.Object, "spec", "template", "metadata", "labels")
		if err != nil {
			// A malformed template has no usable labels; report it as unlabelled.
			labels = nil
		}
		out = append(out, Workload{"Rollout", item.GetName(), labels})
	}
	return out, nil
}

// reconcileNetworkPolicies denies ingress by default and re-admits the two
// sources that are always legitimate: the namespace itself, and whatever the
// platform team declared as its ingress path.
func (r *ServiceDeploymentReconciler) reconcileNetworkPolicies(ctx context.Context, sd *idpv1alpha1.ServiceDeployment, labels map[string]string, c *components) error {
	apply := func(name string, spec networkingv1.NetworkPolicySpec) error {
		np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sd.Name}}
		return r.applyManaged(ctx, sd, np, func() error {
			np.Labels = labels
			np.Spec = spec
			return nil
		})
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
		if err := r.deleteOwned(ctx, sd, stale); err != nil {
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
	if err := r.applyManaged(ctx, sd, pol, func() error {
		pol.SetLabels(labels)
		return unstructured.SetNestedMap(pol.Object, KyvernoPolicySpec(r.Policy), "spec")
	}); err != nil {
		return err
	}

	// "Provisioned", not "Enforced": this operator wrote a policy the API
	// server accepted. Whether it is enforced depends on Kyverno's admission
	// webhook being healthy, which is Kyverno's business and is not checked
	// here — a broken Kyverno would admit privileged pods while this
	// condition still reads green.
	message := "policy accepted, declaring " + strings.Join(KyvernoRuleNames(r.Policy), ", ") +
		" (enforcement depends on Kyverno's webhook, which this operator does not verify)"
	if !r.Policy.ImageVerificationConfigured() {
		message += "; image signatures are NOT verified (no signing identity configured on the operator)"
	} else if !r.Policy.RequireImageDigests {
		message += "; images verified by tag, so a tag moved after admission is not re-checked (-require-image-digests closes this)"
	}
	c.ready(CondAdmissionPolicyReady, "Provisioned", message)
	return nil
}

// reconcileBudgetGate reads the service's error budget and decides whether
// delivery may proceed unattended. Returns the verdict in force — which is
// not always the verdict this reading argued for, because loosening waits.
func (r *ServiceDeploymentReconciler) reconcileBudgetGate(ctx context.Context, sd *idpv1alpha1.ServiceDeployment, c *components) (budget.Verdict, error) {
	if r.BurnSource == nil {
		// Flag-driven, like the signing identity: it cannot appear without a
		// restart, so there is nothing to come back for.
		c.skip(CondBudgetGateReady, "BurnSourceNotConfigured",
			"no burn source configured on the operator; delivery is not budget-gated")
		return budget.Allow, nil
	}
	interval := r.reevalInterval()
	prev := sd.Status.Budget

	// A verdict is only re-derived when its reading has gone stale.
	//
	// This is load-bearing, not an optimisation. Writing status triggers a
	// watch event, which triggers another reconcile, so a reconcile-counting
	// anti-flapping rule burns its whole streak in milliseconds and relaxes
	// the gate almost the moment the budget looks better — the exact
	// behaviour the rule exists to prevent. Tying re-evaluation to elapsed
	// time makes "three consecutive readings" mean three points in time.
	// It also keeps the operator from hammering Prometheus once per event.
	if prev != nil && prev.Verdict != "" && prev.LastEvaluationTime != nil {
		// A negative age means the stored timestamp is in the future — clock
		// skew, or a hand-edited status. Either way it is not something to
		// hold a verdict on, so fall through and take a fresh reading.
		if age := time.Since(prev.LastEvaluationTime.Time); age >= 0 && age < interval {
			c.requeue(interval - age)
			held := budget.Verdict(prev.Verdict)
			c.budgetCondition(held, fmt.Sprintf(
				"standing verdict %q from a reading %s ago (%d%% of the budget left, via %s)",
				held, age.Round(time.Second), prev.RemainingPercent, prev.Source))
			return held, nil
		}
	}
	// Nothing else in the watch set changes when a service starts burning.
	c.requeue(interval)
	target, err := strconv.ParseFloat(sd.Spec.SLO.AvailabilityTarget, 64)
	if err != nil {
		return budget.Allow, fmt.Errorf("availability target %q: %w", sd.Spec.SLO.AvailabilityTarget, err)
	}

	remaining, err := r.BurnSource.Remaining(ctx, budget.Service{
		Namespace:          sd.Name,
		Name:               sd.Name,
		AvailabilityTarget: target,
		Window:             ErrorBudgetWindow,
	})
	if err != nil {
		// Fail closed, with a bounded grace period. A source that cannot be
		// read is not evidence of a healthy budget. The last verdict stands
		// while it is still recent — a Prometheus restart is not a
		// reliability event — but only until the grace period runs out,
		// because monitoring fails alongside the things it monitors and an
		// unbounded hold would keep delivery unattended through exactly the
		// incident nobody can see.
		held, expired := budget.RequireHuman, true
		if prev != nil && prev.Verdict != "" {
			held = budget.Verdict(prev.Verdict)
			if t := prev.LastEvaluationTime; t != nil {
				age := time.Since(t.Time)
				expired = age < 0 || age > r.staleVerdictGrace()
			}
		}
		if expired && budget.Severity(held) < budget.Severity(budget.RequireHuman) {
			// Tighten to require-human, never loosen: a block that cannot be
			// re-measured stays a block.
			held = budget.RequireHuman
		}

		// Make the verdict in force durable. Without this, status keeps
		// saying "allow" while the Application has already lost automated
		// sync — the status lies — and worse, the first healthy reading
		// after recovery compares against that stale "allow", finds no
		// loosening to do, and walks straight back to unattended delivery
		// without serving the three-reading rule at all.
		sd.Status.Budget = carryForward(prev, held)

		detail := fmt.Sprintf("holding %q", held)
		if expired {
			detail = fmt.Sprintf("no reading within %s, so tightening to %q", r.staleVerdictGrace(), held)
		}
		c.degrade(CondBudgetGateReady, "BurnSourceUnavailable", fmt.Sprintf(
			"could not read the error budget from %s (%v); %s rather than assuming the budget is healthy",
			r.BurnSource.Describe(), err, detail))
		// Keep asking: the source coming back is the only thing that ends this.
		c.requeue(interval)
		return held, nil
	}

	decision := budget.Evaluate(budget.Input{
		Tier:       sd.Spec.TierOrDefault(),
		Production: budget.IsProduction(sd.Spec.Environment),
		Remaining:  remaining,
	})
	inForce, streak := resolveVerdict(prev, decision.Verdict)

	now := metav1.Now()
	sd.Status.Budget = &idpv1alpha1.BudgetStatus{
		Verdict:            string(inForce),
		RemainingPercent:   int32(math.Round(remaining * 100)),
		Streak:             streak,
		Source:             r.BurnSource.Describe(),
		Environment:        sd.Spec.Environment,
		Tier:               sd.Spec.TierOrDefault(),
		LastEvaluationTime: &now,
	}

	// The message says what was read and where from, never that the budget
	// is healthy: the reading is a moment, and the timestamp for it lives in
	// status.budget where a machine can use it.
	message := fmt.Sprintf("%s (read from %s)", decision.Reason, r.BurnSource.Describe())
	if inForce != decision.Verdict {
		message += fmt.Sprintf("; holding %q for another %d reading(s) before relaxing to %q",
			inForce, LooseningStreak-streak, decision.Verdict)
	}
	c.budgetCondition(inForce, message)
	return inForce, nil
}

// budgetCondition records the gate's state: ready while delivery is still
// automatic, degraded once the budget has taken that away.
func (c *components) budgetCondition(verdict budget.Verdict, message string) {
	if verdict == budget.Allow {
		c.ready(CondBudgetGateReady, "AutomatedDeliveryAllowed", message)
		return
	}
	reason := "AutomatedDeliveryStopped"
	if verdict == budget.Block {
		reason = "ErrorBudgetExhausted"
	}
	c.degrade(CondBudgetGateReady, reason, message)
}

// carryForward records a verdict reached without a fresh reading.
//
// The measurement fields keep whatever the last real reading said, and
// LastEvaluationTime is deliberately not restamped: nothing here measured
// anything, and pretending otherwise would reset the staleness clock the
// grace period depends on.
func carryForward(prev *idpv1alpha1.BudgetStatus, held budget.Verdict) *idpv1alpha1.BudgetStatus {
	out := &idpv1alpha1.BudgetStatus{Verdict: string(held)}
	if prev != nil {
		copied := *prev
		out = &copied
		out.Verdict = string(held)
		if prev.Verdict != string(held) {
			// The gate moved, so any streak toward a looser verdict is void.
			out.Streak = 0
		}
	}
	return out
}

// resolveVerdict applies the anti-flapping rule: a tighter verdict takes
// effect at once, a looser one only after LooseningStreak consecutive
// readings agree. The streak lives in status, so an operator restart does
// not hand a service back its automated sync for free.
func resolveVerdict(prev *idpv1alpha1.BudgetStatus, raw budget.Verdict) (budget.Verdict, int32) {
	if prev == nil || prev.Verdict == "" {
		return raw, 0
	}
	current := budget.Verdict(prev.Verdict)
	if budget.Severity(raw) >= budget.Severity(current) {
		return raw, 0
	}
	if streak := prev.Streak + 1; streak < LooseningStreak {
		return current, streak
	}
	return raw, 0
}

// syncPolicyFor turns a verdict into ArgoCD's delivery behaviour.
func syncPolicyFor(v budget.Verdict) map[string]any {
	if v == budget.Allow {
		return map[string]any{"automated": map[string]any{"prune": true, "selfHeal": true}}
	}
	// Removing `automated` is the whole of ArgoCD's vocabulary here: a human
	// must sync deliberately. require-human and block produce the same
	// mechanism, and the status is what says which situation you are in.
	return map[string]any{}
}

// reconcileApplication hands the service to ArgoCD.
func (r *ServiceDeploymentReconciler) reconcileApplication(ctx context.Context, sd *idpv1alpha1.ServiceDeployment, labels map[string]string, c *components, verdict budget.Verdict) error {
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(ApplicationGVK)
	app.SetName(sd.Name)
	app.SetNamespace(r.argocdNamespace())
	if err := r.applyManaged(ctx, sd, app, func() error {
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
			"syncPolicy": syncPolicyFor(verdict),
		}, "spec")
	}); err != nil {
		return err
	}
	mode := "automated sync"
	if verdict != budget.Allow {
		mode = "manual sync (" + string(verdict) + ")"
	}
	c.ready(CondDeliveryReady, "Provisioned", fmt.Sprintf("argocd %s of %s (%s) from %s",
		mode, sd.Spec.GitOps.Path, sd.Spec.GitOps.TargetRevision, sd.Spec.GitOps.RepoURL))
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
// Application first (it lives outside the namespace cascade), then, only
// when spec.deletionPolicy is Delete, the namespace, which cascades to
// everything inside it. Every delete is
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
	app.SetNamespace(r.argocdNamespace())
	switch err := r.deleteOwned(ctx, sd, app); {
	case err == nil:
	case errors.As(err, new(*NotOwnedError)):
		// An Application with our name that we did not create: leave it, as
		// with a namespace that is not ours, and let teardown finish.
		log.FromContext(ctx).Info("skipping Application on teardown: not owned by this ServiceDeployment",
			"application", sd.Name)
	default:
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
	case sd.Spec.RetainsNamespace():
		// spec.deletionPolicy is Retain (the default): the namespace holds
		// the team's PVCs, so deleting the CR must not delete the data.
		log.FromContext(ctx).Info("retaining namespace on teardown: spec.deletionPolicy is Retain",
			"namespace", sd.Name)
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
	reason := PhaseError
	if errors.As(err, new(*NotOwnedError)) {
		reason = ReasonNotOwned
	}
	conds := append(c.conds,
		newCondition(sd, condType, metav1.ConditionFalse, reason, msg),
		newCondition(sd, CondProvisioned, metav1.ConditionFalse, reason, msg))
	if serr := r.setStatus(ctx, sd, PhaseError, conds...); serr != nil {
		return ctrl.Result{}, serr
	}
	// Wrap rather than flatten: whether this was RBAC, discovery, or a
	// conflict is the first thing anyone reading the logs wants to know,
	// and errors.Is/As stop working the moment it becomes a string.
	return ctrl.Result{}, fmt.Errorf("%s: %w", component, err)
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
		// Carry the gate's memory: on paths that did not evaluate it, sd
		// still holds what was read from the API, so this preserves it.
		latest.Status.Budget = sd.Status.Budget
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
