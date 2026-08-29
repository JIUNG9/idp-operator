package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	idpv1alpha1 "github.com/JIUNG9/idp-operator/api/v1alpha1"
)

func valid() *idpv1alpha1.ServiceDeployment {
	return &idpv1alpha1.ServiceDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-api", Namespace: "idp-system"},
		Spec: idpv1alpha1.ServiceDeploymentSpec{
			Team:     "checkout",
			Replicas: 3,
			GitOps: idpv1alpha1.GitOps{
				RepoURL: "https://github.com/acme-corp/orders-api-deploy",
				Path:    "overlays/prod",
			},
		},
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*idpv1alpha1.ServiceDeployment)
		wantErr string
	}{
		{"minimal valid", func(sd *idpv1alpha1.ServiceDeployment) {}, ""},
		{"uppercase name", func(sd *idpv1alpha1.ServiceDeployment) { sd.Name = "Orders" }, "DNS-1123"},
		{"name too long", func(sd *idpv1alpha1.ServiceDeployment) { sd.Name = strings.Repeat("a", 64) }, "DNS-1123"},
		{"reserved name", func(sd *idpv1alpha1.ServiceDeployment) { sd.Name = "kube-system" }, "reserved"},
		{"missing team", func(sd *idpv1alpha1.ServiceDeployment) { sd.Spec.Team = "" }, "spec.team"},
		{"negative replicas", func(sd *idpv1alpha1.ServiceDeployment) { sd.Spec.Replicas = -1 }, "negative"},
		{"missing repo", func(sd *idpv1alpha1.ServiceDeployment) { sd.Spec.GitOps.RepoURL = "" }, "gitops"},
		{"missing path", func(sd *idpv1alpha1.ServiceDeployment) { sd.Spec.GitOps.Path = "" }, "gitops"},
		{"availability not a number", func(sd *idpv1alpha1.ServiceDeployment) { sd.Spec.SLO.AvailabilityTarget = "always" }, "percentage"},
		{"availability 100", func(sd *idpv1alpha1.ServiceDeployment) { sd.Spec.SLO.AvailabilityTarget = "100" }, "percentage"},
		{"negative latency", func(sd *idpv1alpha1.ServiceDeployment) { sd.Spec.SLO.LatencyP99Ms = -1 }, "negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sd := valid()
			tc.mutate(sd)
			err := Validate(sd)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("expected valid, got %v", err)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
			}
		})
	}
}

func TestApplySLODefaults(t *testing.T) {
	got := ApplySLODefaults(idpv1alpha1.SLO{})
	if got.AvailabilityTarget != DefaultAvailabilityTarget || got.LatencyP99Ms != DefaultLatencyP99Ms {
		t.Fatalf("empty SLO not defaulted: %+v", got)
	}
	kept := ApplySLODefaults(idpv1alpha1.SLO{AvailabilityTarget: "99.99", LatencyP99Ms: 150})
	if kept.AvailabilityTarget != "99.99" || kept.LatencyP99Ms != 150 {
		t.Fatalf("explicit SLO overwritten: %+v", kept)
	}
}

func TestPDBPolicy(t *testing.T) {
	if PDBWanted(1) || PDBWanted(0) {
		t.Fatal("no PDB below 2 replicas — minAvailable on one replica only blocks drains")
	}
	if !PDBWanted(2) || !PDBWanted(50) {
		t.Fatal("2+ replicas must get a PDB")
	}
	if reason, _ := PDBSkipReason(0); reason != "ScaledToZero" {
		t.Fatalf("0 replicas → ScaledToZero, got %q", reason)
	}
	if reason, _ := PDBSkipReason(1); reason != "SingleReplicaUnprotected" {
		t.Fatalf("1 replica → SingleReplicaUnprotected, got %q", reason)
	}
}

func TestApplyGitOpsDefaults(t *testing.T) {
	got := ApplyGitOpsDefaults(idpv1alpha1.GitOps{RepoURL: "r", Path: "p"})
	if got.TargetRevision != "HEAD" || got.Project != "default" {
		t.Fatalf("gitops defaults wrong: %+v", got)
	}
}
