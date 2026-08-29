package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SLO is the reliability contract every service on the golden path gets.
// Empty fields are defaulted by the CRD schema (99.9% / 300ms); the controller
// re-applies the same defaults as defense-in-depth so a team that writes
// nothing still ends up with a measurable target.
type SLO struct {
	// AvailabilityTarget is a percentage string, e.g. "99.9".
	// +kubebuilder:default="99.9"
	// +kubebuilder:validation:Pattern=`^\d{1,2}(\.\d+)?$`
	// +optional
	AvailabilityTarget string `json:"availabilityTarget,omitempty"`
	// LatencyP99Ms is the p99 latency target in milliseconds.
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=1
	// +optional
	LatencyP99Ms int32 `json:"latencyP99Ms,omitempty"`
}

// GitOps points at the manifests ArgoCD should deliver for this service.
type GitOps struct {
	// +kubebuilder:validation:MinLength=1
	RepoURL string `json:"repoURL"`
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`
	// +kubebuilder:default="HEAD"
	// +optional
	TargetRevision string `json:"targetRevision,omitempty"`
	// +kubebuilder:default="default"
	// +optional
	Project string `json:"project,omitempty"`
}

// ServiceDeploymentSpec is the single manifest a team writes to get a
// namespace, GitOps delivery, and an SLO baseline.
type ServiceDeploymentSpec struct {
	// Team owns the service; stamped on every provisioned object.
	// +kubebuilder:validation:MinLength=1
	Team string `json:"team"`
	// Replicas the workload declares; drives the PDB baseline. 0 means
	// scaled to zero — the golden path provisions no PDB and says so in
	// a status condition instead of shipping a drain-blocking fake one.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// +kubebuilder:default={}
	// +optional
	SLO    SLO    `json:"slo,omitempty"`
	GitOps GitOps `json:"gitops"`
}

// ServiceDeploymentStatus reports what the golden path did. Phase says
// whether the objects exist and match spec — Provisioned does NOT claim the
// application is healthy; delivery health is ArgoCD's job.
type ServiceDeploymentStatus struct {
	// Phase is one of Provisioned, Invalid, Error, Deleting.
	Phase              string `json:"phase,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ServiceDeployment is one service on the golden path.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=svcdep
// +kubebuilder:printcolumn:name="Team",type=string,JSONPath=`.spec.team`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="SLO",type=string,JSONPath=`.spec.slo.availabilityTarget`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ServiceDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceDeploymentSpec   `json:"spec,omitempty"`
	Status ServiceDeploymentStatus `json:"status,omitempty"`
}

// ServiceDeploymentList is a list of ServiceDeployments.
//
// +kubebuilder:object:root=true
type ServiceDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ServiceDeployment{}, &ServiceDeploymentList{})
}
