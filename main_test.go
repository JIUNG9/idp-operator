package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
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

// Operator flags are the platform contract, so an undocumented one is a
// setting nobody knows to turn on. main() defines them as locals and exits on
// bad config, which a test can't call, so read the definitions from the source.
func TestEveryFlagIsInReadmeTable(t *testing.T) {
	defined := flagNamesInMain(t)
	if len(defined) < 15 {
		t.Fatalf("found only %d flags in main.go, the parser has probably stopped matching: %v", len(defined), defined)
	}
	documented := flagNamesInReadmeTables(t)
	for _, name := range defined {
		if !documented[name] {
			t.Errorf("flag -%s is defined in main.go but has no row in the README flag tables", name)
		}
	}
	definedSet := map[string]bool{}
	for _, name := range defined {
		definedSet[name] = true
	}
	for name := range documented {
		if !definedSet[name] {
			t.Errorf("README documents -%s but main.go does not define it", name)
		}
	}
}

// flagNamesInMain returns the first argument of every flag.String, flag.Bool,
// flag.Duration and similar call in main.go.
func flagNamesInMain(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "flag" {
			return true
		}
		switch sel.Sel.Name {
		case "String", "Bool", "Int", "Int64", "Uint", "Uint64", "Float64", "Duration":
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				name, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				names = append(names, name)
			}
		}
		return true
	})
	return names
}

var (
	readmeRowFirstCell = regexp.MustCompile("(?m)^\\|([^|]*)\\|")
	readmeFlagToken    = regexp.MustCompile("`-([a-z][a-z0-9-]*)`")
)

// flagNamesInReadmeTables returns the flag names in the first cell of each
// table row, so a flag named only in prose elsewhere does not count.
func flagNamesInReadmeTables(t *testing.T) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	for _, row := range readmeRowFirstCell.FindAllStringSubmatch(readFile(t, "README.md"), -1) {
		for _, m := range readmeFlagToken.FindAllStringSubmatch(row[1], -1) {
			got[m[1]] = true
		}
	}
	return got
}
