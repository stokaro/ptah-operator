// Package ocireference parses and binds the OCI references crossing the
// controller/data-plane trust boundary.
package ocireference

import (
	_ "crypto/sha256" // Register SHA-256 for OCI digest validation.
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"unicode"

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

// Authority returns the canonical HTTP authority selected by the OCI client.
// ORAS maps a small number of logical registry names to a different request
// host, so authorization must use Reference.Host rather than the parsed
// Registry field. Beyond that client-defined mapping, comparison deliberately
// does not resolve DNS names, collapse explicit ports, or remove trailing dots.
func Authority(rawReference string) (string, error) {
	reference, err := Parse(rawReference)
	if err != nil {
		return "", err
	}
	effective := registry.Reference{Registry: reference.Registry}.Host()
	return CanonicalAuthority(effective)
}

// CanonicalAuthority validates an authority-only host[:port] value and applies
// only the case folding permitted for a registry host. Transport schemes,
// credentials, paths, and encoded separators are rejected rather than
// normalized into a broader credential scope.
func CanonicalAuthority(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", errors.New("OCI registry authority must not be empty or contain surrounding whitespace")
	}
	for _, value := range raw {
		if value > unicode.MaxASCII || unicode.IsSpace(value) || unicode.IsControl(value) {
			return "", errors.New("OCI registry authority must contain printable ASCII without whitespace")
		}
	}
	if strings.ContainsAny(raw, "/@?#\\\x00%") {
		return "", errors.New("OCI registry authority must contain only a host and optional port")
	}
	if err := validateAuthorityHostPort(raw); err != nil {
		return "", err
	}

	canonical := strings.ToLower(raw)
	const sentinelRepository = "ptah-authority-validation"
	parsed, err := registry.ParseReference(canonical + "/" + sentinelRepository)
	if err != nil || parsed.Registry != canonical || parsed.Repository != sentinelRepository || parsed.Reference != "" {
		if err == nil {
			err = errors.New("value is not an authority-only registry name")
		}
		return "", fmt.Errorf("parse OCI registry authority: %w", err)
	}
	return canonical, nil
}

func validateAuthorityHostPort(authority string) error {
	if strings.HasPrefix(authority, "[") {
		closing := strings.IndexByte(authority, ']')
		if closing < 0 || closing == 1 || !strings.Contains(authority[1:closing], ":") ||
			net.ParseIP(authority[1:closing]) == nil {
			return errors.New("OCI registry authority contains an invalid bracketed IPv6 host")
		}
		remainder := authority[closing+1:]
		if remainder == "" {
			return nil
		}
		if !strings.HasPrefix(remainder, ":") || strings.ContainsAny(remainder[1:], "[]:") {
			return errors.New("OCI registry authority contains invalid text after its IPv6 host")
		}
		return validateAuthorityPort(remainder[1:])
	}
	if strings.ContainsAny(authority, "[]") {
		return errors.New("OCI registry authority contains unmatched IPv6 brackets")
	}

	switch strings.Count(authority, ":") {
	case 0:
		return nil
	case 1:
		host, port, _ := strings.Cut(authority, ":")
		if host == "" {
			return errors.New("OCI registry authority host must not be empty")
		}
		return validateAuthorityPort(port)
	default:
		return errors.New("OCI registry IPv6 authority must use brackets")
	}
}

func validateAuthorityPort(port string) error {
	if port == "" {
		return errors.New("OCI registry authority port must not be empty")
	}
	for _, value := range port {
		if value < '0' || value > '9' {
			return errors.New("OCI registry authority port must be decimal")
		}
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 {
		return errors.New("OCI registry authority port must be between 1 and 65535")
	}
	return nil
}

// MatchAuthority requires a Secret-owned authority grant to name the exact
// registry selected by an OCI reference.
func MatchAuthority(rawReference, rawGrant string) error {
	want, err := Authority(rawReference)
	if err != nil {
		return fmt.Errorf("parse OCI reference authority: %w", err)
	}
	got, err := CanonicalAuthority(rawGrant)
	if err != nil {
		return fmt.Errorf("parse OCI credential authority grant: %w", err)
	}
	if want != got {
		return errors.New("OCI credential authority grant does not match the source registry")
	}
	return nil
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
