package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

func TestAlAdmissionMetricsLiveReadings(t *testing.T) {
	t.Parallel()
	read := func(name string) []byte {
		t.Helper()
		body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", name))
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	var nodes corev1.NodeList
	var endpoints discoveryv1.EndpointSliceList
	if err := json.Unmarshal(read("kubernetes-api-scrape-nodes.json"), &nodes); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(read("kubernetes-api-scrape-endpoints.json"), &endpoints); err != nil {
		t.Fatal(err)
	}
	targets, err := alControlPlaneTargets(nodes.Items, endpoints.Items)
	if err != nil || !reflect.DeepEqual(targets, []string{"172.18.0.2:6443"}) {
		t.Fatalf("the recorded control-plane inventory changed: %v, %v", targets, err)
	}
	if !alAPIServerTargetsReady(read("prometheus-api-server-targets.json"), targets) {
		t.Fatal("the authenticated API server scrape was refused")
	}
	if !alAdmissionCounterIncreased(read("prometheus-admission-rejections.json")) {
		t.Fatal("the actual webhook failure counter increase was refused")
	}
}

func apiEndpointFixture() ([]corev1.Node, []discoveryv1.EndpointSlice) {
	var nodes []corev1.Node
	endpoints := discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: "kubernetes"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("https"), Port: ptr.To[int32](6443), Protocol: ptr.To(corev1.ProtocolTCP)}},
	}
	for i := range 3 {
		ip := fmt.Sprintf("10.0.0.%d", i+1)
		nodes = append(nodes, corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("control-plane-%d", i), Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}},
			Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}},
		})
		endpoints.Endpoints = append(endpoints.Endpoints, discoveryv1.Endpoint{
			Addresses: []string{ip}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		})
	}
	return nodes, []discoveryv1.EndpointSlice{endpoints}
}

func TestAlControlPlaneTargets(t *testing.T) {
	t.Parallel()
	nodes, endpoints := apiEndpointFixture()
	targets, err := alControlPlaneTargets(nodes, endpoints)
	if err != nil || !reflect.DeepEqual(targets, []string{"10.0.0.1:6443", "10.0.0.2:6443", "10.0.0.3:6443"}) {
		t.Fatalf("API targets = %v, %v", targets, err)
	}
	for name, mutate := range map[string]func(*[]corev1.Node, *[]discoveryv1.EndpointSlice){
		"no nodes":              func(n *[]corev1.Node, _ *[]discoveryv1.EndpointSlice) { *n = nil },
		"no endpoints":          func(_ *[]corev1.Node, e *[]discoveryv1.EndpointSlice) { *e = nil },
		"missing control plane": func(_ *[]corev1.Node, e *[]discoveryv1.EndpointSlice) { (*e)[0].Endpoints = (*e)[0].Endpoints[:2] },
		"duplicate endpoint":    func(_ *[]corev1.Node, e *[]discoveryv1.EndpointSlice) { (*e)[0].Endpoints[2] = (*e)[0].Endpoints[1] },
		"unrelated endpoint": func(_ *[]corev1.Node, e *[]discoveryv1.EndpointSlice) {
			(*e)[0].Endpoints[0].Addresses = []string{"10.0.0.9"}
		},
		"Service address": func(_ *[]corev1.Node, e *[]discoveryv1.EndpointSlice) {
			(*e)[0].Endpoints[0].Addresses = []string{"kubernetes.default.svc"}
		},
		"unready": func(_ *[]corev1.Node, e *[]discoveryv1.EndpointSlice) {
			(*e)[0].Endpoints[0].Conditions.Ready = ptr.To(false)
		},
		"unknown readiness": func(_ *[]corev1.Node, e *[]discoveryv1.EndpointSlice) { (*e)[0].Endpoints[0].Conditions.Ready = nil },
		"no address":        func(_ *[]corev1.Node, e *[]discoveryv1.EndpointSlice) { (*e)[0].Endpoints[0].Addresses = nil },
		"other service": func(_ *[]corev1.Node, e *[]discoveryv1.EndpointSlice) {
			(*e)[0].Labels[discoveryv1.LabelServiceName] = "other"
		},
		"other namespace": func(_ *[]corev1.Node, e *[]discoveryv1.EndpointSlice) { (*e)[0].Namespace = "other" },
		"no HTTPS port":   func(_ *[]corev1.Node, e *[]discoveryv1.EndpointSlice) { (*e)[0].Ports = nil },
		"no port number":  func(_ *[]corev1.Node, e *[]discoveryv1.EndpointSlice) { (*e)[0].Ports[0].Port = nil },
		"invalid port":    func(_ *[]corev1.Node, e *[]discoveryv1.EndpointSlice) { (*e)[0].Ports[0].Port = ptr.To[int32](65536) },
		"UDP": func(_ *[]corev1.Node, e *[]discoveryv1.EndpointSlice) {
			(*e)[0].Ports[0].Protocol = ptr.To(corev1.ProtocolUDP)
		},
		"two HTTPS ports": func(_ *[]corev1.Node, e *[]discoveryv1.EndpointSlice) {
			(*e)[0].Ports = append((*e)[0].Ports, (*e)[0].Ports[0])
		},
		"worker address": func(n *[]corev1.Node, _ *[]discoveryv1.EndpointSlice) { (*n)[0].Labels = nil },
		"no internal IP": func(n *[]corev1.Node, _ *[]discoveryv1.EndpointSlice) { (*n)[0].Status.Addresses = nil },
		"duplicate internal IP": func(n *[]corev1.Node, _ *[]discoveryv1.EndpointSlice) {
			(*n)[1].Status.Addresses = (*n)[0].Status.Addresses
		},
	} {
		t.Run(name, func(t *testing.T) {
			n, e := apiEndpointFixture()
			mutate(&n, &e)
			if got, err := alControlPlaneTargets(n, e); err == nil {
				t.Fatalf("invalid endpoint inventory accepted: %v", got)
			}
		})
	}
}

func TestAlAPIServerConfigAndGrant(t *testing.T) {
	t.Parallel()
	targets := []string{"10.0.0.1:6443", "[fd00::2]:6443"}
	var config struct {
		Scrapes []struct {
			Job    string `json:"job_name"`
			Scheme string
			Path   string `json:"metrics_path"`
			Token  string `json:"bearer_token_file"`
			TLS    struct {
				CA       string `json:"ca_file"`
				Name     string `json:"server_name"`
				Insecure bool   `json:"insecure_skip_verify"`
			} `json:"tls_config"`
			Static []struct{ Targets []string } `json:"static_configs"`
		} `json:"scrape_configs"`
	}
	raw := alPrometheusConfig("monitoring", "operator", "metrics", targets...)
	if err := yaml.Unmarshal([]byte(raw), &config); err != nil || len(config.Scrapes) != 2 {
		t.Fatalf("scrape config: %+v, %v", config, err)
	}
	api := config.Scrapes[0]
	if api.Job != alAPIServerJob || api.Scheme != "https" || api.Path != "/metrics" ||
		api.Token != "/var/run/secrets/kubernetes.io/serviceaccount/token" ||
		api.TLS.CA != "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt" || api.TLS.Name != "kubernetes.default.svc" || api.TLS.Insecure ||
		len(api.Static) != 1 || !reflect.DeepEqual(api.Static[0].Targets, targets) || config.Scrapes[1].Job != alScrapeJob {
		t.Fatalf("API server scrape loses target or TLS identity: %+v", config)
	}
	// Appending the leader fault must still edit the manager job, leaving
	// the API server targets intact in the same running configuration.
	fault := alScrapeFaultConfig(raw, "leader")
	response, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"yaml": fault}})
	if err != nil || !alScrapeFaultLoaded(response, "leader") {
		t.Fatal("the API server scrape displaced the leader fault from the manager job")
	}
	role, binding := alAPIMetricsRBAC("monitoring")
	if len(role.Rules) != 1 || len(role.Rules[0].Resources) != 0 ||
		!reflect.DeepEqual(role.Rules[0].NonResourceURLs, []string{"/metrics"}) || !reflect.DeepEqual(role.Rules[0].Verbs, []string{"get"}) ||
		binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != role.Name || len(binding.Subjects) != 1 ||
		binding.Subjects[0].Kind != "ServiceAccount" || binding.Subjects[0].Name != "prometheus" || binding.Subjects[0].Namespace != "monitoring" {
		t.Fatalf("API metrics grant is not restricted to Prometheus reading /metrics: %+v, %+v", role, binding)
	}
}

func TestAlAPIServerTargetsReady(t *testing.T) {
	t.Parallel()
	expected := []string{"10.0.0.1:6443", "10.0.0.2:6443"}
	fixture := func() alTargets {
		response := alTargets{Status: "success"}
		for _, address := range expected {
			response.Data.ActiveTargets = append(response.Data.ActiveTargets, alTarget{
				Labels:    map[string]string{"job": alAPIServerJob, "instance": address},
				ScrapeURL: "https://" + address + "/metrics", Health: "up", LastScrape: time.Unix(100, 0),
			})
		}
		return response
	}
	encode := func(response alTargets) []byte {
		body, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	if !alAPIServerTargetsReady(encode(fixture()), expected) {
		t.Fatal("healthy distinct API targets refused")
	}
	for name, mutate := range map[string]func(*alTargets){
		"empty":               func(r *alTargets) { r.Data.ActiveTargets = nil },
		"missing":             func(r *alTargets) { r.Data.ActiveTargets = r.Data.ActiveTargets[:1] },
		"duplicate":           func(r *alTargets) { r.Data.ActiveTargets[1] = r.Data.ActiveTargets[0] },
		"down":                func(r *alTargets) { r.Data.ActiveTargets[0].Health = "down" },
		"scrape error":        func(r *alTargets) { r.Data.ActiveTargets[0].LastError = "denied" },
		"no scrape":           func(r *alTargets) { r.Data.ActiveTargets[0].LastScrape = time.Time{} },
		"HTTP":                func(r *alTargets) { r.Data.ActiveTargets[0].ScrapeURL = "http://10.0.0.1:6443/metrics" },
		"wrong endpoint":      func(r *alTargets) { r.Data.ActiveTargets[0].ScrapeURL = "https://10.0.0.9:6443/metrics" },
		"wrong path":          func(r *alTargets) { r.Data.ActiveTargets[0].ScrapeURL = "https://10.0.0.1:6443/wrong" },
		"wrong job":           func(r *alTargets) { r.Data.ActiveTargets[0].Labels["job"] = alScrapeJob },
		"failed API response": func(r *alTargets) { r.Status = "error" },
	} {
		r := fixture()
		mutate(&r)
		if alAPIServerTargetsReady(encode(r), expected) {
			t.Errorf("%s was accepted", name)
		}
	}
	if alAPIServerTargetsReady(encode(fixture()), nil) || alAPIServerTargetsReady(encode(fixture()), []string{expected[0], expected[0]}) {
		t.Fatal("empty or duplicate expected targets accepted")
	}
}

func TestAlAdmissionCounterIncreased(t *testing.T) {
	t.Parallel()
	response := func(value string) []byte {
		return []byte(`{"status":"success","data":{"resultType":"vector","result":[{"value":[123,"` + value + `"]}]}}`)
	}
	if !alAdmissionCounterIncreased(response("2.5")) {
		t.Fatal("an actual extrapolated increase was refused")
	}
	for _, value := range []string{"0", "-1", "NaN", "+Inf", "not-a-number"} {
		if alAdmissionCounterIncreased(response(value)) {
			t.Errorf("%s was an increase", value)
		}
	}
	for _, body := range []string{
		`{}`, `{"status":"success","data":{"resultType":"vector","result":[]}}`,
		`{"status":"error","data":{"resultType":"vector","result":[{"value":[123,"1"]}]}}`,
		`{"status":"success","data":{"resultType":"matrix","result":[{"value":[123,"1"]}]}}`,
		`{"status":"success","data":{"resultType":"vector","result":[{"value":[0,"1"]}]}}`,
		`{"status":"success","data":{"resultType":"vector","result":[{"value":[123,"1"]},{"value":[123,"1"]}]}}`,
	} {
		if alAdmissionCounterIncreased([]byte(body)) {
			t.Errorf("invalid counter response accepted: %s", body)
		}
	}
}
