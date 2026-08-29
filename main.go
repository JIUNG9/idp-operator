// idp-operator: one ServiceDeployment manifest provisions the golden path —
// namespace, SLO baseline, disruption budget, and ArgoCD delivery.
package main

import (
	"os"

	corev1 "k8s.io/api/core/v1"
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
	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))
	setupLog := ctrl.Log.WithName("setup")

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
