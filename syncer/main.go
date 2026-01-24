package main

import (
	"context"
	goflag "flag"
	"fmt"
	krmv1alpha1 "github.com/gke-labs/kube-etl/api/v1alpha1"
	"os"

	"k8s.io/klog/v2"

	flag "github.com/spf13/pflag"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"

	krmSyncer "github.com/gke-labs/kube-etl/controllers"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(krmv1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	var metricsAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	// Configure logging
	klogFlagSet := goflag.NewFlagSet("klog", goflag.ExitOnError)
	klog.InitFlags(klogFlagSet)
	// Support default klog verbosity `-v`
	flag.CommandLine.AddGoFlag(klogFlagSet.Lookup("v"))
	flag.CommandLine.AddGoFlagSet(goflag.CommandLine)
	flag.Parse()

	ctx = klog.NewContext(ctx, setupLog)
	ctrl.SetLogger(klog.NewKlogr())

	setupLog.Info("Creating manager")
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
	}

	// Create and set up the SyncerReconciler
	setupLog.Info("Creating SyncerReconciler")
	if err = (&krmSyncer.KRMSyncerReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Manager: mgr,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Syncer")
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
	}
	return nil
}
