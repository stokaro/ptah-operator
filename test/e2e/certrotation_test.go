package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Every check below is held to the reading it must accept and to the readings
// it must refuse, the mistakes the shell phase these came from was written
// against. A check that only ever accepted would pass whatever the rotator
// did.

func TestGeneratedSecretRequiresExactSource(t *testing.T) {
	t.Parallel()
	for _, field := range []string{
		"valid", "name", "namespace", "type", "labels", "annotations", "tls.key", "ca.key", "extra data",
		"generateName", "owner", "finalizer", "deleting", "immutable", "stringData", "uid", "resourceVersion",
	} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			secret := generatedSecretFixture()
			switch field {
			case "name":
				secret.Name = "foreign"
			case "namespace":
				secret.Namespace = "foreign"
			case "type":
				secret.Type = corev1.SecretTypeOpaque
			case "labels":
				secret.Labels["unexpected"] = "foreign"
			case "annotations":
				secret.Annotations["unexpected"] = "foreign"
			case "tls.key":
				secret.Data["tls.key"] = nil
			case "ca.key":
				delete(secret.Data, "ca.key")
			case "extra data":
				secret.Data["unexpected"] = []byte("foreign")
			case "generateName":
				secret.GenerateName = "webhook-"
			case "owner":
				secret.OwnerReferences = []metav1.OwnerReference{{Name: "owner"}}
			case "finalizer":
				secret.Finalizers = []string{"example.com/hold"}
			case "deleting":
				secret.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			case "immutable":
				immutable := false
				secret.Immutable = &immutable
			case "stringData":
				secret.StringData = map[string]string{"extra": "value"}
			case "uid":
				secret.UID = ""
			case "resourceVersion":
				secret.ResourceVersion = ""
			}
			err := generatedSecretExact(secret, "webhook-cert", "operator", "ptah")
			if (err == nil) != (field == "valid") {
				t.Fatalf("generatedSecretExact() = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("the refusal carries key material: %v", err)
			}
		})
	}
}

func TestRecreatedSecretShape(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"valid", "type", "labels", "annotations", "missing field", "extra field"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			secret := generatedSecretFixture()
			switch field {
			case "type":
				secret.Type = corev1.SecretTypeOpaque
			case "labels":
				delete(secret.Labels, "app.kubernetes.io/managed-by")
			case "annotations":
				secret.Annotations["meta.helm.sh/release-name"] = "another"
			case "missing field":
				delete(secret.Data, "tls.key")
			case "extra field":
				secret.Data["unexpected"] = []byte("foreign")
			}
			if err := recreatedSecretExact(secret, "operator", "ptah"); (err == nil) != (field == "valid") {
				t.Fatalf("recreatedSecretExact() = %v", err)
			}
		})
	}
}

func TestManagedWebhookInventory(t *testing.T) {
	t.Parallel()
	for _, mutating := range []bool{true, false} {
		for _, scenario := range []string{
			"uniform", "no dormant canary", "foreign namespace", "foreign path", "extra entry on the Service",
			"missing entry", "divergent bundle", "empty bundle", "url entry", "foreign port", "no port", "no path",
		} {
			name := "validating/" + scenario
			if mutating {
				name = "mutating/" + scenario
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				managed := managedValidatingWebhooks()
				canary := "certificate-rotation-canary-validate.operator.ptah.run"
				if mutating {
					managed = managedMutatingWebhooks(true)
					canary = "certificate-rotation-canary-mutate.operator.ptah.run"
				}
				entries := make([]webhookEntry, 0, len(managed)+1)
				for _, webhook := range managed {
					entries = append(entries, serviceEntry(webhook.name, "webhook", "operator", webhook.path, "current-ca"))
				}
				// The dormant canary keeps whatever bundle it last had; the
				// rotator no longer maintains it, and the check must not either.
				entries = append(entries, serviceEntry(canary, "candidate", "operator", "/candidate/mutate", "stale-ca"))
				first := &entries[0].client
				switch scenario {
				case "no dormant canary":
					entries = entries[:len(entries)-1]
				case "foreign namespace":
					first.Service.Namespace = "foreign"
				case "foreign path":
					first.Service.Path = pointer("/foreign")
				case "extra entry on the Service":
					entries = append(entries, serviceEntry("extra.operator.ptah.run", "webhook", "operator", "/extra", "current-ca"))
				case "missing entry":
					entries = entries[1:]
				case "divergent bundle":
					first.CABundle = []byte("other-ca")
				case "empty bundle":
					first.CABundle = nil
				case "url entry":
					first.URL = pointer("https://example.com/validate")
				case "foreign port":
					first.Service.Port = pointer[int32](8443)
				case "no port":
					first.Service.Port = nil
				case "no path":
					first.Service.Path = nil
				}
				bundle, uniform := uniformServiceBundle(entries, managed, "webhook", "operator")
				wantUniform := scenario == "uniform" || scenario == "no dormant canary"
				if uniform != wantUniform || (uniform && string(bundle) != "current-ca") {
					t.Fatalf("uniformServiceBundle() = %q, %v; want uniform %v", bundle, uniform, wantUniform)
				}
			})
		}
	}
}

// The spec-writer entries are part of the mutating inventory only when the
// release turned approvals.requireDistinctApprover on: an installation without
// them reads as uniform with the control off, and one that still carries them
// reads as a mismatch.
func TestManagedWebhookInventoryFollowsFourEyesFlag(t *testing.T) {
	t.Parallel()
	entries := func(names ...string) []webhookEntry {
		paths := map[string]string{}
		for _, webhook := range managedMutatingWebhooks(true) {
			paths[webhook.name] = webhook.path
		}
		var built []webhookEntry
		for _, name := range names {
			built = append(built, serviceEntry(name, "webhook", "operator", paths[name], "current-ca"))
		}
		return built
	}
	approvals := []string{"mapproval.operator.ptah.run", "mmigrationapproval.operator.ptah.run"}
	writers := []string{"mschemawriter.operator.ptah.run", "mmigrationwriter.operator.ptah.run"}
	for _, test := range []struct {
		name        string
		flag        bool
		entries     []webhookEntry
		wantUniform bool
	}{
		{name: "off, only the two approval entries", entries: entries(approvals...), wantUniform: true},
		{name: "off, the spec-writer entries are still present", entries: entries(append(approvals, writers...)...)},
		{name: "on, all four entries", flag: true, entries: entries(append(approvals, writers...)...), wantUniform: true},
		{name: "on, the spec-writer entries are missing", flag: true, entries: entries(approvals...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, uniform := uniformServiceBundle(test.entries, managedMutatingWebhooks(test.flag), "webhook", "operator")
			if uniform != test.wantUniform {
				t.Fatalf("uniform = %v, want %v", uniform, test.wantUniform)
			}
		})
	}
}

func TestReleaseRequiresDistinctApprover(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		values  string
		want    bool
		wantErr bool
	}{
		{values: `{"approvals":{"requireDistinctApprover":true}}`, want: true},
		{values: `{"approvals":{"requireDistinctApprover":false}}`},
		{values: `{"approvals":{}}`},
		{values: `{"approvals":null}`},
		{values: `{}`},
		{values: `{"approvals":{"requireDistinctApprover":"true"}}`, wantErr: true},
		{values: `{"approvals":{"requireDistinctApprover":1}}`, wantErr: true},
		{values: `{"approvals":"on"}`, wantErr: true},
		{values: `not json`, wantErr: true},
	} {
		got, err := releaseRequiresDistinctApprover([]byte(test.values))
		if (err != nil) != test.wantErr || got != test.want {
			t.Errorf("releaseRequiresDistinctApprover(%s) = %v, %v", test.values, got, err)
		}
	}
}

func TestEntryBundleRequiresOneEntryWithABundle(t *testing.T) {
	t.Parallel()
	one := serviceEntry("vapproval.operator.ptah.run", "webhook", "operator", "/validate", "bundle")
	if bundle, found := entryBundle([]webhookEntry{one}, one.name); !found || string(bundle) != "bundle" {
		t.Fatalf("entryBundle() = %q, %v", bundle, found)
	}
	empty := one
	empty.client.CABundle = nil
	for name, entries := range map[string][]webhookEntry{
		"absent":    {serviceEntry("other.operator.ptah.run", "webhook", "operator", "/validate", "bundle")},
		"twice":     {one, one},
		"no bundle": {empty},
	} {
		if _, found := entryBundle(entries, one.name); found {
			t.Errorf("%s: entryBundle() found a bundle", name)
		}
	}
}

func TestWebhookServiceIsTheOneTheApprovalEntryCalls(t *testing.T) {
	t.Parallel()
	configuration := func(services ...string) *admissionregistrationv1.MutatingWebhookConfiguration {
		result := &admissionregistrationv1.MutatingWebhookConfiguration{}
		for _, service := range services {
			webhook := admissionregistrationv1.MutatingWebhook{Name: "mapproval.operator.ptah.run"}
			if service != "" {
				webhook.ClientConfig.Service = &admissionregistrationv1.ServiceReference{Name: service}
			}
			result.Webhooks = append(result.Webhooks, webhook)
		}
		result.Webhooks = append(result.Webhooks, admissionregistrationv1.MutatingWebhook{
			Name:         "mmigrationapproval.operator.ptah.run",
			ClientConfig: admissionregistrationv1.WebhookClientConfig{Service: &admissionregistrationv1.ServiceReference{Name: "unrelated"}},
		})
		return result
	}
	if service, err := webhookService(configuration("webhook", "webhook")); err != nil || service != "webhook" {
		t.Fatalf("webhookService() = %q, %v", service, err)
	}
	for name, candidate := range map[string]*admissionregistrationv1.MutatingWebhookConfiguration{
		"no entry":          configuration(),
		"two Services":      configuration("webhook", "other"),
		"a URL, no Service": configuration(""),
	} {
		if _, err := webhookService(candidate); err == nil {
			t.Errorf("%s: webhookService() accepted it", name)
		}
	}
}

func TestStagingSecretRetired(t *testing.T) {
	t.Parallel()
	retired := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cert-stage", Namespace: "operator",
				Labels: map[string]string{
					"app.kubernetes.io/managed-by":                   "Helm",
					"operator.ptah.run/certificate-rotation-staging": "true",
				},
				Annotations: map[string]string{
					"meta.helm.sh/release-name": "ptah", "meta.helm.sh/release-namespace": "operator",
				},
			},
			Type: corev1.SecretTypeOpaque,
		}
	}
	if !stagingSecretRetired(retired(), "cert-stage", "operator", "ptah") {
		t.Fatal("a retired staging Secret was read as holding a transition")
	}
	for name, edit := range map[string]func(*corev1.Secret){
		"a transition":        func(s *corev1.Secret) { s.Data = map[string][]byte{"phase": []byte("expanded")} },
		"another type":        func(s *corev1.Secret) { s.Type = corev1.SecretTypeTLS },
		"another name":        func(s *corev1.Secret) { s.Name = "foreign" },
		"another namespace":   func(s *corev1.Secret) { s.Namespace = "foreign" },
		"an extra label":      func(s *corev1.Secret) { s.Labels["extra"] = "true" },
		"another release":     func(s *corev1.Secret) { s.Annotations["meta.helm.sh/release-name"] = "other" },
		"no labels":           func(s *corev1.Secret) { s.Labels = nil },
		"an extra annotation": func(s *corev1.Secret) { s.Annotations["extra"] = "true" },
	} {
		secret := retired()
		edit(secret)
		if stagingSecretRetired(secret, "cert-stage", "operator", "ptah") {
			t.Errorf("%s: read as retired", name)
		}
	}
}

func TestExpandedTransitionTime(t *testing.T) {
	t.Parallel()
	record := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cert-stage", Namespace: "operator"},
			Data: map[string][]byte{
				"format":           []byte("v3"),
				"phase":            []byte("expanded"),
				"expanded-at":      []byte("2026-09-26T12:00:00Z"),
				"candidate.ca.key": []byte("CA-PRIVATE-KEY-FIXTURE"),
			},
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(*corev1.Secret)
	}{
		{name: "expanded record"},
		{name: "prepared record", mutate: func(s *corev1.Secret) { s.Data["phase"] = []byte("prepared") }},
		{name: "one-pass record", mutate: func(s *corev1.Secret) { s.Data["format"] = []byte("v2") }},
		{name: "fractional time", mutate: func(s *corev1.Secret) { s.Data["expanded-at"] = []byte("2026-09-26T12:00:00.5Z") }},
		{name: "offset time", mutate: func(s *corev1.Secret) { s.Data["expanded-at"] = []byte("2026-09-26T14:00:00+02:00") }},
		{name: "not a date", mutate: func(s *corev1.Secret) { s.Data["expanded-at"] = []byte("2026-13-45T12:00:00Z") }},
		{name: "no time", mutate: func(s *corev1.Secret) { delete(s.Data, "expanded-at") }},
		{name: "cleared record", mutate: func(s *corev1.Secret) { s.Data = nil }},
		{name: "another Secret", mutate: func(s *corev1.Secret) { s.Name = "foreign" }},
		{name: "another namespace", mutate: func(s *corev1.Secret) { s.Namespace = "foreign" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			secret := record()
			if test.mutate != nil {
				test.mutate(secret)
			}
			instant, accepted := expandedTransitionTime(secret, "cert-stage", "operator")
			if accepted != (test.mutate == nil) {
				t.Fatalf("accepted = %v", accepted)
			}
			if accepted && !instant.Equal(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)) {
				t.Fatalf("expansion time = %s", instant)
			}
		})
	}
}

func TestRotatorCertificateWriteTime(t *testing.T) {
	t.Parallel()
	written := time.Date(2026, 9, 26, 12, 1, 0, 0, time.UTC)
	entry := func(manager string, operation metav1.ManagedFieldsOperationType, data map[string]any) metav1.ManagedFieldsEntry {
		raw, err := json.Marshal(map[string]any{"f:data": data, "f:type": map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		return metav1.ManagedFieldsEntry{
			Manager: manager, Operation: operation, Time: &metav1.Time{Time: written},
			FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: raw},
		}
	}
	certificateFields := map[string]any{"f:ca.crt": map[string]any{}, "f:tls.crt": map[string]any{}}
	for _, test := range []struct {
		name    string
		entries []metav1.ManagedFieldsEntry
		want    bool
	}{
		{name: "rotator wrote the certificate", want: true, entries: []metav1.ManagedFieldsEntry{
			entry("helm", metav1.ManagedFieldsOperationUpdate, map[string]any{"f:ca.key": map[string]any{}}),
			entry("ptah-cert-rotator", metav1.ManagedFieldsOperationUpdate, certificateFields),
		}},
		{name: "another manager wrote it", entries: []metav1.ManagedFieldsEntry{
			entry("kubectl-patch", metav1.ManagedFieldsOperationUpdate, certificateFields),
		}},
		{name: "server-side apply by the rotator's name", entries: []metav1.ManagedFieldsEntry{
			entry("ptah-cert-rotator", metav1.ManagedFieldsOperationApply, certificateFields),
		}},
		{name: "rotator owns other fields only", entries: []metav1.ManagedFieldsEntry{
			entry("ptah-cert-rotator", metav1.ManagedFieldsOperationUpdate, map[string]any{"f:ca.crt": map[string]any{}}),
		}},
		{name: "a subresource write", entries: []metav1.ManagedFieldsEntry{func() metav1.ManagedFieldsEntry {
			status := entry("ptah-cert-rotator", metav1.ManagedFieldsOperationUpdate, certificateFields)
			status.Subresource = "status"
			return status
		}()}},
		{name: "two rotator entries", entries: []metav1.ManagedFieldsEntry{
			entry("ptah-cert-rotator", metav1.ManagedFieldsOperationUpdate, certificateFields),
			entry("ptah-cert-rotator", metav1.ManagedFieldsOperationUpdate, certificateFields),
		}},
		{name: "no time", entries: []metav1.ManagedFieldsEntry{func() metav1.ManagedFieldsEntry {
			untimed := entry("ptah-cert-rotator", metav1.ManagedFieldsOperationUpdate, certificateFields)
			untimed.Time = nil
			return untimed
		}()}},
		{name: "no field management"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{ManagedFields: test.entries}}
			got, err := rotatorCertificateWriteTime(secret)
			if (err == nil) != test.want {
				t.Fatalf("rotatorCertificateWriteTime() = %s, %v", got, err)
			}
			if test.want && !got.Equal(written) {
				t.Fatalf("write time = %s", got)
			}
		})
	}
}

func TestRotatorContainerStartedAt(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Time{Time: started}}}
	status := func(name string, state corev1.ContainerState) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: state}
	}
	for _, test := range []struct {
		name     string
		statuses []corev1.ContainerStatus
		want     bool
	}{
		{name: "rotator running", want: true, statuses: []corev1.ContainerStatus{status("certificate-rotator", running)}},
		{name: "rotator waiting", statuses: []corev1.ContainerStatus{status("certificate-rotator", corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
		})}},
		{name: "rotator terminated", statuses: []corev1.ContainerStatus{status("certificate-rotator", corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{StartedAt: metav1.Time{Time: started}},
		})}},
		{name: "another container running", statuses: []corev1.ContainerStatus{status("verify-candidate-runtime", running)}},
		{name: "two rotator statuses", statuses: []corev1.ContainerStatus{
			status("certificate-rotator", running), status("certificate-rotator", running),
		}},
		{name: "no status"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: test.statuses}}
			got, err := rotatorContainerStartedAt(pod)
			if (err == nil) != test.want {
				t.Fatalf("rotatorContainerStartedAt() = %s, %v", got, err)
			}
			if test.want && !got.Equal(started) {
				t.Fatalf("start time = %s", got)
			}
		})
	}
}

func TestSwitchedAfterDelay(t *testing.T) {
	t.Parallel()
	expanded := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		switched time.Time
		want     bool
	}{
		{name: "exactly at the switch time", switched: expanded.Add(time.Minute), want: true},
		{name: "later", switched: expanded.Add(time.Hour), want: true},
		{name: "one second early", switched: expanded.Add(59 * time.Second)},
		{name: "before the expansion", switched: expanded.Add(-time.Hour)},
	} {
		if got := switchedAfterDelay(test.switched, expanded, time.Minute); got != test.want {
			t.Errorf("%s: switchedAfterDelay() = %v", test.name, got)
		}
	}
}

func TestCASwitchDelay(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		args []string
		want time.Duration
	}{
		{name: "seconds", args: []string{"--run-interval=168h", "--ca-switch-delay=60s"}, want: time.Minute},
		{name: "hours", args: []string{"--ca-switch-delay=6h"}},
		{name: "zero", args: []string{"--ca-switch-delay=0s"}},
		{name: "compound", args: []string{"--ca-switch-delay=1m30s"}},
		{name: "no unit", args: []string{"--ca-switch-delay=60"}},
		{name: "absent", args: []string{"--run-interval=168h"}},
		{name: "twice", args: []string{"--ca-switch-delay=60s", "--ca-switch-delay=60s"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := caSwitchDelay(rotatorDeploymentFixture(test.args...))
			if (err == nil) != (test.want != 0) {
				t.Fatalf("caSwitchDelay() = %s, %v", got, err)
			}
			if test.want != 0 && got != test.want {
				t.Fatalf("delay = %s, want %s", got, test.want)
			}
			if test.want == 0 && !strings.Contains(err.Error(), "--ca-switch-delay") {
				t.Fatalf("refusal does not name the flag: %v", err)
			}
		})
	}
	// Another container carrying the flag is not the rotator's setting.
	deployment := rotatorDeploymentFixture()
	deployment.Spec.Template.Spec.Containers = append(deployment.Spec.Template.Spec.Containers,
		corev1.Container{Name: "sidecar", Args: []string{"--ca-switch-delay=60s"}})
	if _, err := caSwitchDelay(deployment); err == nil {
		t.Error("a sidecar's --ca-switch-delay was read as the rotator's")
	}
}

func TestContainerArguments(t *testing.T) {
	t.Parallel()
	deployment := rotatorDeploymentFixture("--staging-secret-name=cert-stage", "--recreate-missing-secret=true")
	if value, found := containerArgument(deployment, rotatorContainer, "staging-secret-name"); !found || value != "cert-stage" {
		t.Fatalf("containerArgument() = %q, %v", value, found)
	}
	if !containerHasArgument(deployment, rotatorContainer, "--recreate-missing-secret=true") {
		t.Fatal("the recreation opt-in was not found")
	}
	for name, args := range map[string][]string{
		"opted out":    {"--recreate-missing-secret=false"},
		"not the flag": {"--recreate-missing-secret=true-ish"},
		"no arguments": nil,
	} {
		if containerHasArgument(rotatorDeploymentFixture(args...), rotatorContainer, "--recreate-missing-secret=true") {
			t.Errorf("%s: read as opted in", name)
		}
	}
	for name, args := range map[string][]string{
		"absent": {"--recreate-missing-secret=true"},
		"twice":  {"--staging-secret-name=one", "--staging-secret-name=two"},
		"prefix": {"--staging-secret-name-extra=one"},
	} {
		if _, found := containerArgument(rotatorDeploymentFixture(args...), rotatorContainer, "staging-secret-name"); found {
			t.Errorf("%s: found a staging Secret name", name)
		}
	}
}

func TestBundleContainsCertificateDespiteSharedSubjects(t *testing.T) {
	t.Parallel()
	// Every CA the rotator issues for one Service has the same subject, and
	// Go sets no authority key identifier on a self-signed certificate, so
	// nothing but the subject tells a verifier which root in a bundle to try.
	old, staged, absent := sameSubjectCA(t, 1), sameSubjectCA(t, 2), sameSubjectCA(t, 3)
	bundle := append(append([]byte(nil), old.encoded...), staged.encoded...)
	for _, test := range []struct {
		name   string
		bundle []byte
		wanted *x509.Certificate
		want   bool
	}{
		{name: "the first root", bundle: bundle, wanted: old.certificate, want: true},
		{name: "the second root", bundle: bundle, wanted: staged.certificate, want: true},
		{name: "a root the bundle lacks", bundle: bundle, wanted: absent.certificate},
		{name: "an empty bundle", bundle: nil, wanted: old.certificate},
		{name: "a bundle that is not PEM", bundle: []byte("not PEM\n"), wanted: old.certificate},
	} {
		if got := bundleContains(test.bundle, test.wanted); got != test.want {
			t.Errorf("%s: bundleContains() = %v", test.name, got)
		}
	}
}

func TestTrustKeepsEachEntrysRootApart(t *testing.T) {
	t.Parallel()
	now := time.Now()
	serving, servingRoot, err := fixtureAuthority("serving", now)
	if err != nil {
		t.Fatal(err)
	}
	own, ownRoot, err := fixtureAuthority("own", now)
	if err != nil {
		t.Fatal(err)
	}
	_, foreignRoot, err := fixtureAuthority("foreign", now)
	if err != nil {
		t.Fatal(err)
	}
	overlap, err := overlapBundle(serving, own)
	if err != nil {
		t.Fatalf("overlapBundle() = %v", err)
	}
	if !trusts(overlap, servingRoot) || !trusts(overlap, ownRoot) {
		t.Fatal("the overlap does not trust its two roots")
	}
	if trusts(overlap, foreignRoot) {
		t.Fatal("the overlap trusts another entry's root")
	}
	if trusts(nil, servingRoot) || trusts([]byte("not a certificate"), servingRoot) {
		t.Fatal("an empty or broken bundle trusts a root")
	}
	if _, err := overlapBundle(serving, nil); err == nil {
		t.Fatal("an overlap of one certificate was accepted")
	}
	if _, err := overlapBundle(overlap, own); err == nil {
		t.Fatal("an overlap of three certificates was accepted")
	}
	if err := exactlyTwoCertificates(append(append([]byte(nil), serving...), "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"...)); err == nil {
		t.Fatal("a bundle whose second certificate does not parse was accepted")
	}
	// A block the PEM decoder cannot read at all is skipped by it without a
	// word, and OpenSSL refuses the whole file. Two good certificates beside
	// one such block are not a bundle of exactly two.
	for name, broken := range brokenPEMBlocks() {
		bundle := append(append([]byte(nil), overlap...), broken...)
		if err := exactlyTwoCertificates(bundle); err == nil {
			t.Errorf("%s: two certificates beside it were accepted as exactly two", name)
		}
		if trusts(bundle, servingRoot) {
			t.Errorf("%s: a bundle carrying it still trusts the serving root", name)
		}
		if bundleContains(bundle, ownRoot) {
			t.Errorf("%s: a bundle carrying it was read for membership", name)
		}
		// In front of the good blocks, too: the decoder skips it on the way
		// to the first one it can read.
		if err := exactlyTwoCertificates(append(append([]byte(nil), broken...), overlap...)); err == nil {
			t.Errorf("%s: a broken block ahead of two certificates was accepted", name)
		}
	}
}

// brokenPEMBlocks are blocks that begin as a certificate and do not decode:
// one cut off before its END line, one whose body is not base64.
func brokenPEMBlocks() map[string][]byte {
	return map[string][]byte{
		"a truncated block":     []byte("-----BEGIN CERTIFICATE-----\nMIIBszCCAVmgAwIBAgIBATAKBggqhkjOPQQDAjAdMRswGQYDVQQDExJ3ZWJob29r\n"),
		"a block not in base64": []byte("-----BEGIN CERTIFICATE-----\n!!!not base64!!!\n-----END CERTIFICATE-----\n"),
	}
}

func TestFixtureAuthorityIsAShortLivedRoot(t *testing.T) {
	t.Parallel()
	now := time.Now().Truncate(time.Second)
	encoded, certificate, err := fixtureAuthority("mutating", now)
	if err != nil {
		t.Fatal(err)
	}
	if bytes := string(encoded); strings.Contains(bytes, "PRIVATE KEY") {
		t.Fatal("the fixture carries its key")
	}
	switch {
	case !certificate.IsCA || !certificate.BasicConstraintsValid || certificate.MaxPathLen != 0 || !certificate.MaxPathLenZero:
		t.Fatalf("the fixture is not a CA limited to path length zero: %+v", certificate.BasicConstraintsValid)
	case certificate.KeyUsage != x509.KeyUsageCertSign|x509.KeyUsageCRLSign:
		t.Fatalf("key usage = %v", certificate.KeyUsage)
	case certificate.Subject.CommonName != "ptah-e2e-mutating":
		t.Fatalf("subject = %s", certificate.Subject)
	case !certificate.NotAfter.Equal(now.Add(24 * time.Hour)):
		t.Fatalf("valid until %s, want a day", certificate.NotAfter)
	}
}

func TestSelfSignedRootAndIssuer(t *testing.T) {
	t.Parallel()
	root := sameSubjectCA(t, 1)
	other := sameSubjectCA(t, 2)
	leaf := issuedLeaf(t, root)
	if _, err := selfSignedRoot(root.encoded); err != nil {
		t.Fatalf("selfSignedRoot(root) = %v", err)
	}
	for name, candidate := range map[string][]byte{
		"a leaf":            leaf,
		"nothing":           nil,
		"not a certificate": []byte("not a certificate"),
	} {
		if _, err := selfSignedRoot(candidate); err == nil {
			t.Errorf("%s: read as a self-signed root", name)
		}
	}
	for name, broken := range brokenPEMBlocks() {
		if _, err := selfSignedRoot(append(append([]byte(nil), root.encoded...), broken...)); err == nil {
			t.Errorf("a root followed by %s was read as a valid self-signed root", name)
		}
		if err := issuedBy(leaf, append(append([]byte(nil), root.encoded...), broken...)); err == nil {
			t.Errorf("an authority file carrying %s verified a leaf", name)
		}
	}
	if err := issuedBy(leaf, root.encoded); err != nil {
		t.Fatalf("issuedBy(leaf, root) = %v", err)
	}
	if err := issuedBy(leaf, other.encoded); err == nil {
		t.Fatal("a leaf verified against a root that did not issue it")
	}
	if err := issuedBy(nil, root.encoded); err == nil {
		t.Fatal("no certificate verified")
	}
}

func TestMentionsPrivateKey(t *testing.T) {
	t.Parallel()
	for _, log := range []string{
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n",
		"-----BEGIN EC PRIVATE KEY-----",
		"loaded private_key from the Secret",
		"wrote privatekey",
		"Private-Key: (2048 bit)",
	} {
		if !mentionsPrivateKey([]byte(log)) {
			t.Errorf("%q passed the scan", log)
		}
	}
	for _, log := range []string{
		"rotated the serving certificate; expansion recorded at 2026-09-26T12:00:00Z",
		"-----BEGIN CERTIFICATE-----",
		"key usage: cert sign",
	} {
		if mentionsPrivateKey([]byte(log)) {
			t.Errorf("%q failed the scan", log)
		}
	}
}

func TestLivePodIsTheOneReadyPodNotBeingDeleted(t *testing.T) {
	t.Parallel()
	pod := func(name string, ready, deleting bool) corev1.Pod {
		result := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if ready {
			result.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}
		if deleting {
			result.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		}
		return result
	}
	if live, found := livePod([]corev1.Pod{pod("old", true, true), pod("new", true, false)}); !found || live.Name != "new" {
		t.Fatalf("livePod() = %s, %v; want the replacement beside the terminating Pod", live.Name, found)
	}
	for name, pods := range map[string][]corev1.Pod{
		"none":             nil,
		"only terminating": {pod("old", true, true)},
		"not ready":        {pod("new", false, false)},
		"two live":         {pod("one", true, false), pod("two", true, false)},
	} {
		if _, found := livePod(pods); found {
			t.Errorf("%s: found a live Pod", name)
		}
	}
}

func TestReadyEndpointAddressesCountsDistinctReadyAddresses(t *testing.T) {
	t.Parallel()
	ready, notReady := true, false
	endpoint := func(state *bool, addresses ...string) discoveryv1.Endpoint {
		return discoveryv1.Endpoint{Addresses: addresses, Conditions: discoveryv1.EndpointConditions{Ready: state}}
	}
	slices := []discoveryv1.EndpointSlice{
		{Endpoints: []discoveryv1.Endpoint{endpoint(&ready, "10.0.0.1"), endpoint(&notReady, "10.0.0.2"), endpoint(nil, "10.0.0.3")}},
		// The same address in a second slice, as a slice rotation leaves it,
		// is one endpoint.
		{Endpoints: []discoveryv1.Endpoint{endpoint(&ready, "10.0.0.1"), endpoint(&ready, "10.0.0.4")}},
		{},
	}
	if got := readyEndpointAddresses(slices); got != 2 {
		t.Fatalf("readyEndpointAddresses() = %d, want 2", got)
	}
	if got := readyEndpointAddresses(nil); got != 0 {
		t.Fatalf("readyEndpointAddresses(nil) = %d", got)
	}
}

func TestWebhookCertificateSecret(t *testing.T) {
	t.Parallel()
	deployment := func(volumes ...corev1.Volume) *appsv1.Deployment {
		result := &appsv1.Deployment{}
		result.Spec.Template.Spec.Volumes = volumes
		return result
	}
	secretVolume := func(name, secret string) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secret}}}
	}
	if name, err := webhookCertificateSecret(deployment(secretVolume("tmp", "other"), secretVolume("webhook-cert", "ptah-webhook-cert"))); err != nil || name != "ptah-webhook-cert" {
		t.Fatalf("webhookCertificateSecret() = %q, %v", name, err)
	}
	for name, candidate := range map[string]*appsv1.Deployment{
		"no volume":       deployment(secretVolume("tmp", "other")),
		"not a Secret":    deployment(corev1.Volume{Name: "webhook-cert"}),
		"no Secret named": deployment(secretVolume("webhook-cert", "")),
	} {
		if _, err := webhookCertificateSecret(candidate); err == nil {
			t.Errorf("%s: found a Secret", name)
		}
	}
}

func TestSecretStateBracketsAnInterval(t *testing.T) {
	t.Parallel()
	secret := generatedSecretFixture()
	before := presentSecretState(secret)
	if before != presentSecretState(secret.DeepCopy()) {
		t.Fatal("two readings of one Secret differ")
	}
	written := secret.DeepCopy()
	written.ResourceVersion = "later"
	if before == presentSecretState(written) {
		t.Fatal("a write that kept ca.crt went unnoticed")
	}
	switched := secret.DeepCopy()
	switched.Data["ca.crt"] = []byte("another CA")
	if before == presentSecretState(switched) {
		t.Fatal("a switched ca.crt went unnoticed")
	}
	if before == (secretState{absent: true}) {
		t.Fatal("a present Secret reads as absent")
	}
}

func generatedSecretFixture() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "webhook-cert", Namespace: "operator", UID: "original-uid", ResourceVersion: "original",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "Helm", "operator.ptah.run/generated-webhook-certificate": "true",
			},
			Annotations: map[string]string{
				"meta.helm.sh/release-name": "ptah", "meta.helm.sh/release-namespace": "operator",
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"ca.crt": []byte("CA-FIXTURE"), "ca.key": []byte("CA-PRIVATE-KEY-FIXTURE"),
			"tls.crt": []byte("CERT-FIXTURE"), "tls.key": []byte("TLS-PRIVATE-KEY-FIXTURE"),
		},
	}
}

func rotatorDeploymentFixture(args ...string) *appsv1.Deployment {
	deployment := &appsv1.Deployment{}
	deployment.Spec.Template.Spec.Containers = []corev1.Container{{Name: rotatorContainer, Args: args}}
	return deployment
}

func serviceEntry(name, service, namespace, path, bundle string) webhookEntry {
	return webhookEntry{name: name, client: admissionregistrationv1.WebhookClientConfig{
		CABundle: []byte(bundle),
		Service: &admissionregistrationv1.ServiceReference{
			Name: service, Namespace: namespace, Path: pointer(path), Port: pointer[int32](443),
		},
	}}
}

type authority struct {
	encoded     []byte
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
}

func sameSubjectCA(t *testing.T, serial int64) authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: "webhook webhook CA"},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return authority{encoded: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), certificate: certificate, key: key}
}

func issuedLeaf(t *testing.T, issuer authority) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(100),
		Subject:      pkix.Name{CommonName: "webhook.operator.svc"},
		DNSNames:     []string{"webhook.operator.svc"},
		NotBefore:    time.Now().Add(-5 * time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer.certificate, key.Public(), issuer.key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
