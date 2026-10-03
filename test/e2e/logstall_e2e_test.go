//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

const logStallRuleComment = "ptah-e2e-result-read"

// The redirect handles new connections. The reject closes pooled kubelet
// connections, whose existing conntrack entries would bypass the redirect.
// Neither rule touches the node's outbound API traffic or database traffic.
var logStallRules = [][]string{
	{"-t", "nat", "PREROUTING", "-p", "tcp", "--dport", "10250", "-m", "comment", "--comment", logStallRuleComment, "-j", "REDIRECT", "--to-ports", "10443"},
	{"-t", "filter", "INPUT", "-p", "tcp", "--dport", "10250", "-m", "comment", "--comment", logStallRuleComment, "-j", "REJECT", "--reject-with", "tcp-reset"},
}

type logStall struct {
	t                                               *testing.T
	ctx                                             context.Context
	cluster                                         *harness.Cluster
	dockerContext, node, workDir, namespace, suffix string
	container                                       string
	rules                                           [2]bool
	path                                            string
}

func (r *logStall) docker(ctx context.Context, input io.Reader, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "docker", append([]string{"--context", r.dockerContext}, args...)...) //nolint:gosec // Explicit arguments in the isolated lab.
	var output bytes.Buffer
	command.Stdin, command.Stdout, command.Stderr = input, &output, os.Stderr
	err := command.Run()
	return output.Bytes(), err
}

func (r *logStall) kubeletFile(path string) ([]byte, error) {
	archive, err := r.docker(r.ctx, nil, "cp", r.node+":"+path, "-")
	if err != nil {
		return nil, fmt.Errorf("read isolated kubelet file %s: %w", path, err)
	}
	reader := tar.NewReader(bytes.NewReader(archive))
	header, err := reader.Next()
	if err != nil {
		return nil, err
	}
	if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > 1<<20 {
		return nil, fmt.Errorf("kubelet file %s is not a bounded regular file", path)
	}
	return io.ReadAll(reader)
}

func (r *logStall) start(pod string) *logStall {
	r.t.Helper()
	r.path = "/containerLogs/" + r.namespace + "/" + pod + "/ptah"
	t := r.t
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := r.stop(ctx); err != nil {
			t.Errorf("remove result-read fault: %v", err)
		}
	})
	node := &corev1.Node{}
	r.check(r.cluster.Client.Get(r.ctx, types.NamespacedName{Name: r.node}, node), "read isolation worker")
	if !isolationWorkerReady(node) {
		r.fatalf("result-read fault needs the Ready isolation worker")
	}
	arch := node.Status.NodeInfo.Architecture
	if arch != "amd64" && arch != "arm64" {
		r.fatalf("unsupported isolation worker architecture %q", arch)
	}
	binary := filepath.Join(r.workDir, "e2e-logstall")
	build := exec.CommandContext(r.ctx, "go", "build", "-trimpath", "-o", binary, "./test/e2e/logstall") //nolint:gosec // Fixed package, private output directory.
	build.Dir = repositoryRoot
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	r.check(build.Run(), "build the isolated result-read fault")
	program, err := os.ReadFile(binary)
	r.check(err, "read result-read fixture")
	cert, err := r.kubeletFile("/var/lib/kubelet/pki/kubelet.crt")
	r.check(err, "read isolated kubelet serving certificate")
	key, err := r.kubeletFile("/var/lib/kubelet/pki/kubelet.key")
	r.check(err, "read isolated kubelet serving key")
	image, err := r.docker(r.ctx, nil, "inspect", "--format", "{{.Image}}", r.node)
	r.check(err, "read the node image already present on the test daemon")
	created, err := r.docker(r.ctx, nil, "create", "--name", r.node+"-logstall-"+r.suffix,
		"--label", "ptah.run/e2e-purpose=hung-result-read", "--label", "ptah.run/e2e-node="+r.node,
		"--network", "container:"+r.node, "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true", "--user", "65532:65532", "--memory", "64m", "--pids-limit", "32",
		"--entrypoint", "/e2e-logstall", strings.TrimSpace(string(image)), "-path", r.path, "-lifetime", "8m")
	r.check(err, "create the isolated result-read fixture")
	r.container = strings.TrimSpace(string(created))
	if r.container == "" {
		r.fatalf("result-read fixture has no container identity")
	}
	// Private key bytes stay in memory and in this disposable container. The
	// archive sets ownership for its unprivileged process without a host file.
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, entry := range []struct {
		name string
		body []byte
		mode int64
	}{
		{"e2e-logstall", program, 0500}, {"tls.crt", cert, 0400}, {"tls.key", key, 0400},
	} {
		r.check(writer.WriteHeader(&tar.Header{Name: entry.name, Mode: entry.mode, Uid: 65532, Gid: 65532, Size: int64(len(entry.body))}), "write fixture archive header")
		_, err := writer.Write(entry.body)
		r.check(err, "write fixture archive entry")
	}
	r.check(writer.Close(), "finish fixture archive")
	_, err = r.docker(r.ctx, &archive, "cp", "-a", "-", r.container+":/")
	r.check(err, "copy result-read fixture into its container")
	_, err = r.docker(r.ctx, nil, "start", r.container)
	r.check(err, "start result-read fixture")
	r.check(harness.Wait(r.ctx, "the result-read fixture to listen", time.Minute, time.Second, func(context.Context) (bool, string, error) {
		logs, err := r.docker(r.ctx, nil, "logs", r.container)
		return err == nil && bytes.Contains(logs, []byte(`"state":"listening"`)), "waiting for HTTPS listener", err
	}), "wait for result-read fixture")
	for index, rule := range logStallRules {
		r.rules[index] = true
		args := append([]string{"iptables", rule[0], rule[1], "-I", rule[2], "1"}, rule[3:]...)
		_, err := r.nodeExec(r.ctx, os.Stderr, args...)
		r.check(err, "route kubelet log requests to the unfinished response")
	}
	return r
}

func (r *logStall) readings() []logStallReading {
	r.t.Helper()
	logs, err := r.docker(r.ctx, nil, "logs", "--tail", "100", r.container)
	r.check(err, "read result-stall timings")
	decoder := json.NewDecoder(bytes.NewReader(logs))
	decoder.DisallowUnknownFields()
	var readings []logStallReading
	for {
		var reading logStallReading
		err := decoder.Decode(&reading)
		if err == io.EOF {
			break
		}
		r.check(err, "decode result-stall timing")
		if reading.Path != r.path || reading.At.IsZero() {
			r.fatalf("result-stall reading lacks the requested path or timestamp")
		}
		readings = append(readings, reading)
	}
	return readings
}

// holdDiagnostic proves the log fault with a separate client. Durable
// controllers must finish without this client or the log endpoint recovering.
func (r *logStall) holdDiagnostic(pod string) (assertHeld, release func()) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Minute)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.cluster.ContainerLog(ctx, r.namespace, pod, "ptah")
	}()
	release = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			r.t.Error("diagnostic log client did not stop")
		}
	}
	r.t.Cleanup(release)
	r.check(harness.Wait(r.ctx, "one diagnostic request held at the log fault", 30*time.Second, time.Second, func(context.Context) (bool, string, error) {
		select {
		case <-done:
			return false, "diagnostic request ended", errors.New("log fault did not hold the request")
		default:
		}
		return diagnosticLogHeld(r.readings()), "waiting for the exact log request", nil
	}), "verify the diagnostic log is unavailable")
	return func() {
		select {
		case <-done:
			r.fatalf("diagnostic log recovered before durable convergence")
		default:
		}
		if !diagnosticLogHeld(r.readings()) {
			r.fatalf("diagnostic log fault did not remain continuously held")
		}
	}, release
}

func (r *logStall) stop(ctx context.Context) error {
	var failures []error
	for index := len(logStallRules) - 1; index >= 0; index-- {
		if !r.rules[index] {
			continue
		}
		rule := logStallRules[index]
		args := append([]string{"iptables", rule[0], rule[1], "-D", rule[2]}, rule[3:]...)
		if _, err := r.nodeExec(ctx, os.Stderr, args...); err != nil {
			failures = append(failures, fmt.Errorf("remove log-stall rule: %w", err))
		} else {
			r.rules[index] = false
		}
	}
	if r.container != "" {
		if _, err := r.docker(ctx, nil, "rm", "-f", r.container); err != nil {
			failures = append(failures, fmt.Errorf("remove log-stall container: %w", err))
		} else {
			r.container = ""
		}
	}
	return errors.Join(failures...)
}

func (r *logStall) nodeExec(ctx context.Context, stderr io.Writer, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "docker", append([]string{"--context", r.dockerContext, "exec", r.node}, args...)...) //nolint:gosec // Explicit arguments in the isolated lab.
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, stderr
	err := command.Run()
	return output.Bytes(), err
}

func (r *logStall) check(err error, what string) {
	r.t.Helper()
	if err != nil {
		r.t.Fatalf("%s: %v", what, err)
	}
}

func (r *logStall) fatalf(format string, args ...any) {
	r.t.Helper()
	r.t.Fatalf(format, args...)
}
