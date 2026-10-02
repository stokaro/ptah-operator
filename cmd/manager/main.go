package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	approvaladmission "github.com/stokaro/ptah-operator/internal/admission"
	"github.com/stokaro/ptah-operator/internal/controller"
	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/controllerwrite"
	"github.com/stokaro/ptah-operator/internal/managercache"
	"github.com/stokaro/ptah-operator/internal/planseal"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/resultconsumer"
	"github.com/stokaro/ptah-operator/internal/resultservice"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/telemetry"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const (
	mutateApprovalPath          = "/mutate-operator-ptah-run-v1alpha1-ptahschemaapproval"
	validateApprovalPath        = "/validate-operator-ptah-run-v1alpha1-ptahschemaapproval"
	mutateMigrationApprovalPath = "/mutate-operator-ptah-run-v1alpha1-ptahmigrationapproval"

	validateMigrationApprovalPath = "/validate-operator-ptah-run-v1alpha1-ptahmigrationapproval"
	mutateRunAcknowledgmentPath   = "/mutate-operator-ptah-run-v1alpha1-ptahmigrationrunacknowledgment"
	validateRunAcknowledgmentPath = "/validate-operator-ptah-run-v1alpha1-ptahmigrationrunacknowledgment"
	mutateSchemaSpecWriterPath    = "/mutate-operator-ptah-run-v1alpha1-ptahschema"
	mutateMigrationSpecWriterPath = "/mutate-operator-ptah-run-v1alpha1-ptahmigration"
	validatePodIntentPath         = "/validate-v1-pod-ptah-operation-intent"
	validateControllerWritePath   = "/validate-operator-controller-write"
	leaderElectionID              = "ptah-operator.operator.ptah.run"
)

// controllerRevision is injected by the release build. An unversioned manager
// must fail before it can interpret or write durable controller state.
var controllerRevision string

func main() {
	var metricsAddress string
	var probeAddress string
	var leaderElection bool
	var controllerImage string
	var executorImage string
	var runnerImage string
	var ptahVersion string
	var targetLockNamespace string
	var controllerServiceAccountUsername string
	var webhookCertDir string
	var webhookPort int
	var resultEndpoint, resultCertDir, resultAddress string
	var defaultTolerationsEnabled bool
	var defaultNotReadyTolerationSeconds int64
	var defaultUnreachableTolerationSeconds int64
	var extendedResourceTolerationEnabled bool
	var alwaysPullImagesEnabled bool
	var requireDistinctApprover bool

	flag.StringVar(&metricsAddress, "metrics-bind-address", ":8080", "address for Prometheus metrics")
	flag.StringVar(&probeAddress, "health-probe-bind-address", ":8081", "address for health probes")
	flag.BoolVar(&leaderElection, "leader-elect", true, "use a Kubernetes Lease for leader election")
	flag.StringVar(&controllerImage, "controller-image", "", "content-addressed manager image identity")
	flag.StringVar(&executorImage, "executor-image", "", "content-addressed Ptah executor image")
	flag.StringVar(&runnerImage, "runner-image", "", "content-addressed operator runner image")
	flag.StringVar(&ptahVersion, "ptah-version", "", "Ptah CLI version bound into plans")
	flag.StringVar(&targetLockNamespace, "target-lock-namespace", os.Getenv("POD_NAMESPACE"), "shared namespace for database target Leases")
	flag.StringVar(&controllerServiceAccountUsername, "controller-service-account-username", "", "exact Kubernetes username of the operator manager ServiceAccount")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs", "directory containing tls.crt and tls.key")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "approval webhook TLS port")
	flag.StringVar(&resultEndpoint, "result-endpoint", "", "HTTPS origin for durable runner results; requires result-cert-dir")
	flag.StringVar(&resultCertDir, "result-cert-dir", "", "directory containing dedicated result server and client-signing trust")
	flag.StringVar(&resultAddress, "result-bind-address", ":9444", "listen address for the durable result receiver")
	flag.BoolVar(&defaultTolerationsEnabled, "default-tolerations-enabled", true, "whether kube-apiserver enables DefaultTolerationSeconds admission")
	flag.Int64Var(&defaultNotReadyTolerationSeconds, "default-not-ready-toleration-seconds", 300, "expected kube-apiserver not-ready NoExecute toleration seconds")
	flag.Int64Var(&defaultUnreachableTolerationSeconds, "default-unreachable-toleration-seconds", 300, "expected kube-apiserver unreachable NoExecute toleration seconds")
	flag.BoolVar(&extendedResourceTolerationEnabled, "extended-resource-toleration-enabled", false, "whether kube-apiserver enables ExtendedResourceToleration admission")
	flag.BoolVar(&alwaysPullImagesEnabled, "always-pull-images-enabled", false, "whether kube-apiserver enables AlwaysPullImages admission")
	flag.BoolVar(&requireDistinctApprover, "require-distinct-approver", false, "refuse an approval whose approver is the identity that last wrote the resource's spec")
	zapOptions := zap.Options{Development: false}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))
	log := ctrl.Log.WithName("setup")

	// Generated once per process and never persisted: a restart gets a new
	// key, which is exactly what makes a Plan sealed to the old one safe to
	// abandon and re-plan rather than wait for. See internal/planseal.
	sealKey, err := planseal.Generate()
	if err != nil {
		log.Error(err, "generate the Plan payload seal key")
		os.Exit(1)
	}

	builder := workload.Builder{
		ExecutorImage:          executorImage,
		RunnerImage:            runnerImage,
		PtahVersion:            ptahVersion,
		ControllerImage:        controllerImage,
		ControllerRevision:     controllerRevision,
		ControllerStateVersion: controllerstate.CurrentVersion,
		PlanSealPublicKey:      sealKey.PublicKey(),
		ResultEndpoint:         resultEndpoint,
	}
	if err := builder.Validate(); err != nil {
		log.Error(err, "invalid immutable execution configuration")
		os.Exit(1)
	}
	if problems := validation.IsDNS1123Label(targetLockNamespace); len(problems) != 0 {
		log.Error(fmt.Errorf("target lock namespace is invalid: %s", problems[0]), "invalid coordination configuration")
		os.Exit(1)
	}
	managerIdentity := strings.Split(controllerServiceAccountUsername, ":")
	if len(managerIdentity) != 4 || managerIdentity[0] != "system" || managerIdentity[1] != "serviceaccount" ||
		len(validation.IsDNS1123Label(managerIdentity[2])) != 0 ||
		len(validation.IsDNS1123Subdomain(managerIdentity[3])) != 0 {
		log.Error(fmt.Errorf("controller ServiceAccount username must have the exact system:serviceaccount:<namespace>:<name> form"), "invalid admission configuration")
		os.Exit(1)
	}
	admissionOptions := podintent.Options{
		DefaultTolerationsEnabled:           defaultTolerationsEnabled,
		DefaultNotReadyTolerationSeconds:    defaultNotReadyTolerationSeconds,
		DefaultUnreachableTolerationSeconds: defaultUnreachableTolerationSeconds,
		ExtendedResourceTolerationEnabled:   extendedResourceTolerationEnabled,
		AlwaysPullImagesEnabled:             alwaysPullImagesEnabled,
	}
	if err := admissionOptions.Validate(); err != nil {
		log.Error(err, "invalid Pod admission configuration")
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(batchv1.AddToScheme(scheme))
	utilruntime.Must(coordinationv1.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(operatorv1alpha1.AddToScheme(scheme))

	manager, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Cache:                   managercache.Options(),
		Client:                  managercache.ClientOptions(),
		Metrics:                 metricsserver.Options{BindAddress: metricsAddress},
		HealthProbeBindAddress:  probeAddress,
		LeaderElection:          leaderElection,
		LeaderElectionID:        leaderElectionID,
		LeaderElectionNamespace: targetLockNamespace,
		WebhookServer: webhook.NewServer(webhook.Options{
			Port: webhookPort, CertDir: webhookCertDir,
		}),
	})
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}

	if (resultEndpoint == "") != (resultCertDir == "") {
		log.Error(fmt.Errorf("result-endpoint and result-cert-dir must be configured together"), "invalid result delivery configuration")
		os.Exit(1)
	}
	var results *resultservice.Service
	if resultEndpoint != "" {
		results, err = resultservice.New(resultservice.Config{Endpoint: resultEndpoint, Address: resultAddress, CertificateDirectory: resultCertDir, Uploads: 1, UploadTimeout: 2 * time.Minute, Consumer: resultconsumer.Options{Workers: 1, Entries: 4, Timeout: 30 * time.Second, Retention: time.Minute}}, manager.GetClient(), manager.GetAPIReader())
		if err != nil {
			log.Error(err, "configure durable result service")
			os.Exit(1)
		}
		if err := manager.Add(results); err != nil {
			log.Error(err, "register durable result service")
			os.Exit(1)
		}
		if err := manager.AddReadyzCheck("result-service", results.Ready); err != nil {
			log.Error(err, "register result service readiness")
			os.Exit(1)
		}
	}

	clientset, err := kubernetes.NewForConfig(manager.GetConfig())
	if err != nil {
		log.Error(err, "create Kubernetes clientset")
		os.Exit(1)
	}
	operatorMetrics := telemetry.New(ctrlmetrics.Registry)
	// Read alert state directly from the API once per leader scrape. A warm
	// controller cache can keep serving stale state after API access fails.
	stateMetrics := telemetry.NewSnapshotCollector(ctrlmetrics.Registry, manager.GetAPIReader(), nil)
	if err := manager.Add(stateMetrics); err != nil {
		log.Error(err, "register the leader's state metrics")
		os.Exit(1)
	}
	telemetry.NewCertificateCollector(ctrlmetrics.Registry, filepath.Join(webhookCertDir, "tls.crt"))
	// Both families are indexed by coordination realm before either controller
	// is registered. The realm census reads both kinds whichever controller
	// asks for it, so neither can own the registration on its own.
	if err := controller.RegisterRealmIndexes(context.Background(), manager.GetFieldIndexer()); err != nil {
		log.Error(err, "index resources by coordination realm")
		os.Exit(1)
	}
	reconciler := &controller.SchemaReconciler{
		Client: manager.GetClient(), APIReader: manager.GetAPIReader(), Scheme: manager.GetScheme(),
		Recorder:         manager.GetEventRecorderFor("ptah-schema-controller"),
		Logs:             controller.ClientsetPodLogs{Client: clientset},
		Jobs:             builder,
		SealKey:          sealKey,
		Plans:            planstore.Store{Client: manager.GetClient(), Reader: manager.GetAPIReader()},
		Locks:            targetlock.New(manager.GetAPIReader(), manager.GetClient(), nil),
		LockNamespace:    targetLockNamespace,
		Telemetry:        operatorMetrics,
		AdmissionOptions: admissionOptions,
	}
	if results != nil {
		reconciler.Results = results.Consumer
		reconciler.ResultCredentials = results
	}
	if err := reconciler.SetupWithManager(manager); err != nil {
		log.Error(err, "register PtahSchema controller")
		os.Exit(1)
	}
	migrations := &controller.MigrationReconciler{
		Client: manager.GetClient(), APIReader: manager.GetAPIReader(), Scheme: manager.GetScheme(),
		Recorder:         manager.GetEventRecorderFor("ptah-migration-controller"),
		Logs:             controller.ClientsetPodLogs{Client: clientset},
		Jobs:             builder,
		Locks:            targetlock.New(manager.GetAPIReader(), manager.GetClient(), nil),
		LockNamespace:    targetLockNamespace,
		Telemetry:        operatorMetrics,
		AdmissionOptions: admissionOptions,
	}
	if results != nil {
		migrations.Results = results.Consumer
		migrations.ResultCredentials = results
	}
	if err := migrations.SetupWithManager(manager); err != nil {
		log.Error(err, "register PtahMigration controller")
		os.Exit(1)
	}

	decoder := cradmission.NewDecoder(manager.GetScheme())
	// Approvals are judged against what this manager executes with, and not
	// against its own release: a plan an earlier release published stays
	// approvable while the execution binding is the same.
	var execution approvaladmission.Execution
	execution.ControllerStateVersion, execution.PtahVersion, execution.ExecutorImage,
		execution.RunnerProtocolVersion = builder.ExecutionBinding()
	manager.GetWebhookServer().Register(mutateApprovalPath, &cradmission.Webhook{Handler: &approvaladmission.ApprovalHandler{
		Reader: manager.GetAPIReader(), Decoder: decoder, Mutate: true, Execution: execution,
		RequireDistinctApprover: requireDistinctApprover,
	}})
	manager.GetWebhookServer().Register(validateApprovalPath, &cradmission.Webhook{Handler: &approvaladmission.ApprovalHandler{
		Reader: manager.GetAPIReader(), Decoder: decoder, Mutate: false, Execution: execution,
		RequireDistinctApprover: requireDistinctApprover,
	}})
	manager.GetWebhookServer().Register(mutateMigrationApprovalPath, &cradmission.Webhook{Handler: &approvaladmission.MigrationApprovalHandler{
		Reader: manager.GetAPIReader(), Decoder: decoder, Mutate: true, Execution: execution,
		RequireDistinctApprover: requireDistinctApprover,
	}})
	manager.GetWebhookServer().Register(validateMigrationApprovalPath, &cradmission.Webhook{Handler: &approvaladmission.MigrationApprovalHandler{
		Reader: manager.GetAPIReader(), Decoder: decoder, Mutate: false, Execution: execution,
		RequireDistinctApprover: requireDistinctApprover,
	}})
	// An acknowledgment is how a person settles a run the migration recorded
	// as unresolved. It is stamped and checked the way an approval is, and it
	// is not held to the execution binding: it approves no plan.
	manager.GetWebhookServer().Register(mutateRunAcknowledgmentPath, &cradmission.Webhook{Handler: &approvaladmission.RunAcknowledgmentHandler{
		Reader: manager.GetAPIReader(), Decoder: decoder, Mutate: true,
	}})
	manager.GetWebhookServer().Register(validateRunAcknowledgmentPath, &cradmission.Webhook{Handler: &approvaladmission.RunAcknowledgmentHandler{
		Reader: manager.GetAPIReader(), Decoder: decoder, Mutate: false,
	}})
	manager.GetWebhookServer().Register(mutateSchemaSpecWriterPath, &cradmission.Webhook{Handler: &approvaladmission.SchemaSpecWriterHandler{
		Decoder: decoder,
	}})
	manager.GetWebhookServer().Register(mutateMigrationSpecWriterPath, &cradmission.Webhook{Handler: &approvaladmission.MigrationSpecWriterHandler{
		Decoder: decoder,
	}})
	manager.GetWebhookServer().Register(validatePodIntentPath, &cradmission.Webhook{Handler: &podintent.ValidationHandler{
		Reader: manager.GetAPIReader(), Decoder: decoder,
	}})
	writeValidator := &controllerwrite.Validator{Reader: manager.GetAPIReader(), Jobs: builder, ManagerUsername: controllerServiceAccountUsername}
	if results != nil {
		writeValidator.ResultCredentials = results
	}
	manager.GetWebhookServer().Register(validateControllerWritePath, &cradmission.Webhook{Handler: &controllerwrite.ValidationHandler{Validator: writeValidator}})

	if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "register health check")
		os.Exit(1)
	}
	if err := manager.AddReadyzCheck("webhook-started", manager.GetWebhookServer().StartedChecker()); err != nil {
		log.Error(err, "register readiness check")
		os.Exit(1)
	}

	log.Info("starting manager",
		"controllerImage", controllerImage,
		"controllerRevision", controllerRevision,
		"controllerStateVersion", controllerstate.CurrentVersion,
		"ptahVersion", ptahVersion,
		"runnerProtocol", workload.ProtocolVersion,
		"coordinationNamespace", targetLockNamespace,
		"leaderElection", leaderElection,
		"leaderElectionID", leaderElectionID,
	)
	if err := manager.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager stopped")
		_, _ = fmt.Fprintln(os.Stderr, "ptah-operator manager stopped")
		os.Exit(1)
	}
}
