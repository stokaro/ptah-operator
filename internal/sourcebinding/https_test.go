package sourcebinding_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
)

const (
	httpsURL      = "https://files.example.com/app/1.4.0/app.tar.gz"
	httpsDownload = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

func httpsBinding() *operatorv1alpha1.HTTPSSourceBinding {
	return &operatorv1alpha1.HTTPSSourceBinding{
		URL: httpsURL, Digest: httpsDownload, Format: operatorv1alpha1.HTTPSFormatTarGzip, Path: "app/db/migrations",
		ContentDigest: content,
	}
}

// The revision is fixed by the URL and the path, and never by the download
// digest, as the API documents it.
func TestHTTPSRevisionNamesTheURLAndThePath(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte(httpsURL + "\napp/db/migrations"))
	want := "sha256:" + hex.EncodeToString(sum[:])
	got := sourcebinding.HTTPSRevision(httpsURL, "app/db/migrations")
	if got != want {
		t.Fatalf("HTTPSRevision() = %q, want %q", got, want)
	}
	for name, other := range map[string]string{
		"another URL":    sourcebinding.HTTPSRevision("https://files.example.com/app/1.5.0/app.tar.gz", "app/db/migrations"),
		"another path":   sourcebinding.HTTPSRevision(httpsURL, "app/db"),
		"the join moved": sourcebinding.HTTPSRevision(httpsURL+"\napp", "db/migrations"),
	} {
		if other == got {
			t.Errorf("%s gives the same revision", name)
		}
	}
}

func TestValidateAccessHoldsAnHTTPSDownload(t *testing.T) {
	t.Parallel()
	ca := &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "ca"}, Key: "ca.pem"}
	for name, edit := range map[string]func(*operatorv1alpha1.HTTPSSourceBinding){
		"an archive":   func(*operatorv1alpha1.HTTPSSourceBinding) {},
		"a zip's root": func(b *operatorv1alpha1.HTTPSSourceBinding) { b.Format, b.Path = operatorv1alpha1.HTTPSFormatZip, "." },
		"a File": func(b *operatorv1alpha1.HTTPSSourceBinding) {
			b.Format, b.Path = operatorv1alpha1.HTTPSFormatFile, "schema.hcl"
		},
		"a credential and a CA": func(b *operatorv1alpha1.HTTPSSourceBinding) {
			b.AuthFrom, b.Transport.CAFrom = &operatorv1alpha1.HTTPSAuthSource{Name: "token"}, ca
		},
	} {
		binding := httpsBinding()
		edit(binding)
		if err := sourcebinding.ValidateAccess(operatorv1alpha1.SourceAccessBinding{HTTPS: binding}); err != nil {
			t.Errorf("ValidateAccess refused %s: %v", name, err)
		}
	}
	for name, edit := range map[string]func(*operatorv1alpha1.SourceAccessBinding){
		"a download and a snapshot":  func(b *operatorv1alpha1.SourceAccessBinding) { b.Snapshot = snapshot() },
		"a download and a commit":    func(b *operatorv1alpha1.SourceAccessBinding) { b.Git = gitBinding() },
		"a download and a digest":    func(b *operatorv1alpha1.SourceAccessBinding) { b.Digest = artifact },
		"a download and registry CA": func(b *operatorv1alpha1.SourceAccessBinding) { b.Transport.CAFrom = ca },
		"http://":                    func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.URL = "http://files.example.com/app.tar.gz" },
		"a credential in the URL": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.HTTPS.URL = "https://u:p@files.example.com/app.tar.gz"
		},
		"a query":                   func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.URL += "?x=1" },
		"a fragment":                func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.URL += "#x" },
		"no path":                   func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.URL = "https://files.example.com/" },
		"no download digest":        func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.Digest = "" },
		"no content digest":         func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.ContentDigest = "sha256:11" },
		"a format the API lacks":    func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.Format = "Tar" },
		"a File with a nested path": func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.Format = operatorv1alpha1.HTTPSFormatFile },
		"a File naming the root": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.HTTPS.Format, b.HTTPS.Path = operatorv1alpha1.HTTPSFormatFile, "."
		},
		"an escaping path": func(b *operatorv1alpha1.SourceAccessBinding) { b.HTTPS.Path = "../db" },
		"an unnamed credential": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.HTTPS.AuthFrom = &operatorv1alpha1.HTTPSAuthSource{Name: " "}
		},
		"a CA without a key": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.HTTPS.Transport.CAFrom = &corev1.ConfigMapKeySelector{LocalObjectReference: ca.LocalObjectReference}
		},
		"an optional CA": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.HTTPS.Transport.CAFrom = &corev1.ConfigMapKeySelector{LocalObjectReference: ca.LocalObjectReference, Key: "ca.pem", Optional: new(true)}
		},
		"an uppercase download digest": func(b *operatorv1alpha1.SourceAccessBinding) {
			b.HTTPS.Digest = "sha256:3333333333333333333333333333333333333333333333333333333333333AAA"
		},
	} {
		binding := operatorv1alpha1.SourceAccessBinding{HTTPS: httpsBinding()}
		edit(&binding)
		if err := sourcebinding.ValidateAccess(binding); err == nil {
			t.Errorf("ValidateAccess accepted %s", name)
		}
	}
}

func TestAnHTTPSPlanSourceBindsTheURLPathAndContent(t *testing.T) {
	t.Parallel()
	binding := operatorv1alpha1.SourceAccessBinding{HTTPS: httpsBinding()}
	source := sourcebinding.HTTPSPlanSource(httpsURL, binding.HTTPS.Path, content)
	if source.Kind != operatorv1alpha1.SourceKindHTTPS || source.ContentDigest != content ||
		sourcebinding.ContractVersion(source) != fingerprint.SourcePlanContractVersion {
		t.Fatalf("HTTPSPlanSource() = %#v", source)
	}
	if !sourcebinding.AccessGives(binding, "", source) {
		t.Fatal("an HTTPS binding does not give the plan computed from it")
	}
	republished := operatorv1alpha1.SourceAccessBinding{HTTPS: httpsBinding()}
	republished.HTTPS.Digest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	if !sourcebinding.AccessGives(republished, "", source) {
		t.Fatal("a download published again with the same files does not give the plan")
	}
	for name, plan := range map[string]*operatorv1alpha1.PlanSourceBinding{
		"other content":                sourcebinding.HTTPSPlanSource(httpsURL, binding.HTTPS.Path, artifact),
		"another path":                 sourcebinding.HTTPSPlanSource(httpsURL, "app/db", content),
		"another URL":                  sourcebinding.HTTPSPlanSource("https://files.example.com/other.tar.gz", binding.HTTPS.Path, content),
		"a Git plan of the same files": {Kind: operatorv1alpha1.SourceKindGit, Revision: source.Revision, ContentDigest: content},
		"no source":                    nil,
	} {
		if sourcebinding.AccessGives(binding, "", plan) {
			t.Errorf("an HTTPS binding gives a plan with %s", name)
		}
	}
	if sourcebinding.AccessGives(binding, artifact, source) {
		t.Error("an HTTPS binding gives a plan that also names an artifact")
	}
	if sourcebinding.AccessGives(operatorv1alpha1.SourceAccessBinding{Git: &operatorv1alpha1.GitSourceBinding{
		URL: httpsURL, Path: binding.HTTPS.Path, ContentDigest: content,
	}}, "", source) {
		t.Error("a Git binding gives an HTTPS plan of the same URL, path and content")
	}

	revision := &operatorv1alpha1.HTTPSSourceRevision{
		URL: httpsURL, Format: binding.HTTPS.Format, Path: binding.HTTPS.Path, Digest: httpsDownload, ContentDigest: content,
	}
	status := operatorv1alpha1.SchemaSourceStatus{HTTPS: revision}
	if digest, plan := sourcebinding.SchemaPlanSource(status); digest != "" || !sourcebinding.Equal(plan, source) {
		t.Fatalf("SchemaPlanSource() = %q %#v, want the HTTPS source", digest, plan)
	}
	if digest, plan := sourcebinding.MigrationPlanSource(&binding, nil, nil); digest != "" || !sourcebinding.Equal(plan, source) {
		t.Fatalf("MigrationPlanSource() = %q %#v, want the HTTPS source", digest, plan)
	}
	spec := operatorv1alpha1.SourceSpec{HTTPS: &operatorv1alpha1.HTTPSSourceSpec{
		URL: revision.URL, Digest: revision.Digest, Format: revision.Format, Path: revision.Path,
		AuthFrom: &operatorv1alpha1.HTTPSAuthSource{Name: "token"},
	}}
	if !sourcebinding.SchemaSourceReady(spec, status) {
		t.Fatal("a resolved HTTPS source is not ready")
	}
	access := sourcebinding.SchemaAccess(spec, status)
	if access == nil || access.HTTPS == nil || access.HTTPS.Digest != httpsDownload || access.HTTPS.ContentDigest != content ||
		access.HTTPS.AuthFrom == nil || access.HTTPS.AuthFrom.Name != "token" || access.Git != nil {
		t.Fatalf("SchemaAccess() = %#v, want the resolved download with the spec's credential", access)
	}
	for name, edit := range map[string]func(*operatorv1alpha1.HTTPSSourceSpec){
		"another URL":    func(s *operatorv1alpha1.HTTPSSourceSpec) { s.URL = "https://files.example.com/other.tar.gz" },
		"another digest": func(s *operatorv1alpha1.HTTPSSourceSpec) { s.Digest = artifact },
		"another format": func(s *operatorv1alpha1.HTTPSSourceSpec) { s.Format = operatorv1alpha1.HTTPSFormatZip },
		"another path":   func(s *operatorv1alpha1.HTTPSSourceSpec) { s.Path = "app/db" },
	} {
		moved := operatorv1alpha1.SourceSpec{HTTPS: spec.HTTPS.DeepCopy()}
		edit(moved.HTTPS)
		if sourcebinding.SchemaSourceReady(moved, status) {
			t.Errorf("a resolution is ready for a spec naming %s", name)
		}
	}
	if sourcebinding.SchemaSourceReady(operatorv1alpha1.SourceSpec{OCIRef: "oci://registry.example/schema:1"}, status) ||
		sourcebinding.SchemaAccess(operatorv1alpha1.SourceSpec{OCIRef: "oci://registry.example/schema:1"}, status) != nil {
		t.Error("an HTTPS resolution answers a spec that names no HTTPS source")
	}
}
