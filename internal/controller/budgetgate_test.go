package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	idpv1alpha1 "github.com/JIUNG9/idp-operator/api/v1alpha1"
	"github.com/JIUNG9/idp-operator/internal/budget"
)

// brokenSource stands in for a Prometheus that is down, misconfigured, or
// returning something nobody can act on.
type brokenSource struct{}

func (brokenSource) Remaining(context.Context, budget.Service) (float64, error) {
	return 0, errors.New("connection refused")
}
func (brokenSource) Describe() string { return "broken" }

// automatedSync reports whether ArgoCD was told to sync this service by itself.
func automatedSync(t *testing.T, c client.Client) bool {
	t.Helper()
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(ApplicationGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "orders-api", Namespace: ArgoCDNamespace}, app); err != nil {
		t.Fatalf("application: %v", err)
	}
	_, found, err := unstructured.NestedMap(app.Object, "spec", "syncPolicy", "automated")
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// withBudget wires a burn source and makes every reconcile take a fresh
// reading, so tests that call Reconcile by hand control the clock.
// TestBudgetGateHoldsAStandingVerdict covers the staleness path itself.
func withBudget(source budget.Source) func(*ServiceDeploymentReconciler) {
	return func(r *ServiceDeploymentReconciler) {
		r.BurnSource = source
		r.ReevaluationInterval = time.Nanosecond
	}
}

func budgetCond(t *testing.T, c client.Client, sd *idpv1alpha1.ServiceDeployment) *metav1.Condition {
	t.Helper()
	got := getSD(t, c, sd.Name, sd.Namespace)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, CondBudgetGateReady)
	if cond == nil {
		t.Fatalf("BudgetGateReady missing: %+v", got.Status.Conditions)
	}
	return cond
}

func TestBudgetGateNotConfiguredLeavesDeliveryAlone(t *testing.T) {
	sd := valid()
	// Kyverno present, so the only thing that could ask for a requeue here
	// is the budget gate; a labelled workload keeps WorkloadsSelected quiet.
	c, r, req := newFixtureOpts(t, sd, kyvernoMapper(), nil, deploymentWithPodLabels("api", map[string]string{LabelName: "orders-api"}))

	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !automatedSync(t, c) {
		t.Fatal("without a burn source there is nothing to gate on; sync stays automated")
	}
	// Flag-driven: a source cannot appear without a restart, so asking to be
	// woken up again would be pointless work forever.
	if res.RequeueAfter != 0 {
		t.Fatalf("no requeue expected without a burn source, got %v", res.RequeueAfter)
	}
	cond := budgetCond(t, c, sd)
	if cond.Status != metav1.ConditionFalse || cond.Reason != "BurnSourceNotConfigured" {
		t.Fatalf("the absence of a gate must be stated, not implied: %+v", cond)
	}
}

func TestBudgetGateAllowsHealthyService(t *testing.T) {
	sd := valid()
	c, r, req := newFixtureOpts(t, sd, nil, withBudget(budget.StaticSource{Value: 0.8}))

	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !automatedSync(t, c) {
		t.Fatal("a healthy budget must keep Git-to-production automatic")
	}
	if res.RequeueAfter != r.reevalInterval() {
		t.Fatalf("a budget reading has a shelf life; expected re-evaluation, got %v", res.RequeueAfter)
	}
	cond := budgetCond(t, c, sd)
	if cond.Status != metav1.ConditionTrue || !strings.Contains(cond.Message, "static") {
		t.Fatalf("condition must name where the number came from: %+v", cond)
	}

	got := getSD(t, c, sd.Name, sd.Namespace)
	if got.Status.Budget == nil || got.Status.Budget.Verdict != string(budget.Allow) {
		t.Fatalf("verdict not persisted: %+v", got.Status.Budget)
	}
	if got.Status.Budget.RemainingPercent != 80 {
		t.Fatalf("remaining = %d%%, want 80", got.Status.Budget.RemainingPercent)
	}
}

func TestBudgetGateStopsAutomatedSyncWhenBudgetIsLow(t *testing.T) {
	sd := valid() // no environment set, so production
	c, r, req := newFixtureOpts(t, sd, nil, withBudget(budget.StaticSource{Value: 0.1}))

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if automatedSync(t, c) {
		t.Fatal("a production service with 10% budget left must not keep deploying itself")
	}
	cond := budgetCond(t, c, sd)
	if cond.Status != metav1.ConditionFalse || cond.Reason != "AutomatedDeliveryStopped" {
		t.Fatalf("condition wrong: %+v", cond)
	}
	got := getSD(t, c, sd.Name, sd.Namespace)
	// Delivery is still provisioned — it is the automation that stopped, and
	// the aggregate has to say degraded rather than not-provisioned.
	agg := apimeta.FindStatusCondition(got.Status.Conditions, CondProvisioned)
	if agg == nil || agg.Reason != "Degraded" || !strings.Contains(agg.Message, CondBudgetGateReady) {
		t.Fatalf("aggregate must report the gate as degraded: %+v", agg)
	}
	if delivery := apimeta.FindStatusCondition(got.Status.Conditions, CondDeliveryReady); delivery == nil ||
		!strings.Contains(delivery.Message, "manual sync") {
		t.Fatalf("delivery condition should say how it syncs now: %+v", delivery)
	}
}

func TestBudgetGateKeepsDevServicesMoving(t *testing.T) {
	// The deliberate divergence from error-budget-gate: it escalates on a low
	// budget in any environment. A dev service losing automated sync is
	// friction with no risk on the other side of it.
	sd := valid()
	sd.Spec.Environment = "dev"
	c, r, req := newFixtureOpts(t, sd, nil, withBudget(budget.StaticSource{Value: 0.1}))

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !automatedSync(t, c) {
		t.Fatal("a low budget in dev must not stop delivery")
	}
}

func TestBudgetGateBlocksExhaustedTier0(t *testing.T) {
	sd := valid()
	sd.Spec.Tier = ptr(int32(0))
	c, r, req := newFixtureOpts(t, sd, nil, withBudget(budget.StaticSource{Value: 0}))

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if automatedSync(t, c) {
		t.Fatal("an exhausted budget must stop unattended delivery")
	}
	cond := budgetCond(t, c, sd)
	if cond.Reason != "ErrorBudgetExhausted" {
		t.Fatalf("block must be distinguishable from require-human: %+v", cond)
	}
	got := getSD(t, c, sd.Name, sd.Namespace)
	if got.Status.Budget.Verdict != string(budget.Block) {
		t.Fatalf("verdict = %q", got.Status.Budget.Verdict)
	}
}

func TestBudgetGateTightensImmediatelyButLoosensSlowly(t *testing.T) {
	sd := valid()
	c, r, req := newFixtureOpts(t, sd, nil, withBudget(budget.StaticSource{Value: 0.8}))
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if !automatedSync(t, c) {
		t.Fatal("healthy to start with")
	}

	// Budget collapses. Tightening waits for nothing.
	r.BurnSource = budget.StaticSource{Value: 0.05}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if automatedSync(t, c) {
		t.Fatal("tightening must take effect on the first reading, not the third")
	}
	if s := getSD(t, c, sd.Name, sd.Namespace).Status.Budget.Streak; s != 0 {
		t.Fatalf("tightening resets the streak, got %d", s)
	}

	// Budget recovers. Relaxing waits for LooseningStreak agreeing readings,
	// so one good sample does not hand back automation mid-incident.
	r.BurnSource = budget.StaticSource{Value: 0.9}
	for i := int32(1); i < LooseningStreak; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		if automatedSync(t, c) {
			t.Fatalf("relaxed after only %d agreeing reading(s)", i)
		}
		got := getSD(t, c, sd.Name, sd.Namespace)
		if got.Status.Budget.Streak != i {
			t.Fatalf("streak = %d, want %d", got.Status.Budget.Streak, i)
		}
		if cond := budgetCond(t, c, sd); !strings.Contains(cond.Message, "before relaxing") {
			t.Fatalf("a held verdict should say it is being held: %q", cond.Message)
		}
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if !automatedSync(t, c) {
		t.Fatalf("after %d agreeing readings the gate must relax", LooseningStreak)
	}
	if s := getSD(t, c, sd.Name, sd.Namespace).Status.Budget.Streak; s != 0 {
		t.Fatalf("streak should reset once relaxed, got %d", s)
	}
}

func TestBudgetGateSurvivesAnOperatorRestart(t *testing.T) {
	// The streak and verdict live in status precisely so a restart cannot
	// hand a burning service its automation back for free.
	sd := valid()
	c, r, req := newFixtureOpts(t, sd, nil, withBudget(budget.StaticSource{Value: 0.05}))
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if automatedSync(t, c) {
		t.Fatal("expected the gate to be closed")
	}

	// A brand-new reconciler over the same cluster state: no memory but what
	// is in status. The budget now reads healthy again.
	restarted := &ServiceDeploymentReconciler{
		Client: c, BurnSource: budget.StaticSource{Value: 0.95}, ReevaluationInterval: time.Nanosecond,
		AllowedProjects: []string{"default"},
	}
	if _, err := restarted.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if automatedSync(t, c) {
		t.Fatal("a restart must not reset the anti-flapping streak")
	}
	if s := getSD(t, c, sd.Name, sd.Namespace).Status.Budget.Streak; s != 1 {
		t.Fatalf("streak should continue across the restart, got %d", s)
	}
}

func TestBudgetGateFailsClosedWithNoPriorVerdict(t *testing.T) {
	sd := valid()
	c, r, req := newFixtureOpts(t, sd, nil, withBudget(brokenSource{}))

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("an unreadable budget is a degraded state, not a reconcile failure: %v", err)
	}
	if automatedSync(t, c) {
		t.Fatal("a source that cannot be read is not evidence of a healthy budget")
	}
	cond := budgetCond(t, c, sd)
	if cond.Reason != "BurnSourceUnavailable" || !strings.Contains(cond.Message, "rather than assuming") {
		t.Fatalf("condition must say it is failing closed: %+v", cond)
	}
}

func TestBudgetGateHoldsTheLastVerdictWhenTheSourceBreaks(t *testing.T) {
	// Within the grace period the last measured verdict stands: a Prometheus
	// restart is not a reliability event, and turning every scrape blip into
	// a delivery freeze teaches people to rip the gate out.
	sd := valid()
	c, r, req := newFixtureOpts(t, sd, nil, withBudget(budget.StaticSource{Value: 0.9}))
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if !automatedSync(t, c) {
		t.Fatal("expected an open gate to start with")
	}

	// Age the reading past the re-evaluation interval (so a fresh read is
	// attempted) but well inside the grace period (so the hold is valid).
	r.ReevaluationInterval = time.Minute
	aged := metav1.NewTime(time.Now().Add(-90 * time.Second))
	live := getSD(t, c, sd.Name, sd.Namespace)
	live.Status.Budget.LastEvaluationTime = &aged
	if err := c.Status().Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	r.BurnSource = brokenSource{}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if !automatedSync(t, c) {
		t.Fatal("inside the grace period, holding the last verdict means holding allow")
	}
	if cond := budgetCond(t, c, sd); !strings.Contains(cond.Message, `holding "allow"`) {
		t.Fatalf("condition should name the verdict being held: %q", cond.Message)
	}
	// The stale reading must not be restamped as if it were fresh.
	if got := getSD(t, c, sd.Name, sd.Namespace); got.Status.Budget.Source == "broken" {
		t.Fatal("a failed read must not overwrite the record of the last real one")
	}
}

func TestSyncPolicyForVerdict(t *testing.T) {
	if _, ok := syncPolicyFor(budget.Allow)["automated"]; !ok {
		t.Fatal("allow means ArgoCD syncs by itself")
	}
	for _, v := range []budget.Verdict{budget.RequireHuman, budget.Block} {
		if _, ok := syncPolicyFor(v)["automated"]; ok {
			t.Fatalf("%q must remove automated sync", v)
		}
	}
}

func TestResolveVerdictFirstReadingApplies(t *testing.T) {
	v, streak := resolveVerdict(nil, budget.Block)
	if v != budget.Block || streak != 0 {
		t.Fatalf("with no history the reading stands: %v %d", v, streak)
	}
}

func TestBudgetGateHoldsAStandingVerdict(t *testing.T) {
	// Writing status triggers a watch event, which triggers another
	// reconcile. If every reconcile re-read the budget, the anti-flapping
	// streak would burn through in milliseconds and the gate would relax
	// almost immediately — so a verdict is only re-derived once its reading
	// has gone stale. (Found by envtest; the fake client cannot show it,
	// because its Reconcile calls are made by hand.)
	sd := valid()
	c, r, req := newFixtureOpts(t, sd, nil, func(r *ServiceDeploymentReconciler) {
		r.BurnSource = budget.StaticSource{Value: 0}
		r.ReevaluationInterval = time.Hour
	})
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if automatedSync(t, c) {
		t.Fatal("expected the gate closed on an exhausted budget")
	}

	// The budget recovers, and the controller loops several times in the
	// blink of an eye, as it does in a real cluster.
	r.BurnSource = budget.StaticSource{Value: 1}
	for range 5 {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if automatedSync(t, c) {
		t.Fatal("a burst of reconciles must not be able to walk the streak to zero")
	}
	got := getSD(t, c, sd.Name, sd.Namespace)
	if got.Status.Budget.Streak != 0 {
		t.Fatalf("no fresh reading was due, so no reading happened: streak = %d", got.Status.Budget.Streak)
	}
	if cond := budgetCond(t, c, sd); !strings.Contains(cond.Message, "standing verdict") {
		t.Fatalf("the condition should say the verdict is standing, not freshly measured: %q", cond.Message)
	}
}

func TestBudgetGateTightensOnceTheHoldExpires(t *testing.T) {
	// Holding the last verdict through a brief outage is right. Holding it
	// forever is not: monitoring fails alongside the things it monitors, so
	// an unbounded hold keeps delivery unattended through exactly the
	// incident nobody can see.
	sd := valid()
	c, r, req := newFixtureOpts(t, sd, nil, withBudget(budget.StaticSource{Value: 0.9}))
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if !automatedSync(t, c) {
		t.Fatal("expected an open gate to start with")
	}

	// Age the last real reading past the grace period, then break the source.
	r.ReevaluationInterval = time.Minute
	stale := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	live := getSD(t, c, sd.Name, sd.Namespace)
	live.Status.Budget.LastEvaluationTime = &stale
	if err := c.Status().Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	r.BurnSource = brokenSource{}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if automatedSync(t, c) {
		t.Fatal("an allow nobody has been able to re-measure for three intervals must not keep standing")
	}
	cond := budgetCond(t, c, sd)
	if !strings.Contains(cond.Message, "tightening") {
		t.Fatalf("condition should say the hold expired: %q", cond.Message)
	}

	// The tightening has to be durable, or status says "allow" while
	// delivery is manual — and the comparison on recovery is made against
	// that stale allow.
	after := getSD(t, c, sd.Name, sd.Namespace).Status.Budget
	if after.Verdict != string(budget.RequireHuman) {
		t.Fatalf("persisted verdict = %q, want require-human", after.Verdict)
	}
	// Nothing was measured here, so the staleness clock must not restart.
	// (Compared as an age rather than an exact instant: metav1.Time is
	// stored to second precision.)
	if age := time.Since(after.LastEvaluationTime.Time); age < 9*time.Minute {
		t.Fatalf("last evaluation is %s old; it was restamped as if something had been read", age)
	}
	if after.RemainingPercent != 90 || after.Source == "broken" {
		t.Fatalf("the last real reading must stay on the record: %+v", after)
	}

	// Prometheus comes back healthy. That is a loosening, and loosening
	// serves the three-reading rule like any other — the outage must not
	// have bought a shortcut.
	r.BurnSource = budget.StaticSource{Value: 0.95}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if automatedSync(t, c) {
		t.Fatal("the first healthy reading after an expired hold must not reopen automated delivery")
	}
	if s := getSD(t, c, sd.Name, sd.Namespace).Status.Budget.Streak; s != 1 {
		t.Fatalf("streak = %d, want 1 — recovery starts the loosening rule from the beginning", s)
	}
}

func TestBudgetGateNeverLoosensOnAnUnreadableSource(t *testing.T) {
	// A block that cannot be re-measured stays a block; the grace rule
	// tightens, it never relaxes.
	sd := valid()
	c, r, req := newFixtureOpts(t, sd, nil, withBudget(budget.StaticSource{Value: 0}))
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}

	r.ReevaluationInterval = time.Minute
	stale := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	live := getSD(t, c, sd.Name, sd.Namespace)
	live.Status.Budget.LastEvaluationTime = &stale
	if err := c.Status().Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	r.BurnSource = brokenSource{}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if v := getSD(t, c, sd.Name, sd.Namespace).Status.Budget.Verdict; v != string(budget.Block) {
		t.Fatalf("verdict = %q; an expired hold must not weaken a block", v)
	}
}

func TestBudgetGateRereadsOnAFutureTimestamp(t *testing.T) {
	// A timestamp in the future — clock skew, or a hand-edited status —
	// would otherwise hold a verdict indefinitely, since age stays below the
	// interval forever.
	sd := valid()
	c, r, req := newFixtureOpts(t, sd, nil, func(r *ServiceDeploymentReconciler) {
		r.BurnSource = budget.StaticSource{Value: 0.9}
		r.ReevaluationInterval = time.Hour
	})
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}

	future := metav1.NewTime(time.Now().Add(24 * time.Hour))
	live := getSD(t, c, sd.Name, sd.Namespace)
	live.Status.Budget.LastEvaluationTime = &future
	live.Status.Budget.RemainingPercent = 99
	if err := c.Status().Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	r.BurnSource = budget.StaticSource{Value: 0}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	got := getSD(t, c, sd.Name, sd.Namespace)
	if got.Status.Budget.Verdict != string(budget.Block) {
		t.Fatalf("a future timestamp must force a fresh reading, got verdict %q", got.Status.Budget.Verdict)
	}
}

func TestBudgetStatusRecordsWhatTheGateBelieved(t *testing.T) {
	// environment and tier are self-service fields that weaken the verdict,
	// so the decision records them: relabelling prod as dev to win back
	// automated delivery shows up in the object's own status.
	sd := valid()
	sd.Spec.Environment = "dev"
	sd.Spec.Tier = ptr(int32(1))
	c, r, req := newFixtureOpts(t, sd, nil, withBudget(budget.StaticSource{Value: 0.05}))
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	b := getSD(t, c, sd.Name, sd.Namespace).Status.Budget
	if b.Environment != "dev" || b.Tier != 1 {
		t.Fatalf("the gate must record the inputs that weakened it: %+v", b)
	}
}
