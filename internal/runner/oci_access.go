package runner

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/stokaro/ptah-operator/internal/ocireference"
)

const (
	maxOCICABytes int64 = 1 << 20

	// EnvOCIAuthMode records whether the Job projects an Environment or
	// DockerConfigJSON registry credential. It is runner-only policy input.
	EnvOCIAuthMode = "PTAH_OPERATOR_OCI_AUTH_MODE"
	// EnvOCIAuthRegistryGrant carries the fixed registry key selected from the
	// credential Secret in either supported mode. It is never passed to Ptah.
	EnvOCIAuthRegistryGrant = "PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT"
	// EnvOCIAllowPlainHTTPGrant carries the credential owner's explicit
	// cleartext-transport grant. The only accepted value is exactly "true".
	EnvOCIAllowPlainHTTPGrant = "PTAH_OPERATOR_OCI_ALLOW_PLAIN_HTTP"
	// EnvOCIHasCA binds custom trust configuration to the same validation used
	// by runner operations and the pre-fetch guard.
	EnvOCIHasCA = "PTAH_OPERATOR_OCI_HAS_CA"
	// EnvOCICASourceFile is the runner-only path to the mutable ConfigMap
	// projection. Ptah receives only a verified immutable snapshot path.
	EnvOCICASourceFile = "PTAH_OPERATOR_OCI_CA_SOURCE_FILE"
	// EnvOCICASHA256Grant carries the fixed caSHA256 key from the same Secret
	// that owns registry credentials. It is never passed to Ptah.
	EnvOCICASHA256Grant = "PTAH_OPERATOR_OCI_CA_SHA256_GRANT"

	envOCIRegistry          = "PTAH_OCI_REGISTRY"
	envOCICAFile            = "PTAH_OCI_CA_FILE"
	envOCIClientCertificate = "PTAH_OCI_CLIENT_CERT"
	envOCIClientKey         = "PTAH_OCI_CLIENT_KEY"
	envPlainHTTP            = "PTAH_PLAIN_HTTP"

	ociAuthEnvironment      = "Environment"
	ociAuthDockerConfigJSON = "DockerConfigJSON"
)

// ValidateOCISourceAccess validates every Secret-owned registry capability for
// a source without a custom CA. CA callers must use PrepareOCISourceAccess or
// SnapshotOCISourceAccess so validation and use stay bound to the same bytes.
func ValidateOCISourceAccess(reference string, environment []string) error {
	ca, err := validateOCISourceAccess(reference, environment)
	if err != nil {
		return err
	}
	if len(ca) != 0 {
		return errors.New("registry certificate authority requires an immutable snapshot")
	}
	return nil
}

// PrepareOCISourceAccess validates registry authority and custom trust bytes,
// then replaces a mutable CA projection with a private per-operation snapshot.
// The returned cleanup must be called when validation succeeds.
func PrepareOCISourceAccess(reference string, environment []string, tempDir string) ([]string, func(), error) {
	ca, err := validateOCISourceAccess(reference, environment)
	if err != nil {
		return nil, nil, err
	}
	prepared := append([]string(nil), environment...)
	if len(ca) == 0 {
		return prepared, func() {}, nil
	}

	temporary, err := os.CreateTemp(tempDir, "ptah-oci-ca-*")
	if err != nil {
		return nil, nil, errors.New("create registry certificate authority snapshot")
	}
	path := temporary.Name()
	cleanup := func() { _ = os.Remove(path) }
	if _, err := temporary.Write(ca); err != nil {
		_ = temporary.Close()
		cleanup()
		return nil, nil, errors.New("write registry certificate authority snapshot")
	}
	if err := temporary.Chmod(0o400); err != nil {
		_ = temporary.Close()
		cleanup()
		return nil, nil, errors.New("protect registry certificate authority snapshot")
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return nil, nil, errors.New("close registry certificate authority snapshot")
	}
	prepared = append(prepared, envOCICAFile+"="+path)
	return prepared, cleanup, nil
}

// SnapshotOCISourceAccess validates registry authority and custom trust bytes,
// then copies the exact validated CA bytes to an exclusive destination. It is
// used by the credential-free Observe/Plan init guard before any network child.
func SnapshotOCISourceAccess(reference string, environment []string, destination string) error {
	ca, err := validateOCISourceAccess(reference, environment)
	if err != nil {
		return err
	}
	if len(ca) == 0 {
		return errors.New("registry certificate authority snapshot requested without a custom CA")
	}
	if destination == "" {
		return errors.New("registry certificate authority snapshot destination is empty")
	}

	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return errors.New("create registry certificate authority snapshot")
	}
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(destination)
		}
	}()
	if _, err := output.Write(ca); err != nil {
		_ = output.Close()
		return errors.New("write registry certificate authority snapshot")
	}
	if err := output.Close(); err != nil {
		return errors.New("close registry certificate authority snapshot")
	}
	remove = false
	return nil
}

func validateOCISourceAccess(reference string, environment []string) ([]byte, error) {
	values := environmentMap(environment)
	authority, err := ocireference.Authority(reference)
	if err != nil {
		return nil, errors.New("OCI source reference does not contain a valid authority")
	}

	plainHTTP, err := strictOptionalBool(values[envPlainHTTP], envPlainHTTP)
	if err != nil {
		return nil, err
	}
	hasCA, err := strictOptionalBool(values[EnvOCIHasCA], EnvOCIHasCA)
	if err != nil {
		return nil, err
	}
	if values[envOCIClientCertificate] != "" || values[envOCIClientKey] != "" {
		return nil, errors.New("OCI client certificates are not supported until the executor can scope them across redirects")
	}
	if plainHTTP && hasCA {
		return nil, errors.New("plain HTTP cannot be combined with a registry certificate authority")
	}
	if values[envOCICAFile] != "" {
		return nil, errors.New("Ptah registry certificate authority path must be supplied only by the runner")
	}
	caSourcePath := values[EnvOCICASourceFile]
	if hasCA != (caSourcePath != "") {
		return nil, errors.New("registry certificate authority source binding is incomplete")
	}

	authMode := values[EnvOCIAuthMode]
	switch authMode {
	case "":
		if values[EnvOCIAuthRegistryGrant] != "" {
			return nil, errors.New("registry authority grant was supplied without registry authentication")
		}
	case ociAuthEnvironment, ociAuthDockerConfigJSON:
		if err := ocireference.MatchAuthority(reference, values[EnvOCIAuthRegistryGrant]); err != nil {
			return nil, errors.New("registry credential is not granted for the OCI source authority")
		}
		if scope, err := ocireference.CanonicalAuthority(values[envOCIRegistry]); err != nil || scope != authority {
			return nil, errors.New("Ptah registry credential scope does not match the OCI source authority")
		}
	default:
		return nil, fmt.Errorf("unsupported OCI registry authentication mode %q", authMode)
	}

	if plainHTTP && authMode != "" && values[EnvOCIAllowPlainHTTPGrant] != "true" {
		return nil, errors.New("authenticated plain HTTP requires an explicit credential-owner grant")
	}
	caGrant := values[EnvOCICASHA256Grant]
	if !hasCA {
		if caGrant != "" {
			return nil, errors.New("registry certificate authority digest grant was supplied without a custom CA")
		}
		return nil, nil
	}
	if authMode != "" && !validProtocolDigest(caGrant) {
		return nil, errors.New("authenticated custom CA requires a lowercase SHA-256 credential-owner grant")
	}
	if authMode == "" && caGrant != "" {
		return nil, errors.New("registry certificate authority digest grant was supplied without registry authentication")
	}

	ca, err := readOCICA(caSourcePath)
	if err != nil {
		return nil, err
	}
	if authMode != "" {
		digest := sha256.Sum256(ca)
		actual := fmt.Sprintf("sha256:%x", digest)
		if caGrant != actual {
			return nil, errors.New("registry certificate authority bytes do not match the credential-owner grant")
		}
	}
	return ca, nil
}

func readOCICA(path string) ([]byte, error) {
	input, err := os.Open(path)
	if err != nil {
		return nil, errors.New("open registry certificate authority")
	}
	content, readErr := io.ReadAll(io.LimitReader(input, maxOCICABytes+1))
	closeErr := input.Close()
	if readErr != nil {
		return nil, errors.New("read registry certificate authority")
	}
	if closeErr != nil {
		return nil, errors.New("close registry certificate authority")
	}
	if len(content) == 0 {
		return nil, errors.New("registry certificate authority is empty")
	}
	if int64(len(content)) > maxOCICABytes {
		return nil, fmt.Errorf("registry certificate authority exceeds %d bytes", maxOCICABytes)
	}
	return content, nil
}

func strictOptionalBool(raw, name string) (bool, error) {
	switch raw {
	case "":
		return false, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be exactly true or false", name)
	}
}
