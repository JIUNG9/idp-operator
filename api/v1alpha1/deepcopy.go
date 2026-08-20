package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Hand-written deepcopy implementations (no codegen yet — see README roadmap).

func (in *SLO) DeepCopyInto(out *SLO) { *out = *in }

func (in *GitOps) DeepCopyInto(out *GitOps) { *out = *in }

func (in *ServiceDeploymentSpec) DeepCopyInto(out *ServiceDeploymentSpec) {
	*out = *in
	in.SLO.DeepCopyInto(&out.SLO)
	in.GitOps.DeepCopyInto(&out.GitOps)
}

func (in *ServiceDeploymentStatus) DeepCopyInto(out *ServiceDeploymentStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *ServiceDeployment) DeepCopyInto(out *ServiceDeployment) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *ServiceDeployment) DeepCopy() *ServiceDeployment {
	if in == nil {
		return nil
	}
	out := new(ServiceDeployment)
	in.DeepCopyInto(out)
	return out
}

func (in *ServiceDeployment) DeepCopyObject() runtime.Object { return in.DeepCopy() }

func (in *ServiceDeploymentList) DeepCopyInto(out *ServiceDeploymentList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ServiceDeployment, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ServiceDeploymentList) DeepCopy() *ServiceDeploymentList {
	if in == nil {
		return nil
	}
	out := new(ServiceDeploymentList)
	in.DeepCopyInto(out)
	return out
}

func (in *ServiceDeploymentList) DeepCopyObject() runtime.Object { return in.DeepCopy() }
