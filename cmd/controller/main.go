package main

import (
	"flag"
	"os"

	discoveryv1 "k8s.io/api/discovery/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/embewi/core/api/v1alpha1"
	"github.com/embewi/core/internal/controller"
	"github.com/embewi/core/internal/heartbeat"
	"github.com/embewi/core/internal/oci"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(discoveryv1.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr   string
		probeAddr     string
		heartbeatAddr string
		tokenSecret   string
		leaderElect   bool
	)
	flag.StringVar(&metricsAddr,   "metrics-bind-address", ":8082",         "Adresse des métriques")
	flag.StringVar(&probeAddr,     "health-probe-address", ":8083",         "Adresse des health probes")
	flag.StringVar(&heartbeatAddr, "heartbeat-address",    ":8080",         "Adresse du serveur heartbeat ESP→Core")
	flag.StringVar(&tokenSecret,   "token-secret",         "embewi-tokens", "Nom du Secret K8s contenant les tokens Bearer")
	flag.BoolVar(&leaderElect,     "leader-elect",         false,           "Activer l'élection de leader")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{})))
	logger := ctrl.Log.WithName("main")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "embewi-core-leader",
	})
	if err != nil {
		logger.Error(err, "NewManager failed")
		os.Exit(1)
	}

	// Client OCI configuré via variables d'environnement.
	// OCI_REGISTRY_USER / OCI_REGISTRY_PASS : auth Basic (Harbor, Zot, etc.)
	// OCI_INSECURE_TLS=true                 : skip vérification certificat TLS
	ociOpts := []oci.Option{}
	if user := os.Getenv("OCI_REGISTRY_USER"); user != "" {
		ociOpts = append(ociOpts, oci.WithBasicAuth(user, os.Getenv("OCI_REGISTRY_PASS")))
	}
	if os.Getenv("OCI_INSECURE_TLS") == "true" {
		ociOpts = append(ociOpts, oci.WithInsecureTLS())
	}

	// McuNode controller — EndpointSlice + timeout heartbeat.
	if err := (&controller.McuNodeReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "McuNodeReconciler setup failed")
		os.Exit(1)
	}

	// McuDeployment controller — cycle OTA complet.
	// Les tokens Bearer sont lus depuis le Secret K8s `tokenSecret` (clé = nodeId).
	if err := (&controller.McuDeploymentReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		OCI:         oci.New(ociOpts...),
		TokenSecret: tokenSecret,
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "McuDeploymentReconciler setup failed")
		os.Exit(1)
	}

	// Serveur heartbeat : reçoit les POST ESP→Core, met à jour les McuNode status.
	hbSrv := heartbeat.New(heartbeatAddr, mgr.GetClient())
	if err := mgr.Add(hbSrv); err != nil {
		logger.Error(err, "heartbeat server add failed")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		logger.Error(err, "healthz check failed")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		logger.Error(err, "readyz check failed")
		os.Exit(1)
	}

	logger.Info("démarrage embewi-core", "heartbeat", heartbeatAddr, "tokenSecret", tokenSecret)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
