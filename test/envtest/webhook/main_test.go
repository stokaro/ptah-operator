// Package webhook_test sends real requests to a kube-apiserver that routes them
// through the chart's own webhook configurations to the manager's own admission
// handlers, served in this process. The unit tests call each handler with a
// hand-built AdmissionReview; here the API server builds the review, decides
// which request reaches which handler from the rules and matchConditions the
// chart renders, and applies the handler's verdict and patch.
package webhook_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	approvaladmission "github.com/stokaro/ptah-operator/internal/admission"
	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/controllerwrite"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/workload"
	"github.com/stokaro/ptah-operator/test/envtest/internal/harness"
)

// The paths cmd/manager/main.go registers. They are repeated because they live
// in package main; TestMain refuses a chart entry that names a path missing
// here, so a rename on either side fails instead of routing to a 404.
const (
	mutateApprovalPath            = "/mutate-operator-ptah-run-v1alpha1-ptahschemaapproval"
	validateApprovalPath          = "/validate-operator-ptah-run-v1alpha1-ptahschemaapproval"
	mutateMigrationApprovalPath   = "/mutate-operator-ptah-run-v1alpha1-ptahmigrationapproval"
	validateMigrationApprovalPath = "/validate-operator-ptah-run-v1alpha1-ptahmigrationapproval"
	mutateSchemaSpecWriterPath    = "/mutate-operator-ptah-run-v1alpha1-ptahschema"
	mutateMigrationSpecWriterPath = "/mutate-operator-ptah-run-v1alpha1-ptahmigration"
	validatePodIntentPath         = "/validate-v1-pod-ptah-operation-intent"
	validateControllerWritePath   = "/validate-operator-controller-write"
)

// controllerRevision stands in for the revision a release build injects into
// the manager. The handlers require one, and the plans the tests write carry
// the same value.
const controllerRevision = "envtest"

var (
	plane  = harness.New(&envtest.Environment{CRDDirectoryPaths: []string{harness.CRDDirectory()}})
	scheme = runtime.NewScheme()

	// manager is the manager's configuration as the chart renders it.
	manager managerConfig
	// admin reads and writes as the envtest administrator. The handlers read
	// through it too: the manager hands them its uncached API reader.
	admin client.Client
	// validatingConfigurationName names the chart's ValidatingWebhookConfiguration.
	validatingConfigurationName string
	// chartMutating and chartValidating are the configurations as rendered,
	// before envtest pointed them at this process.
	chartMutating   *admissionregistrationv1.MutatingWebhookConfiguration
	chartValidating *admissionregistrationv1.ValidatingWebhookConfiguration
)

// managerConfig is what the chart passes the manager on its command line.
type managerConfig struct {
	controllerImage string
	executorImage   string
	runnerImage     string
	ptahVersion     string
	username        string
	admission       podintent.Options
}

func (config managerConfig) builder() workload.Builder {
	return workload.Builder{
		ExecutorImage:          config.executorImage,
		RunnerImage:            config.runnerImage,
		PtahVersion:            config.ptahVersion,
		ControllerImage:        config.controllerImage,
		ControllerRevision:     controllerRevision,
		ControllerStateVersion: controllerstate.CurrentVersion,
	}
}

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.WriteTo(io.Discard)))
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, operatorv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			fmt.Fprintf(os.Stderr, "build the scheme: %v\n", err)
			os.Exit(1)
		}
	}
	plane.Environment.Scheme = scheme

	// The chart is rendered only when a control plane will run: without one
	// every test skips, and a machine without helm should skip too.
	if os.Getenv(harness.AssetsVariable) != "" {
		if err := installChartWebhooks(); err != nil {
			fmt.Fprintf(os.Stderr, "prepare the chart's webhooks: %v\n", err)
			os.Exit(1)
		}
	}

	os.Exit(plane.Main(m, func() error {
		var err error
		admin, err = client.New(plane.Config, client.Options{Scheme: scheme})
		if err != nil {
			return err
		}
		stop, err := serveManagerHandlers()
		if err != nil {
			return err
		}
		plane.Cleanup(stop)
		return nil
	}))
}

// installChartWebhooks renders the chart and hands its two webhook
// configurations to envtest, which rewrites each service reference to the
// local serving address and CA before installing them. Nothing else about the
// entries changes: the rules, matchConditions, failure policies and paths are
// the chart's.
func installChartWebhooks() error {
	objects, err := harness.RenderChart(context.Background(), harness.DefaultRelease())
	if err != nil {
		return err
	}
	var mutating []*admissionregistrationv1.MutatingWebhookConfiguration
	var validating []*admissionregistrationv1.ValidatingWebhookConfiguration
	var deployments []*appsv1.Deployment
	for _, object := range objects {
		switch object.GetKind() {
		case "MutatingWebhookConfiguration":
			typed := &admissionregistrationv1.MutatingWebhookConfiguration{}
			if err := fromUnstructured(object, typed); err != nil {
				return err
			}
			mutating = append(mutating, typed)
		case "ValidatingWebhookConfiguration":
			typed := &admissionregistrationv1.ValidatingWebhookConfiguration{}
			if err := fromUnstructured(object, typed); err != nil {
				return err
			}
			validating = append(validating, typed)
		case "Deployment":
			typed := &appsv1.Deployment{}
			if err := fromUnstructured(object, typed); err != nil {
				return err
			}
			deployments = append(deployments, typed)
		}
	}
	if len(mutating) != 1 || len(validating) != 1 {
		return fmt.Errorf("the chart rendered %d mutating and %d validating webhook configurations, want one of each",
			len(mutating), len(validating))
	}
	manager, err = managerConfigFrom(deployments)
	if err != nil {
		return err
	}
	if problems := unservedManagerPaths(mutating[0], validating[0], servedPaths()); len(problems) > 0 {
		return fmt.Errorf("the chart and the manager disagree about webhook paths: %s", strings.Join(problems, "; "))
	}
	// envtest rewrites the entries it installs in place, so the rendered
	// routing is kept aside for the test that measures the check above.
	chartMutating = mutating[0].DeepCopy()
	chartValidating = validating[0].DeepCopy()
	validatingConfigurationName = validating[0].Name
	plane.Environment.WebhookInstallOptions = envtest.WebhookInstallOptions{
		MutatingWebhooks:   mutating,
		ValidatingWebhooks: validating,
	}
	return nil
}

func fromUnstructured(object *unstructured.Unstructured, into any) error {
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, into); err != nil {
		return fmt.Errorf("decode the rendered %s %s: %w", object.GetKind(), object.GetName(), err)
	}
	return nil
}

// managerConfigFrom reads the manager container's flags out of the rendered
// Deployment, so the handlers run with the configuration a release gives them.
func managerConfigFrom(deployments []*appsv1.Deployment) (managerConfig, error) {
	var arguments []string
	found := 0
	for _, deployment := range deployments {
		for _, container := range deployment.Spec.Template.Spec.Containers {
			if container.Name == "manager" && len(container.Command) == 1 && container.Command[0] == "/manager" {
				arguments = container.Args
				found++
			}
		}
	}
	if found != 1 {
		return managerConfig{}, fmt.Errorf("the chart renders %d manager containers, want 1", found)
	}
	flags := map[string]string{}
	for _, argument := range arguments {
		name, value, ok := strings.Cut(strings.TrimPrefix(argument, "--"), "=")
		if ok {
			flags[name] = value
		}
	}
	required := func(name string) (string, error) {
		value := flags[name]
		if value == "" {
			return "", fmt.Errorf("the manager container has no --%s", name)
		}
		return value, nil
	}
	boolean := func(name string) (bool, error) {
		value, err := required(name)
		if err != nil {
			return false, err
		}
		return strconv.ParseBool(value)
	}
	seconds := func(name string) (int64, error) {
		value, err := required(name)
		if err != nil {
			return 0, err
		}
		return strconv.ParseInt(value, 10, 64)
	}
	var config managerConfig
	var errs []error
	collect := func(target *string, name string) {
		value, err := required(name)
		errs = append(errs, err)
		*target = value
	}
	collect(&config.controllerImage, "controller-image")
	collect(&config.executorImage, "executor-image")
	collect(&config.runnerImage, "runner-image")
	collect(&config.ptahVersion, "ptah-version")
	collect(&config.username, "controller-service-account-username")
	var err error
	config.admission.DefaultTolerationsEnabled, err = boolean("default-tolerations-enabled")
	errs = append(errs, err)
	config.admission.DefaultNotReadyTolerationSeconds, err = seconds("default-not-ready-toleration-seconds")
	errs = append(errs, err)
	config.admission.DefaultUnreachableTolerationSeconds, err = seconds("default-unreachable-toleration-seconds")
	errs = append(errs, err)
	config.admission.ExtendedResourceTolerationEnabled, err = boolean("extended-resource-toleration-enabled")
	errs = append(errs, err)
	config.admission.AlwaysPullImagesEnabled, err = boolean("always-pull-images-enabled")
	errs = append(errs, err)
	if err := errors.Join(errs...); err != nil {
		return managerConfig{}, err
	}
	if err := config.admission.Validate(); err != nil {
		return managerConfig{}, fmt.Errorf("the chart's Pod admission flags: %w", err)
	}
	if err := config.builder().Validate(); err != nil {
		return managerConfig{}, fmt.Errorf("the chart's execution flags: %w", err)
	}
	return config, nil
}

// servedPaths is every path the manager registers.
func servedPaths() map[string]bool {
	return map[string]bool{
		mutateApprovalPath: true, validateApprovalPath: true,
		mutateMigrationApprovalPath: true, validateMigrationApprovalPath: true,
		mutateSchemaSpecWriterPath: true, mutateMigrationSpecWriterPath: true,
		validatePodIntentPath: true, validateControllerWritePath: true,
	}
}

// unservedManagerPaths names every disagreement between the chart's routing
// and the manager's handlers: an entry that sends requests to the manager's
// webhook Service at a path the manager does not serve, and a path the manager
// serves that no entry sends anything to. The configuration annotates the
// Service it expects the manager behind; entries addressed to another Service
// -- the certificate rotator's canaries -- are that program's.
func unservedManagerPaths(
	mutating *admissionregistrationv1.MutatingWebhookConfiguration,
	validating *admissionregistrationv1.ValidatingWebhookConfiguration,
	served map[string]bool,
) []string {
	var problems []string
	routed := map[string]bool{}
	check := func(annotations map[string]string, name string, config admissionregistrationv1.WebhookClientConfig) {
		service := annotations["operator.ptah.run/webhook-service-name"]
		if service == "" || config.Service == nil || config.Service.Name != service {
			return
		}
		path := "<none>"
		if config.Service.Path != nil {
			path = *config.Service.Path
		}
		routed[path] = true
		if !served[path] {
			problems = append(problems, name+" routes to "+path+", which the manager does not serve")
		}
	}
	for _, entry := range mutating.Webhooks {
		check(mutating.Annotations, entry.Name, entry.ClientConfig)
	}
	for _, entry := range validating.Webhooks {
		check(validating.Annotations, entry.Name, entry.ClientConfig)
	}
	for path := range served {
		if !routed[path] {
			problems = append(problems, "the manager serves "+path+", and no chart entry routes to it")
		}
	}
	slices.Sort(problems)
	return problems
}

// serveManagerHandlers registers the handlers exactly as cmd/manager/main.go
// does and serves them where envtest pointed the configurations. It returns
// once the server answers.
func serveManagerHandlers() (func(), error) {
	options := plane.Environment.WebhookInstallOptions
	server := webhook.NewServer(webhook.Options{
		Host: options.LocalServingHost, Port: options.LocalServingPort, CertDir: options.LocalServingCertDir,
	})
	decoder := cradmission.NewDecoder(scheme)
	// Approvals are judged against what the manager executes with, as
	// cmd/manager/main.go derives it from the builder.
	var execution approvaladmission.Execution
	execution.ControllerStateVersion, execution.PtahVersion, execution.ExecutorImage,
		execution.RunnerProtocolVersion = manager.builder().ExecutionBinding()
	approval := func(mutate bool) *approvaladmission.ApprovalHandler {
		return &approvaladmission.ApprovalHandler{Reader: admin, Decoder: decoder, Mutate: mutate, Execution: execution}
	}
	migrationApproval := func(mutate bool) *approvaladmission.MigrationApprovalHandler {
		return &approvaladmission.MigrationApprovalHandler{Reader: admin, Decoder: decoder, Mutate: mutate, Execution: execution}
	}
	server.Register(mutateApprovalPath, &cradmission.Webhook{Handler: approval(true)})
	server.Register(validateApprovalPath, &cradmission.Webhook{Handler: approval(false)})
	server.Register(mutateMigrationApprovalPath, &cradmission.Webhook{Handler: migrationApproval(true)})
	server.Register(validateMigrationApprovalPath, &cradmission.Webhook{Handler: migrationApproval(false)})
	server.Register(mutateSchemaSpecWriterPath, &cradmission.Webhook{Handler: &approvaladmission.SchemaSpecWriterHandler{
		Decoder: decoder,
	}})
	server.Register(mutateMigrationSpecWriterPath, &cradmission.Webhook{Handler: &approvaladmission.MigrationSpecWriterHandler{
		Decoder: decoder,
	}})
	server.Register(validatePodIntentPath, &cradmission.Webhook{Handler: &podintent.ValidationHandler{
		Reader: admin, Decoder: decoder,
	}})
	server.Register(validateControllerWritePath, &cradmission.Webhook{Handler: &controllerwrite.ValidationHandler{
		Validator: &controllerwrite.Validator{
			Reader: admin, Jobs: manager.builder(), ManagerUsername: manager.username,
		},
	}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()
	stop := func() {
		cancel()
		<-done
	}
	deadline := time.Now().Add(30 * time.Second)
	for server.StartedChecker()(nil) != nil {
		select {
		case err := <-done:
			cancel()
			return func() {}, fmt.Errorf("the webhook server stopped before serving: %w", err)
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			stop()
			return func() {}, fmt.Errorf("the webhook server did not serve on %s:%d within 30s",
				options.LocalServingHost, options.LocalServingPort)
		}
	}
	return stop, nil
}
