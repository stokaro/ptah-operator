package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

// install puts the operator into a cluster from the chart a release published
// to its OCI registry. That chart carries its release's manager, runner and
// executor digests and the verified Ptah version, so the command passes no
// image value and every image stays pinned by digest.
//
// Helm does the work. The chart refuses an install it cannot verify by reading
// the cluster, runs hooks before and after, and is upgraded and uninstalled
// with helm afterwards, so the plugin hands helm one argument list, the same
// for an install and for --yaml, rather than rendering anything itself.

const (
	// chartRepository is where a release publishes the chart. hack/chartstamp
	// pushes it there, and a test holds the two to the same value.
	chartRepository = "ghcr.io/stokaro/charts/ptah-operator"
	// releaseName is the Helm release the install page names. One release per
	// cluster is supported.
	releaseName = "ptah-operator"
	// defaultReleaseNamespace is the namespace the install page installs into.
	defaultReleaseNamespace = "ptah-system"
	// releaseChartName is the directory helm template --output-dir writes
	// under: the chart's own name.
	releaseChartName = "ptah-operator"
	// approverSubjectsValue is the chart value --approver fills.
	approverSubjectsValue = "approverClusterRole.subjects"
	// installTimeout bounds helm's wait for the release to become ready.
	installTimeout = 5 * time.Minute
)

// firstPublishedChart is the first release that published its chart to the
// registry. An older one installs from its assets, as the install page says.
var firstPublishedChart = chartVersion{0, 2, 10}

// upgradePage is where a person upgrades the operator. install does not: an
// upgrade to a new manager image also needs --force-conflicts, which an
// install must not pass.
const upgradePage = "Upgrade in the Operations page"

// chartVersion is a release's version as the chart and its registry tag carry
// it: three numbers, with no leading v.
type chartVersion [3]int

var releaseVersionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func parseChartVersion(value string) (chartVersion, bool) {
	match := releaseVersionPattern.FindStringSubmatch(value)
	if match == nil {
		return chartVersion{}, false
	}
	var parsed chartVersion
	for index := range parsed {
		number, err := strconv.Atoi(match[index+1])
		if err != nil {
			return chartVersion{}, false
		}
		parsed[index] = number
	}
	return parsed, true
}

func (v chartVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

func (v chartVersion) before(other chartVersion) bool {
	for index := range v {
		if v[index] != other[index] {
			return v[index] < other[index]
		}
	}
	return false
}

// helmRunner runs helm with arguments, its output going where the plugin's
// goes. Tests replace it.
type helmRunner func(ctx context.Context, arguments []string, stdout, stderr io.Writer) error

// installer is what install needs from outside its flags: the chart, the
// plugin's own version, and helm.
type installer struct {
	repository string
	// plainHTTP reaches the registry without TLS. Only a test sets it, for a
	// registry it serves itself.
	plainHTTP     bool
	pluginVersion string
	helm          helmRunner
}

func installCommand(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	return installer{repository: chartRepository, pluginVersion: version, helm: execHelm}.run(ctx, arguments, stdout, stderr)
}

// installation is one install, as the flags gave it.
type installation struct {
	version     chartVersion
	namespace   string
	approvers   []string
	valuesFiles []string
	yaml        bool
	outDir      string
	kubeconfig  string
	kubeContext string
	timeout     time.Duration
}

func (i installer) run(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	flags := pflag.NewFlagSet("kubectl ptah install", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { usage(stderr) }
	var (
		requested   = flags.String("version", "", "the release to install (default: this plugin's own)")
		namespace   = flags.StringP("namespace", "n", defaultReleaseNamespace, "namespace to install the release into")
		approvers   = flags.StringArray("approver", nil, "a username to bind the approver ClusterRole to, in every namespace (repeatable)")
		values      = flags.StringArrayP("values", "f", nil, "a values file for the chart (repeatable)")
		printYAML   = flags.Bool("yaml", false, "write what would be installed instead of installing it")
		outDir      = flags.String("out-dir", "", "with --yaml, the directory to write the objects to (default: standard output)")
		kubeconfig  = flags.String("kubeconfig", "", "path to a kubeconfig file (default: KUBECONFIG, then ~/.kube/config)")
		kubeContext = flags.String("context", "", "kubeconfig context to use (default: the current one)")
		timeout     = flags.Duration("timeout", installTimeout, "how long helm waits for the release to become ready")
	)
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "kubectl-ptah: install takes no arguments, and was given %q\n", flags.Args())
		return exitUsage
	}
	request := installation{
		namespace: *namespace, approvers: *approvers, valuesFiles: *values,
		yaml: *printYAML, outDir: *outDir, kubeconfig: *kubeconfig, kubeContext: *kubeContext, timeout: *timeout,
	}
	if problem := i.settle(&request, *requested, flags.Changed("version")); problem != "" {
		fmt.Fprintf(stderr, "kubectl-ptah: %s\n", problem)
		return exitUsage
	}
	if request.outDir != "" {
		// helm writes what this release renders into <dir>/<chart> and leaves
		// any other file there, so a file an earlier render wrote and this one
		// does not would be applied beside it.
		target := filepath.Join(request.outDir, releaseChartName)
		if _, err := os.Lstat(target); err == nil {
			fmt.Fprintf(stderr, "kubectl-ptah: %s already exists; remove it first, since a file an earlier render left there would be applied with this one\n", target)
			return exitUsage
		} else if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
			return exitFailed
		}
	}

	if !request.yaml {
		status, problem := i.checkInstalled(ctx, request, stderr)
		if problem != "" {
			fmt.Fprintf(stderr, "kubectl-ptah: %s\n", problem)
			return status
		}
	}

	command := i.helmArguments(request)
	fmt.Fprintf(stderr, "kubectl-ptah: running %s\n", shellQuote(append([]string{"helm"}, command...)))
	if err := i.helm(ctx, command, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return exitFailed
	}
	return exitOK
}

// settle checks what the flags asked for, before helm is reached. It returns
// what is wrong, or "".
func (i installer) settle(request *installation, requested string, versionGiven bool) string {
	switch {
	case versionGiven:
		parsed, ok := parseChartVersion(requested)
		if !ok {
			return fmt.Sprintf("--version %q is not a release version, such as 0.2.11", requested)
		}
		request.version = parsed
	default:
		// The chart of the plugin's own release is the one this plugin was
		// built and tested beside. A build from a checkout belongs to no
		// release, so it installs only what it is told to.
		parsed, ok := parseChartVersion(i.pluginVersion)
		if !ok {
			return fmt.Sprintf("this kubectl-ptah is version %q, which is no release; name the release to install with --version", i.pluginVersion)
		}
		request.version = parsed
	}
	if request.version.before(firstPublishedChart) {
		return fmt.Sprintf("release %s published no chart to %s; the first that did is %s. "+
			"Install an older release from its assets, as the install page shows", request.version, chartRepository, firstPublishedChart)
	}
	if request.namespace == "" {
		return "--namespace is empty"
	}
	for _, approver := range request.approvers {
		if strings.TrimSpace(approver) == "" || strings.TrimSpace(approver) != approver {
			return fmt.Sprintf("--approver %q is not a username: it is empty or has leading or trailing space", approver)
		}
	}
	for _, file := range request.valuesFiles {
		// helm reads --values as a comma-separated list, so a comma or a quote
		// would make one path two or none, and helm is not handed standard input.
		if file == "-" || strings.ContainsAny(file, `,"`) {
			return fmt.Sprintf("--values %q is not a path install can hand helm: it is - or holds a comma or a double quote", file)
		}
	}
	if request.outDir != "" && !request.yaml {
		return "--out-dir is where --yaml writes; without --yaml nothing is written"
	}
	if request.yaml && (request.kubeconfig != "" || request.kubeContext != "") {
		return "--yaml reads no cluster, so --kubeconfig and --context do not apply"
	}
	if request.timeout <= 0 {
		return "--timeout must be positive"
	}
	return ""
}

// chartArguments are what an install and a --yaml render share: the release,
// the chart, its version, the namespace and the values. Keeping them in one
// list is what makes the render the objects the install would apply.
func (i installer) chartArguments(request installation) []string {
	arguments := []string{releaseName, "oci://" + i.repository, "--version", request.version.String(), "--namespace", request.namespace}
	if i.plainHTTP {
		arguments = append(arguments, "--plain-http")
	}
	for _, file := range request.valuesFiles {
		arguments = append(arguments, "--values", file)
	}
	if len(request.approvers) > 0 {
		arguments = append(arguments, "--set-json", approverSubjectsValue+"="+approverSubjects(request.approvers))
	}
	return arguments
}

// listArguments asks helm for the release install would write, in any state an
// upgrade would act on.
func (i installer) listArguments(request installation) []string {
	command := []string{"list", "--namespace", request.namespace, "--filter", "^" + releaseName + "$",
		"--deployed", "--failed", "--pending", "--output", "json"}
	return append(command, i.clusterArguments(request)...)
}

// clusterArguments name the cluster a command reads or writes.
func (i installer) clusterArguments(request installation) []string {
	var arguments []string
	if request.kubeconfig != "" {
		arguments = append(arguments, "--kubeconfig", request.kubeconfig)
	}
	if request.kubeContext != "" {
		arguments = append(arguments, "--kube-context", request.kubeContext)
	}
	return arguments
}

// checkInstalled reads the release install would write. A release of another
// chart version is an upgrade, and install refuses it: it returns the exit
// status and what is wrong, or "".
func (i installer) checkInstalled(ctx context.Context, request installation, stderr io.Writer) (int, string) {
	var listed bytes.Buffer
	if err := i.helm(ctx, i.listArguments(request), &listed, stderr); err != nil {
		return exitFailed, fmt.Sprintf("read the installed release: %v", err)
	}
	var releases []struct {
		Name  string `json:"name"`
		Chart string `json:"chart"`
	}
	if err := json.Unmarshal(listed.Bytes(), &releases); err != nil {
		return exitFailed, fmt.Sprintf("read the installed release: helm list printed what install cannot read: %v", err)
	}
	for _, release := range releases {
		if release.Name != releaseName {
			continue
		}
		installed, ok := parseChartVersion(strings.TrimPrefix(release.Chart, releaseChartName+"-"))
		if !ok || !strings.HasPrefix(release.Chart, releaseChartName+"-") {
			return exitUsage, fmt.Sprintf("release %s in namespace %s is chart %s, which install did not put there", releaseName, request.namespace, release.Chart)
		}
		if installed != request.version {
			return exitUsage, fmt.Sprintf("release %s in namespace %s is %s, and this is %s: install does not upgrade. "+
				"Upgrade with helm as %s says", releaseName, request.namespace, installed, request.version, upgradePage)
		}
	}
	return exitOK, ""
}

// helmArguments is the whole helm command line for request.
func (i installer) helmArguments(request installation) []string {
	shared := i.chartArguments(request)
	if request.yaml {
		command := append([]string{"template"}, shared...)
		if request.outDir != "" {
			command = append(command, "--output-dir", request.outDir)
		}
		return command
	}
	// --wait: helm otherwise returns once its hooks are done, which is not the
	// manager serving admission. The chart finishes replacing its bootstrap
	// certificate before the rotator reports ready, so the wait covers that.
	// --reset-values: an upgrade given no values keeps the last run's, so a run
	// without --approver would keep the approvers an earlier run bound. With
	// it, each run installs exactly its own flags.
	command := append([]string{"upgrade", "--install"}, shared...)
	command = append(command, "--reset-values", "--create-namespace", "--wait", "--timeout", request.timeout.String())
	return append(command, i.clusterArguments(request)...)
}

// approverSubjects is the chart value for usernames, as JSON, so that a name
// holding a comma, a dot or a quote reaches the chart as written.
func approverSubjects(usernames []string) string {
	type subject struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	subjects := make([]subject, 0, len(usernames))
	for _, name := range usernames {
		subjects = append(subjects, subject{Kind: "User", Name: name})
	}
	encoded, err := json.Marshal(subjects)
	if err != nil {
		// Strings always encode.
		panic(err)
	}
	return string(encoded)
}

// minimumHelm is the first Helm major this chart is tested with. Helm 3 renders
// it differently, applying without server-side apply, and is not supported.
const minimumHelm = 4

var helmVersionPattern = regexp.MustCompile(`^v([0-9]+)\.[0-9]+\.[0-9]+`)

// execHelm runs the helm on PATH, in the plugin's own environment.
var execHelm = helmOnPath(nil)

// helmOnPath runs the helm on PATH after checking it is Helm 4 or newer. A nil
// environment is the plugin's own.
func helmOnPath(environment []string) helmRunner {
	return func(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
		path, err := exec.LookPath("helm")
		if err != nil {
			return fmt.Errorf("install runs Helm %d or newer, and no helm is on PATH", minimumHelm)
		}
		var reported, failure bytes.Buffer
		check := exec.CommandContext(ctx, path, "version", "--template", "{{.Version}}")
		check.Env, check.Stdout, check.Stderr = environment, &reported, &failure
		if err := check.Run(); err != nil {
			return fmt.Errorf("%s version: %w: %s", path, err, strings.TrimSpace(failure.String()))
		}
		if err := requireHelm(reported.String()); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		command := exec.CommandContext(ctx, path, arguments...)
		command.Env, command.Stdout, command.Stderr = environment, stdout, stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("helm %s: %w", arguments[0], err)
		}
		return nil
	}
}

// requireHelm reads `helm version --template {{.Version}}`.
func requireHelm(reported string) error {
	reported = strings.TrimSpace(reported)
	match := helmVersionPattern.FindStringSubmatch(reported)
	if match == nil {
		return fmt.Errorf("helm reports version %q, which this command cannot read", reported)
	}
	major, err := strconv.Atoi(match[1])
	if err != nil || major < minimumHelm {
		return fmt.Errorf("helm is %s, and install runs Helm %d or newer", reported, minimumHelm)
	}
	return nil
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote prints a command line a person can paste into a shell.
func shellQuote(arguments []string) string {
	quoted := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		if shellSafe.MatchString(argument) {
			quoted = append(quoted, argument)
			continue
		}
		quoted = append(quoted, "'"+strings.ReplaceAll(argument, "'", `'\''`)+"'")
	}
	return strings.Join(quoted, " ")
}
