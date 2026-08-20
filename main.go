// idp-operator: one ServiceDeployment manifest provisions the golden path —
// namespace, SLO baseline, disruption budget, and ArgoCD delivery.
package main

import (
	"os"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
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

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.ServiceDeploymentReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ServiceDeployment")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}
