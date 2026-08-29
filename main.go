// idp-operator: one ServiceDeployment manifest provisions the golden path —
// namespace, SLO baseline, disruption budget, policy, and ArgoCD delivery.
package main

import (
	"flag"
	"os"
	"strings"

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
		ingressNamespaces = flag.String("allow-ingress-from-namespaces", "", "comma-separated namespaces allowed to reach services, e.g. the ingress controller's")
		ingressCIDRs      = flag.String("allow-ingress-from-cidrs", "", "comma-separated CIDRs allowed to reach services, for load balancers that target pod IPs directly")
	)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))
	setupLog := ctrl.Log.WithName("setup")

	policyConfig := controller.PolicyConfig{
		CosignIssuer:    *cosignIssuer,
		CosignSubject:   *cosignSubject,
		ImageReferences: splitList(*imageReferences),
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
		Policy: policyConfig,
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
