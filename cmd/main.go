package main

import (
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
	cf "github.com/SteadyInfraTech/ai-agent-edge-tunnel/internal/cloudflare"
	"github.com/SteadyInfraTech/ai-agent-edge-tunnel/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agentopsv1alpha1.AddToScheme(scheme))
}

func main() {
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(nil)
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	accountID := os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	apiToken := os.Getenv("CLOUDFLARE_API_TOKEN")

	var cfClient cf.TunnelService
	if accountID != "" && apiToken != "" {
		setupLog.Info("Using Real Cloudflare Client")
		realClient, err := cf.NewRealCloudflareClient(accountID, apiToken)
		if err != nil {
			setupLog.Error(err, "unable to initialize Cloudflare client")
			os.Exit(1)
		}
		cfClient = realClient
	} else {
		setupLog.Info("CLOUDFLARE_API_TOKEN/ACCOUNT_ID not provided; using Mock Cloudflare Client")
		cfClient = cf.NewMockCloudflareClient()
	}

	if err = (&controller.AgentEdgeTunnelReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		CloudflareClient: cfClient,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgentEdgeTunnel")
		os.Exit(1)
	}

	setupLog.Info("Starting manager for ai-agent-edge-tunnel")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
