package sourcepolicy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourcepolicy"
)

const document = `allowedGitHosts:
  - git.example.com
  - git.example.com:2222
allowedConfigMaps:
  - inventory-schema
maxContentBytes: 100
`

func TestAPolicyRefusesWhatItsRulesDoNotAllow(t *testing.T) {
	t.Parallel()
	policy, err := sourcepolicy.Parse([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	for name, check := range map[string]error{
		"an allowed host":              policy.CheckGitHost("git.example.com"),
		"an allowed host in uppercase": policy.CheckGitHost("GIT.EXAMPLE.COM"),
		"an allowed host and port":     policy.CheckGitHost("git.example.com:2222"),
		"an allowed ConfigMap":         policy.CheckConfigMap("inventory-schema"),
		"content at the bound":         policy.CheckContentBytes(100),
	} {
		if check != nil {
			t.Errorf("%s was refused: %v", name, check)
		}
	}
	for name, check := range map[string]error{
		"another host":            policy.CheckGitHost("attacker.example"),
		"the host on a port":      policy.CheckGitHost("git.example.com:443"),
		"another ConfigMap":       policy.CheckConfigMap("inventory-schema-copy"),
		"a ConfigMap by its case": policy.CheckConfigMap("Inventory-Schema"),
		"content past the bound":  policy.CheckContentBytes(101),
	} {
		if !errors.Is(check, sourcepolicy.ErrRefused) {
			t.Errorf("%s was not refused: %v", name, check)
		}
	}
}

func TestAnAbsentRuleRefusesNothingAndAnEmptyListAllowsNothing(t *testing.T) {
	t.Parallel()
	unrestricted, err := sourcepolicy.Parse([]byte("maxContentBytes: 10\n"))
	if err != nil {
		t.Fatal(err)
	}
	if unrestricted.CheckGitHost("anything.example") != nil || unrestricted.CheckConfigMap("anything") != nil {
		t.Fatal("an absent rule refused a source")
	}
	closed, err := sourcepolicy.Parse([]byte("allowedGitHosts: []\nallowedConfigMaps: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if closed.CheckGitHost("git.example.com") == nil || closed.CheckConfigMap("inventory-schema") == nil {
		t.Fatal("an empty list allowed a source")
	}
	if closed.CheckContentBytes(1<<40) != nil {
		t.Fatal("an absent size rule refused content")
	}
}

func TestParseRefusesWhatItCannotReadExactly(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{
		"a misspelled rule":      "allowedGitHost:\n  - git.example.com\n",
		"a zero size":            "maxContentBytes: 0\n",
		"a negative size":        "maxContentBytes: -1\n",
		"an empty host":          "allowedGitHosts:\n  - \"\"\n",
		"a padded ConfigMap":     "allowedConfigMaps:\n  - \" inventory\"\n",
		"not YAML":               "allowedGitHosts: [\n",
		"a list where a size is": "maxContentBytes: [1]\n",
		"too large":              "allowedConfigMaps:\n" + strings.Repeat("  - name\n", sourcepolicy.MaxDocumentBytes/8),
	} {
		if _, err := sourcepolicy.Parse([]byte(text)); err == nil {
			t.Errorf("Parse accepted %s", name)
		}
	}
}

func TestReadBindsTheImmutableObjectAndItsBytes(t *testing.T) {
	t.Parallel()
	immutable := true
	stored := func(name, uid, text string, isImmutable bool) *corev1.ConfigMap {
		object := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: name, UID: types.UID("uid-" + uid)},
			Data:       map[string]string{"policy.yaml": text},
		}
		if isImmutable {
			object.Immutable = &immutable
		}
		return object
	}
	reader := fake.NewClientBuilder().WithObjects(
		stored("source-policy", "a", document, true),
		stored("same-bytes", "b", document, true),
		stored("mutable-policy", "c", document, false),
		stored("broken-policy", "d", "maxContentBytes: 0\n", true),
	).Build()
	selector := func(name, key string) *corev1.ConfigMapKeySelector {
		return &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}
	}
	policy, binding, err := sourcepolicy.Read(context.Background(), reader, "application", selector("source-policy", "policy.yaml"))
	if err != nil || policy.MaxContentBytes == nil || *policy.MaxContentBytes != 100 || binding.UID != "uid-a" || !strings.HasPrefix(binding.Digest, "sha256:") {
		t.Fatalf("Read() = %+v %+v, %v", policy, binding, err)
	}
	_, same, err := sourcepolicy.Read(context.Background(), reader, "application", selector("same-bytes", "policy.yaml"))
	if err != nil || same.Digest != binding.Digest || same.UID == binding.UID {
		t.Fatalf("another object with the same bytes gave %+v, %v", same, err)
	}
	for name, ref := range map[string]*corev1.ConfigMapKeySelector{
		"a mutable ConfigMap": selector("mutable-policy", "policy.yaml"),
		"a broken document":   selector("broken-policy", "policy.yaml"),
		"a missing key":       selector("source-policy", "other.yaml"),
		"a missing ConfigMap": selector("absent", "policy.yaml"),
		"no selector":         nil,
	} {
		if _, _, err := sourcepolicy.Read(context.Background(), reader, "application", ref); err == nil {
			t.Errorf("Read accepted %s", name)
		}
	}
}

// A plan binds the policy its resource names; it stops binding when the
// resource names another policy, or none, or one it did not.
func TestStillBindsNeedsTheSamePolicyOrNone(t *testing.T) {
	t.Parallel()
	immutable := true
	reader := fake.NewClientBuilder().WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: "source-policy", UID: "policy-uid"},
		Immutable:  &immutable, Data: map[string]string{"policy.yaml": document},
	}).Build()
	selector := &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "source-policy"}, Key: "policy.yaml"}
	live, err := sourcepolicy.LiveBinding(context.Background(), reader, "application", selector)
	if err != nil || live == nil {
		t.Fatalf("LiveBinding() = %v, %v", live, err)
	}
	if err := sourcepolicy.StillBinds(context.Background(), reader, "application", selector, live); err != nil {
		t.Fatalf("the same policy no longer binds: %v", err)
	}
	if err := sourcepolicy.StillBinds(context.Background(), reader, "application", nil, nil); err != nil {
		t.Fatalf("no policy no longer binds a plan with none: %v", err)
	}
	other := *live
	other.UID = "replacement-uid"
	for name, row := range map[string]struct {
		selector *corev1.ConfigMapKeySelector
		bound    *operatorv1alpha1.SourcePolicyBinding
	}{
		"another object":            {selector, &other},
		"a policy the plan lacks":   {selector, nil},
		"no policy where one bound": {nil, live},
		"a policy that is gone": {&corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "absent"}, Key: "policy.yaml",
		}, live},
	} {
		if sourcepolicy.StillBinds(context.Background(), reader, "application", row.selector, row.bound) == nil {
			t.Errorf("StillBinds accepted %s", name)
		}
	}
}
