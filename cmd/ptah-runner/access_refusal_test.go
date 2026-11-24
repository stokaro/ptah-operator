package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// A step that checks a source's access leaves its refusal in the termination
// message, as the record the controller reads, and logs the same words. Every
// other failure of the same step leaves the message as the kubelet made it:
// empty, so a fetch's log tail stands in for it and is never read as a record.
func TestRunLeavesTheAccessRefusalInTheTerminationMessage(t *testing.T) {
	t.Parallel()
	pinned := "oci://registry.example/team/schema@sha256:" + strings.Repeat("a", 64)
	digest := "sha256:" + strings.Repeat("c", 64)
	gitSource := []string{
		runnerProtocol, "PATH=" + os.Getenv("PATH"),
		runner.EnvGitURL + "=https://git.example.com/acme/app.git", runner.EnvGitBranch + "=main",
		runner.EnvGitCommit + "=" + strings.Repeat("3", 40), runner.EnvGitPath + "=db/schema.hcl",
		runner.EnvGitSelects + "=file", runner.EnvExpectedContentDigest + "=" + digest,
	}
	httpsSource := []string{
		runnerProtocol, "PATH=" + os.Getenv("PATH"),
		runner.EnvHTTPSURL + "=https://files.example.com/app/1.4.0/app.tar.gz",
		runner.EnvHTTPSDigest + "=sha256:" + strings.Repeat("d", 64), runner.EnvHTTPSFormat + "=TarGzip",
		runner.EnvHTTPSPath + "=app/db/migrations", runner.EnvHTTPSSelects + "=directory",
		runner.EnvExpectedContentDigest + "=" + digest,
	}
	s3Source := []string{
		runnerProtocol, "PATH=" + os.Getenv("PATH"),
		runner.EnvS3Endpoint + "=https://objects.example.com", runner.EnvS3Region + "=us-east-1",
		runner.EnvS3Bucket + "=orders-releases", runner.EnvS3Key + "=app/1.4.0/app.tar.gz",
		runner.EnvS3Digest + "=sha256:" + strings.Repeat("d", 64), runner.EnvS3Format + "=TarGzip",
		runner.EnvS3Path + "=app/db/migrations", runner.EnvS3Selects + "=directory",
		runner.EnvExpectedContentDigest + "=" + digest,
	}
	for _, row := range []struct {
		name        string
		arguments   func(binary, destination string) []string
		environment []string
		// record is what the termination message has to hold; empty, it has
		// to stay empty.
		record   runner.AccessRefusal
		ptahRuns bool
	}{
		{
			name:      "the OCI guard refuses a grant for another registry",
			arguments: func(string, string) []string { return []string{"--validate-oci-source", pinned} },
			environment: []string{runnerProtocol, runner.EnvOCIAuthMode + "=Environment", "PTAH_OCI_REGISTRY=registry.example",
				runner.EnvOCIAuthRegistryGrant + "=attacker.example"},
			record: runner.AccessRefusal{Code: runner.CodeInvalidOCIAccess, Message: "registry credential is not granted for the OCI source authority"},
		},
		{
			name:      "the OCI guard grants the access",
			arguments: func(string, string) []string { return []string{"--validate-oci-source", pinned} },
			environment: []string{runnerProtocol, runner.EnvOCIAuthMode + "=Environment", "PTAH_OCI_REGISTRY=registry.example",
				runner.EnvOCIAuthRegistryGrant + "=registry.example"},
		},
		{
			name:      "the OCI guard refuses a Job built for another protocol",
			arguments: func(string, string) []string { return []string{"--validate-oci-source", pinned} },
			environment: []string{runner.EnvRunnerProtocolVersion + "=0", runner.EnvOCIAuthMode + "=Environment",
				"PTAH_OCI_REGISTRY=registry.example", runner.EnvOCIAuthRegistryGrant + "=attacker.example"},
		},
		{
			name:        "the Git fetch refuses a credential granted for another host",
			arguments:   gitFetchArguments,
			environment: append(append([]string(nil), gitSource...), runner.EnvGitPassword+"=git-password-41aa", runner.EnvGitHostGrant+"=attacker.example"),
			record:      runner.AccessRefusal{Code: runner.CodeInvalidGitAccess, Message: "the Git credential is not granted for the source's host"},
		},
		{
			name:        "the HTTPS fetch refuses a credential granted for another host",
			arguments:   httpsFetchArguments,
			environment: append(append([]string(nil), httpsSource...), runner.EnvHTTPSToken+"=https-token-7f3a", runner.EnvHTTPSHostGrant+"=attacker.example"),
			record:      runner.AccessRefusal{Code: runner.CodeInvalidHTTPSAccess, Message: "the HTTPS credential is not granted for the source's host"},
		},
		{
			name:      "the S3 fetch refuses a key pair granted for another bucket",
			arguments: s3FetchArguments,
			environment: append(append([]string(nil), s3Source...), runner.EnvS3AccessKeyID+"=AKIA-s3-0b1e",
				runner.EnvS3SecretAccessKey+"=s3-secret-c4fe", runner.EnvS3HostGrant+"=objects.example.com",
				runner.EnvS3BucketGrant+"=orders-private"),
			record: runner.AccessRefusal{Code: runner.CodeInvalidS3Access,
				Message: `the S3 credential is granted for the bucket "orders-private", not the source's`},
		},
		// Ptah ran and refused the content: that is no refusal of the access,
		// and its words stay in the log.
		{
			name:        "the Git fetch's pull fails",
			arguments:   gitFetchArguments,
			environment: gitSource,
			ptahRuns:    true,
		},
		{
			name:        "the HTTPS fetch is given no source",
			arguments:   httpsFetchArguments,
			environment: []string{runnerProtocol},
		},
		// The endpoint is not reached: the object's read fails, which is no
		// refusal of the access either.
		{
			name:        "the S3 fetch cannot reach the endpoint",
			arguments:   s3FetchArguments,
			environment: append(append([]string(nil), s3Source...), runner.EnvS3Endpoint+"=https://127.0.0.1:1"),
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			binary, ran := fetchScript(t, 2)
			terminationLog := filepath.Join(t.TempDir(), "termination-log")
			if err := os.WriteFile(terminationLog, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			exitCode := run(context.Background(), row.arguments(binary, filepath.Join(t.TempDir(), "source")), &stdout, &stderr, row.environment, terminationLog)
			message, err := os.ReadFile(terminationLog)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(ran); row.ptahRuns != (err == nil) {
				t.Fatalf("Ptah ran: %v, want %v", err == nil, row.ptahRuns)
			}
			if row.record == (runner.AccessRefusal{}) {
				if len(message) != 0 {
					t.Fatalf("the termination message is %q, want it empty (exit %d, stderr %q)", message, exitCode, stderr.String())
				}
				return
			}
			if exitCode != 2 || stdout.Len() != 0 {
				t.Fatalf("run() = exit %d, stdout %q, want 2 and nothing", exitCode, stdout.String())
			}
			parsed, err := runner.ParseAccessRefusal(string(message))
			if err != nil || parsed != row.record {
				t.Fatalf("the termination message %q reads %#v, %v; want %#v", message, parsed, err, row.record)
			}
			if !strings.Contains(stderr.String(), row.record.Message) {
				t.Fatalf("the log %q does not say %q", stderr.String(), row.record.Message)
			}
			for _, credential := range []string{"git-password-41aa", "https-token-7f3a", "s3-secret-c4fe"} {
				if strings.Contains(string(message), credential) || strings.Contains(stderr.String(), credential) {
					t.Fatalf("the refusal carries the credential %q", credential)
				}
			}
		})
	}
}

// The kubelet creates the termination message's file. A runner that finds
// none creates nothing, says so, and still refuses.
func TestRunRefusesWithoutATerminationMessageFile(t *testing.T) {
	t.Parallel()
	terminationLog := filepath.Join(t.TempDir(), "termination-log")
	var stdout, stderr bytes.Buffer
	exitCode := run(context.Background(),
		[]string{"--validate-oci-source", "oci://registry.example/team/schema@sha256:" + strings.Repeat("a", 64)},
		&stdout, &stderr,
		[]string{runnerProtocol, runner.EnvOCIAuthMode + "=Environment", "PTAH_OCI_REGISTRY=registry.example", runner.EnvOCIAuthRegistryGrant + "=attacker.example"},
		terminationLog,
	)
	if exitCode != 2 || !strings.Contains(stderr.String(), "could not write the access refusal record") ||
		!strings.Contains(stderr.String(), "registry credential is not granted for the OCI source authority") {
		t.Fatalf("run() = exit %d, stderr %q", exitCode, stderr.String())
	}
	if _, err := os.Stat(terminationLog); !os.IsNotExist(err) {
		t.Fatalf("the runner created the termination message's file: %v", err)
	}
}

func gitFetchArguments(binary, destination string) []string {
	return []string{"--fetch-git-source", destination, "--ptah-binary", binary}
}

func httpsFetchArguments(binary, destination string) []string {
	return []string{"--fetch-https-source", destination, "--ptah-binary", binary}
}

func s3FetchArguments(binary, destination string) []string {
	return []string{"--fetch-s3-source", destination, "--ptah-binary", binary}
}
