// Package v1alpha1 contains the API types for the idp-operator golden path.
//
// +kubebuilder:object:generate=true
// +groupName=idp.jiung9.github.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group/version for the ServiceDeployment API.
	GroupVersion = schema.GroupVersion{Group: "idp.jiung9.github.io", Version: "v1alpha1"}

	// SchemeBuilder registers our types with a scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
