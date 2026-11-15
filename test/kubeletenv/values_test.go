package kubeletenv_test

import (
	"errors"
	"maps"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/stokaro/ptah-operator/test/kubeletenv"
)

func TestValuesResolvesTheWayTheKubeletDoes(t *testing.T) {
	t.Parallel()
	secret := func(name, key string) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: key,
		}}}
	}
	secrets := map[string]string{"present": "from-$(A)-$$"}
	source := func(variable corev1.EnvVar) (string, bool, error) {
		value, present := secrets[variable.ValueFrom.SecretKeyRef.Key]
		return value, present, nil
	}
	for _, row := range []struct {
		name string
		env  []corev1.EnvVar
		want map[string]string
	}{
		{"a doubled operator is one", []corev1.EnvVar{{Name: "V", Value: "a$$b"}}, map[string]string{"V": "a$b"}},
		{"a trailing operator stays", []corev1.EnvVar{{Name: "V", Value: "x$"}}, map[string]string{"V": "x$"}},
		{"an operator before another rune stays", []corev1.EnvVar{{Name: "V", Value: "$x"}}, map[string]string{"V": "$x"}},
		{"an incomplete reference stays", []corev1.EnvVar{{Name: "V", Value: "$(A"}}, map[string]string{"V": "$(A"}},
		{
			"a reference to an earlier variable expands",
			[]corev1.EnvVar{{Name: "A", Value: "one"}, {Name: "V", Value: "<$(A)>"}},
			map[string]string{"A": "one", "V": "<one>"},
		},
		{
			"a reference to a later variable stays",
			[]corev1.EnvVar{{Name: "V", Value: "<$(A)>"}, {Name: "A", Value: "one"}},
			map[string]string{"A": "one", "V": "<$(A)>"},
		},
		{
			"an escaped reference stays a reference",
			[]corev1.EnvVar{{Name: "A", Value: "one"}, {Name: "V", Value: "$$(A)"}},
			map[string]string{"A": "one", "V": "$(A)"},
		},
		{
			"a later variable of a name replaces the earlier",
			[]corev1.EnvVar{{Name: "V", Value: "first"}, {Name: "V", Value: "second"}},
			map[string]string{"V": "second"},
		},
		{
			"a value from a source is taken as it is and can be referenced",
			[]corev1.EnvVar{{Name: "A", Value: "one"}, secret("S", "present"), {Name: "V", Value: "$(S)"}},
			map[string]string{"A": "one", "S": "from-$(A)-$$", "V": "from-$(A)-$$"},
		},
		{
			"an optional value its source lacks is left out",
			[]corev1.EnvVar{secret("S", "absent"), {Name: "V", Value: "$(S)"}},
			map[string]string{"V": "$(S)"},
		},
		{"an empty literal is empty", []corev1.EnvVar{{Name: "V"}}, map[string]string{"V": ""}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got, err := kubeletenv.Values(row.env, source)
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(got, row.want) {
				t.Fatalf("Values = %v, want %v", got, row.want)
			}
		})
	}
}

func TestValuesReturnsTheSourceError(t *testing.T) {
	t.Parallel()
	refused := errors.New("refused")
	_, err := kubeletenv.Values([]corev1.EnvVar{{Name: "S", ValueFrom: &corev1.EnvVarSource{}}},
		func(corev1.EnvVar) (string, bool, error) { return "", false, refused })
	if !errors.Is(err, refused) {
		t.Fatalf("Values returned %v, want the source's error", err)
	}
}
