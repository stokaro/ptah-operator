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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
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
	m         *migrationRun
	container string
	rules     [2]bool
	path      string
}

func (m *migrationRun) docker(ctx context.Context, input io.Reader, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "docker", append([]string{"--context", m.in.DockerContext}, args...)...) //nolint:gosec // Explicit arguments in the isolated lab.
	var output bytes.Buffer
	command.Stdin, command.Stdout, command.Stderr = input, &output, os.Stderr
	err := command.Run()
	return output.Bytes(), err
}

func (m *migrationRun) kubeletFile(path string) ([]byte, error) {
	archive, err := m.docker(m.ctx, nil, "cp", m.miIsolatedNode()+":"+path, "-")
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

func (m *migrationRun) startLogStall(pod string) *logStall {
	m.t.Helper()
	r := &logStall{m: m, path: "/containerLogs/" + m.in.TestNamespace + "/" + pod + "/ptah"}
	t := m.t
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := r.stop(ctx); err != nil {
			t.Errorf("remove result-read fault: %v", err)
		}
	})
	node := &corev1.Node{}
	m.check(m.cluster.Client.Get(m.ctx, types.NamespacedName{Name: m.miIsolatedNode()}, node), "read isolation worker")
	if !isolationWorkerReady(node) {
		m.fatalf("result-read fault needs the Ready isolation worker")
	}
	arch := node.Status.NodeInfo.Architecture
	if arch != "amd64" && arch != "arm64" {
		m.fatalf("unsupported isolation worker architecture %q", arch)
	}
	binary := filepath.Join(m.workDir, "e2e-logstall")
	build := exec.CommandContext(m.ctx, "go", "build", "-trimpath", "-o", binary, "./test/e2e/logstall") //nolint:gosec // Fixed package, private output directory.
	build.Dir = repositoryRoot
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	m.check(build.Run(), "build the isolated result-read fault")
	program, err := os.ReadFile(binary)
	m.check(err, "read result-read fixture")
	cert, err := m.kubeletFile("/var/lib/kubelet/pki/kubelet.crt")
	m.check(err, "read isolated kubelet serving certificate")
	key, err := m.kubeletFile("/var/lib/kubelet/pki/kubelet.key")
	m.check(err, "read isolated kubelet serving key")
	image, err := m.docker(m.ctx, nil, "inspect", "--format", "{{.Image}}", m.miIsolatedNode())
	m.check(err, "read the node image already present on the test daemon")
	created, err := m.docker(m.ctx, nil, "create", "--name", m.in.KindClusterName+"-logstall-"+m.engine.name,
		"--label", "ptah.run/e2e-purpose=hung-result-read", "--label", "ptah.run/e2e-cluster="+m.in.KindClusterName,
		"--network", "container:"+m.miIsolatedNode(), "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true", "--user", "65532:65532", "--memory", "64m", "--pids-limit", "32",
		"--entrypoint", "/e2e-logstall", strings.TrimSpace(string(image)), "-path", r.path, "-lifetime", "8m")
	m.check(err, "create the isolated result-read fixture")
	r.container = strings.TrimSpace(string(created))
	if r.container == "" {
		m.fatalf("result-read fixture has no container identity")
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
		m.check(writer.WriteHeader(&tar.Header{Name: entry.name, Mode: entry.mode, Uid: 65532, Gid: 65532, Size: int64(len(entry.body))}), "write fixture archive header")
		_, err := writer.Write(entry.body)
		m.check(err, "write fixture archive entry")
	}
	m.check(writer.Close(), "finish fixture archive")
	_, err = m.docker(m.ctx, &archive, "cp", "-a", "-", r.container+":/")
	m.check(err, "copy result-read fixture into its container")
	_, err = m.docker(m.ctx, nil, "start", r.container)
	m.check(err, "start result-read fixture")
	m.poll("the result-read fixture to listen", time.Second, func() bool {
		logs, err := m.docker(m.ctx, nil, "logs", r.container)
		return err == nil && bytes.Contains(logs, []byte(`"state":"listening"`))
	})
	for index, rule := range logStallRules {
		r.rules[index] = true
		args := append([]string{"iptables", rule[0], rule[1], "-I", rule[2], "1"}, rule[3:]...)
		_, err := m.miNodeExec(m.ctx, os.Stderr, args...)
		m.check(err, "route kubelet log requests to the unfinished response")
	}
	return r
}

func (r *logStall) readings() []logStallReading {
	r.m.t.Helper()
	logs, err := r.m.docker(r.m.ctx, nil, "logs", "--tail", "100", r.container)
	r.m.check(err, "read result-stall timings")
	decoder := json.NewDecoder(bytes.NewReader(logs))
	decoder.DisallowUnknownFields()
	var readings []logStallReading
	for {
		var reading logStallReading
		err := decoder.Decode(&reading)
		if err == io.EOF {
			break
		}
		r.m.check(err, "decode result-stall timing")
		if reading.Path != r.path || reading.At.IsZero() {
			r.m.fatalf("result-stall reading lacks the requested path or timestamp")
		}
		readings = append(readings, reading)
	}
	return readings
}

func (r *logStall) stop(ctx context.Context) error {
	var failures []error
	for index := len(logStallRules) - 1; index >= 0; index-- {
		if !r.rules[index] {
			continue
		}
		rule := logStallRules[index]
		args := append([]string{"iptables", rule[0], rule[1], "-D", rule[2]}, rule[3:]...)
		if _, err := r.m.miNodeExec(ctx, os.Stderr, args...); err != nil {
			failures = append(failures, fmt.Errorf("remove log-stall rule: %w", err))
		} else {
			r.rules[index] = false
		}
	}
	if r.container != "" {
		if _, err := r.m.docker(ctx, nil, "rm", "-f", r.container); err != nil {
			failures = append(failures, fmt.Errorf("remove log-stall container: %w", err))
		} else {
			r.container = ""
		}
	}
	return errors.Join(failures...)
}
