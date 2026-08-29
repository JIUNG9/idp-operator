package controller

import (
	"strings"
	"testing"
)

func TestDenyIngressLeavesEgressAlone(t *testing.T) {
	spec := DenyIngressSpec()
	if len(spec.PolicyTypes) != 1 || string(spec.PolicyTypes[0]) != "Ingress" {
		t.Fatalf("policy types = %v; egress default-deny needs declared dependencies and has none yet", spec.PolicyTypes)
	}
	if len(spec.Ingress) != 0 {
		t.Fatalf("a deny policy must carry no ingress rules, got %+v", spec.Ingress)
	}
	if spec.PodSelector.Size() != 0 {
		t.Fatal("the deny must select every pod in the namespace")
	}
}

func TestSameNamespaceIngressAllowsOnlyLocalPods(t *testing.T) {
	spec := SameNamespaceIngressSpec()
	if len(spec.Ingress) != 1 || len(spec.Ingress[0].From) != 1 {
		t.Fatalf("expected exactly one from-peer, got %+v", spec.Ingress)
	}
	peer := spec.Ingress[0].From[0]
	if peer.PodSelector == nil {
		t.Fatal("same-namespace rule must select pods")
	}
	// A namespaceSelector here would widen this to the whole cluster.
	if peer.NamespaceSelector != nil {
		t.Fatal("same-namespace rule must NOT carry a namespaceSelector")
	}
}

func TestPlatformIngressUnconfigured(t *testing.T) {
	if _, ok := (NetworkConfig{}).PlatformIngressSpec(); ok {
		t.Fatal("no configuration means no platform policy at all")
	}
}

func TestPlatformIngressMixesNamespacesAndCIDRs(t *testing.T) {
	nc := NetworkConfig{
		AllowIngressFromNamespaces: []string{"ingress-nginx"},
		AllowIngressFromCIDRs:      []string{"10.0.0.0/16"},
	}
	spec, ok := nc.PlatformIngressSpec()
	if !ok {
		t.Fatal("expected a policy")
	}
	peers := spec.Ingress[0].From
	if len(peers) != 2 {
		t.Fatalf("expected 2 peers, got %d", len(peers))
	}
	if peers[0].NamespaceSelector == nil ||
		peers[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "ingress-nginx" {
		t.Fatalf("namespace peer wrong: %+v", peers[0])
	}
	if peers[1].IPBlock == nil || peers[1].IPBlock.CIDR != "10.0.0.0/16" {
		t.Fatalf("cidr peer wrong: %+v", peers[1])
	}
}

func TestImageVerificationConfigured(t *testing.T) {
	cases := []struct {
		name string
		cfg  PolicyConfig
		want bool
	}{
		{"nothing", PolicyConfig{}, false},
		{"key", PolicyConfig{CosignPublicKey: "-----BEGIN PUBLIC KEY-----"}, true},
		{"keyless", PolicyConfig{CosignIssuer: "https://accounts.example.com", CosignSubject: "ci@example.com"}, true},
		{"half keyless", PolicyConfig{CosignIssuer: "https://accounts.example.com"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ImageVerificationConfigured(); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBaselineRulesShipWithoutSigningIdentity(t *testing.T) {
	spec := KyvernoPolicySpec(PolicyConfig{})
	rules, _ := spec["rules"].([]any)
	if len(rules) != 2 {
		t.Fatalf("expected the 2 workload rules, got %d", len(rules))
	}
	names := KyvernoRuleNames(PolicyConfig{})
	if strings.Join(names, ",") != "disallow-privileged,disallow-host-namespaces" {
		t.Fatalf("rule names = %v", names)
	}
	for _, r := range rules {
		rule := r.(map[string]any)
		v, ok := rule["validate"].(map[string]any)
		if !ok {
			t.Fatalf("rule %v has no validate block", rule["name"])
		}
		// The spec-level validationFailureAction is deprecated; the action
		// belongs on the rule.
		if v["failureAction"] != "Enforce" {
			t.Fatalf("rule %v failureAction = %v", rule["name"], v["failureAction"])
		}
	}
}

func TestPrivilegedRuleUsesOptionalAnchors(t *testing.T) {
	spec := KyvernoPolicySpec(PolicyConfig{})
	rule := spec["rules"].([]any)[0].(map[string]any)
	pattern := rule["validate"].(map[string]any)["pattern"].(map[string]any)["spec"].(map[string]any)
	// containers is required (every pod has one); the others are anchored so
	// a pod without initContainers still passes.
	if _, ok := pattern["containers"]; !ok {
		t.Fatal("containers must be matched")
	}
	for _, key := range []string{"=(initContainers)", "=(ephemeralContainers)"} {
		if _, ok := pattern[key]; !ok {
			t.Fatalf("%s must use an optional anchor so pods without it pass", key)
		}
	}
	container := pattern["containers"].([]any)[0].(map[string]any)
	sc, ok := container["=(securityContext)"].(map[string]any)
	if !ok {
		t.Fatal("securityContext must be optional: this baseline rejects pods that ask for privilege, not pods that stay silent")
	}
	if sc["=(privileged)"] != "false" {
		t.Fatalf("privileged anchor = %v", sc["=(privileged)"])
	}
}

func TestVerifyImagesNeverMutatesTheImage(t *testing.T) {
	cfg := PolicyConfig{CosignPublicKey: "-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----"}
	spec := KyvernoPolicySpec(cfg)
	rules := spec["rules"].([]any)
	if len(rules) != 3 {
		t.Fatalf("expected 3 rules with a signing identity, got %d", len(rules))
	}
	verify := rules[2].(map[string]any)["verifyImages"].([]any)[0].(map[string]any)

	// Kyverno defaults mutateDigest to true, which rewrites the tag to a
	// digest at admission. Every service here also gets an ArgoCD
	// Application with selfHeal on, so that rewrite is a permanent fight:
	// Kyverno mutates, ArgoCD syncs the tag back, Kyverno mutates again.
	if verify["mutateDigest"] != false {
		t.Fatal("mutateDigest must be explicitly false — the default fights ArgoCD selfHeal")
	}
	// With no mutation, requiring a digest would reject every tag-based
	// manifest; pinning belongs in the pipeline that builds them.
	if verify["verifyDigest"] != false {
		t.Fatal("verifyDigest must be explicitly false while mutateDigest is off")
	}
	if verify["required"] != true {
		t.Fatal("a matched image must be verified")
	}
	if verify["failureAction"] != "Enforce" {
		t.Fatalf("failureAction = %v", verify["failureAction"])
	}
	entry := verify["attestors"].([]any)[0].(map[string]any)["entries"].([]any)[0].(map[string]any)
	keys, ok := entry["keys"].(map[string]any)
	if !ok || !strings.Contains(keys["publicKeys"].(string), "BEGIN PUBLIC KEY") {
		t.Fatalf("public key attestor wrong: %+v", entry)
	}
}

func TestVerifyImagesKeylessAttestor(t *testing.T) {
	cfg := PolicyConfig{
		CosignIssuer:    "https://token.actions.githubusercontent.com",
		CosignSubject:   "https://github.com/acme-corp/orders-api/.github/workflows/release.yaml@refs/heads/main",
		ImageReferences: []string{"ghcr.io/acme-corp/*"},
	}
	spec := KyvernoPolicySpec(cfg)
	verify := spec["rules"].([]any)[2].(map[string]any)["verifyImages"].([]any)[0].(map[string]any)
	if refs := verify["imageReferences"].([]any); len(refs) != 1 || refs[0] != "ghcr.io/acme-corp/*" {
		t.Fatalf("imageReferences = %v", verify["imageReferences"])
	}
	entry := verify["attestors"].([]any)[0].(map[string]any)["entries"].([]any)[0].(map[string]any)
	keyless, ok := entry["keyless"].(map[string]any)
	if !ok || keyless["issuer"] != cfg.CosignIssuer || keyless["subject"] != cfg.CosignSubject {
		t.Fatalf("keyless attestor wrong: %+v", entry)
	}
}

func TestImageReferencesDefaultToEverything(t *testing.T) {
	refs := PolicyConfig{}.imageReferences()
	if len(refs) != 1 || refs[0] != "*" {
		t.Fatalf("refs = %v", refs)
	}
}

func TestRequireImageDigestsClosesTheMovedTagGap(t *testing.T) {
	cfg := PolicyConfig{CosignPublicKey: "-----BEGIN PUBLIC KEY-----", RequireImageDigests: true}
	spec := KyvernoPolicySpec(cfg)
	verify := spec["rules"].([]any)[2].(map[string]any)["verifyImages"].([]any)[0].(map[string]any)
	if verify["verifyDigest"] != true {
		t.Fatal("opting in must require digests; verifying a tag that can move afterwards is not verification")
	}
	// Even then, the operator must not rewrite the reference itself.
	if verify["mutateDigest"] != false {
		t.Fatal("mutateDigest stays false regardless — it is what fights ArgoCD selfHeal")
	}
}
