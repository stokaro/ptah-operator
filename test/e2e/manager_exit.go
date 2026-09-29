package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

const managerExitLogLimit = 256 << 10

// All strings in this diagnostic are fixed vocabulary. Previous logs and
// API errors may contain credentials, even when the process is stopping.
type managerExitDiagnostic struct {
	Read    string   `json:"read"`
	Limited bool     `json:"limited"`
	Signals []string `json:"signals,omitempty"`
}

// The log endpoint addresses a Pod by name, not UID or restart generation.
// Bracket the read with both identities so a newer process cannot explain
// the termination the caller saw. This is a diagnostic, not a complete audit.
func readManagerExit(ctx context.Context, clientset kubernetes.Interface, snapshot *corev1.Pod, status corev1.ContainerStatus) managerExitDiagnostic {
	out := managerExitDiagnostic{Read: "unavailable"}
	pods := clientset.CoreV1().Pods(snapshot.Namespace)
	before, err := pods.Get(ctx, snapshot.Name, metav1.GetOptions{})
	if err != nil {
		return out
	}
	if !sameRestartedContainer(before, snapshot, status) {
		out.Read = "identity_changed"
		return out
	}
	stream, err := pods.GetLogs(snapshot.Name, &corev1.PodLogOptions{
		Container: status.Name, Previous: true, TailLines: ptr.To[int64](512), LimitBytes: ptr.To[int64](managerExitLogLimit),
	}).Stream(ctx)
	if err != nil {
		return out
	}
	defer stream.Close()
	body, err := io.ReadAll(io.LimitReader(stream, managerExitLogLimit+1))
	if err != nil {
		return out
	}
	after, err := pods.Get(ctx, snapshot.Name, metav1.GetOptions{})
	if err != nil {
		return out
	}
	if !sameRestartedContainer(after, snapshot, status) {
		out.Read = "identity_changed"
		return out
	}
	out.Read = "read"
	out.Limited = len(body) >= managerExitLogLimit
	if len(body) > managerExitLogLimit {
		body = body[:managerExitLogLimit]
	}
	out.Signals = managerExitSignals(body)
	return out
}

func sameRestartedContainer(pod, snapshot *corev1.Pod, status corev1.ContainerStatus) bool {
	if snapshot.UID == "" || pod.UID != snapshot.UID || pod.Name != snapshot.Name || pod.Namespace != snapshot.Namespace ||
		status.RestartCount == 0 || status.LastTerminationState.Terminated == nil {
		return false
	}
	for _, current := range allStatuses(pod) {
		if current.Name == status.Name {
			return current.RestartCount == status.RestartCount && equality.Semantic.DeepEqual(current.LastTerminationState, status.LastTerminationState)
		}
	}
	return false
}

// client-go's election logger can use klog text while the manager uses JSON.
// Only the source location and fixed message select a line; its free text is
// used for coarse error categories and never returned.
var managerExitElectionLine = regexp.MustCompile(`^[IWEF][0-9]{4} .* leaderelection\.go:[0-9]+\] "([^"]+)"(.*)$`)

func managerExitSignals(body []byte) []string {
	found := map[string]bool{}
	for line := range bytes.SplitSeq(body, []byte("\n")) {
		var record struct {
			Logger  string `json:"logger"`
			Message string `json:"msg"`
			Error   string `json:"error"`
			Err     string `json:"err"`
		}
		if json.Unmarshal(line, &record) != nil {
			parts := managerExitElectionLine.FindSubmatch(line)
			if parts == nil {
				continue
			}
			record.Message, record.Error = string(parts[1]), string(parts[2])
		}
		detail := record.Error + " " + record.Err
		switch record.Message {
		case "manager stopped":
			if record.Logger != "setup" {
				continue
			}
			found["manager_stopped"] = true
			if record.Error == "leader election lost" {
				found["leader_election_lost"] = true
			}
			if strings.HasPrefix(record.Error, "failed to wait for ") && strings.Contains(record.Error, " caches to sync ") {
				found["cache_sync_failed"] = true
			}
		case "Error retrieving lease lock", "error retrieving resource lock":
			found["lease_read_failed"] = true
		case "Failed to update lease", "Failed to update lock":
			found["lease_update_failed"] = true
		case "Failed to renew lease":
			found["lease_renew_failed"] = true
		default:
			continue
		}
		for signal, fragment := range map[string]string{
			"request_deadline":   "context deadline exceeded",
			"request_canceled":   "context canceled",
			"connection_refused": "connection refused",
			"tls_error":          "tls:",
			"certificate_error":  "x509:",
			"api_forbidden":      "forbidden",
			"api_unauthorized":   "Unauthorized",
		} {
			if strings.Contains(detail, fragment) {
				found[signal] = true
			}
		}
	}
	out := make([]string, 0, len(found))
	for signal := range found {
		out = append(out, signal)
	}
	slices.Sort(out)
	return out
}
