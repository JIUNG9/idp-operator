// Package controller implements the golden path: the pure decisions live in
// this file so they can be tested without a cluster; the reconciler applies them.
package controller

import (
	"fmt"
	"regexp"
	"strconv"

	idpv1alpha1 "github.com/JIUNG9/idp-operator/api/v1alpha1"
)

const (
	// DefaultAvailabilityTarget applies when a team writes no SLO at all.
	// Every service on the path gets a measurable target — silence is not
	// an opt-out of reliability.
	DefaultAvailabilityTarget = "99.9"
	// DefaultLatencyP99Ms mirrors the org p99 baseline.
	DefaultLatencyP99Ms int32 = 300
	// ErrorBudgetWindow is the rolling window the budget is computed over.
	ErrorBudgetWindow = "28d"

	// LabelName is the selector key the whole golden path keys on. The
	// workload comes from the team's GitOps repo, not from this operator,
	// so manifests MUST carry this label — see ROADMAP "The label contract".
	LabelName      = "app.kubernetes.io/name"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedByValue = "idp-operator"
	LabelTeam      = "idp.jiung9.github.io/team"
	// LabelOwnerNamespace records which namespace the owning ServiceDeployment
	// lives in, so drift on a provisioned object can be routed back to it and
	// so two ServiceDeployments with the same name can't fight over one
	// cluster namespace.
	LabelOwnerNamespace = "idp.jiung9.github.io/owner-namespace"
)

var rfc1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// reservedNames are namespaces the golden path must never claim: the CR's
// name becomes the provisioned namespace's name.
var reservedNames = map[string]bool{
	"default":         true,
	"kube-system":     true,
	"kube-public":     true,
	"kube-node-lease": true,
	ArgoCDNamespace:   true,
}

// Validate rejects a ServiceDeployment the golden path cannot provision.
func Validate(sd *idpv1alpha1.ServiceDeployment) error {
	if !rfc1123.MatchString(sd.Name) || len(sd.Name) > 63 {
		return fmt.Errorf("name %q must be a DNS-1123 label (it becomes the namespace)", sd.Name)
	}
	if reservedNames[sd.Name] {
		return fmt.Errorf("name %q is reserved: the golden path would claim that namespace", sd.Name)
	}
	if sd.Spec.Team == "" {
		return fmt.Errorf("spec.team is required: every service needs an owner")
	}
	if sd.Spec.Replicas < 0 {
		return fmt.Errorf("spec.replicas must not be negative")
	}
	if sd.Spec.GitOps.RepoURL == "" || sd.Spec.GitOps.Path == "" {
		return fmt.Errorf("spec.gitops.repoURL and spec.gitops.path are required")
	}
	if sd.Spec.SLO.AvailabilityTarget != "" {
		v, err := strconv.ParseFloat(sd.Spec.SLO.AvailabilityTarget, 64)
		if err != nil || v <= 0 || v >= 100 {
			return fmt.Errorf("spec.slo.availabilityTarget %q must be a percentage in (0, 100)", sd.Spec.SLO.AvailabilityTarget)
		}
	}
	if sd.Spec.SLO.LatencyP99Ms < 0 {
		return fmt.Errorf("spec.slo.latencyP99Ms must not be negative")
	}
	return nil
}

// ApplySLODefaults fills the reliability contract a team left blank. The CRD
// schema is the authoritative defaulter; this is defense-in-depth for clients
// (and tests) that bypass the API server's defaulting.
func ApplySLODefaults(slo idpv1alpha1.SLO) idpv1alpha1.SLO {
	if slo.AvailabilityTarget == "" {
		slo.AvailabilityTarget = DefaultAvailabilityTarget
	}
	if slo.LatencyP99Ms == 0 {
		slo.LatencyP99Ms = DefaultLatencyP99Ms
	}
	return slo
}

// ApplyGitOpsDefaults fills optional delivery fields (same defense-in-depth
// note as ApplySLODefaults).
func ApplyGitOpsDefaults(g idpv1alpha1.GitOps) idpv1alpha1.GitOps {
	if g.TargetRevision == "" {
		g.TargetRevision = "HEAD"
	}
	if g.Project == "" {
		g.Project = "default"
	}
	return g
}

// PDBWanted says whether a PodDisruptionBudget can actually protect this
// service. Below two replicas it can't: minAvailable=1 on a single replica
// only blocks node drains while the pod still dies with its node.
func PDBWanted(replicas int32) bool {
	return replicas >= 2
}

// PDBSkipReason explains, as a condition reason + message, why no PDB was
// provisioned — so "intentionally unprotected" is distinguishable from
// "misconfigured".
func PDBSkipReason(replicas int32) (reason, message string) {
	if replicas <= 0 {
		return "ScaledToZero", "no replicas declared; nothing to protect"
	}
	return "SingleReplicaUnprotected", "a single replica cannot be disruption-safe; set spec.replicas to 2 or more for a PDB"
}

// Labels stamps ownership and provenance on every provisioned object.
func Labels(sd *idpv1alpha1.ServiceDeployment) map[string]string {
	return map[string]string{
		LabelName:           sd.Name,
		LabelManagedBy:      ManagedByValue,
		LabelTeam:           sd.Spec.Team,
		LabelOwnerNamespace: sd.Namespace,
	}
}
