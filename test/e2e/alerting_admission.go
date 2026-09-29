package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	alAPIServerJob      = "kube-apiserver"
	alAPIMetricsRole    = "e2e-alerting-apiserver-metrics"
	alAdmissionAlert    = "PtahOperatorAdmissionUnavailable"
	alAdmissionWindow   = time.Minute
	alAdmissionRecovery = alCertificateProjection + alAdmissionWindow + 2*alDetectionSlack
)

var alAdmissionRejections = fmt.Sprintf(`sum(increase(apiserver_admission_webhook_rejection_count{job=%q,name=%q,error_type="calling_webhook_error"}[%ds]))`,
	alAPIServerJob, alApprovalWebhook, int(alAdmissionWindow/time.Second))

// Scrape each API server directly. Scraping the kubernetes Service address
// would alternate between independent counters under one instance label.
func alControlPlaneTargets(nodes []corev1.Node, endpoints []discoveryv1.EndpointSlice) ([]string, error) {
	ips := map[netip.Addr]string{}
	wanted := map[string]bool{}
	for _, node := range nodes {
		if _, controlPlane := node.Labels["node-role.kubernetes.io/control-plane"]; !controlPlane {
			continue
		}
		if node.Name == "" || wanted[node.Name] {
			return nil, errors.New("control-plane nodes need distinct names")
		}
		wanted[node.Name] = true
		for _, address := range node.Status.Addresses {
			if address.Type != corev1.NodeInternalIP {
				continue
			}
			ip, err := netip.ParseAddr(address.Address)
			if err != nil || ips[ip] != "" {
				return nil, errors.New("control-plane internal addresses must be distinct IP addresses")
			}
			ips[ip] = node.Name
		}
	}
	if len(wanted) == 0 {
		return nil, errors.New("no control-plane nodes were found")
	}
	seenNodes, seenTargets := map[string]bool{}, map[string]bool{}
	var targets []string
	for _, slice := range endpoints {
		if slice.Namespace != "default" || slice.Labels[discoveryv1.LabelServiceName] != "kubernetes" {
			return nil, errors.New("an EndpointSlice does not belong to the Kubernetes API Service")
		}
		var port int32
		for _, candidate := range slice.Ports {
			if candidate.Name != nil && *candidate.Name == "https" {
				if port != 0 || candidate.Protocol == nil || *candidate.Protocol != corev1.ProtocolTCP ||
					candidate.Port == nil || *candidate.Port < 1 || *candidate.Port > 65535 {
					return nil, errors.New("API endpoints need one HTTPS TCP port")
				}
				port = *candidate.Port
			}
		}
		if port == 0 || len(slice.Endpoints) == 0 {
			return nil, errors.New("API endpoints have no HTTPS port or addresses")
		}
		for _, endpoint := range slice.Endpoints {
			if endpoint.Conditions.Ready == nil || !*endpoint.Conditions.Ready || len(endpoint.Addresses) == 0 {
				return nil, errors.New("an API endpoint is not ready")
			}
			for _, address := range endpoint.Addresses {
				ip, err := netip.ParseAddr(address)
				if err != nil || ips[ip] == "" {
					return nil, errors.New("an API endpoint is not a control-plane internal address")
				}
				target := net.JoinHostPort(ip.String(), strconv.Itoa(int(port)))
				if seenTargets[target] {
					return nil, errors.New("an API endpoint was listed twice")
				}
				seenNodes[ips[ip]], seenTargets[target] = true, true
				targets = append(targets, target)
			}
		}
	}
	if len(seenNodes) != len(wanted) {
		return nil, errors.New("the Kubernetes API endpoints do not cover every control-plane node")
	}
	slices.Sort(targets)
	return targets, nil
}

func alAPIServerScrapeConfig(targets []string) string {
	if len(targets) == 0 {
		return ""
	}
	encoded, _ := json.Marshal(targets)
	return fmt.Sprintf(`  - job_name: %s
    scheme: https
    metrics_path: /metrics
    bearer_token_file: /var/run/secrets/kubernetes.io/serviceaccount/token
    tls_config:
      ca_file: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
      server_name: kubernetes.default.svc
    static_configs:
      - targets: %s
`, alAPIServerJob, encoded)
}

func alAPIMetricsRBAC(namespace string) (*rbacv1.ClusterRole, *rbacv1.ClusterRoleBinding) {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: alAPIMetricsRole},
		Rules:      []rbacv1.PolicyRule{{NonResourceURLs: []string{"/metrics"}, Verbs: []string{"get"}}},
	}, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: alAPIMetricsRole},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: alAPIMetricsRole},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: namespace, Name: "prometheus"}},
	}
}

func alAPIServerTargetsReady(body []byte, expected []string) bool {
	wanted := map[string]bool{}
	for _, target := range expected {
		if target == "" || wanted[target] {
			return false
		}
		wanted[target] = true
	}
	var response alTargets
	if len(wanted) == 0 || json.Unmarshal(body, &response) != nil || response.Status != "success" {
		return false
	}
	for _, target := range response.Data.ActiveTargets {
		if target.Labels["job"] != alAPIServerJob {
			continue
		}
		address, err := url.Parse(target.ScrapeURL)
		if err != nil || address.Scheme != "https" || address.Path != "/metrics" ||
			address.Host != target.Labels["instance"] || !wanted[address.Host] ||
			target.Health != "up" || target.LastError != "" || target.LastScrape.IsZero() {
			return false
		}
		delete(wanted, address.Host)
	}
	return len(wanted) == 0
}

func alAdmissionCounterIncreased(body []byte) bool {
	var response struct {
		Status string
		Data   struct {
			ResultType string
			Result     []struct{ Value []json.RawMessage }
		}
	}
	if json.Unmarshal(body, &response) != nil || response.Status != "success" || response.Data.ResultType != "vector" ||
		len(response.Data.Result) != 1 || len(response.Data.Result[0].Value) != 2 {
		return false
	}
	var sampledAt float64
	var raw string
	value := response.Data.Result[0].Value
	if json.Unmarshal(value[0], &sampledAt) != nil || sampledAt <= 0 || json.Unmarshal(value[1], &raw) != nil {
		return false
	}
	count, err := strconv.ParseFloat(raw, 64)
	return err == nil && count > 0 && !math.IsInf(count, 0)
}
