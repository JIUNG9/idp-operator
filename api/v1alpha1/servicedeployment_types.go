package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SLO is the reliability contract every service on the golden path gets.
// Empty fields are defaulted by the controller (99.9% / 300ms) so a team
// that writes nothing still ends up with a measurable target.
type SLO struct {
	// AvailabilityTarget is a percentage string, e.g. "99.9".
	AvailabilityTarget string `json:"availabilityTarget,omitempty"`
	// LatencyP99Ms is the p99 latency target in milliseconds.
	LatencyP99Ms int32 `json:"latencyP99Ms,omitempty"`
}

// GitOps points at the manifests ArgoCD should deliver for this service.
type GitOps struct {
	RepoURL        string `json:"repoURL"`
	Path           string `json:"path"`
	TargetRevision string `json:"targetRevision,omitempty"` // defaults to HEAD
	Project        string `json:"project,omitempty"`        // defaults to "default"
}

// ServiceDeploymentSpec is the single manifest a team writes to get a
// namespace, GitOps delivery, and an SLO baseline.
type ServiceDeploymentSpec struct {
	// Team owns the service; stamped on every provisioned object.
	Team string `json:"team"`
	// Replicas the workload is expected to run; informs the PDB baseline.
	Replicas int32  `json:"replicas,omitempty"`
	SLO      SLO    `json:"slo,omitempty"`
	GitOps   GitOps `json:"gitops"`
}

// ServiceDeploymentStatus reports what the golden path did.
type ServiceDeploymentStatus struct {
	Phase              string             `json:"phase,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// ServiceDeployment is one service on the golden path.
type ServiceDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceDeploymentSpec   `json:"spec,omitempty"`
	Status ServiceDeploymentStatus `json:"status,omitempty"`
}

// ServiceDeploymentList is a list of ServiceDeployments.
type ServiceDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ServiceDeployment{}, &ServiceDeploymentList{})
}
