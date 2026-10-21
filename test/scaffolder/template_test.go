// Package scaffolder keeps the Backstage template honest. The form is a second
// copy of the ServiceDeployment schema; these tests fail when it drifts from
// the CRD, and when the skeleton stops producing a valid ServiceDeployment.
package scaffolder

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	idpv1 "github.com/JIUNG9/idp-operator/api/v1alpha1"
)

const (
	templateDir = "../../templates/backstage/service-deployment"
	crdFile     = "../../config/crd/idp.jiung9.github.io_servicedeployments.yaml"
)

// formToCRD maps each form field to its path in spec. `name` is not here: it
// is metadata.name, which the CRD does not constrain.
var formToCRD = map[string][]string{
	"team":               {"team"},
	"tier":               {"tier"},
	"environment":        {"environment"},
	"replicas":           {"replicas"},
	"availabilityTarget": {"slo", "availabilityTarget"},
	"latencyP99Ms":       {"slo", "latencyP99Ms"},
	"repoURL":            {"gitops", "repoURL"},
	"path":               {"gitops", "path"},
	"targetRevision":     {"gitops", "targetRevision"},
	"project":            {"gitops", "project"},
}

// constrained are the schema keywords a form can enforce for the user.
var constrained = []string{"type", "default", "enum", "minimum", "maximum", "minLength", "pattern"}

type formStep struct {
	Required   []string                  `json:"required"`
	Properties map[string]map[string]any `json:"properties"`
}

type template struct {
	Spec struct {
		Parameters []formStep `json:"parameters"`
	} `json:"spec"`
}

func loadTemplate(t *testing.T) (props map[string]map[string]any, required map[string]bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(templateDir, "template.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var tpl template
	if err := yaml.Unmarshal(raw, &tpl); err != nil {
		t.Fatal(err)
	}
	props, required = map[string]map[string]any{}, map[string]bool{}
	for _, step := range tpl.Spec.Parameters {
		for name, p := range step.Properties {
			props[name] = p
		}
		for _, r := range step.Required {
			required[r] = true
		}
	}
	return props, required
}

func loadSpecSchema(t *testing.T) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	raw, err := os.ReadFile(crdFile)
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	return crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
}

// asMap round-trips a schema node through JSON so it compares with the
// template's decoded YAML (numbers become float64 on both sides).
func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func lookup(t *testing.T, spec apiextensionsv1.JSONSchemaProps, path []string) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	node := spec
	for _, seg := range path {
		next, ok := node.Properties[seg]
		if !ok {
			t.Fatalf("CRD has no spec.%v", path)
		}
		node = next
	}
	return node
}

func TestFormMatchesCRD(t *testing.T) {
	props, _ := loadTemplate(t)
	spec := loadSpecSchema(t)

	for field, path := range formToCRD {
		form, ok := props[field]
		if !ok {
			t.Errorf("form has no %q field", field)
			continue
		}
		crd := asMap(t, lookup(t, spec, path))
		for _, kw := range constrained {
			if !reflect.DeepEqual(form[kw], crd[kw]) {
				t.Errorf("%s: %s is %v in the form, %v in the CRD", field, kw, form[kw], crd[kw])
			}
		}
	}
}

func TestFormCoversEveryCRDField(t *testing.T) {
	props, _ := loadTemplate(t)
	spec := loadSpecSchema(t)

	mapped := map[string]bool{}
	for _, path := range formToCRD {
		mapped[joinPath(path)] = true
	}
	var walk func(prefix []string, node apiextensionsv1.JSONSchemaProps)
	walk = func(prefix []string, node apiextensionsv1.JSONSchemaProps) {
		for name, child := range node.Properties {
			p := append(append([]string{}, prefix...), name)
			if child.Type == "object" {
				walk(p, child)
				continue
			}
			if !mapped[joinPath(p)] {
				t.Errorf("CRD field spec.%s has no form field; add it to the template and to formToCRD", joinPath(p))
			}
		}
	}
	walk(nil, spec)

	known := map[string]bool{"name": true}
	for f := range formToCRD {
		known[f] = true
	}
	var extra []string
	for f := range props {
		if !known[f] {
			extra = append(extra, f)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("form fields with no CRD counterpart: %v", extra)
	}
}

func TestFormRequiresWhatTheCRDRequires(t *testing.T) {
	_, required := loadTemplate(t)
	spec := loadSpecSchema(t)

	for _, r := range spec.Required {
		if spec.Properties[r].Type == "object" {
			continue // gitops: its own required fields are checked below
		}
		if !required[r] {
			t.Errorf("spec.%s is required by the CRD but not by the form", r)
		}
	}
	for _, r := range spec.Properties["gitops"].Required {
		if !required[r] {
			t.Errorf("spec.gitops.%s is required by the CRD but not by the form", r)
		}
	}
	if !required["name"] {
		t.Error("the form must require a name")
	}
}

var placeholder = regexp.MustCompile(`\$\{\{\s*values\.(\w+)\s*\}\}`)

// TestSkeletonRendersAValidServiceDeployment fills the skeleton with the
// form's own defaults and sample values for the required fields, then decodes
// it strictly into the API type.
func TestSkeletonRendersAValidServiceDeployment(t *testing.T) {
	props, _ := loadTemplate(t)
	samples := map[string]string{
		"name":    "orders-api",
		"team":    "checkout",
		"repoURL": "https://github.com/acme-corp/orders-api-deploy",
		"path":    "overlays/prod",
	}

	files, err := filepath.Glob(filepath.Join(templateDir, "skeleton", "*.yaml"))
	if err != nil || len(files) != 1 {
		t.Fatalf("want exactly one skeleton manifest, got %v (err %v)", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}

	used := map[string]bool{}
	rendered := placeholder.ReplaceAllFunc(raw, func(m []byte) []byte {
		field := string(placeholder.FindSubmatch(m)[1])
		used[field] = true
		if v, ok := samples[field]; ok {
			return []byte(v)
		}
		def, ok := props[field]["default"]
		if !ok {
			t.Errorf("skeleton uses values.%s, which is neither a sample nor a defaulted form field", field)
		}
		b, _ := json.Marshal(def)
		return []byte(trimQuotes(string(b)))
	})

	for field := range props {
		if !used[field] {
			t.Errorf("form field %q never reaches the skeleton", field)
		}
	}

	var sd idpv1.ServiceDeployment
	if err := yaml.UnmarshalStrict(rendered, &sd); err != nil {
		t.Fatalf("rendered skeleton does not decode: %v\n%s", err, rendered)
	}
	if sd.Name != "orders-api" || sd.Spec.Team != "checkout" {
		t.Errorf("unexpected identity: %s / %s", sd.Name, sd.Spec.Team)
	}
	if sd.Spec.TierOrDefault() != idpv1.DefaultTier || sd.Spec.ReplicasOrDefault() != idpv1.DefaultReplicas {
		t.Errorf("tier/replicas did not round-trip: %d/%d", sd.Spec.TierOrDefault(), sd.Spec.ReplicasOrDefault())
	}
	if sd.Spec.SLO.AvailabilityTarget != "99.9" {
		t.Errorf("availabilityTarget must stay a string, got %q", sd.Spec.SLO.AvailabilityTarget)
	}
	if sd.Spec.GitOps.TargetRevision != "HEAD" || sd.Spec.GitOps.Project != "default" {
		t.Errorf("gitops defaults lost: %+v", sd.Spec.GitOps)
	}
}

func joinPath(p []string) string {
	out := ""
	for i, s := range p {
		if i > 0 {
			out += "."
		}
		out += s
	}
	return out
}

func trimQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
