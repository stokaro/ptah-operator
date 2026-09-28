package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

const (
	externalID      = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	externalImage   = "postgres@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	externalOwner   = "ptah-e2e-1.35"
	externalAddress = "172.18.0.6"
)

func externalInspection(t *testing.T, edit func(map[string]any)) dockerInspection {
	t.Helper()
	document := map[string]any{
		"Id":    externalID,
		"State": map[string]any{"Running": true},
		"Config": map[string]any{"Image": externalImage, "Labels": map[string]any{
			"operator.ptah.run/e2e-owner": externalOwner, "operator.ptah.run/e2e-component": "external-postgresql",
		}},
		"HostConfig": map[string]any{
			"RestartPolicy":   map[string]any{"Name": "no"},
			"PublishAllPorts": false,
			"PortBindings":    map[string]any{},
			"Tmpfs":           map[string]any{"/var/lib/postgresql/data": "rw,noexec,nosuid,nodev,size=536870912"},
		},
		"Mounts": []any{},
		"NetworkSettings": map[string]any{
			"Ports":    map[string]any{"5432/tcp": nil},
			"Networks": map[string]any{"kind": map[string]any{"IPAddress": externalAddress}},
		},
	}
	if edit != nil {
		edit(document)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var inspection dockerInspection
	if err := json.Unmarshal(encoded, &inspection); err != nil {
		t.Fatal(err)
	}
	return inspection
}

func TestExternalContainerContractIsADisposableDatabase(t *testing.T) {
	t.Parallel()
	check := func(inspection dockerInspection) error {
		return externalContainerContract(inspection, externalID, externalImage, externalOwner, externalAddress)
	}
	if err := check(externalInspection(t, nil)); err != nil {
		t.Fatalf("a disposable external database was refused: %v", err)
	}
	// Docker 29 stopped listing tmpfs in Mounts; a daemon that still lists it
	// is the same container.
	listed := externalInspection(t, func(document map[string]any) {
		document["Mounts"] = []any{map[string]any{"Type": "tmpfs", "Destination": "/var/lib/postgresql/data"}}
	})
	if err := check(listed); err != nil {
		t.Fatalf("a daemon that lists the tmpfs was refused: %v", err)
	}
	for name, edit := range map[string]func(map[string]any){
		"another container": func(document map[string]any) { document["Id"] = strings.Repeat("e", 64) },
		"stopped":           func(document map[string]any) { document["State"] = map[string]any{"Running": false} },
		"a restart policy": func(document map[string]any) {
			document["HostConfig"].(map[string]any)["RestartPolicy"] = map[string]any{"Name": "always"}
		},
		"every port published": func(document map[string]any) {
			document["HostConfig"].(map[string]any)["PublishAllPorts"] = true
		},
		"another image": func(document map[string]any) {
			document["Config"].(map[string]any)["Image"] = "postgres:17"
		},
		"another owner": func(document map[string]any) {
			document["Config"].(map[string]any)["Labels"].(map[string]any)["operator.ptah.run/e2e-owner"] = "other"
		},
		"a host port binding": func(document map[string]any) {
			document["HostConfig"].(map[string]any)["PortBindings"] = map[string]any{"5432/tcp": []any{map[string]any{"HostPort": "5432"}}}
		},
		"an exposed host port": func(document map[string]any) {
			document["NetworkSettings"].(map[string]any)["Ports"] = map[string]any{"5432/tcp": []any{map[string]any{"HostPort": "5432"}}}
		},
		"no tmpfs": func(document map[string]any) {
			document["HostConfig"].(map[string]any)["Tmpfs"] = map[string]any{}
		},
		"a volume": func(document map[string]any) {
			document["Mounts"] = []any{map[string]any{"Type": "volume", "Destination": "/var/lib/postgresql/data"}}
		},
		"a bind elsewhere": func(document map[string]any) {
			document["Mounts"] = []any{map[string]any{"Type": "bind", "Destination": "/etc"}}
		},
		"a second network": func(document map[string]any) {
			document["NetworkSettings"].(map[string]any)["Networks"] = map[string]any{
				"kind": map[string]any{"IPAddress": externalAddress}, "bridge": map[string]any{"IPAddress": "172.17.0.2"},
			}
		},
		"another address": func(document map[string]any) {
			document["NetworkSettings"].(map[string]any)["Networks"] = map[string]any{"kind": map[string]any{"IPAddress": "172.18.0.9"}}
		},
	} {
		if err := check(externalInspection(t, edit)); err == nil {
			t.Errorf("a container with %s was accepted", name)
		}
	}
}

func TestExternalEndpointSliceIsTheOneRoute(t *testing.T) {
	t.Parallel()
	valid := func() *discoveryv1.EndpointSlice {
		return &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"kubernetes.io/service-name": "external-pg", "endpointslice.kubernetes.io/managed-by": "ptah-operator-e2e",
					"app.kubernetes.io/component": "e2e-external-database", "operator.ptah.run/e2e-owner": externalOwner,
				},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "v1", Kind: "Service", Name: "external-pg", UID: "service-uid",
					Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(false),
				}},
			},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{externalAddress}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}},
			Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("postgresql"), Port: ptr.To[int32](5432), Protocol: ptr.To(corev1.ProtocolTCP)}},
		}
	}
	if err := externalEndpointSliceExact(valid(), "external-pg", "service-uid", externalOwner, externalAddress); err != nil {
		t.Fatalf("the route was refused: %v", err)
	}
	for name, edit := range map[string]func(*discoveryv1.EndpointSlice){
		"IPv6":             func(s *discoveryv1.EndpointSlice) { s.AddressType = discoveryv1.AddressTypeIPv6 },
		"another manager":  func(s *discoveryv1.EndpointSlice) { s.Labels["endpointslice.kubernetes.io/managed-by"] = "x" },
		"a blocking owner": func(s *discoveryv1.EndpointSlice) { s.OwnerReferences[0].BlockOwnerDeletion = ptr.To(true) },
		"a second address": func(s *discoveryv1.EndpointSlice) {
			s.Endpoints[0].Addresses = append(s.Endpoints[0].Addresses, "10.0.0.1")
		},
		"a serving condition": func(s *discoveryv1.EndpointSlice) { s.Endpoints[0].Conditions.Serving = ptr.To(true) },
		"an app protocol":     func(s *discoveryv1.EndpointSlice) { s.Ports[0].AppProtocol = ptr.To("postgresql") },
	} {
		slice := valid()
		edit(slice)
		if err := externalEndpointSliceExact(slice, "external-pg", "service-uid", externalOwner, externalAddress); err == nil {
			t.Errorf("a slice with %s was accepted", name)
		}
	}
}

func TestDockerConfigJSONRoundTrips(t *testing.T) {
	t.Parallel()
	config := dockerConfigJSON("registry.ns.svc.cluster.local:5000", "ptah", "pw")
	if config != `{"auths":{"registry.ns.svc.cluster.local:5000":{"username":"ptah","password":"pw","auth":"cHRhaDpwdw=="}}}` {
		t.Fatalf("dockerConfigJSON() = %s", config)
	}
	secret := &corev1.Secret{
		Immutable: ptr.To(true), Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			".dockerconfigjson": []byte(config), "registry": []byte("registry.ns.svc.cluster.local:5000"),
			"allowPlainHTTP": []byte("true"),
		},
	}
	if err := digestPinDockerSecretExact(secret, "registry.ns.svc.cluster.local:5000", "ptah", "pw"); err != nil {
		t.Fatalf("the digest-pin Secret was refused: %v", err)
	}
	for name, edit := range map[string]func(*corev1.Secret){
		"mutable":          func(s *corev1.Secret) { s.Immutable = nil },
		"plain HTTP off":   func(s *corev1.Secret) { s.Data["allowPlainHTTP"] = []byte("false") },
		"an extra key":     func(s *corev1.Secret) { s.Data["caSHA256"] = []byte("x") },
		"another registry": func(s *corev1.Secret) { s.Data["registry"] = []byte("other:5000") },
		"a second auth": func(s *corev1.Secret) {
			s.Data[".dockerconfigjson"] = []byte(`{"auths":{"registry.ns.svc.cluster.local:5000":{"username":"ptah","password":"pw","auth":"cHRhaDpwdw=="},"other":{}}}`)
		},
		"a forged auth": func(s *corev1.Secret) {
			s.Data[".dockerconfigjson"] = []byte(`{"auths":{"registry.ns.svc.cluster.local:5000":{"username":"ptah","password":"pw","auth":"b3RoZXI6cHc="}}}`)
		},
	} {
		candidate := secret.DeepCopy()
		edit(candidate)
		if err := digestPinDockerSecretExact(candidate, "registry.ns.svc.cluster.local:5000", "ptah", "pw"); err == nil {
			t.Errorf("a Secret with %s was accepted", name)
		}
	}
}

func proxyAuthSecrets(proxy tlsProxyAddress) map[string]*corev1.Secret {
	secret := func(name, authority, ca string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name}, Immutable: ptr.To(true), Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"username": []byte("ptah"), "password": []byte("pw"), "registry": []byte(authority), "caSHA256": []byte(ca),
			},
		}
	}
	return map[string]*corev1.Secret{
		tlsProxyGoodAuthSecret:     secret(tlsProxyGoodAuthSecret, proxy.authority, proxy.caSHA256),
		tlsProxyBadCAAuthSecret:    secret(tlsProxyBadCAAuthSecret, proxy.authority, tlsProxyBadCASHA256),
		tlsProxyBadAuthoritySecret: secret(tlsProxyBadAuthoritySecret, tlsProxyWrongAuthority, proxy.caSHA256),
	}
}

func TestTLSProxyAuthSecretsDifferInOneGrantEach(t *testing.T) {
	t.Parallel()
	proxy := tlsProxyAddress{authority: "tls-proxy.ns.svc.cluster.local:5443", caSHA256: "sha256:" + strings.Repeat("a", 64)}
	if err := tlsProxyGrantsDistinct(proxy); err != nil {
		t.Fatalf("distinct grants were refused: %v", err)
	}
	if err := tlsProxyAuthSecretsOrthogonal(proxyAuthSecrets(proxy), proxy); err != nil {
		t.Fatalf("orthogonal grants were refused: %v", err)
	}
	for name, edit := range map[string]func(map[string]*corev1.Secret){
		"a mutable grant": func(s map[string]*corev1.Secret) { s[tlsProxyBadCAAuthSecret].Immutable = nil },
		"the bad CA grant is right": func(s map[string]*corev1.Secret) {
			s[tlsProxyBadCAAuthSecret].Data["caSHA256"] = []byte(proxy.caSHA256)
		},
		"the bad authority is right": func(s map[string]*corev1.Secret) {
			s[tlsProxyBadAuthoritySecret].Data["registry"] = []byte(proxy.authority)
		},
		"another login":    func(s map[string]*corev1.Secret) { s[tlsProxyBadCAAuthSecret].Data["password"] = []byte("other") },
		"an extra key":     func(s map[string]*corev1.Secret) { s[tlsProxyGoodAuthSecret].Data["token"] = []byte("t") },
		"a missing Secret": func(s map[string]*corev1.Secret) { delete(s, tlsProxyBadAuthoritySecret) },
	} {
		secrets := proxyAuthSecrets(proxy)
		edit(secrets)
		if err := tlsProxyAuthSecretsOrthogonal(secrets, proxy); err == nil {
			t.Errorf("grants with %s were accepted", name)
		}
	}
	if err := tlsProxyGrantsDistinct(tlsProxyAddress{authority: proxy.authority, caSHA256: tlsProxyBadCASHA256}); err == nil {
		t.Error("a live CA equal to the refused grant was accepted")
	}
	if err := tlsProxyGrantsDistinct(tlsProxyAddress{authority: tlsProxyWrongAuthority, caSHA256: proxy.caSHA256}); err == nil {
		t.Error("a live authority equal to the refused grant was accepted")
	}
}

func hardenedProxy() *appsv1.Deployment {
	mode := ptr.To[int32](288)
	return &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
		Replicas: ptr.To[int32](1),
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			AutomountServiceAccountToken: ptr.To(false), EnableServiceLinks: ptr.To(false),
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: registryPullSecret}},
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](65532), RunAsGroup: ptr.To[int64](65532),
				FSGroup: ptr.To[int64](65532), FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Image: "fixture", Command: []string{"/e2e-handcraft-oci"},
				Args: []string{"tls-proxy", "--listen=:5443", "--upstream=http://registry:5000", "--cert-file=/tls/tls.crt", "--key-file=/tls/tls.key"},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), RunAsNonRoot: ptr.To(true),
					RunAsUser: ptr.To[int64](65532), RunAsGroup: ptr.To[int64](65532),
					Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "tls", ReadOnly: true, MountPath: "/tls"}},
			}},
			Volumes: []corev1.Volume{{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: tlsProxyCertSecret, DefaultMode: mode,
				Items: []corev1.KeyToPath{{Key: "tls.crt", Path: "tls.crt", Mode: mode}, {Key: "tls.key", Path: "tls.key", Mode: mode}},
			}}}},
		}},
	}}
}

func TestTLSProxyDeploymentIsHardened(t *testing.T) {
	t.Parallel()
	if err := tlsProxyDeploymentHardened(hardenedProxy(), "fixture", "http://registry:5000"); err != nil {
		t.Fatalf("the hardened proxy was refused: %v", err)
	}
	for name, edit := range map[string]func(*appsv1.Deployment){
		"two replicas":  func(d *appsv1.Deployment) { d.Spec.Replicas = ptr.To[int32](2) },
		"a token":       func(d *appsv1.Deployment) { d.Spec.Template.Spec.AutomountServiceAccountToken = nil },
		"service links": func(d *appsv1.Deployment) { d.Spec.Template.Spec.EnableServiceLinks = ptr.To(true) },
		"root":          func(d *appsv1.Deployment) { d.Spec.Template.Spec.SecurityContext.RunAsUser = ptr.To[int64](0) },
		"a localhost profile": func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.SecurityContext.SeccompProfile.LocalhostProfile = ptr.To("profile")
		},
		"environment": func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "PASSWORD", Value: "x"}}
		},
		"another upstream": func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Args[2] = "--upstream=http://other" },
		"a capability kept": func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities.Drop = nil
		},
		"a writable mount": func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].VolumeMounts[0].ReadOnly = false },
		"a world-readable key": func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Volumes[0].Secret.Items[1].Mode = ptr.To[int32](0o644)
		},
		"a second container": func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers, corev1.Container{Name: "debug"})
		},
	} {
		deployment := hardenedProxy()
		edit(deployment)
		if err := tlsProxyDeploymentHardened(deployment, "fixture", "http://registry:5000"); err == nil {
			t.Errorf("a proxy with %s was accepted", name)
		}
	}
}

func TestTLSProxyServiceIsSingleStack(t *testing.T) {
	t.Parallel()
	valid := func() *corev1.Service {
		return &corev1.Service{Spec: corev1.ServiceSpec{
			IPFamilyPolicy: ptr.To(corev1.IPFamilyPolicySingleStack), IPFamilies: []corev1.IPFamily{corev1.IPv4Protocol},
			Selector: map[string]string{"app.kubernetes.io/name": "tls-proxy", "app.kubernetes.io/component": "e2e-tls-registry-proxy"},
			Ports:    []corev1.ServicePort{{Name: "tls", Port: 5443, TargetPort: intstr.FromString("tls"), Protocol: corev1.ProtocolTCP}},
		}}
	}
	if !tlsProxyServiceSingleStack(valid(), "tls-proxy") {
		t.Fatal("the proxy Service was refused")
	}
	for name, edit := range map[string]func(*corev1.Service){
		"dual stack":          func(s *corev1.Service) { s.Spec.IPFamilies = append(s.Spec.IPFamilies, corev1.IPv6Protocol) },
		"not-ready addresses": func(s *corev1.Service) { s.Spec.PublishNotReadyAddresses = true },
		"a wider selector":    func(s *corev1.Service) { delete(s.Spec.Selector, "app.kubernetes.io/name") },
		"a numeric target":    func(s *corev1.Service) { s.Spec.Ports[0].TargetPort = intstr.FromInt32(5443) },
		"the admin port": func(s *corev1.Service) {
			s.Spec.Ports = append(s.Spec.Ports, corev1.ServicePort{Name: "admin", Port: 8081})
		},
	} {
		service := valid()
		edit(service)
		if tlsProxyServiceSingleStack(service, "tls-proxy") {
			t.Errorf("a Service with %s was accepted", name)
		}
	}
}

func TestExternalNotHostedRefusesAnImpersonator(t *testing.T) {
	t.Parallel()
	database := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-postgresql"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Image: "postgres@sha256:" + strings.Repeat("1", 64)}},
		}}},
	}
	if err := externalNotHosted([]appsv1.Deployment{database}, nil, nil, nil, "external-pg", externalImage); err != nil {
		t.Fatalf("the lifecycle's own database read as the external one: %v", err)
	}
	for name, workloads := range map[string]func() error{
		"a Deployment named for the Service": func() error {
			named := database
			named.Name = "external-pg"
			return externalNotHosted([]appsv1.Deployment{named}, nil, nil, nil, "external-pg", externalImage)
		},
		"a StatefulSet labeled as it": func() error {
			labeled := appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", Labels: map[string]string{
				"app.kubernetes.io/component": "e2e-external-database",
			}}}
			return externalNotHosted(nil, []appsv1.StatefulSet{labeled}, nil, nil, "external-pg", externalImage)
		},
		"a Pod running its image": func() error {
			pod := corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: externalImage}}}}
			return externalNotHosted(nil, nil, []corev1.Pod{pod}, nil, "external-pg", externalImage)
		},
		"a Job running its image": func() error {
			job := batchv1.Job{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Image: externalImage}},
			}}}}
			return externalNotHosted(nil, nil, nil, []batchv1.Job{job}, "external-pg", externalImage)
		},
	} {
		if workloads() == nil {
			t.Errorf("%s was not refused", name)
		}
	}
}
