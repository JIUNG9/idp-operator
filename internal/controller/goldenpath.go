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
)

var rfc1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Validate rejects a ServiceDeployment the golden path cannot provision.
func Validate(sd *idpv1alpha1.ServiceDeployment) error {
	if !rfc1123.MatchString(sd.Name) || len(sd.Name) > 63 {
		return fmt.Errorf("name %q must be a DNS-1123 label (it becomes the namespace)", sd.Name)
	}
	if sd.Spec.Team == "" {
		return fmt.Errorf("spec.team is required: every service needs an owner")
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

// ApplySLODefaults fills the reliability contract a team left blank.
func ApplySLODefaults(slo idpv1alpha1.SLO) idpv1alpha1.SLO {
	if slo.AvailabilityTarget == "" {
		slo.AvailabilityTarget = DefaultAvailabilityTarget
	}
	if slo.LatencyP99Ms == 0 {
		slo.LatencyP99Ms = DefaultLatencyP99Ms
	}
	return slo
}

// ApplyGitOpsDefaults fills optional delivery fields.
func ApplyGitOpsDefaults(g idpv1alpha1.GitOps) idpv1alpha1.GitOps {
	if g.TargetRevision == "" {
		g.TargetRevision = "HEAD"
	}
	if g.Project == "" {
		g.Project = "default"
	}
	return g
}

// Labels stamps ownership and provenance on every provisioned object.
func Labels(sd *idpv1alpha1.ServiceDeployment) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       sd.Name,
		"app.kubernetes.io/managed-by": "idp-operator",
		"idp.jiung9.github.io/team":    sd.Spec.Team,
	}
}
