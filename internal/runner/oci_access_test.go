package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestValidateOCISourceAccessAuthorityGrants(t *testing.T) {
	t.Parallel()

	reference := "oci://registry.example:5443/team/schema:stable"
	for name, environment := range map[string][]string{
		"matching Environment grant": {
			EnvOCIAuthMode + "=Environment",
			envOCIRegistry + "=registry.example:5443",
			EnvOCIAuthRegistryGrant + "=REGISTRY.EXAMPLE:5443",
		},
		"matching Docker config grant": {
			EnvOCIAuthMode + "=DockerConfigJSON",
			envOCIRegistry + "=registry.example:5443",
			EnvOCIAuthRegistryGrant + "=REGISTRY.EXAMPLE:5443",
		},
		"anonymous": nil,
	} {
		environment := environment
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateOCISourceAccess(reference, environment); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestValidateOCISourceAccessFailsClosed(t *testing.T) {
	t.Parallel()

	reference := "oci://registry.example/team/schema:stable"
	for name, environment := range map[string][]string{
		"missing Environment grant": {
			EnvOCIAuthMode + "=Environment", envOCIRegistry + "=registry.example",
		},
		"mismatched Environment grant": {
			EnvOCIAuthMode + "=Environment", envOCIRegistry + "=registry.example",
			EnvOCIAuthRegistryGrant + "=attacker.example",
		},
		"malformed Environment grant": {
			EnvOCIAuthMode + "=Environment", envOCIRegistry + "=registry.example",
			EnvOCIAuthRegistryGrant + "=https://registry.example",
		},
		"mismatched child scope": {
			EnvOCIAuthMode + "=Environment", envOCIRegistry + "=attacker.example",
			EnvOCIAuthRegistryGrant + "=registry.example",
		},
		"missing Docker config grant": {
			EnvOCIAuthMode + "=DockerConfigJSON", envOCIRegistry + "=registry.example",
		},
		"mismatched Docker config grant": {
			EnvOCIAuthMode + "=DockerConfigJSON", envOCIRegistry + "=registry.example",
			EnvOCIAuthRegistryGrant + "=attacker.example",
		},
		"malformed Docker config grant": {
			EnvOCIAuthMode + "=DockerConfigJSON", envOCIRegistry + "=registry.example",
			EnvOCIAuthRegistryGrant + "=https://registry.example",
		},
		"mismatched Docker config child scope": {
			EnvOCIAuthMode + "=DockerConfigJSON", envOCIRegistry + "=attacker.example",
			EnvOCIAuthRegistryGrant + "=registry.example",
		},
		"client certificate": {
			envOCIClientCertificate + "=/credentials/tls.crt",
		},
		"client private key": {
			envOCIClientKey + "=/credentials/tls.key",
		},
		"client certificate pair over plain HTTP": {
			envPlainHTTP + "=true", envOCIClientCertificate + "=/credentials/tls.crt",
			envOCIClientKey + "=/credentials/tls.key",
		},
		"custom CA over plain HTTP": {
			envPlainHTTP + "=true", EnvOCIHasCA + "=true",
		},
		"plain HTTP with surrounding whitespace": {
			envPlainHTTP + "= true",
		},
		"custom CA flag with surrounding whitespace": {
			EnvOCIHasCA + "=true ",
		},
		"authenticated plain HTTP without owner grant": {
			envPlainHTTP + "=true", EnvOCIAuthMode + "=DockerConfigJSON",
			envOCIRegistry + "=registry.example", EnvOCIAuthRegistryGrant + "=registry.example",
		},
		"case-variant cleartext grant": {
			envPlainHTTP + "=true", EnvOCIAuthMode + "=DockerConfigJSON",
			envOCIRegistry + "=registry.example", EnvOCIAuthRegistryGrant + "=registry.example",
			EnvOCIAllowPlainHTTPGrant + "=TRUE",
		},
	} {
		environment := environment
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateOCISourceAccess(reference, environment); err == nil {
				t.Fatal("ValidateOCISourceAccess() accepted unauthorized access")
			}
		})
	}
}

func TestValidateOCISourceAccessUsesEffectiveHTTPAuthority(t *testing.T) {
	t.Parallel()

	reference := "oci://docker.io/team/schema:stable"
	for _, mode := range []string{ociAuthEnvironment, ociAuthDockerConfigJSON} {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			environment := []string{
				EnvOCIAuthMode + "=" + mode,
				envOCIRegistry + "=registry-1.docker.io",
				EnvOCIAuthRegistryGrant + "=registry-1.docker.io",
			}
			if err := ValidateOCISourceAccess(reference, environment); err != nil {
				t.Fatal(err)
			}

			for _, name := range []string{envOCIRegistry, EnvOCIAuthRegistryGrant} {
				mismatched := append([]string(nil), environment...)
				for index := range mismatched {
					if strings.HasPrefix(mismatched[index], name+"=") {
						mismatched[index] = name + "=docker.io"
					}
				}
				if err := ValidateOCISourceAccess(reference, mismatched); err == nil {
					t.Errorf("ValidateOCISourceAccess() accepted logical registry in %s", name)
				}
			}
		})
	}
}

func TestValidateOCISourceAccessPlainHTTPGrants(t *testing.T) {
	t.Parallel()

	reference := "oci://registry.example/team/schema:stable"
	for name, environment := range map[string][]string{
		"anonymous": {envPlainHTTP + "=true"},
		"Environment": {
			envPlainHTTP + "=true", EnvOCIAuthMode + "=Environment",
			envOCIRegistry + "=registry.example", EnvOCIAuthRegistryGrant + "=registry.example",
			EnvOCIAllowPlainHTTPGrant + "=true",
		},
		"Docker config": {
			envPlainHTTP + "=true", EnvOCIAuthMode + "=DockerConfigJSON",
			envOCIRegistry + "=registry.example", EnvOCIAuthRegistryGrant + "=registry.example",
			EnvOCIAllowPlainHTTPGrant + "=true",
		},
	} {
		environment := environment
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateOCISourceAccess(reference, environment); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunnerRejectsUnauthorizedRegistryGrantBeforeChild(t *testing.T) {
	t.Parallel()

	for _, operation := range []Operation{OperationResolve, OperationVerify} {
		operation := operation
		for _, mode := range []string{ociAuthEnvironment, ociAuthDockerConfigJSON} {
			mode := mode
			for name, grant := range map[string]string{
				"missing":   "",
				"malformed": "https://registry.example",
				"mismatch":  "attacker.example",
			} {
				name, grant := name, grant
				t.Run(string(operation)+"/"+mode+"/"+name, func(t *testing.T) {
					t.Parallel()
					executor := &scriptedExecutor{t: t}
					environment := []string{
						envOperationID + "=" + string(operation) + "-authority-guard",
						envRequestedReference + "=oci://registry.example/team/schema:stable",
						EnvOCIAuthMode + "=" + mode,
						envOCIRegistry + "=registry.example",
					}
					if grant != "" {
						environment = append(environment, EnvOCIAuthRegistryGrant+"="+grant)
					}
					result := Run(t.Context(), Config{
						Operation:   operation,
						Environment: environment,
						Executor:    executor,
					})
					if result.Error == nil || result.Error.Code != "invalid_oci_access" || result.ChildExitCode != -1 {
						t.Fatalf("Run() = %#v", result)
					}
					requireInvalidOCIAccessPreChildFrame(t, result)
					if len(executor.calls) != 0 {
						t.Fatalf("executor received %d calls before authority refusal", len(executor.calls))
					}
				})
			}
		}
	}
}

func requireInvalidOCIAccessPreChildFrame(t *testing.T, result Result) {
	t.Helper()

	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	parsed, err := ParseResultFor(frame, result.Operation, result.OperationID)
	if err != nil {
		t.Fatalf("ParseResultFor() error = %v", err)
	}
	if !reflect.DeepEqual(parsed, result) {
		t.Fatalf("ParseResultFor() = %#v, want %#v", parsed, result)
	}
}

func TestRunnerRejectsClientCertificateBeforeChild(t *testing.T) {
	t.Parallel()

	for _, operation := range []Operation{OperationResolve, OperationVerify} {
		operation := operation
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t}
			result := Run(t.Context(), Config{
				Operation: operation,
				Environment: []string{
					envOperationID + "=" + string(operation) + "-client-certificate-refusal",
					envRequestedReference + "=oci://registry.example/team/schema:stable",
					envOCIClientCertificate + "=/credentials/tls.crt",
					envOCIClientKey + "=/credentials/tls.key",
				},
				Executor: executor,
			})
			if result.Error == nil || result.Error.Code != "invalid_oci_access" || result.ChildExitCode != -1 {
				t.Fatalf("Run() = %#v", result)
			}
			if len(executor.calls) != 0 {
				t.Fatalf("executor received %d calls before client-certificate refusal", len(executor.calls))
			}
		})
	}
}

func TestRunnerRejectsUnauthorizedCABeforeChild(t *testing.T) {
	t.Parallel()

	ca := []byte("-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n")
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, ca, 0o600); err != nil {
		t.Fatal(err)
	}
	validGrant := sha256Digest(ca)
	for _, operation := range []Operation{OperationResolve, OperationVerify} {
		operation := operation
		for name, grant := range map[string]string{
			"missing":   "",
			"malformed": "sha256:" + strings.Repeat("A", 64),
			"mismatch":  "sha256:" + strings.Repeat("0", 64),
		} {
			name, grant := name, grant
			t.Run(string(operation)+"/"+name, func(t *testing.T) {
				t.Parallel()
				executor := &scriptedExecutor{t: t}
				environment := []string{
					envOperationID + "=" + string(operation) + "-ca-guard",
					envRequestedReference + "=oci://registry.example/team/schema:stable",
					EnvOCIAuthMode + "=Environment",
					envOCIRegistry + "=registry.example",
					EnvOCIAuthRegistryGrant + "=registry.example",
					EnvOCIHasCA + "=true",
					EnvOCICASourceFile + "=" + caPath,
				}
				if grant != "" {
					environment = append(environment, EnvOCICASHA256Grant+"="+grant)
				}
				result := Run(t.Context(), Config{
					Operation:   operation,
					Environment: environment,
					Executor:    executor,
				})
				if result.Error == nil || result.Error.Code != "invalid_oci_access" || result.ChildExitCode != -1 {
					t.Fatalf("Run() = %#v; valid grant would be %q", result, validGrant)
				}
				if len(executor.calls) != 0 {
					t.Fatalf("executor received %d calls before CA refusal", len(executor.calls))
				}
			})
		}
	}
}

func TestPrepareOCISourceAccessSnapshotsExactCAAndCleansUp(t *testing.T) {
	t.Parallel()

	original := []byte("-----BEGIN CERTIFICATE-----\noriginal\n-----END CERTIFICATE-----\n")
	mutated := []byte("-----BEGIN CERTIFICATE-----\nmutated\n-----END CERTIFICATE-----\n")
	directory := t.TempDir()
	source := filepath.Join(directory, "source.pem")
	if err := os.WriteFile(source, original, 0o600); err != nil {
		t.Fatal(err)
	}
	environment := authenticatedCAEnvironment(source, sha256Digest(original))
	prepared, cleanup, err := PrepareOCISourceAccess(
		"oci://registry.example/team/schema:stable",
		environment,
		directory,
	)
	if err != nil {
		t.Fatal(err)
	}
	values := environmentMap(prepared)
	snapshot := values[envOCICAFile]
	if snapshot == "" || snapshot == source {
		cleanup()
		t.Fatalf("prepared CA path = %q, want a distinct snapshot", snapshot)
	}
	info, err := os.Stat(snapshot)
	if err != nil {
		cleanup()
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o400 {
		cleanup()
		t.Fatalf("snapshot mode = %#o, want 0400", got)
	}
	if err := os.WriteFile(source, mutated, 0o600); err != nil {
		cleanup()
		t.Fatal(err)
	}
	content, err := os.ReadFile(snapshot)
	if err != nil {
		cleanup()
		t.Fatal(err)
	}
	if string(content) != string(original) {
		cleanup()
		t.Fatalf("snapshot changed after source mutation: %q", content)
	}
	child := environmentMap(childEnvironment(prepared))
	if child[envOCICAFile] != snapshot {
		cleanup()
		t.Fatalf("child CA path = %q, want snapshot %q", child[envOCICAFile], snapshot)
	}
	for _, name := range []string{EnvOCICASourceFile, EnvOCICASHA256Grant, EnvOCIHasCA} {
		if _, ok := child[name]; ok {
			cleanup()
			t.Fatalf("child received runner-only %s", name)
		}
	}
	cleanup()
	if _, err := os.Stat(snapshot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot remains after cleanup: %v", err)
	}
}

func TestRunnerPassesOnlyCASnapshotToPtahAndRemovesIt(t *testing.T) {
	t.Parallel()

	original := []byte("-----BEGIN CERTIFICATE-----\nrunner\n-----END CERTIFICATE-----\n")
	directory := t.TempDir()
	source := filepath.Join(directory, "source.pem")
	if err := os.WriteFile(source, original, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	var snapshotPath string
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout: fmt.Sprintf(
			`{"reference":"oci://registry.example/team/schema:stable","pinned_reference":"oci://registry.example/team/schema@%s","digest":%q,"media_type":"application/vnd.oci.image.manifest.v1+json","size":42}`,
			digest,
			digest,
		),
		inspect: func(spec CommandSpec) {
			values := environmentMap(spec.Env)
			snapshotPath = values[envOCICAFile]
			if snapshotPath == "" || snapshotPath == source {
				t.Fatalf("Ptah CA path = %q, want a private snapshot", snapshotPath)
			}
			for _, name := range []string{EnvOCICASourceFile, EnvOCICASHA256Grant, EnvOCIHasCA} {
				if _, ok := values[name]; ok {
					t.Fatalf("Ptah received runner-only %s", name)
				}
			}
			if err := os.WriteFile(source, []byte("mutated after validation"), 0o600); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(snapshotPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != string(original) {
				t.Fatalf("Ptah snapshot = %q, want exact pre-mutation bytes", content)
			}
		},
	}}}
	environment := append(
		authenticatedCAEnvironment(source, sha256Digest(original)),
		envOperationID+"=resolve-ca-snapshot",
		envRequestedReference+"=oci://registry.example/team/schema:stable",
	)
	result := Run(t.Context(), Config{
		Operation:   OperationResolve,
		Environment: environment,
		Executor:    executor,
		TempDir:     directory,
	})
	if result.Error != nil || result.ResolvedDigest != digest {
		t.Fatalf("Run() = %#v", result)
	}
	if snapshotPath == "" {
		t.Fatal("Ptah child was not inspected")
	}
	if _, err := os.Stat(snapshotPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runner CA snapshot remains after Run(): %v", err)
	}
}

func TestSnapshotOCISourceAccessCopiesValidatedBytesExclusively(t *testing.T) {
	t.Parallel()

	content := []byte("-----BEGIN CERTIFICATE-----\nguard\n-----END CERTIFICATE-----\n")
	directory := t.TempDir()
	source := filepath.Join(directory, "source.pem")
	destination := filepath.Join(directory, "snapshot.pem")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SnapshotOCISourceAccess(
		"oci://registry.example/team/schema:stable",
		authenticatedCAEnvironment(source, sha256Digest(content)),
		destination,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot) != string(content) {
		t.Fatalf("snapshot = %q, want exact validated bytes", snapshot)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o400 {
		t.Fatalf("snapshot mode = %#o, want 0400", got)
	}
	if err := SnapshotOCISourceAccess(
		"oci://registry.example/team/schema:stable",
		authenticatedCAEnvironment(source, sha256Digest([]byte("changed"))),
		destination,
	); err == nil {
		t.Fatal("SnapshotOCISourceAccess() overwrote an existing snapshot")
	}
}

func TestAnonymousCASnapshotDoesNotRequireSecretGrant(t *testing.T) {
	t.Parallel()

	content := []byte("anonymous CA")
	directory := t.TempDir()
	source := filepath.Join(directory, "source.pem")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	environment := []string{
		EnvOCIHasCA + "=true",
		EnvOCICASourceFile + "=" + source,
	}
	prepared, cleanup, err := PrepareOCISourceAccess(
		"oci://registry.example/team/schema:stable",
		environment,
		directory,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if environmentMap(prepared)[envOCICAFile] == "" {
		t.Fatal("anonymous custom CA was not snapshotted")
	}
	if err := ValidateOCISourceAccess("oci://registry.example/team/schema:stable", environment); err == nil {
		t.Fatal("validate-only API accepted a mutable custom CA without snapshotting it")
	}
}

func TestPrepareOCISourceAccessRejectsInvalidCAFiles(t *testing.T) {
	t.Parallel()

	for name, content := range map[string][]byte{
		"empty":     {},
		"oversized": make([]byte, maxOCICABytes+1),
	} {
		name, content := name, content
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			directory := t.TempDir()
			source := filepath.Join(directory, "ca.pem")
			if err := os.WriteFile(source, content, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, cleanup, err := PrepareOCISourceAccess(
				"oci://registry.example/team/schema:stable",
				authenticatedCAEnvironment(source, sha256Digest(content)),
				directory,
			); err == nil {
				cleanup()
				t.Fatal("PrepareOCISourceAccess() accepted invalid CA bytes")
			}
		})
	}
}

func TestChildEnvironmentRemovesOCIAuthorityGrants(t *testing.T) {
	t.Parallel()

	environment := []string{
		EnvOCIAuthRegistryGrant + "=registry.example",
		EnvOCIAllowPlainHTTPGrant + "=true",
		EnvOCIAuthMode + "=Environment",
		EnvOCIHasCA + "=false",
		EnvOCICASourceFile + "=/credentials/ca-source/ca.pem",
		EnvOCICASHA256Grant + "=sha256:" + strings.Repeat("a", 64),
		"PTAH_OCI_USERNAME=robot",
	}
	child := strings.Join(childEnvironment(environment), "\n")
	if strings.Contains(child, "PTAH_OPERATOR_OCI_") || !strings.Contains(child, "PTAH_OCI_USERNAME=robot") {
		t.Fatalf("child environment = %q", child)
	}
}

func authenticatedCAEnvironment(path, grant string) []string {
	return []string{
		EnvOCIAuthMode + "=Environment",
		envOCIRegistry + "=registry.example",
		EnvOCIAuthRegistryGrant + "=registry.example",
		EnvOCIHasCA + "=true",
		EnvOCICASourceFile + "=" + path,
		EnvOCICASHA256Grant + "=" + grant,
	}
}
