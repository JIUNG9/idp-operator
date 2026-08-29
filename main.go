// idp-operator: one ServiceDeployment manifest provisions the golden path —
// namespace, SLO baseline, disruption budget, policy, and ArgoCD delivery.
package main

import (
	"flag"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/labels"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	idpv1alpha1 "github.com/JIUNG9/idp-operator/api/v1alpha1"
	"github.com/JIUNG9/idp-operator/internal/budget"
	"github.com/JIUNG9/idp-operator/internal/controller"
)

func main() {
	// Cluster-level configuration. The ingress topology and the signing
	// identity are properties of the platform, so they are set once here
	// rather than repeated in every team's ServiceDeployment.
	var (
		cosignKeyFile     = flag.String("cosign-key-file", "", "path to a PEM public key (typically a mounted Secret) that container images must be signed with")
		cosignIssuer      = flag.String("cosign-issuer", "", "OIDC issuer for keyless (Fulcio) image verification")
		cosignSubject     = flag.String("cosign-subject", "", "OIDC subject for keyless (Fulcio) image verification")
		imageReferences   = flag.String("verify-image-references", "*", "comma-separated image globs that signature verification applies to")
		requireDigests    = flag.Bool("require-image-digests", false, "reject images referenced by tag; a verified tag can be moved afterwards, so this is what makes verification hold. Enable once your pipeline pins digests")
		ingressNamespaces = flag.String("allow-ingress-from-namespaces", "", "comma-separated namespaces allowed to reach services, e.g. the ingress controller's")
		ingressCIDRs      = flag.String("allow-ingress-from-cidrs", "", "comma-separated CIDRs allowed to reach services, for load balancers that target pod IPs directly")

		prometheusURL = flag.String("prometheus-url", "", "Prometheus base URL for error-budget readings. Empty means delivery is not budget-gated at all")
		burnQuery     = flag.String("burn-query", "", "Go template for the instant query returning a service's error ratio; {{.Namespace}}, {{.Name}} and {{.Window}} are substituted. Defaults to an http_requests_total query")
		burnTimeout   = flag.Duration("burn-query-timeout", 10*time.Second, "how long to wait for a burn reading before failing closed")
		staticBudget  = flag.Float64("static-budget", -1, "fixed error budget fraction in [0,1] instead of querying Prometheus. For demos and testing only")
	)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))
	setupLog := ctrl.Log.WithName("setup")

	// Fail fast on a half-configured trust root. Every one of these would
	// otherwise start cleanly and verify against the wrong thing, or against
	// nothing at all, which is the failure you least want to discover from a
	// running cluster.
	switch {
	case *cosignKeyFile != "" && (*cosignIssuer != "" || *cosignSubject != ""):
		setupLog.Error(nil, "set either -cosign-key-file or -cosign-issuer with -cosign-subject, not both: "+
			"a key and a keyless identity are two different trust roots")
		os.Exit(1)
	case (*cosignIssuer == "") != (*cosignSubject == ""):
		setupLog.Error(nil, "keyless verification needs both -cosign-issuer and -cosign-subject; "+
			"with only one, images would silently not be verified at all")
		os.Exit(1)
	}

	policyConfig := controller.PolicyConfig{
		CosignIssuer:        *cosignIssuer,
		CosignSubject:       *cosignSubject,
		ImageReferences:     splitList(*imageReferences),
		RequireImageDigests: *requireDigests,
	}
	if *cosignKeyFile != "" {
		// Read once at startup: rotating the key means restarting the
		// operator, and that restart re-reconciles every service anyway.
		key, err := os.ReadFile(*cosignKeyFile)
		if err != nil {
			setupLog.Error(err, "unable to read cosign key file", "path", *cosignKeyFile)
			os.Exit(1)
		}
		policyConfig.CosignPublicKey = string(key)
	}
	if !policyConfig.ImageVerificationConfigured() {
		setupLog.Info("no signing identity configured; image signatures will NOT be verified " +
			"(set -cosign-key-file, or -cosign-issuer with -cosign-subject)")
		if *requireDigests {
			// The digest requirement lives inside the verification rule, so
			// without a signing identity there is no rule to carry it.
			setupLog.Info("-require-image-digests has no effect without a signing identity")
		}
	}

	// The burn source. Without one, nothing is budget-gated and the status
	// says so on every service rather than implying a gate that isn't there.
	var burnSource budget.Source
	switch {
	case *prometheusURL != "" && *staticBudget >= 0:
		setupLog.Error(nil, "set either -prometheus-url or -static-budget, not both")
		os.Exit(1)
	case *prometheusURL != "":
		src, err := budget.NewPrometheusSource(*prometheusURL, *burnQuery, *burnTimeout)
		if err != nil {
			setupLog.Error(err, "unable to build the burn source")
			os.Exit(1)
		}
		burnSource = src
	case *staticBudget >= 0:
		setupLog.Info("using a STATIC error budget; delivery gating will not reflect real burn",
			"budget", *staticBudget)
		burnSource = budget.StaticSource{Value: *staticBudget}
	default:
		setupLog.Info("no burn source configured; delivery is not budget-gated " +
			"(set -prometheus-url to enable it)")
	}

	scheme := clientgoscheme.Scheme
	if err := idpv1alpha1.AddToScheme(scheme); err != nil {
		setupLog.Error(err, "unable to register API types")
		os.Exit(1)
	}

	// The drift watches are label-mapped, so the cache only needs the
	// objects this operator stamped — not every ConfigMap in the cluster.
	managed := labels.SelectorFromSet(labels.Set{controller.LabelManagedBy: controller.ManagedByValue})
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Namespace{}:             {Label: managed},
				&corev1.ConfigMap{}:             {Label: managed},
				&policyv1.PodDisruptionBudget{}: {Label: managed},
				&networkingv1.NetworkPolicy{}:   {Label: managed},
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Reader bypasses the filtered cache: the namespace-ownership guard must
	// see namespaces this operator does NOT manage.
	if err := (&controller.ServiceDeploymentReconciler{
		Client: mgr.GetClient(),
		Reader: mgr.GetAPIReader(),
		Network: controller.NetworkConfig{
			AllowIngressFromNamespaces: splitList(*ingressNamespaces),
			AllowIngressFromCIDRs:      splitList(*ingressCIDRs),
		},
		Policy:     policyConfig,
		BurnSource: burnSource,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ServiceDeployment")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
