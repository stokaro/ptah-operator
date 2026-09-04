// Command hooklogcapture captures the exact CRD reconciliation hook log even
// when Helm deletes the hook immediately after it fails.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	defaultCaptureTimeout  = 6 * time.Minute
	defaultLogStartTimeout = 45 * time.Second
	defaultLogRetryDelay   = 250 * time.Millisecond
	defaultMaxLogBytes     = int64(2 << 20)
)

type options struct {
	kubeconfig string
	namespace  string
	jobName    string
	renderFile string
	logFile    string
	statusFile string
	readyFile  string
	errorFile  string
	timeout    time.Duration
}

func main() {
	os.Exit(execute(os.Args[1:]))
}

func execute(args []string) int {
	opts, err := parseOptions(args)
	if err != nil {
		bestEffortStartupError(opts.errorFile, err)
		return 2
	}

	output, err := prepareOutputs(outputPaths{
		log:    opts.logFile,
		status: opts.statusFile,
		ready:  opts.readyFile,
		error:  opts.errorFile,
	})
	if err != nil {
		if output != nil {
			output.reportFailure(err)
			_ = output.close()
		}
		return 1
	}
	defer func() { _ = output.close() }()

	if err := output.setStatus(statusStarting); err != nil {
		output.reportFailure(err)
		return 1
	}
	expectedJob, err := loadRenderedJob(opts.renderFile, opts.namespace, opts.jobName)
	if err != nil {
		output.reportFailure(errors.New("load exact rendered hook Job: " + err.Error()))
		return 1
	}

	restConfig, err := clientcmd.BuildConfigFromFlags("", opts.kubeconfig)
	if err != nil {
		output.reportFailure(errors.New("build Kubernetes client configuration: " + err.Error()))
		return 1
	}
	restConfig.UserAgent = "ptah-operator-hook-log-capture"
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		output.reportFailure(errors.New("create Kubernetes client: " + err.Error()))
		return 1
	}

	signalContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(signalContext, opts.timeout)
	defer cancel()

	err = capture(ctx, kubernetesResourceClient{client: clientset}, captureConfig{
		namespace:        opts.namespace,
		jobName:          opts.jobName,
		expectedJob:      expectedJob,
		logStartTimeout:  defaultLogStartTimeout,
		logRetryInterval: defaultLogRetryDelay,
		maxLogBytes:      defaultMaxLogBytes,
	}, output)
	if err == nil {
		return 0
	}
	if errors.Is(err, context.Canceled) {
		output.reportCanceled(err)
		return 1
	}
	output.reportFailure(err)
	return 1
}

func parseOptions(args []string) (options, error) {
	opts := options{timeout: defaultCaptureTimeout}
	flags := flag.NewFlagSet("hooklogcapture", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to the kubeconfig used for the capture")
	flags.StringVar(&opts.namespace, "namespace", "", "namespace containing the exact hook Job")
	flags.StringVar(&opts.jobName, "job-name", "", "exact hook Job name")
	flags.StringVar(&opts.renderFile, "render-file", "", "path to the candidate multi-document render")
	flags.StringVar(&opts.logFile, "log-file", "", "private destination for captured log bytes")
	flags.StringVar(&opts.statusFile, "status-file", "", "private destination for capture status")
	flags.StringVar(&opts.readyFile, "ready-file", "", "private readiness marker destination")
	flags.StringVar(&opts.errorFile, "error-file", "", "private destination for errors")
	flags.DurationVar(&opts.timeout, "timeout", opts.timeout, "maximum lifetime of the capture")
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if flags.NArg() != 0 {
		return opts, errors.New("positional arguments are not accepted")
	}
	required := []struct {
		name  string
		value string
	}{
		{"--kubeconfig", opts.kubeconfig},
		{"--namespace", opts.namespace},
		{"--job-name", opts.jobName},
		{"--render-file", opts.renderFile},
		{"--log-file", opts.logFile},
		{"--status-file", opts.statusFile},
		{"--ready-file", opts.readyFile},
		{"--error-file", opts.errorFile},
	}
	for _, item := range required {
		if item.value == "" {
			return opts, errors.New(item.name + " is required")
		}
	}
	if opts.timeout <= 0 || opts.timeout > 15*time.Minute {
		return opts, errors.New("--timeout must be greater than zero and no more than 15m")
	}
	return opts, nil
}
