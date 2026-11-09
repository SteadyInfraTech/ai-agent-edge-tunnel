package main

import (
	"errors"
	"flag"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
	cf "github.com/SteadyInfraTech/ai-agent-edge-tunnel/internal/cloudflare"
	"github.com/SteadyInfraTech/ai-agent-edge-tunnel/internal/cloudflare/fake"
	"github.com/SteadyInfraTech/ai-agent-edge-tunnel/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// init registers the Kubernetes and operator API types with the scheme.
func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agentopsv1alpha1.AddToScheme(scheme))
}

// namespaceConfig turns a comma-separated list into the cache's namespace
// restriction. An empty list returns nil, which means every namespace.
func namespaceSet(list string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, ns := range strings.Split(list, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			out[ns] = struct{}{}
		}
	}
	return out
}

func namespaceConfig(list string) map[string]cache.Config {
	var out map[string]cache.Config
	for _, ns := range strings.Split(list, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			if out == nil {
				out = make(map[string]cache.Config)
			}
			out[ns] = cache.Config{}
		}
	}
	return out
}

// main parses flags, builds the Cloudflare client (real, or the in-memory fake
// with --dry-run) and runs the controller manager.
func main() {
	var (
		dryRun         bool
		baseDomain     string
		connectorImage string
		leaderElect    bool
		probeAddr      string
		metricsAddr    string
		watchNS        string
		zoneID         string
	)
	flag.BoolVar(&dryRun, "dry-run", false,
		"Use an in-memory fake instead of the Cloudflare API. Nothing is provisioned in Cloudflare; for local development only.")
	flag.StringVar(&baseDomain, "base-domain", os.Getenv("EDGE_BASE_DOMAIN"),
		"Domain under which agent hostnames are created, e.g. agents.example.com. Env: EDGE_BASE_DOMAIN.")
	flag.StringVar(&connectorImage, "connector-image", os.Getenv("CONNECTOR_IMAGE"),
		"cloudflared image run for each tunnel with spec.service. Pin a version or digest. Env: CONNECTOR_IMAGE.")
	flag.BoolVar(&leaderElect, "leader-elect", false,
		"Enable leader election so only one replica reconciles. Required when running more than one replica.")
	flag.StringVar(&watchNS, "watch-namespaces", os.Getenv("EDGE_WATCH_NAMESPACES"),
		"Comma-separated namespaces to watch. Empty watches every namespace and needs cluster-wide RBAC; set it to run with namespaced Roles only (see config/overlays/namespaced). Env: EDGE_WATCH_NAMESPACES.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0",
		"Address for the Prometheus metrics endpoint, e.g. :8080. \"0\" disables it. Served over plain HTTP without authentication, so restrict access with a NetworkPolicy.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for the /healthz and /readyz endpoints.")

	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Validate configuration before touching the cluster. A missing credential
	// must be a startup failure: silently falling back to a fake would report
	// tunnels as Active that do not exist.
	var (
		cfClient  cf.TunnelService
		accountID string
	)
	publicNamespaces := namespaceSet(os.Getenv("EDGE_PUBLIC_NAMESPACES"))
	if dryRun {
		setupLog.Info("DRY RUN: using an in-memory Cloudflare fake; no real Cloudflare resources will be created")
		cfClient = fake.New()
		accountID = "fake-account"
		zoneID = "fake-zone"
		if zoneID == "" {
			setupLog.Error(errors.New("missing Cloudflare zone"), "CLOUDFLARE_ZONE_ID is required to pin the operator to one Cloudflare zone")
			os.Exit(1)
		}
		if baseDomain == "" {
			baseDomain = "agents.example.invalid"
		}
		if connectorImage == "" {
			connectorImage = "example.invalid/cloudflared:dry-run"
		}
	} else {
		accountID = os.Getenv("CLOUDFLARE_ACCOUNT_ID")
		apiToken := os.Getenv("CLOUDFLARE_API_TOKEN")
		zoneID = os.Getenv("CLOUDFLARE_ZONE_ID")
		if accountID == "" || apiToken == "" {
			setupLog.Error(errors.New("missing credentials"),
				"CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_API_TOKEN are required (use --dry-run for local development)")
			os.Exit(1)
		}
		if baseDomain == "" {
			setupLog.Error(errors.New("missing base domain"), "--base-domain or EDGE_BASE_DOMAIN is required")
			os.Exit(1)
		}
		if connectorImage == "" {
			setupLog.Error(errors.New("missing connector image"), "--connector-image or CONNECTOR_IMAGE is required; pin a cloudflared version or digest")
			os.Exit(1)
		}
		realClient, err := cf.NewRealCloudflareClient(accountID, apiToken)
		if err != nil {
			setupLog.Error(err, "unable to initialize Cloudflare client")
			os.Exit(1)
		}
		cfClient = realClient
	}

	if err := validateBaseDomain(baseDomain); err != nil {
		setupLog.Error(err, "invalid --base-domain / EDGE_BASE_DOMAIN")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		Cache:                  cache.Options{DefaultNamespaces: namespaceConfig(watchNS)},
		LeaderElection:         leaderElect,
		LeaderElectionID:       "edge-tunnel.steadyinfra.com",
		// Secrets are read straight from the API server. Caching them would
		// start a cluster-wide Secret informer, which needs list and watch on
		// all Secrets; the operator's RBAC deliberately grants neither.
		Client: client.Options{
			Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.AgentEdgeTunnelReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		CloudflareClient: cfClient,
		AccountID:        accountID,
		ZoneID:           zoneID,
		PublicNamespaces: publicNamespaces,
		BaseDomain:       baseDomain,
		ConnectorImage:   connectorImage,
		DryRun:           dryRun,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgentEdgeTunnel")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager for ai-agent-edge-tunnel", "baseDomain", baseDomain, "connectorImage", connectorImage, "leaderElection", leaderElect, "watchNamespaces", watchNS, "publicNamespaces", len(publicNamespaces), "zoneID", zoneID)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
