// Package urlsource says where an operation Pod reads a database URL from: a
// key of a Secret, or an object a SecretProviderClass of the Secrets Store CSI
// driver writes into the volume it mounts. The API carries the two as sibling
// fields of every target, development and shadow database; this package is
// the one place that reads them as one choice.
//
// Nothing here reads the URL, the Secret or the class. The manager knows the
// names, and the kubelet and the CSI driver do the reading for the Pod.
package urlsource

import (
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// Source is where one database URL comes from. A valid Source sets exactly
// one of its fields, completely.
type Source struct {
	// Secret is the Secret key holding the URL.
	Secret *corev1.SecretKeySelector
	// Class is the SecretProviderClass object holding the URL.
	Class *operatorv1alpha1.SecretProviderClassObjectSelector
}

// OfSecret is a URL from the Secret key selector names.
func OfSecret(selector corev1.SecretKeySelector) Source {
	return Source{Secret: &selector}
}

// OfTarget is where a resource's target URL comes from.
func OfTarget(target operatorv1alpha1.DatabaseTargetSpec) Source {
	return Source{Secret: target.URLFrom, Class: target.URLFromSecretProviderClass}
}

// OfBinding is where an operation that bound target reads its URL from.
func OfBinding(binding operatorv1alpha1.DatabaseTargetBinding) Source {
	return Source{Secret: binding.URLFrom, Class: binding.URLFromSecretProviderClass}
}

// OfRef is where a development or shadow database's URL comes from.
func OfRef(ref operatorv1alpha1.DatabaseTargetRef) Source {
	return Source{Secret: ref.URLFrom, Class: ref.URLFromSecretProviderClass}
}

// Validate refuses a Source that names neither place or both, or a place
// without its name: a Secret key needs the Secret and the key, and a
// SecretProviderClass object the class and the object. It refuses nothing
// the API admits.
func (s Source) Validate() error {
	switch {
	case s.Secret != nil && s.Class != nil:
		return errors.New("the database URL names both a Secret key and a SecretProviderClass object")
	case s.Secret != nil:
		if s.Secret.Name == "" || s.Secret.Key == "" {
			return errors.New("the database URL Secret name and key are required")
		}
	case s.Class != nil:
		if s.Class.Name == "" || s.Class.ObjectName == "" {
			return errors.New("the database URL SecretProviderClass name and object name are required")
		}
		// The object name becomes a file name in the volume the class mounts,
		// and the API's pattern admits none of these.
		if strings.Contains(s.Class.ObjectName, "/") || strings.HasPrefix(s.Class.ObjectName, "..") || s.Class.ObjectName == "." {
			return fmt.Errorf("the database URL SecretProviderClass object name %q is not a file name", s.Class.ObjectName)
		}
	default:
		return errors.New("the database URL names neither a Secret key nor a SecretProviderClass object")
	}
	return nil
}

// Holder is the URL's place as the operator's sentences name it, after "the
// URL that": `Secret "orders-db" holds under key "url"`, or
// `SecretProviderClass "orders-db" writes as object "url"`. The names are the
// resource's own data, and nothing the Secret or the store holds.
func (s Source) Holder() string {
	switch {
	case s.Secret != nil:
		return fmt.Sprintf("Secret %q holds under key %q", s.Secret.Name, s.Secret.Key)
	case s.Class != nil:
		return fmt.Sprintf("SecretProviderClass %q writes as object %q", s.Class.Name, s.Class.ObjectName)
	}
	return "the resource names"
}
