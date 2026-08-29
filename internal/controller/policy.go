package controller

import (
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Provisioned policy object names. Prefixed so a team can add its own
// policies alongside without the operator clobbering them.
const (
	NetworkPolicyDenyIngress   = "idp-default-deny-ingress"
	NetworkPolicyAllowSameNS   = "idp-allow-same-namespace"
	NetworkPolicyAllowPlatform = "idp-allow-platform-ingress"
	KyvernoPolicyName          = "idp-baseline"
)

// KyvernoPolicyGVK is the namespaced Kyverno Policy. Namespaced rather than
// ClusterPolicy on purpose: the policy's blast radius is one service, and it
// is garbage-collected by the namespace cascade for free.
var KyvernoPolicyGVK = schema.GroupVersionKind{Group: "kyverno.io", Version: "v1", Kind: "Policy"}

// NetworkConfig describes the cluster's ingress topology. It is operator-level
// configuration, set once by whoever runs the platform, because it is a
// property of the cluster and not of any one service.
//
// The golden path denies ingress by default and allows same-namespace traffic.
// Anything reaching a service from outside its namespace — an ingress
// controller, a service mesh gateway — has to be named here, or every exposed
// service breaks the moment the policy lands.
type NetworkConfig struct {
	// AllowIngressFromNamespaces are namespaces whose pods may reach services
	// on the golden path, e.g. the ingress controller's namespace.
	AllowIngressFromNamespaces []string
	// AllowIngressFromCIDRs covers load balancers that reach pods directly and
	// therefore have no namespace to select — AWS Load Balancer Controller
	// with target-type=ip puts the balancer's ENI addresses on the wire, not
	// a pod IP.
	AllowIngressFromCIDRs []string
}

// PlatformIngressConfigured reports whether anything outside the service's own
// namespace is allowed to reach it.
func (nc NetworkConfig) PlatformIngressConfigured() bool {
	return len(nc.AllowIngressFromNamespaces) > 0 || len(nc.AllowIngressFromCIDRs) > 0
}

// DenyIngressSpec denies all ingress to every pod in the namespace. Egress is
// deliberately untouched: an egress default-deny needs each service's
// dependencies declared before it is anything but an outage, and that
// declaration does not exist yet (see ROADMAP).
func DenyIngressSpec() networkingv1.NetworkPolicySpec {
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
	}
}

// SameNamespaceIngressSpec lets pods in the namespace talk to each other, so
// a multi-pod service still works under the default deny.
func SameNamespaceIngressSpec() networkingv1.NetworkPolicySpec {
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{},
			}},
		}},
	}
}

// PlatformIngressSpec admits the sources the platform team declared. Reports
// false when nothing is configured, in which case no policy is provisioned.
func (nc NetworkConfig) PlatformIngressSpec() (networkingv1.NetworkPolicySpec, bool) {
	if !nc.PlatformIngressConfigured() {
		return networkingv1.NetworkPolicySpec{}, false
	}
	peers := make([]networkingv1.NetworkPolicyPeer, 0,
		len(nc.AllowIngressFromNamespaces)+len(nc.AllowIngressFromCIDRs))
	for _, ns := range nc.AllowIngressFromNamespaces {
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			// kubernetes.io/metadata.name is set on every namespace by the
			// API server, so this needs no cooperation from the namespace.
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns},
			},
		})
	}
	for _, cidr := range nc.AllowIngressFromCIDRs {
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: cidr},
		})
	}
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: peers}},
	}, true
}

// PolicyConfig is the admission-policy half of the operator's configuration:
// what it takes to prove an image is trustworthy here. Signing identity is an
// organisation-level fact, so like NetworkConfig it is set on the operator,
// not per service.
type PolicyConfig struct {
	// CosignPublicKey is a PEM public key, typically mounted from a Secret.
	CosignPublicKey string
	// CosignIssuer and CosignSubject configure keyless (Fulcio) verification.
	CosignIssuer  string
	CosignSubject string
	// ImageReferences are the image globs verification applies to.
	ImageReferences []string
}

// ImageVerificationConfigured reports whether the operator was given anything
// to verify signatures against. Without it the golden path ships the workload
// baseline and says in status that verification was not configured — it does
// not pretend to verify.
func (p PolicyConfig) ImageVerificationConfigured() bool {
	return p.CosignPublicKey != "" || (p.CosignIssuer != "" && p.CosignSubject != "")
}

func (p PolicyConfig) imageReferences() []any {
	refs := p.ImageReferences
	if len(refs) == 0 {
		refs = []string{"*"}
	}
	out := make([]any, 0, len(refs))
	for _, r := range refs {
		out = append(out, r)
	}
	return out
}

// attestor renders the Cosign trust root. A key, when one is configured,
// otherwise the keyless identity.
func (p PolicyConfig) attestor() map[string]any {
	if p.CosignPublicKey != "" {
		return map[string]any{"keys": map[string]any{"publicKeys": p.CosignPublicKey}}
	}
	return map[string]any{"keyless": map[string]any{
		"issuer":  p.CosignIssuer,
		"subject": p.CosignSubject,
	}}
}

// KyvernoPolicySpec builds the admission baseline for one service.
//
// The workload rules are deliberately written with Kyverno's optional anchors
// (`=(field)`), so a pod that never mentions the field passes. The baseline
// rejects pods that ask for privilege, not pods that forgot to say they don't
// want it — a baseline that fails every existing manifest gets switched off,
// and a switched-off baseline protects nothing.
func KyvernoPolicySpec(p PolicyConfig) map[string]any {
	rules := []any{
		map[string]any{
			"name":  "disallow-privileged",
			"match": podMatch(),
			"validate": map[string]any{
				"failureAction": "Enforce",
				"message":       "privileged containers are not allowed on the golden path",
				"pattern": map[string]any{
					"spec": map[string]any{
						"=(ephemeralContainers)": []any{privilegedPattern()},
						"=(initContainers)":      []any{privilegedPattern()},
						"containers":             []any{privilegedPattern()},
					},
				},
			},
		},
		map[string]any{
			"name":  "disallow-host-namespaces",
			"match": podMatch(),
			"validate": map[string]any{
				"failureAction": "Enforce",
				"message":       "host namespaces (hostNetwork, hostPID, hostIPC) are not allowed on the golden path",
				"pattern": map[string]any{
					"spec": map[string]any{
						"=(hostNetwork)": "false",
						"=(hostPID)":     "false",
						"=(hostIPC)":     "false",
					},
				},
			},
		},
	}

	if p.ImageVerificationConfigured() {
		rules = append(rules, map[string]any{
			"name":  "verify-image-signatures",
			"match": podMatch(),
			"verifyImages": []any{map[string]any{
				"imageReferences": p.imageReferences(),
				"failureAction":   "Enforce",
				"required":        true,
				// Kyverno rewrites the tag to the verified digest by default.
				// This operator also gives every service an ArgoCD Application
				// with selfHeal on, so that rewrite becomes a fight: Kyverno
				// mutates the image at admission, ArgoCD sees live drift from
				// Git and syncs the tag back, Kyverno mutates again. Verify
				// the signature; leave the reference alone.
				"mutateDigest": false,
				// With no mutation, demanding a digest would reject every
				// tag-based manifest. Pinning belongs in the build pipeline
				// that produces the manifests, not in admission.
				"verifyDigest": false,
				"attestors":    []any{map[string]any{"entries": []any{p.attestor()}}},
			}},
		})
	}

	return map[string]any{"rules": rules}
}

// KyvernoRuleNames lists the rules a given config actually ships, for the
// status message — so the condition says what is enforced, not just "ready".
func KyvernoRuleNames(p PolicyConfig) []string {
	names := []string{"disallow-privileged", "disallow-host-namespaces"}
	if p.ImageVerificationConfigured() {
		names = append(names, "verify-image-signatures")
	}
	return names
}

func podMatch() map[string]any {
	return map[string]any{
		"any": []any{map[string]any{"resources": map[string]any{"kinds": []any{"Pod"}}}},
	}
}

func privilegedPattern() map[string]any {
	return map[string]any{
		"=(securityContext)": map[string]any{"=(privileged)": "false"},
	}
}
