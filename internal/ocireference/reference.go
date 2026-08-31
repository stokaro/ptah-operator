// Package ocireference parses and binds the OCI references crossing the
// controller/data-plane trust boundary.
package ocireference

import (
	_ "crypto/sha256" // Register SHA-256 for OCI digest validation.
	"errors"
	"fmt"
	"regexp"
	"strings"

	"oras.land/oras-go/v2/registry"
)

const scheme = "oci://"

var sha256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Reference is the security-relevant identity of an OCI reference. Selector
// is the effective tag or digest; an omitted selector is represented as
// "latest", matching the data-plane parser.
type Reference struct {
	Registry   string
	Repository string
	Selector   string
	IsDigest   bool
}

// Parse rejects transport decorations and returns the effective OCI identity.
func Parse(raw string) (Reference, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, " \t\r\n") {
		return Reference{}, errors.New("OCI reference must not be empty or contain whitespace")
	}
	value, ok := strings.CutPrefix(raw, scheme)
	if !ok {
		return Reference{}, errors.New("OCI reference must use the oci:// scheme")
	}
	if err := validateText(value); err != nil {
		return Reference{}, err
	}
	parsed, err := registry.ParseReference(value)
	if err != nil {
		return Reference{}, fmt.Errorf("parse OCI reference: %w", err)
	}
	selector := parsed.ReferenceOrDefault()
	return Reference{
		Registry:   parsed.Registry,
		Repository: parsed.Repository,
		Selector:   selector,
		IsDigest:   strings.Contains(selector, ":"),
	}, nil
}

// MatchRequested verifies that a resolver report describes exactly the
// requested repository and effective selector. This deliberately treats an
// omitted selector and :latest as equivalent.
func MatchRequested(requested, reported string) error {
	want, err := Parse(requested)
	if err != nil {
		return fmt.Errorf("parse requested OCI reference: %w", err)
	}
	got, err := Parse(reported)
	if err != nil {
		return fmt.Errorf("parse reported OCI reference: %w", err)
	}
	if want.Registry != got.Registry || want.Repository != got.Repository || want.Selector != got.Selector {
		return errors.New("reported OCI reference does not match the requested repository and selector")
	}
	return nil
}

// ValidateResolution binds a pinned reference to the same repository as the
// request and to the exact reported SHA-256 digest.
func ValidateResolution(requested, pinned, digest string) error {
	request, err := Parse(requested)
	if err != nil {
		return fmt.Errorf("parse requested OCI reference: %w", err)
	}
	pin, err := Parse(pinned)
	if err != nil {
		return fmt.Errorf("parse pinned OCI reference: %w", err)
	}
	if request.Registry != pin.Registry || request.Repository != pin.Repository {
		return errors.New("pinned OCI reference does not use the requested repository")
	}
	if request.IsDigest && request.Selector != digest {
		return errors.New("pinned OCI reference does not preserve the requested digest")
	}
	return validatePin(pin, digest)
}

// ValidatePinned verifies an already-bound immutable reference and digest.
func ValidatePinned(pinned, digest string) error {
	pin, err := Parse(pinned)
	if err != nil {
		return fmt.Errorf("parse pinned OCI reference: %w", err)
	}
	return validatePin(pin, digest)
}

func validatePin(pin Reference, digest string) error {
	if !sha256Pattern.MatchString(digest) || !pin.IsDigest || pin.Selector != digest {
		return errors.New("pinned OCI reference does not select the exact SHA-256 digest")
	}
	return nil
}

func validateText(value string) error {
	lower := strings.ToLower(value)
	switch {
	case strings.ContainsAny(value, "?#\\\x00"):
		return errors.New("OCI reference contains unsupported query, fragment, or path data")
	case strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c"):
		return errors.New("OCI reference contains an escaped path separator")
	case hasUserInfo(value):
		return errors.New("OCI reference must not contain embedded credentials")
	default:
		return nil
	}
}

func hasUserInfo(value string) bool {
	firstSlash := strings.IndexByte(value, '/')
	firstAt := strings.IndexByte(value, '@')
	return firstAt >= 0 && (firstSlash < 0 || firstAt < firstSlash)
}
