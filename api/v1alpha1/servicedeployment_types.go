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
	//
	// A pointer, because 0 is a meaningful value here: with a plain int and
	// `omitempty`, a client setting 0 would serialise nothing, the API
	// server would apply the default of 1, and the request would be
	// silently inverted.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
	// Tier is service criticality, 0 the most critical, matching
	// error-budget-gate's tiers. It changes how hard a depleted error budget
	// stops delivery. Pointer for the same reason as Replicas — and here it
	// matters more, since tier 0 is the value you least want silently
	// rewritten to the default.
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3
	// +optional
	Tier *int32 `json:"tier,omitempty"`
	// Environment decides whether a depleted budget merely warns or stops
	// unattended delivery. Defaults to prod: an unlabelled service is the
	// one most likely to be mislabelled.
	// +kubebuilder:default=prod
	// +kubebuilder:validation:Enum=prod;staging;dev
	// +optional
	Environment string `json:"environment,omitempty"`
	// DeletionPolicy decides what happens to the service's namespace when
	// this ServiceDeployment is deleted. Retain (the default) leaves the
	// namespace, and the workloads and PersistentVolumeClaims in it, in
	// place; the ArgoCD Application is still deleted. Delete removes the
	// namespace, which deletes everything inside it.
	// +kubebuilder:default=Retain
	// +kubebuilder:validation:Enum=Retain;Delete
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
	// +kubebuilder:default={}
	// +optional
	SLO    SLO    `json:"slo,omitempty"`
	GitOps GitOps `json:"gitops"`
}

// Values of spec.deletionPolicy.
const (
	DeletionPolicyRetain = "Retain"
	DeletionPolicyDelete = "Delete"
)

// Defaults mirroring the CRD schema, for clients that bypass API-server
// defaulting (the fake client in tests, mostly).
const (
	DefaultReplicas int32 = 1
	DefaultTier     int32 = 2
)

// RetainsNamespace reports whether deleting the ServiceDeployment leaves the
// namespace alone. Anything other than an explicit Delete retains, so an
// unset field (a client that bypasses API-server defaulting) cannot delete data.
func (s ServiceDeploymentSpec) RetainsNamespace() bool {
	return s.DeletionPolicy != DeletionPolicyDelete
}

// ReplicasOrDefault returns the declared replica count.
func (s ServiceDeploymentSpec) ReplicasOrDefault() int32 {
	if s.Replicas == nil {
		return DefaultReplicas
	}
	return *s.Replicas
}

// TierOrDefault returns the declared criticality tier.
func (s ServiceDeploymentSpec) TierOrDefault() int32 {
	if s.Tier == nil {
		return DefaultTier
	}
	return *s.Tier
}

// BudgetStatus is the delivery gate's memory. It is persisted rather than
// recomputed from scratch because the anti-flapping rule needs to know what
// the last verdict was, and because a verdict that resets every time the
// operator restarts is not a gate.
type BudgetStatus struct {
	// Verdict is allow, require-human, or block.
	Verdict string `json:"verdict,omitempty"`
	// RemainingPercent is the error budget left at the last reading.
	RemainingPercent int32 `json:"remainingPercent,omitempty"`
	// Streak counts consecutive readings arguing for a looser verdict than
	// the one in force. Loosening waits; tightening never does.
	Streak int32 `json:"streak,omitempty"`
	// Source names where the reading came from.
	Source string `json:"source,omitempty"`
	// Environment and Tier record what the gate believed about the service
	// when it decided. Both are self-service spec fields that weaken the
	// verdict, so the decision carries them: a service relabelled to dev to
	// win back automated delivery says so in its own status.
	Environment string `json:"environment,omitempty"`
	Tier        int32  `json:"tier,omitempty"`
	// LastEvaluationTime is when the reading was taken.
	// +optional
	LastEvaluationTime *metav1.Time `json:"lastEvaluationTime,omitempty"`
}

// ServiceDeploymentStatus reports what the golden path did. Phase says
// whether the objects exist and match spec — Provisioned does NOT claim the
// application is healthy; delivery health is ArgoCD's job.
type ServiceDeploymentStatus struct {
	// Phase is one of Provisioned, Invalid, Error, Deleting.
	Phase              string `json:"phase,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	// +optional
	Budget *BudgetStatus `json:"budget,omitempty"`
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
// +kubebuilder:printcolumn:name="Budget",type=string,JSONPath=`.status.budget.verdict`
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
