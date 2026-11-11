package main

import (
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

func TestManagerOptions(t *testing.T) {
	o := managerOptions(true, ":9090", ":9091")
	if !o.LeaderElection || o.LeaderElectionID == "" {
		t.Errorf("leader election not configured: %+v", o)
	}
	if o.Metrics.BindAddress != ":9090" || o.HealthProbeBindAddress != ":9091" {
		t.Errorf("bind addresses not passed through: metrics=%q probes=%q",
			o.Metrics.BindAddress, o.HealthProbeBindAddress)
	}
	if managerOptions(false, "0", "0").LeaderElection {
		t.Error("leader election should follow the flag")
	}
}

// The Deployment's args, ports and probes have to match what main.go serves.
// A probe path or port that drifts makes the pod restart-loop, and only a
// cluster run would otherwise show it.
func TestManagerManifestMatchesMain(t *testing.T) {
	var dep appsv1.Deployment
	for _, doc := range strings.Split(readFile(t, "config/manager/manager.yaml"), "\n---\n") {
		if strings.Contains(doc, "kind: Deployment") {
			if err := yaml.Unmarshal([]byte(doc), &dep); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("want one container, got %d", len(dep.Spec.Template.Spec.Containers))
	}
	c := dep.Spec.Template.Spec.Containers[0]

	if !contains(c.Args, "-leader-elect") {
		t.Errorf("manager.yaml must pass -leader-elect, args: %v", c.Args)
	}
	if c.LivenessProbe == nil || c.LivenessProbe.HTTPGet == nil || c.LivenessProbe.HTTPGet.Path != "/healthz" {
		t.Errorf("liveness probe must GET /healthz: %+v", c.LivenessProbe)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet == nil || c.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Errorf("readiness probe must GET /readyz: %+v", c.ReadinessProbe)
	}
	ports := map[string]int32{}
	for _, p := range c.Ports {
		ports[p.Name] = p.ContainerPort
	}
	// The defaults of -metrics-bind-address and -health-probe-bind-address.
	if ports["metrics"] != 8080 || ports["health"] != 8081 {
		t.Errorf("container ports must match the flag defaults :8080 and :8081, got %v", ports)
	}
	if c.LivenessProbe != nil && c.LivenessProbe.HTTPGet != nil && c.LivenessProbe.HTTPGet.Port.StrVal != "health" {
		t.Errorf("liveness probe must target the health port, got %v", c.LivenessProbe.HTTPGet.Port)
	}
}

func TestLeaderElectionRoleCoversLease(t *testing.T) {
	var role rbacv1.Role
	doc := strings.Split(readFile(t, "config/rbac/leader_election_role.yaml"), "\n---\n")[0]
	if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
		t.Fatal(err)
	}
	if role.Namespace != "idp-system" {
		t.Errorf("Role must live in the operator's namespace, got %q", role.Namespace)
	}
	need := map[string]bool{"get": false, "create": false, "update": false}
	for _, r := range role.Rules {
		if contains(r.APIGroups, "coordination.k8s.io") && contains(r.Resources, "leases") {
			for _, v := range r.Verbs {
				if _, ok := need[v]; ok {
					need[v] = true
				}
			}
		}
	}
	for v, ok := range need {
		if !ok {
			t.Errorf("leases rule lacks verb %q", v)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
