package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// tlsProxyAddress is where the authenticated HTTPS registry proxy answers.
type tlsProxyAddress struct {
	authority string
	reference string
	caSHA256  string
}

// tlsProxyGrantsDistinct holds the refused grants to differ from the live
// ones, or a refusal row would measure an accepted grant.
func tlsProxyGrantsDistinct(proxy tlsProxyAddress) error {
	if !sha256Pattern.MatchString(proxy.caSHA256) {
		return errors.New("TLS proxy CA does not have a lowercase SHA-256 digest")
	}
	if proxy.caSHA256 == tlsProxyBadCASHA256 {
		return errors.New("fixed mismatched TLS proxy CA grant unexpectedly matched the generated CA")
	}
	if proxy.authority == tlsProxyWrongAuthority {
		return errors.New("fixed mismatched TLS proxy registry authority unexpectedly matched the live authority")
	}
	return nil
}

// secretData is a Secret's data as the API stores it.
func secretData(values map[string]string) map[string]any {
	data := map[string]any{}
	for key, value := range values {
		data[key] = base64String(value)
	}
	return data
}

// dockerConfigJSON is the pull credential for one registry, as the shell
// phase's jq wrote it: the username, the password and the basic
// authentication they make, in that order.
func dockerConfigJSON(registry, username, password string) string {
	type auth struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Auth     string `json:"auth"`
	}
	encoded, err := canonicalJSON(struct {
		Auths map[string]auth `json:"auths"`
	}{map[string]auth{registry: {username, password, base64String(username + ":" + password)}}})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// dockerInspection is the part of `docker container inspect` the fixture
// contracts read.
type dockerInspection struct {
	ID    string `json:"Id"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
		PublishAllPorts bool                       `json:"PublishAllPorts"`
		PortBindings    map[string]json.RawMessage `json:"PortBindings"`
		Tmpfs           map[string]string          `json:"Tmpfs"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Ports    map[string]json.RawMessage `json:"Ports"`
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// onlyKindNetwork is a container attached to the kind network alone, at the
// address given.
func onlyKindNetwork(inspection dockerInspection, address string) bool {
	networks := inspection.NetworkSettings.Networks
	kind, found := networks["kind"]
	return len(networks) == 1 && found && kind.IPAddress == address
}

// dockerConfigAuth reads the one auths entry a Docker config holds, for the
// registry given, and refuses a config that holds anything else.
func dockerConfigAuth(config []byte, registry string) (username, password, auth string, err error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(config, &document); err != nil {
		return "", "", "", err
	}
	if len(document) != 1 || document["auths"] == nil {
		return "", "", "", fmt.Errorf("the Docker config holds %v, not auths alone", sortedKeys(document))
	}
	var auths map[string]struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Auth     string `json:"auth"`
	}
	if err := json.Unmarshal(document["auths"], &auths); err != nil {
		return "", "", "", err
	}
	entry, found := auths[registry]
	if len(auths) != 1 || !found {
		return "", "", "", fmt.Errorf("the Docker config names %v, not %s alone", sortedKeys(auths), registry)
	}
	return entry.Username, entry.Password, entry.Auth, nil
}

func sortedKeys[V any](values map[string]V) []string {
	return slices.Sorted(maps.Keys(values))
}

// tlsProxyAuthSecretsOrthogonal holds the proxy's three registry credentials
// to one login and to grants that differ in exactly one way each: the good
// one grants the proxy's authority and CA, one grants a CA that is not the
// proxy's, and one an authority that is not the proxy's.
func tlsProxyAuthSecretsOrthogonal(secrets map[string]*corev1.Secret, proxy tlsProxyAddress) error {
	if proxy.authority == tlsProxyWrongAuthority || proxy.caSHA256 == tlsProxyBadCASHA256 {
		return errors.New("a refused grant matches the live one")
	}
	good, badCA, badAuthority := secrets[tlsProxyGoodAuthSecret], secrets[tlsProxyBadCAAuthSecret], secrets[tlsProxyBadAuthoritySecret]
	if good == nil || badCA == nil || badAuthority == nil {
		return errors.New("missing exact auth Secret")
	}
	for _, secret := range []*corev1.Secret{good, badCA, badAuthority} {
		if secret.Immutable == nil || !*secret.Immutable || secret.Type != corev1.SecretTypeOpaque ||
			!slices.Equal(sortedKeys(secret.Data), []string{"caSHA256", "password", "registry", "username"}) {
			return fmt.Errorf("%s is not an immutable Opaque Secret of the four grant keys", secret.Name)
		}
	}
	for _, grant := range []struct {
		secret              *corev1.Secret
		authority, caSHA256 string
	}{
		{good, proxy.authority, proxy.caSHA256},
		{badCA, proxy.authority, tlsProxyBadCASHA256},
		{badAuthority, tlsProxyWrongAuthority, proxy.caSHA256},
	} {
		if string(grant.secret.Data["registry"]) != grant.authority || string(grant.secret.Data["caSHA256"]) != grant.caSHA256 {
			return fmt.Errorf("%s grants %s and %s", grant.secret.Name, grant.secret.Data["registry"], grant.secret.Data["caSHA256"])
		}
	}
	if len(good.Data["username"]) == 0 || len(good.Data["password"]) == 0 {
		return errors.New("the good grant carries no login")
	}
	for _, secret := range []*corev1.Secret{badCA, badAuthority} {
		if string(secret.Data["username"]) != string(good.Data["username"]) ||
			string(secret.Data["password"]) != string(good.Data["password"]) {
			return fmt.Errorf("%s carries another login than the good grant", secret.Name)
		}
	}
	return nil
}

var restrictedSeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}

// tlsProxyDeploymentHardened holds the proxy's Deployment to one replica that
// runs as a non-root user with no token, no service links, no environment, a
// read-only root and no capability, and mounts nothing but its own
// certificate.
func tlsProxyDeploymentHardened(deployment *appsv1.Deployment, image, upstream string) error {
	spec := deployment.Spec.Template.Spec
	security := spec.SecurityContext
	switch {
	case deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1:
		return errors.New("replicas is not 1")
	case spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken:
		return errors.New("the Pod mounts a ServiceAccount token")
	case spec.EnableServiceLinks == nil || *spec.EnableServiceLinks:
		return errors.New("the Pod carries service links")
	case !reflect.DeepEqual(spec.ImagePullSecrets, []corev1.LocalObjectReference{{Name: registryPullSecret}}):
		return errors.New("the Pod pulls with another Secret")
	case security == nil || !ptr.Equal(security.RunAsNonRoot, ptr.To(true)) || !ptr.Equal(security.RunAsUser, ptr.To[int64](65532)) ||
		!ptr.Equal(security.RunAsGroup, ptr.To[int64](65532)) || !ptr.Equal(security.FSGroup, ptr.To[int64](65532)) ||
		security.FSGroupChangePolicy == nil || *security.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch ||
		!reflect.DeepEqual(security.SeccompProfile, restrictedSeccompProfile):
		return errors.New("the Pod security context is not the restricted one")
	case len(spec.Containers) != 1:
		return errors.New("the Pod runs more than the proxy")
	}
	container := spec.Containers[0]
	containerSecurity := container.SecurityContext
	wantArgs := []string{"tls-proxy", "--listen=:5443", "--upstream=" + upstream, "--cert-file=/tls/tls.crt", "--key-file=/tls/tls.key"}
	switch {
	case container.Image != image || !slices.Equal(container.Command, []string{"/e2e-handcraft-oci"}) ||
		!slices.Equal(container.Args, wantArgs):
		return errors.New("the proxy runs another image or command")
	case len(container.Env) != 0 || len(container.EnvFrom) != 0:
		return errors.New("the proxy carries environment")
	case containerSecurity == nil || !ptr.Equal(containerSecurity.AllowPrivilegeEscalation, ptr.To(false)) ||
		!ptr.Equal(containerSecurity.ReadOnlyRootFilesystem, ptr.To(true)) || !ptr.Equal(containerSecurity.RunAsNonRoot, ptr.To(true)) ||
		!ptr.Equal(containerSecurity.RunAsUser, ptr.To[int64](65532)) || !ptr.Equal(containerSecurity.RunAsGroup, ptr.To[int64](65532)) ||
		containerSecurity.Capabilities == nil ||
		!slices.Equal(containerSecurity.Capabilities.Drop, []corev1.Capability{"ALL"}) ||
		!reflect.DeepEqual(containerSecurity.SeccompProfile, restrictedSeccompProfile):
		return errors.New("the proxy container's security context is not the restricted one")
	case !reflect.DeepEqual(container.VolumeMounts, []corev1.VolumeMount{{Name: "tls", ReadOnly: true, MountPath: "/tls"}}):
		return errors.New("the proxy mounts more than its certificate")
	}
	mode := ptr.To[int32](288)
	wantVolumes := []corev1.Volume{{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName: tlsProxyCertSecret, DefaultMode: mode,
		Items: []corev1.KeyToPath{{Key: "tls.crt", Path: "tls.crt", Mode: mode}, {Key: "tls.key", Path: "tls.key", Mode: mode}},
	}}}}
	if !reflect.DeepEqual(spec.Volumes, wantVolumes) {
		return errors.New("the Pod mounts another volume than its certificate")
	}
	return nil
}

// tlsProxyServiceSingleStack holds the proxy's Service to one address family,
// ready endpoints only, the proxy's own selector and its one TLS port.
func tlsProxyServiceSingleStack(service *corev1.Service, name string) bool {
	spec := service.Spec
	return spec.IPFamilyPolicy != nil && *spec.IPFamilyPolicy == corev1.IPFamilyPolicySingleStack &&
		len(spec.IPFamilies) == 1 && !spec.PublishNotReadyAddresses &&
		maps.Equal(spec.Selector, map[string]string{
			"app.kubernetes.io/name": name, "app.kubernetes.io/component": "e2e-tls-registry-proxy",
		}) &&
		len(spec.Ports) == 1 && spec.Ports[0].Name == "tls" && spec.Ports[0].Port == 5443 &&
		spec.Ports[0].TargetPort == intstr.FromString("tls") && spec.Ports[0].Protocol == corev1.ProtocolTCP
}

// digestPinDockerSecretExact holds the digest-pin row's Docker config Secret
// to the grants its owner fixed: the registry, plain HTTP allowed, and one
// auths entry for that registry whose basic authentication is its login.
func digestPinDockerSecretExact(secret *corev1.Secret, registry, username, password string) error {
	if secret.Immutable == nil || !*secret.Immutable || secret.Type != corev1.SecretTypeDockerConfigJson ||
		!slices.Equal(sortedKeys(secret.Data), []string{".dockerconfigjson", "allowPlainHTTP", "registry"}) ||
		string(secret.Data["registry"]) != registry || string(secret.Data["allowPlainHTTP"]) != "true" {
		return errors.New("the Secret does not carry exactly the registry, plain HTTP and Docker config grants")
	}
	gotUsername, gotPassword, auth, err := dockerConfigAuth(secret.Data[".dockerconfigjson"], registry)
	if err != nil {
		return err
	}
	if gotUsername != username || gotPassword != password || auth != base64String(username+":"+password) {
		return errors.New("the Docker config does not carry the registry's login")
	}
	return nil
}

// externalEndpointSliceExact holds the external PostgreSQL's EndpointSlice to
// the one route the phase wrote: the container's address, ready, on the
// PostgreSQL port, owned by the Service.
func externalEndpointSliceExact(slice *discoveryv1.EndpointSlice, service string, serviceUID types.UID, owner, address string) error {
	wantOwners := []metav1.OwnerReference{{
		APIVersion: "v1", Kind: "Service", Name: service, UID: serviceUID,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(false),
	}}
	wantEndpoints := []discoveryv1.Endpoint{{
		Addresses: []string{address}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
	}}
	wantPorts := []discoveryv1.EndpointPort{{Name: ptr.To("postgresql"), Port: ptr.To[int32](5432), Protocol: ptr.To(corev1.ProtocolTCP)}}
	switch {
	case slice.AddressType != discoveryv1.AddressTypeIPv4:
		return errors.New("the address type is not IPv4")
	case slice.Labels["kubernetes.io/service-name"] != service ||
		slice.Labels["endpointslice.kubernetes.io/managed-by"] != "ptah-operator-e2e" ||
		slice.Labels["app.kubernetes.io/component"] != "e2e-external-database" ||
		slice.Labels["operator.ptah.run/e2e-owner"] != owner:
		return errors.New("the labels are not the route's")
	case !reflect.DeepEqual(slice.OwnerReferences, wantOwners):
		return errors.New("the Service is not its one owner")
	case !reflect.DeepEqual(slice.Endpoints, wantEndpoints):
		return errors.New("the endpoints are not the container's one ready address")
	case !reflect.DeepEqual(slice.Ports, wantPorts):
		return errors.New("the ports are not PostgreSQL's alone")
	}
	return nil
}

// externalContainerContract holds the external PostgreSQL container to what
// makes it a disposable database outside the cluster. HostConfig.Tmpfs pins
// the tmpfs itself; Docker 29 stopped listing tmpfs in Mounts, so what Mounts
// is held to is the absence of anything persistent, on daemons that list the
// tmpfs and on daemons that do not.
func externalContainerContract(inspection dockerInspection, id, image, owner, address string) error {
	const dataDirectory = "/var/lib/postgresql/data"
	switch {
	case inspection.ID != id:
		return errors.New("external PostgreSQL container identity changed")
	case !inspection.State.Running:
		return errors.New("external PostgreSQL container is not running")
	case inspection.HostConfig.RestartPolicy.Name != "no":
		return errors.New("external PostgreSQL container gained a restart policy")
	case inspection.HostConfig.PublishAllPorts:
		return errors.New("external PostgreSQL container publishes all ports")
	case inspection.Config.Image != image:
		return errors.New("external PostgreSQL container lost its digest-pinned image")
	case inspection.Config.Labels["operator.ptah.run/e2e-owner"] != owner:
		return errors.New("external PostgreSQL container lost its task owner label")
	case inspection.Config.Labels["operator.ptah.run/e2e-component"] != "external-postgresql":
		return errors.New("external PostgreSQL container lost its component label")
	case len(inspection.HostConfig.PortBindings) != 0:
		return errors.New("external PostgreSQL container has host port bindings")
	}
	for _, binding := range inspection.NetworkSettings.Ports {
		if string(binding) != "null" {
			return errors.New("external PostgreSQL container exposes a host port")
		}
	}
	if !slices.Equal(sortedKeys(inspection.HostConfig.Tmpfs), []string{dataDirectory}) {
		return errors.New("external PostgreSQL data directory is not an exact tmpfs")
	}
	for _, mount := range inspection.Mounts {
		if mount.Type != "tmpfs" || mount.Destination != dataDirectory {
			return errors.New("external PostgreSQL container has a persistent or unexpected mount")
		}
	}
	if !onlyKindNetwork(inspection, address) {
		return errors.New("external PostgreSQL container left its exact kind-network address")
	}
	return nil
}

// externalNotHosted refuses a workload in the namespace that is, or stands in
// for, the external database: one named for its Service, labeled as it, or
// running its image.
func externalNotHosted(deployments []appsv1.Deployment, statefulSets []appsv1.StatefulSet, pods []corev1.Pod,
	jobs []batchv1.Job, service, image string,
) error {
	impersonates := func(meta metav1.ObjectMeta, containers []corev1.Container) bool {
		if meta.Name == service || meta.Labels["app.kubernetes.io/component"] == "e2e-external-database" {
			return true
		}
		return slices.ContainsFunc(containers, func(container corev1.Container) bool { return container.Image == image })
	}
	for _, workload := range deployments {
		if impersonates(workload.ObjectMeta, workload.Spec.Template.Spec.Containers) {
			return fmt.Errorf("Deployment %s", workload.Name)
		}
	}
	for _, workload := range statefulSets {
		if impersonates(workload.ObjectMeta, workload.Spec.Template.Spec.Containers) {
			return fmt.Errorf("StatefulSet %s", workload.Name)
		}
	}
	for _, workload := range pods {
		if impersonates(workload.ObjectMeta, workload.Spec.Containers) {
			return fmt.Errorf("Pod %s", workload.Name)
		}
	}
	for _, workload := range jobs {
		if impersonates(workload.ObjectMeta, workload.Spec.Template.Spec.Containers) {
			return fmt.Errorf("Job %s", workload.Name)
		}
	}
	return nil
}
