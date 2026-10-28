package main

import (
	"flag"
	"os"
	"time"

	widgetv1alpha1 "github.com/sojoner/kro-lab/platform-mvp/widget-operator/api/v1alpha1"
	"github.com/sojoner/kro-lab/platform-mvp/widget-operator/controller"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// defaultReadyDelay is how long a Widget stays Pending before the reconciler
// marks it Ready. Overridable via --ready-delay so it isn't a hardcoded
// behavior baked into the binary.
const defaultReadyDelay = 2 * time.Second

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log

	readyDelay := flag.Duration("ready-delay", defaultReadyDelay, "delay before a Widget transitions from Pending to Ready")
	flag.Parse()

	scheme := runtime.NewScheme()
	if err := widgetv1alpha1.AddToScheme(scheme); err != nil {
		log.Error(err, "failed to add widget scheme")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "failed to create manager")
		os.Exit(1)
	}

	r := &controller.WidgetReconciler{Client: mgr.GetClient(), ReadyDelay: *readyDelay}
	if err := controller.SetupWithManager(mgr, r); err != nil {
		log.Error(err, "failed to setup widget controller")
		os.Exit(1)
	}

	log.Info("starting widget operator", "readyDelay", readyDelay.String())
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
