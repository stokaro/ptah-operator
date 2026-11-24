package urlsource_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/urlsource"
)

func secret(name, key string) *corev1.SecretKeySelector {
	return &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}
}

func class(name, object string) *operatorv1alpha1.SecretProviderClassObjectSelector {
	return &operatorv1alpha1.SecretProviderClassObjectSelector{Name: name, ObjectName: object}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		source urlsource.Source
		want   string
	}{
		{name: "a Secret key", source: urlsource.Source{Secret: secret("orders-db", "url")}},
		{name: "a class object", source: urlsource.Source{Class: class("orders-db", "url")}},
		{name: "a hidden object file", source: urlsource.Source{Class: class("orders-db", ".url")}},
		{name: "neither", source: urlsource.Source{}, want: "names neither"},
		{name: "both", source: urlsource.Source{Secret: secret("orders-db", "url"), Class: class("orders-db", "url")}, want: "names both"},
		{name: "a Secret without its name", source: urlsource.Source{Secret: secret("", "url")}, want: "Secret name and key are required"},
		{name: "a Secret without its key", source: urlsource.Source{Secret: secret("orders-db", "")}, want: "Secret name and key are required"},
		{name: "a class without its name", source: urlsource.Source{Class: class("", "url")}, want: "object name are required"},
		{name: "a class without its object", source: urlsource.Source{Class: class("orders-db", "")}, want: "object name are required"},
		{name: "an object in a directory", source: urlsource.Source{Class: class("orders-db", "db/url")}, want: "is not a file name"},
		{name: "the parent directory", source: urlsource.Source{Class: class("orders-db", "..")}, want: "is not a file name"},
		{name: "a name the driver keeps for itself", source: urlsource.Source{Class: class("orders-db", "..data")}, want: "is not a file name"},
		{name: "the volume itself", source: urlsource.Source{Class: class("orders-db", ".")}, want: "is not a file name"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.source.Validate()
			switch {
			case test.want == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
				t.Fatalf("Validate() = %v, want an error containing %q", err, test.want)
			}
		})
	}
}

// TestValidateRefusesNoObjectNameTheAPIAdmits holds the object-name refusals
// to the pattern the generated CRD enforces: each name Validate refuses as a
// path is one the API server refuses too, so a stored resource never meets
// the refusal.
func TestValidateRefusesNoObjectNameTheAPIAdmits(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../config/crd/bases/operator.ptah.run_ptahschemas.yaml")
	if err != nil {
		t.Fatal(err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(data, crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) != 1 || crd.Spec.Versions[0].Schema == nil {
		t.Fatalf("CRD has %d versions, want one with a schema", len(crd.Spec.Versions))
	}
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	selector := spec.Properties["target"].Properties["urlFromSecretProviderClass"]
	pattern := selector.Properties["objectName"].Pattern
	if pattern == "" {
		t.Fatal("the CRD declares no objectName pattern")
	}
	admitted := regexp.MustCompile(pattern)

	refused := 0
	for _, name := range []string{"db/url", "/url", "url/", "..", "..data", ".", "a/../b", "url", ".url", "a..b", "a.b-c_D9"} {
		err := urlsource.Source{Class: class("orders-db", name)}.Validate()
		if err == nil {
			continue
		}
		refused++
		if admitted.MatchString(name) {
			t.Errorf("Validate() refuses %q, which the API admits: %v", name, err)
		}
	}
	if refused != 7 {
		t.Fatalf("Validate() refused %d object names, want 7", refused)
	}
	for _, name := range []string{"url", ".url", "a..b", "a.b-c_D9"} {
		if !admitted.MatchString(name) {
			t.Errorf("the API refuses %q, which the refusal rows treat as admitted", name)
		}
	}
}

func TestHolder(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		source urlsource.Source
		want   string
	}{
		{source: urlsource.Source{Secret: secret("orders-db", "url")}, want: `Secret "orders-db" holds under key "url"`},
		{source: urlsource.Source{Class: class("orders-store", "dsn")}, want: `SecretProviderClass "orders-store" writes as object "dsn"`},
		{source: urlsource.Source{}, want: "the resource names"},
	} {
		if got := test.source.Holder(); got != test.want {
			t.Errorf("Holder() = %q, want %q", got, test.want)
		}
	}
}

func TestSelectorsReadTheFieldTheyAreNamedFor(t *testing.T) {
	t.Parallel()

	url, store := secret("orders-db", "url"), class("orders-store", "dsn")
	for name, got := range map[string]urlsource.Source{
		"target":  urlsource.OfTarget(operatorv1alpha1.DatabaseTargetSpec{URLFrom: url, URLFromSecretProviderClass: store}),
		"binding": urlsource.OfBinding(operatorv1alpha1.DatabaseTargetBinding{URLFrom: url, URLFromSecretProviderClass: store}),
		"ref":     urlsource.OfRef(operatorv1alpha1.DatabaseTargetRef{URLFrom: url, URLFromSecretProviderClass: store}),
	} {
		if got.Secret != url || got.Class != store {
			t.Errorf("%s = %+v, want both fields carried", name, got)
		}
	}
	if got := urlsource.OfSecret(*url); got.Secret == nil || *got.Secret != *url || got.Class != nil {
		t.Errorf("OfSecret() = %+v, want the selector alone", got)
	}
}
