// Package sourcepolicy reads and applies a source policy: what a team refuses
// for every ConfigMap and Git source, held in an immutable ConfigMap a
// resource selects with sourcePolicyFrom.
//
// The document is YAML with three optional rules:
//
//	allowedGitHosts:      # the only host[:port] a Git source may name
//	  - git.example.com
//	allowedConfigMaps:    # the only ConfigMaps a ConfigMap source may name
//	  - inventory-schema
//	maxContentBytes: 1048576   # the most the selected files may hold together
//
// A rule that is absent refuses nothing. A list that is present and empty
// allows nothing of its kind. Any other field is refused, so a rule misspelled
// does not read as a rule kept.
package sourcepolicy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

// MaxDocumentBytes bounds the policy document.
const MaxDocumentBytes = 64 << 10

// ErrRefused is every refusal of a source by its policy, matched with
// errors.Is. The message names the rule.
var ErrRefused = errors.New("the source policy refuses this source")

// Policy is a parsed source policy.
type Policy struct {
	AllowedGitHosts   *[]string `json:"allowedGitHosts,omitempty"`
	AllowedConfigMaps *[]string `json:"allowedConfigMaps,omitempty"`
	MaxContentBytes   *int64    `json:"maxContentBytes,omitempty"`
}

// Binding is the policy object and the digest of the bytes it was read from:
// another ConfigMap under the same name, or other bytes, is another policy.
type Binding struct {
	UID    types.UID
	Digest string
}

// Parse reads a policy document and refuses one it cannot read exactly.
func Parse(document []byte) (Policy, error) {
	if len(document) > MaxDocumentBytes {
		return Policy{}, fmt.Errorf("the source policy is larger than %d bytes", MaxDocumentBytes)
	}
	var policy Policy
	if err := yaml.UnmarshalStrict(document, &policy); err != nil {
		return Policy{}, fmt.Errorf("the source policy is not a policy document: %w", err)
	}
	if policy.MaxContentBytes != nil && *policy.MaxContentBytes < 1 {
		return Policy{}, errors.New("the source policy's maxContentBytes is not positive")
	}
	for name, list := range map[string]*[]string{"allowedGitHosts": policy.AllowedGitHosts, "allowedConfigMaps": policy.AllowedConfigMaps} {
		if list == nil {
			continue
		}
		for _, entry := range *list {
			if strings.TrimSpace(entry) == "" || entry != strings.TrimSpace(entry) {
				return Policy{}, fmt.Errorf("the source policy's %s holds an empty or padded entry", name)
			}
		}
	}
	return policy, nil
}

// Read reads the policy a selector names, from an immutable ConfigMap, and
// returns it with its binding.
func Read(ctx context.Context, reader client.Reader, namespace string, selector *corev1.ConfigMapKeySelector) (Policy, Binding, error) {
	if reader == nil || namespace == "" || selector == nil || selector.Name == "" || selector.Key == "" {
		return Policy{}, Binding{}, errors.New("the source policy reference is incomplete")
	}
	configMap := &corev1.ConfigMap{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: selector.Name}, configMap); err != nil {
		return Policy{}, Binding{}, fmt.Errorf("read the source policy ConfigMap: %w", err)
	}
	if configMap.UID == "" || configMap.DeletionTimestamp != nil {
		return Policy{}, Binding{}, errors.New("the source policy ConfigMap has no stable identity")
	}
	if configMap.Immutable == nil || !*configMap.Immutable {
		return Policy{}, Binding{}, errors.New("the source policy ConfigMap must set immutable: true")
	}
	document, ok := configMap.BinaryData[selector.Key]
	if !ok {
		value, found := configMap.Data[selector.Key]
		if !found {
			return Policy{}, Binding{}, errors.New("the source policy ConfigMap key is missing")
		}
		document = []byte(value)
	}
	policy, err := Parse(document)
	if err != nil {
		return Policy{}, Binding{}, err
	}
	return policy, Binding{UID: configMap.UID, Digest: fingerprint.DigestBytes(document)}, nil
}

// CheckGitHost refuses a Git source whose authority, host[:port], the policy
// does not allow. Hosts compare without case.
func (p Policy) CheckGitHost(authority string) error {
	if p.AllowedGitHosts == nil {
		return nil
	}
	if slices.ContainsFunc(*p.AllowedGitHosts, func(allowed string) bool { return strings.EqualFold(allowed, authority) }) {
		return nil
	}
	return fmt.Errorf("%w: allowedGitHosts does not name %s", ErrRefused, authority)
}

// CheckConfigMap refuses a ConfigMap source the policy does not allow.
func (p Policy) CheckConfigMap(name string) error {
	if p.AllowedConfigMaps == nil || slices.Contains(*p.AllowedConfigMaps, name) {
		return nil
	}
	return fmt.Errorf("%w: allowedConfigMaps does not name %s", ErrRefused, name)
}

// CheckContentBytes refuses selected files that hold more than the policy
// allows together.
func (p Policy) CheckContentBytes(size int64) error {
	if p.MaxContentBytes == nil || size <= *p.MaxContentBytes {
		return nil
	}
	return fmt.Errorf("%w: the selected files hold %d bytes, more than maxContentBytes %d", ErrRefused, size, *p.MaxContentBytes)
}

// LiveBinding is the binding of the policy a selector names now, as a plan
// binds it, or nil when the selector names none. A plan is current only while
// it names the same policy, or none and no policy is named.
func LiveBinding(ctx context.Context, reader client.Reader, namespace string, selector *corev1.ConfigMapKeySelector) (*operatorv1alpha1.SourcePolicyBinding, error) {
	if selector == nil {
		return nil, nil
	}
	_, binding, err := Read(ctx, reader, namespace, selector)
	if err != nil {
		return nil, err
	}
	return &operatorv1alpha1.SourcePolicyBinding{UID: binding.UID, Digest: binding.Digest}, nil
}

// StillBinds refuses a plan computed under another source policy than the
// one its resource names now.
func StillBinds(ctx context.Context, reader client.Reader, namespace string, selector *corev1.ConfigMapKeySelector, bound *operatorv1alpha1.SourcePolicyBinding) error {
	live, err := LiveBinding(ctx, reader, namespace, selector)
	if err != nil {
		return fmt.Errorf("read the source policy the plan was decided under: %w", err)
	}
	if (live == nil) != (bound == nil) || (live != nil && *live != *bound) {
		return errors.New("the source policy changed after the plan was published")
	}
	return nil
}
