// Package e2e holds the acceptance phases the Go harness runs against the
// cluster hack/e2e-kind.sh stands up.
//
// Each phase is one test function in a file built only with the e2e tag, and
// test/e2e/phases declares it. What a phase decides from what it read lives
// in untagged files beside it, as plain functions over typed objects, so the
// unit tests hold every one of them to a reading it must accept and the
// readings it must refuse without a cluster.
package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	"github.com/stokaro/ptah-operator/internal/certrotation"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

const (
	rotatorContainer = "certificate-rotator"
	rotatorManager   = "ptah-cert-rotator"

	generatedCertificateLabel = "operator.ptah.run/generated-webhook-certificate"
	stagingSecretLabel        = "operator.ptah.run/certificate-rotation-staging"

	// recoveryGuardRefusal is the message the rotator's Secret CREATE guard
	// denies with, which internal/certrotation/secret_create_guard.go writes.
	recoveryGuardRefusal = "certificate rotator Secret CREATE is outside its exact recovery contract"
)

// managedWebhook is one entry the rotator keeps: its name and the path on the
// webhook Service it calls.
type managedWebhook struct {
	name string
	path string
}

// managedMutatingWebhooks is the mutating entries this release's admission
// singleton carries. The spec-writer entries exist only when the release has
// approvals.requireDistinctApprover on, as charts/ptah-operator/templates/webhook.yaml
// renders them, so the inventory follows the live release value.
func managedMutatingWebhooks(requireDistinctApprover bool) []managedWebhook {
	entries := []managedWebhook{
		{name: "mapproval.operator.ptah.run", path: "/mutate-operator-ptah-run-v1alpha1-ptahschemaapproval"},
		{name: "mmigrationapproval.operator.ptah.run", path: "/mutate-operator-ptah-run-v1alpha1-ptahmigrationapproval"},
		{name: "mmigrationrunacknowledgment.operator.ptah.run", path: "/mutate-operator-ptah-run-v1alpha1-ptahmigrationrunacknowledgment"},
	}
	if requireDistinctApprover {
		entries = append(entries,
			managedWebhook{name: "mschemawriter.operator.ptah.run", path: "/mutate-operator-ptah-run-v1alpha1-ptahschema"},
			managedWebhook{name: "mmigrationwriter.operator.ptah.run", path: "/mutate-operator-ptah-run-v1alpha1-ptahmigration"},
		)
	}
	return entries
}

// managedValidatingWebhooks is the validating entries the admission singleton
// carries.
func managedValidatingWebhooks() []managedWebhook {
	return []managedWebhook{
		{name: "vapproval.operator.ptah.run", path: "/validate-operator-ptah-run-v1alpha1-ptahschemaapproval"},
		{name: "vmigrationapproval.operator.ptah.run", path: "/validate-operator-ptah-run-v1alpha1-ptahmigrationapproval"},
		{name: "vmigrationrunacknowledgment.operator.ptah.run", path: "/validate-operator-ptah-run-v1alpha1-ptahmigrationrunacknowledgment"},
		{name: "vpodintent.operator.ptah.run", path: "/validate-v1-pod-ptah-operation-intent"},
		{name: "vcontrollerwrite.operator.ptah.run", path: "/validate-operator-controller-write"},
	}
}

// webhookEntry is the part of a mutating or validating webhook the rotator
// manages, so one check reads both kinds.
type webhookEntry struct {
	name   string
	client admissionregistrationv1.WebhookClientConfig
}

func mutatingEntries(configuration *admissionregistrationv1.MutatingWebhookConfiguration) []webhookEntry {
	entries := make([]webhookEntry, 0, len(configuration.Webhooks))
	for _, webhook := range configuration.Webhooks {
		entries = append(entries, webhookEntry{name: webhook.Name, client: webhook.ClientConfig})
	}
	return entries
}

func validatingEntries(configuration *admissionregistrationv1.ValidatingWebhookConfiguration) []webhookEntry {
	entries := make([]webhookEntry, 0, len(configuration.Webhooks))
	for _, webhook := range configuration.Webhooks {
		entries = append(entries, webhookEntry{name: webhook.Name, client: webhook.ClientConfig})
	}
	return entries
}

// releaseRequiresDistinctApprover reads approvals.requireDistinctApprover out
// of `helm get values --all -o json`. Absent reads as false, as the chart
// defaults it; anything that is not a boolean is refused rather than guessed.
func releaseRequiresDistinctApprover(values []byte) (bool, error) {
	var document map[string]any
	if err := json.Unmarshal(values, &document); err != nil {
		return false, fmt.Errorf("the release values are not a JSON object: %w", err)
	}
	approvals, present := document["approvals"]
	if !present || approvals == nil {
		return false, nil
	}
	section, ok := approvals.(map[string]any)
	if !ok {
		return false, fmt.Errorf("approvals on the live release is not an object: %v", approvals)
	}
	switch value := section["requireDistinctApprover"].(type) {
	case nil:
		return false, nil
	case bool:
		return value, nil
	default:
		return false, fmt.Errorf("approvals.requireDistinctApprover on the live release is not a boolean: %v", value)
	}
}

// uniformServiceBundle returns the one caBundle every entry that targets the
// webhook Service carries. It returns false unless those entries are exactly
// the managed inventory, each calling its own path on port 443 of the Service
// in the release namespace, and every one carries the same non-empty bundle.
// Entries that target another Service, such as the retired certificate
// canaries, keep whatever bundle they had and are not read.
func uniformServiceBundle(entries []webhookEntry, managed []managedWebhook, service, namespace string) ([]byte, bool) {
	paths := map[string]string{}
	for _, entry := range managed {
		paths[entry.name] = entry.path
	}
	var names []string
	var bundle []byte
	for _, entry := range entries {
		reference := entry.client.Service
		if reference == nil || reference.Name != service {
			continue
		}
		names = append(names, entry.name)
		path, known := paths[entry.name]
		if !known ||
			entry.client.URL != nil ||
			reference.Namespace != namespace ||
			reference.Path == nil || *reference.Path != path ||
			reference.Port == nil || *reference.Port != 443 ||
			len(entry.client.CABundle) == 0 {
			return nil, false
		}
		if bundle == nil {
			bundle = entry.client.CABundle
		} else if !bytes.Equal(bundle, entry.client.CABundle) {
			return nil, false
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, slices.Sorted(maps.Keys(paths))) {
		return nil, false
	}
	return bundle, true
}

// entryBundle returns the caBundle of the one entry with the name, and false
// when there is not exactly one or it carries none.
func entryBundle(entries []webhookEntry, name string) ([]byte, bool) {
	var found []webhookEntry
	for _, entry := range entries {
		if entry.name == name {
			found = append(found, entry)
		}
	}
	if len(found) != 1 || len(found[0].client.CABundle) == 0 {
		return nil, false
	}
	return found[0].client.CABundle, true
}

// webhookService is the one Service the schema-approval mutating entry calls.
func webhookService(configuration *admissionregistrationv1.MutatingWebhookConfiguration) (string, error) {
	services := map[string]bool{}
	for _, webhook := range configuration.Webhooks {
		if webhook.Name != "mapproval.operator.ptah.run" {
			continue
		}
		if webhook.ClientConfig.Service == nil || webhook.ClientConfig.Service.Name == "" {
			return "", errors.New("the schema-approval entry calls no Service")
		}
		services[webhook.ClientConfig.Service.Name] = true
	}
	if len(services) != 1 {
		return "", fmt.Errorf("the schema-approval entries call %d Services, want exactly one", len(services))
	}
	return slices.Collect(maps.Keys(services))[0], nil
}

// stagingSecretRetired reports whether the rotator's staging Secret holds no
// transition: the exact Helm-owned shape the chart creates, with no data.
func stagingSecretRetired(secret *corev1.Secret, name, namespace, release string) bool {
	return secret.Type == corev1.SecretTypeOpaque &&
		secret.Name == name && secret.Namespace == namespace &&
		maps.Equal(secret.Labels, map[string]string{
			"app.kubernetes.io/managed-by": "Helm",
			stagingSecretLabel:             "true",
		}) &&
		maps.Equal(secret.Annotations, releaseAnnotations(release, namespace)) &&
		len(secret.Data) == 0
}

func releaseAnnotations(release, namespace string) map[string]string {
	return map[string]string{
		"meta.helm.sh/release-name":      release,
		"meta.helm.sh/release-namespace": namespace,
	}
}

// containerArgument returns the value of the one --name= argument the
// rotator container carries, and false when it carries none or several.
func containerArgument(deployment *appsv1.Deployment, container, name string) (string, bool) {
	prefix := "--" + name + "="
	var values []string
	for _, candidate := range deployment.Spec.Template.Spec.Containers {
		if candidate.Name != container {
			continue
		}
		for _, argument := range candidate.Args {
			if value, found := strings.CutPrefix(argument, prefix); found {
				values = append(values, value)
			}
		}
	}
	if len(values) != 1 {
		return "", false
	}
	return values[0], true
}

// containerHasArgument reports whether the container carries the exact
// argument.
func containerHasArgument(deployment *appsv1.Deployment, container, argument string) bool {
	for _, candidate := range deployment.Spec.Template.Spec.Containers {
		if candidate.Name == container && slices.Contains(candidate.Args, argument) {
			return true
		}
	}
	return false
}

var wholeSeconds = regexp.MustCompile(`^[1-9][0-9]*s$`)

// caSwitchDelay is the rotator's --ca-switch-delay. The ordering proofs
// measure the switch against it, so a value that is not one whole number of
// seconds is refused rather than parsed into something the proofs did not
// mean.
func caSwitchDelay(deployment *appsv1.Deployment) (time.Duration, error) {
	value, ok := containerArgument(deployment, rotatorContainer, "ca-switch-delay")
	if !ok || !wholeSeconds.MatchString(value) {
		if !ok {
			value = "absent"
		}
		return 0, fmt.Errorf("the rotator's --ca-switch-delay is not one whole number of seconds: %s", value)
	}
	seconds, err := strconv.ParseInt(strings.TrimSuffix(value, "s"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("the rotator's --ca-switch-delay is not one whole number of seconds: %s", value)
	}
	return time.Duration(seconds) * time.Second, nil
}

var transitionInstant = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)

// expandedTransitionTime returns when the rotator expanded trust, from a
// staging Secret whose record is in its expanded phase, and false for any
// other staging state. It reads the record's public fields only.
func expandedTransitionTime(secret *corev1.Secret, name, namespace string) (time.Time, bool) {
	if secret.Name != name || secret.Namespace != namespace ||
		string(secret.Data["format"]) != certrotation.StagingFormat || string(secret.Data["phase"]) != "expanded" {
		return time.Time{}, false
	}
	value := string(secret.Data["expanded-at"])
	if !transitionInstant.MatchString(value) {
		return time.Time{}, false
	}
	instant, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}
	return instant, true
}

// rotatorCertificateWriteTime returns when the rotator last changed the
// generated Secret's serving certificate, from the Secret's own field
// management rather than from when the harness looked. It refuses unless
// exactly one entry records the rotator updating tls.crt on the object itself.
func rotatorCertificateWriteTime(secret *corev1.Secret) (time.Time, error) {
	var times []time.Time
	for _, entry := range secret.ManagedFields {
		if entry.Manager != rotatorManager || entry.Operation != "Update" || entry.Subresource != "" ||
			entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(entry.FieldsV1.Raw, &fields); err != nil {
			return time.Time{}, fmt.Errorf("the rotator's field management does not parse: %w", err)
		}
		data, present := fields["f:data"]
		if !present {
			continue
		}
		var dataFields map[string]json.RawMessage
		if err := json.Unmarshal(data, &dataFields); err != nil {
			return time.Time{}, fmt.Errorf("the rotator's field management of data does not parse: %w", err)
		}
		if _, owns := dataFields["f:tls.crt"]; !owns {
			continue
		}
		if entry.Time == nil {
			return time.Time{}, errors.New("the rotator's write of the serving certificate carries no time")
		}
		times = append(times, entry.Time.Time)
	}
	if len(times) != 1 {
		return time.Time{}, fmt.Errorf("the generated Secret records %d rotator writes of its serving certificate, want exactly one", len(times))
	}
	return times[0], nil
}

// switchedAfterDelay reports whether the switch lies at least delay after the
// expansion.
func switchedAfterDelay(switched, expanded time.Time, delay time.Duration) bool {
	return !switched.Before(expanded.Add(delay))
}

// rotatorContainerStartedAt returns when the Pod's certificate-rotator
// container last started, from the Pod's own status, and refuses unless
// exactly one such container is running.
func rotatorContainerStartedAt(pod *corev1.Pod) (time.Time, error) {
	var started []time.Time
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == rotatorContainer && status.State.Running != nil && !status.State.Running.StartedAt.IsZero() {
			started = append(started, status.State.Running.StartedAt.Time)
		}
	}
	if len(started) != 1 {
		return time.Time{}, fmt.Errorf("pod %s has %d running %s containers with a start time, want exactly one",
			pod.Name, len(started), rotatorContainer)
	}
	return started[0], nil
}

// generatedSecretExact refuses a generated webhook Secret that is not exactly
// the one the chart and the rotator produce: its type, name, labels, release
// annotations, the absence of owners, finalizers and deletion, and exactly
// the four certificate fields, none empty. The error names the first field
// that differs and never a value.
func generatedSecretExact(secret *corev1.Secret, name, namespace, release string) error {
	switch {
	case secret.Type != corev1.SecretTypeTLS:
		return fmt.Errorf("type is %q", secret.Type)
	case secret.Name != name || secret.Namespace != namespace:
		return fmt.Errorf("it is %s/%s", secret.Namespace, secret.Name)
	case secret.GenerateName != "":
		return errors.New("it carries a generateName")
	case secret.UID == "":
		return errors.New("it has no UID")
	case secret.ResourceVersion == "":
		return errors.New("it has no resourceVersion")
	case !maps.Equal(secret.Labels, map[string]string{
		"app.kubernetes.io/managed-by": "Helm",
		generatedCertificateLabel:      "true",
	}):
		return errors.New("its labels are not exactly the Helm owner and the generated-certificate label")
	case !maps.Equal(secret.Annotations, releaseAnnotations(release, namespace)):
		return errors.New("its annotations are not exactly the release's")
	case len(secret.OwnerReferences) != 0:
		return errors.New("it has owner references")
	case len(secret.Finalizers) != 0:
		return errors.New("it has finalizers")
	case secret.DeletionTimestamp != nil:
		return errors.New("it is being deleted")
	case secret.Immutable != nil:
		return errors.New("it sets immutable")
	case len(secret.StringData) != 0:
		return errors.New("it carries stringData")
	}
	return certificateFieldsExact(secret)
}

// recreatedSecretExact is the shape a Secret the rotator recreated must have:
// the chart's type, labels and release annotations, and exactly the four
// fields. That the certificates in it are not empty is the caller's to hold,
// since it compares them with what came before.
func recreatedSecretExact(secret *corev1.Secret, namespace, release string) error {
	switch {
	case secret.Type != corev1.SecretTypeTLS:
		return fmt.Errorf("type is %q", secret.Type)
	case !maps.Equal(secret.Labels, map[string]string{
		generatedCertificateLabel:      "true",
		"app.kubernetes.io/managed-by": "Helm",
	}):
		return errors.New("its labels are not exactly the Helm owner and the generated-certificate label")
	case !maps.Equal(secret.Annotations, releaseAnnotations(release, namespace)):
		return errors.New("its annotations are not exactly the release's")
	case !certificateFieldNames(secret):
		return errors.New("its data is not exactly ca.crt, ca.key, tls.crt and tls.key")
	}
	return nil
}

func certificateFieldNames(secret *corev1.Secret) bool {
	return slices.Equal(slices.Sorted(maps.Keys(secret.Data)), []string{"ca.crt", "ca.key", "tls.crt", "tls.key"})
}

func certificateFieldsExact(secret *corev1.Secret) error {
	if !certificateFieldNames(secret) {
		return errors.New("its data is not exactly ca.crt, ca.key, tls.crt and tls.key")
	}
	for key, value := range secret.Data {
		if len(value) == 0 {
			return fmt.Errorf("its %s is empty", key)
		}
	}
	return nil
}

// secretState is what the generated Secret held at one reading: its ca.crt
// and resourceVersion, or its absence. Two readings that compare equal
// bracket an interval in which the Secret did not change.
type secretState struct {
	absent          bool
	caCertificate   string
	resourceVersion string
}

func presentSecretState(secret *corev1.Secret) secretState {
	return secretState{caCertificate: string(secret.Data["ca.crt"]), resourceVersion: secret.ResourceVersion}
}

// certificates parses every CERTIFICATE block in a PEM bundle, in order, and
// refuses a block that does not parse. Text between blocks and blocks of other
// types are passed over, as a bundle loader passes over them; a block that
// begins and does not decode is refused, as OpenSSL refuses to load the file.
//
// pem.Decode does not report such a block: it skips a BEGIN line whose body is
// not base64 or has no END line, and returns the next block that decodes. So
// every BEGIN line the decoder consumed has to be the one block it returned,
// and none may be left once it finds no more.
func certificates(bundle []byte) ([]*x509.Certificate, error) {
	begin := []byte("-----BEGIN")
	var parsed []*x509.Certificate
	rest := bundle
	for {
		block, next := pem.Decode(rest)
		if block == nil {
			if bytes.Contains(rest, begin) {
				return nil, fmt.Errorf("a PEM block after certificate %d does not decode", len(parsed))
			}
			return parsed, nil
		}
		if consumed := rest[:len(rest)-len(next)]; bytes.Count(consumed, begin) != 1 {
			return nil, fmt.Errorf("a PEM block before certificate %d does not decode", len(parsed)+1)
		}
		rest = next
		if block.Type != "CERTIFICATE" {
			continue
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("certificate %d does not parse: %w", len(parsed)+1, err)
		}
		parsed = append(parsed, certificate)
	}
}

// firstCertificate is the certificate a verifier reads out of a file: the
// first one in it.
func firstCertificate(bundle []byte) (*x509.Certificate, error) {
	parsed, err := certificates(bundle)
	if err != nil {
		return nil, err
	}
	if len(parsed) == 0 {
		return nil, errors.New("it holds no certificate")
	}
	return parsed[0], nil
}

// trusts reports whether a verifier that trusts every certificate in the
// bundle accepts the certificate: it is one of them, or one of them issued
// it. The certificate's intended usage is not a question here, so any is
// accepted.
func trusts(bundle []byte, certificate *x509.Certificate) bool {
	roots, err := certificates(bundle)
	if err != nil || len(roots) == 0 {
		return false
	}
	pool := x509.NewCertPool()
	for _, root := range roots {
		pool.AddCert(root)
	}
	_, err = certificate.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	return err == nil
}

// selfSignedRoot refuses anything but a valid certificate authority that
// signed itself, read as the first certificate of the bundle and trusted by
// the bundle.
func selfSignedRoot(bundle []byte) (*x509.Certificate, error) {
	root, err := firstCertificate(bundle)
	if err != nil {
		return nil, err
	}
	if err := root.CheckSignatureFrom(root); err != nil {
		return nil, fmt.Errorf("it is not self-signed: %w", err)
	}
	if !trusts(bundle, root) {
		return nil, errors.New("it does not verify against itself")
	}
	return root, nil
}

// issuedBy refuses a certificate the authority in the bundle did not issue.
func issuedBy(certificate, authority []byte) error {
	leaf, err := firstCertificate(certificate)
	if err != nil {
		return err
	}
	if !trusts(authority, leaf) {
		return errors.New("it does not verify against the authority")
	}
	return nil
}

// bundleContains reports whether the bundle holds a certificate byte for byte
// identical to want. It compares the certificates rather than asking a
// verifier: every CA the rotator issues for a Service has the same subject,
// and a verifier that picks an issuer by subject tries only the first of two
// same-subject roots and refuses the second.
func bundleContains(bundle []byte, want *x509.Certificate) bool {
	held, err := certificates(bundle)
	if err != nil {
		return false
	}
	for _, certificate := range held {
		if bytes.Equal(certificate.Raw, want.Raw) {
			return true
		}
	}
	return false
}

// exactlyTwoCertificates refuses a bundle that is not two valid certificates.
func exactlyTwoCertificates(bundle []byte) error {
	parsed, err := certificates(bundle)
	if err != nil {
		return err
	}
	if len(parsed) != 2 {
		return fmt.Errorf("it holds %d certificates, want exactly two", len(parsed))
	}
	return nil
}

// fixtureAuthority generates a certificate authority valid for a day, which
// the harness writes into one webhook entry's bundle to prove Helm keeps each
// entry's trust apart. Its key is discarded here: nothing is ever signed with
// it, and a key that is never written cannot leak.
func fixtureAuthority(name string, now time.Time) ([]byte, *x509.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "ptah-e2e-" + name},
		NotBefore:             now,
		NotAfter:              now.Add(24 * time.Hour),
		SignatureAlgorithm:    x509.SHA256WithRSA,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if _, err := selfSignedRoot(encoded); err != nil {
		return nil, nil, fmt.Errorf("the %s fixture is not a valid self-signed root: %w", name, err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return encoded, certificate, nil
}

// overlapBundle is the serving CA followed by one fixture authority: the
// bundle an entry starts from before the upgrade, holding the serving root and
// a distinct root of its own.
func overlapBundle(servingCA, fixture []byte) ([]byte, error) {
	bundle := make([]byte, 0, len(servingCA)+1+len(fixture))
	bundle = append(bundle, servingCA...)
	bundle = append(bundle, '\n')
	bundle = append(bundle, fixture...)
	if err := exactlyTwoCertificates(bundle); err != nil {
		return nil, err
	}
	return bundle, nil
}

var privateKeyMaterial = regexp.MustCompile(`(?i)PRIVATE[ _-]?KEY|-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)

// mentionsPrivateKey reports whether a log carries anything that reads as
// private key material, or names it.
func mentionsPrivateKey(log []byte) bool {
	return privateKeyMaterial.Match(log)
}

// livePod returns the one Pod that is Ready and not being deleted. A Pod
// deletion leaves the replaced Pod terminating while its replacement is
// already available, so a rollout that returned still has two Pods for a
// moment; the terminating one is not the live one.
func livePod(pods []corev1.Pod) (corev1.Pod, bool) {
	var live []corev1.Pod
	for _, pod := range pods {
		if pod.DeletionTimestamp == nil && harness.PodReady(&pod) {
			live = append(live, pod)
		}
	}
	if len(live) != 1 {
		return corev1.Pod{}, false
	}
	return live[0], true
}

// readyEndpointAddresses counts the distinct addresses of the ready endpoints
// in the slices.
func readyEndpointAddresses(endpointSlices []discoveryv1.EndpointSlice) int {
	addresses := map[string]bool{}
	for _, slice := range endpointSlices {
		for _, endpoint := range slice.Endpoints {
			if endpoint.Conditions.Ready == nil || !*endpoint.Conditions.Ready {
				continue
			}
			for _, address := range endpoint.Addresses {
				addresses[address] = true
			}
		}
	}
	return len(addresses)
}

// webhookCertificateSecret is the Secret the manager mounts as its webhook
// certificate.
func webhookCertificateSecret(deployment *appsv1.Deployment) (string, error) {
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name != "webhook-cert" {
			continue
		}
		if volume.Secret == nil || volume.Secret.SecretName == "" {
			return "", errors.New("the webhook-cert volume mounts no Secret")
		}
		return volume.Secret.SecretName, nil
	}
	return "", errors.New("the manager mounts no webhook-cert volume")
}
