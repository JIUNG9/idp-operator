// Package budget decides whether a service's error budget can afford for
// delivery to happen unattended.
//
// It shares its verdict vocabulary — allow / require-human / block — and its
// budget thresholds with error-budget-gate (https://github.com/JIUNG9/error-budget-gate),
// which is Python. This is a native Go implementation of the same idea, not a
// port: one rule differs, deliberately, and the difference is documented on
// Evaluate.
package budget

import "fmt"

// Verdict is what the budget says about acting unattended.
type Verdict string

const (
	// Allow: automated sync stays on.
	Allow Verdict = "allow"
	// RequireHuman: automated sync is removed; a person syncs deliberately.
	RequireHuman Verdict = "require-human"
	// Block: automated sync is removed and the service is flagged. In ArgoCD
	// terms this is the same mechanism as RequireHuman — neither invents a
	// stronger stop than "no automated sync" — but the status says which of
	// the two situations you are in, and only one of them is an emergency.
	Block Verdict = "block"
)

// LowBudgetThreshold is error-budget-gate's own escalation point: below a
// quarter of the budget remaining, stop acting unattended.
const LowBudgetThreshold = 0.25

// Tier0 is the most critical service class. Lower is more critical, matching
// error-budget-gate's Tier enum.
const Tier0 int32 = 0

// Severity orders verdicts so a state machine can tell tightening from
// loosening. Higher is tighter.
func Severity(v Verdict) int {
	switch v {
	case Block:
		return 2
	case RequireHuman:
		return 1
	default:
		return 0
	}
}

// Input is everything the decision depends on.
type Input struct {
	// Tier is service criticality; 0 is the most critical.
	Tier int32
	// Production is whether this service faces real users.
	Production bool
	// Remaining is the fraction of error budget left, in [0,1].
	Remaining float64
}

// Decision is a verdict and the sentence explaining it.
type Decision struct {
	Verdict Verdict
	Reason  string
}

// Evaluate returns the verdict for delivering this service unattended.
//
// # Where this differs from error-budget-gate, and why
//
// error-budget-gate returns require-human for *any* production action,
// full stop. That is right for what it gates: an action a remediation system
// invented on its own, where production alone is reason enough to ask a
// person first.
//
// This gates something else — delivering a commit a human already wrote,
// reviewed and merged. Applying the same rule would strip automated sync from
// every production service permanently, which is not a safety property; it is
// the feature being switched off in the only environment anyone cares about.
// So production alone does not require a human here. A depleted budget does.
//
//	                                    error-budget-gate     this operator
//	budget exhausted                    block                 block
//	tier-0 + production                 block                 block only when budget is low
//	production, healthy budget          require-human         allow
//	production, budget < 25%            require-human         require-human
//	non-production, budget < 25%        require-human         allow
//
// The last line is the other deliberate difference: a development service
// losing automated sync is friction with no risk on the other side of it.
// Exhaustion still blocks everywhere, because that is categorical.
func Evaluate(in Input) Decision {
	switch {
	case in.Remaining <= 0:
		return Decision{Block, "error budget exhausted; delivery must not proceed unattended"}

	case in.Production && in.Tier == Tier0 && in.Remaining < LowBudgetThreshold:
		return Decision{Block, fmt.Sprintf(
			"tier-0 service in production with %.0f%% of the error budget left", in.Remaining*100)}

	case in.Production && in.Remaining < LowBudgetThreshold:
		return Decision{RequireHuman, fmt.Sprintf(
			"production service with %.0f%% of the error budget left; sync deliberately", in.Remaining*100)}

	default:
		return Decision{Allow, fmt.Sprintf("%.0f%% of the error budget left", in.Remaining*100)}
	}
}

// IsProduction reports whether an environment name means real users.
// Anything unrecognised counts as production: an unlabelled service is the
// one most likely to be mislabelled, and this is the safe direction to be
// wrong in.
func IsProduction(env string) bool {
	switch env {
	case "dev", "development", "staging", "stage", "test":
		return false
	default:
		return true
	}
}
