package main

import (
	"errors"
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
	cf "github.com/SteadyInfraTech/ai-agent-edge-tunnel/internal/cloudflare"
	"github.com/SteadyInfraTech/ai-agent-edge-tunnel/internal/cloudflare/fake"
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

// main parses flags, builds the Cloudflare client (real, or the in-memory fake
// with --dry-run) and runs the controller manager.
func main() {
	var (
		dryRun     bool
		baseDomain string
	)
	flag.BoolVar(&dryRun, "dry-run", false,
		"Use an in-memory fake instead of the Cloudflare API. Nothing is provisioned; for local development only.")
	flag.StringVar(&baseDomain, "base-domain", os.Getenv("EDGE_BASE_DOMAIN"),
		"Domain under which agent hostnames are created, e.g. agents.example.com. Env: EDGE_BASE_DOMAIN.")

	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Validate configuration before touching the cluster. A missing credential
	// must be a startup failure: silently falling back to a fake would report
	// tunnels as Active that do not exist.
	var cfClient cf.TunnelService
	if dryRun {
		setupLog.Info("DRY RUN: using an in-memory Cloudflare fake; no real resources will be created")
		cfClient = fake.New()
		if baseDomain == "" {
			baseDomain = "agents.example.invalid"
		}
	} else {
		accountID := os.Getenv("CLOUDFLARE_ACCOUNT_ID")
		apiToken := os.Getenv("CLOUDFLARE_API_TOKEN")
		if accountID == "" || apiToken == "" {
			setupLog.Error(errors.New("missing credentials"),
				"CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_API_TOKEN are required (use --dry-run for local development)")
			os.Exit(1)
		}
		if baseDomain == "" {
			setupLog.Error(errors.New("missing base domain"), "--base-domain or EDGE_BASE_DOMAIN is required")
			os.Exit(1)
		}
		realClient, err := cf.NewRealCloudflareClient(accountID, apiToken)
		if err != nil {
			setupLog.Error(err, "unable to initialize Cloudflare client")
			os.Exit(1)
		}
		cfClient = realClient
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.AgentEdgeTunnelReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		APIReader:        mgr.GetAPIReader(),
		CloudflareClient: cfClient,
		BaseDomain:       baseDomain,
		DryRun:           dryRun,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgentEdgeTunnel")
		os.Exit(1)
	}

	setupLog.Info("Starting manager for ai-agent-edge-tunnel", "baseDomain", baseDomain)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
