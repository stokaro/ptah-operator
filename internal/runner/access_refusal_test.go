package runner

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// The credential values no access refusal may carry. Each is distinct from
// every word a check writes, and none needs escaping, so a value that reached
// a record reads in it as written.
const (
	refusalUsername = "deploy-bot-USERNAME-9c1e"
	refusalPassword = "s3cr3t-PASSWORD-41aa"
	refusalToken    = "ghp-TOKEN-7f3a0b"
	refusalSSHKey   = "/credentials/git/ssh-KEY-c0de"
)

var refusalCredentials = []string{refusalUsername, refusalPassword, refusalToken, refusalSSHKey}

func TestAnAccessRefusalRoundTrips(t *testing.T) {
	t.Parallel()
	for name, refusal := range map[string]AccessRefusal{
		"an OCI refusal": {Code: CodeInvalidOCIAccess, Message: "registry credential is not granted for the OCI source authority"},
		// The grant is escaped by the check, and the escape survives.
		"an escaped grant":             {Code: CodeInvalidGitAccess, Message: `the Git credential's host grant is refused: the host "git.example.com\n" is not printable ASCII`},
		"characters HTML would escape": {Code: CodeInvalidHTTPSAccess, Message: "<the HTTPS credential & its host>"},
		"a message at its bound":       {Code: CodeInvalidHTTPSAccess, Message: strings.Repeat("é", maxAccessRefusalMessageBytes/2)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			record, err := EncodeAccessRefusal(refusal)
			if err != nil {
				t.Fatalf("EncodeAccessRefusal() error = %v", err)
			}
			if !strings.HasSuffix(string(record), "\n") || strings.Count(string(record), "\n") != 1 {
				t.Fatalf("the record %q is not one line", record)
			}
			for _, message := range []string{string(record), strings.TrimSuffix(string(record), "\n")} {
				parsed, err := ParseAccessRefusal(message)
				if err != nil || parsed != refusal {
					t.Fatalf("ParseAccessRefusal(%q) = %#v, %v; want %#v", message, parsed, err, refusal)
				}
			}
		})
	}
}

// TestParseAccessRefusalReadsOnlyTheRecord holds the reader to the record and
// nothing else. A Git or an HTTPS fetch falls back to its log, so what the
// kubelet reports may be a log tail; the shapes a tail can take, and the ways a
// record can be damaged, are each refused, and the record itself is read.
func TestParseAccessRefusalReadsOnlyTheRecord(t *testing.T) {
	t.Parallel()
	encoded, err := EncodeAccessRefusal(AccessRefusal{Code: CodeInvalidGitAccess, Message: "the Git credential is not granted for the source's host"})
	if err != nil {
		t.Fatal(err)
	}
	record := string(encoded)
	line := strings.TrimSuffix(record, "\n")
	document := strings.TrimPrefix(line, accessRefusalMarker)
	for _, row := range []struct {
		name, message, refusal string
	}{
		{name: "the record", message: record},
		{name: "text in front of the record", message: ": " + record, refusal: "is not an access refusal"},
		{name: "the line the runner logs", message: "ptah-runner: git_source_refused: the Git credential is not granted for the source's host\n",
			refusal: "is not an access refusal"},
		// What a fetch's log line holds when Ptah's own words carry a record.
		{name: "a record written as free text", message: freeText(record, NewRedactor(nil), 2048) + "\n", refusal: "is not an access refusal"},
		{name: "a record after a log line", message: "ptah-runner: an earlier line\n" + record, refusal: "is not an access refusal"},
		{name: "a log line after the record", message: record + "ptah-runner: a later line\n", refusal: "is not one line"},
		{name: "two records", message: record + record, refusal: "is not one line"},
		{name: "a carriage return", message: line + "\r\n", refusal: "is not one line"},
		{name: "the start cut off", message: record[4:], refusal: "is not an access refusal"},
		{name: "the end cut off", message: record[:len(record)-8], refusal: "is not a document this build can decode"},
		{name: "the marker without its space", message: strings.TrimSpace(accessRefusalMarker) + document, refusal: "is not an access refusal"},
		{name: "the marker in lower case", message: strings.ToLower(accessRefusalMarker) + document, refusal: "is not an access refusal"},
		{name: "a field this build does not know", message: accessRefusalMarker + `{"code":"invalid_git_access","message":"x","username":"bot"}`,
			refusal: "is not a document this build can decode"},
		{name: "a second document", message: line + " " + document, refusal: "carries data after its document"},
		{name: "a byte after the document", message: line + "x", refusal: "carries data after its document"},
		{name: "a code no access check writes", message: accessRefusalMarker + `{"code":"invalid_configuration","message":"x"}`,
			refusal: "is not a source access refusal"},
		{name: "no document", message: accessRefusalMarker + "null", refusal: "is not a source access refusal"},
		{name: "no message", message: accessRefusalMarker + `{"code":"invalid_git_access"}`, refusal: "carries no message"},
		{name: "a message over its bound",
			message: accessRefusalMarker + `{"code":"invalid_git_access","message":"` + strings.Repeat("x", maxAccessRefusalMessageBytes+1) + `"}`,
			refusal: "message exceeds its bound"},
		{name: "a record over its bound",
			message: accessRefusalMarker + `{"code":"invalid_git_access","message":"x"` + strings.Repeat(" ", MaxAccessRefusalBytes) + `}`,
			refusal: "the access refusal exceeds its bound"},
		{name: "nothing", message: "", refusal: "is not an access refusal"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := ParseAccessRefusal(row.message)
			if row.refusal == "" {
				if err != nil {
					t.Fatalf("ParseAccessRefusal() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), row.refusal) {
				t.Fatalf("ParseAccessRefusal(%q) = %#v, %v; want a refusal naming %q", row.message, parsed, err, row.refusal)
			}
		})
	}
}

func TestEncodeAccessRefusalRefusesWhatTheReaderWould(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name    string
		refusal AccessRefusal
		want    string
	}{
		{name: "a code no access check writes", refusal: AccessRefusal{Code: CodeGitSourceRefused, Message: "x"}, want: "is not a source access refusal"},
		{name: "no message", refusal: AccessRefusal{Code: CodeInvalidOCIAccess}, want: "carries no message"},
		{name: "a message over its bound", refusal: AccessRefusal{Code: CodeInvalidOCIAccess, Message: strings.Repeat("x", maxAccessRefusalMessageBytes+1)},
			want: "message exceeds its bound"},
		{name: "a message that is not UTF-8", refusal: AccessRefusal{Code: CodeInvalidOCIAccess, Message: "a\xffb"}, want: "is not valid UTF-8"},
		{name: "a message JSON escapes past the bound", refusal: AccessRefusal{Code: CodeInvalidOCIAccess, Message: strings.Repeat("\x01", 512)},
			want: "the maximum is"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if record, err := EncodeAccessRefusal(row.refusal); err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("EncodeAccessRefusal() = %q, %v; want a refusal naming %q", record, err, row.want)
			}
		})
	}
}

// TestAnAccessRefusalIsRedactedAndFits holds the record to the frame's
// redaction for a credential the sentence came to hold, and to its bound for a
// message JSON has to escape: either would otherwise leave the status a
// credential or no sentence at all.
func TestAnAccessRefusalIsRedactedAndFits(t *testing.T) {
	t.Parallel()
	environment := []string{EnvGitPassword + "=" + refusalPassword, EnvHTTPSToken + "=" + refusalToken, EnvOCIPassword + "=" + refusalPassword}
	t.Run("a credential in the sentence", func(t *testing.T) {
		t.Parallel()
		refused := accessRefused(CodeInvalidGitAccess, errors.New("near "+refusalPassword+" and "+refusalToken), NewRedactor(environment))
		if want := "near " + RedactionMarker + " and " + RedactionMarker; refused.Refusal.Message != want || refused.Error() != want {
			t.Fatalf("the refusal reads %q and %q, want %q", refused.Refusal.Message, refused.Error(), want)
		}
	})
	for name, sentence := range map[string]string{
		"a long sentence":              strings.Repeat("a long sentence ", 512),
		"a sentence JSON escapes":      strings.Repeat("\x01", 2048),
		"escapes between characters":   strings.Repeat("é\x01", 1024),
		"a sentence that is not UTF-8": strings.Repeat("\xff", 2048),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			refused := accessRefused(CodeInvalidHTTPSAccess, errors.New(sentence), NewRedactor(environment))
			record, err := EncodeAccessRefusal(refused.Refusal)
			if err != nil {
				t.Fatalf("EncodeAccessRefusal() error = %v", err)
			}
			if parsed, err := ParseAccessRefusal(string(record)); err != nil || parsed != refused.Refusal {
				t.Fatalf("ParseAccessRefusal() = %#v, %v", parsed, err)
			}
			if !utf8.ValidString(refused.Refusal.Message) || refused.Refusal.Message == "" {
				t.Fatalf("the message %q is empty or not UTF-8", refused.Refusal.Message)
			}
		})
	}
}

// TestARefusedAccessLeavesItsSentenceAndNoCredential walks the refusals each
// source-access check writes before a fetch: the OCI guard's, and the Git and
// HTTPS fetches', which check the access themselves. Every credential the
// branch allows is set, the username included, which the redactor does not
// protect; each row has to leave a record under its kind's code, holding the
// check's own sentence and none of the credentials, before Ptah runs.
func TestARefusedAccessLeavesItsSentenceAndNoCredential(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	ca := []byte("-----BEGIN CERTIFICATE-----\nrefusal\n-----END CERTIFICATE-----\n")
	caFile := writeGitTrustSource(t, directory, "ca.pem", ca)
	emptyFile := writeGitTrustSource(t, directory, "empty.pem", nil)
	knownHosts := []byte("git.example.com ssh-ed25519 AAAA\n")
	knownHostsFile := writeGitTrustSource(t, directory, "known_hosts", knownHosts)
	missing := filepath.Join(directory, "missing")
	otherDigest := "sha256:" + strings.Repeat("0", 64)

	const ociReference = "oci://registry.example/team/schema@sha256:3f1c2b0a9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3f1c2b0a9d8e7f6a5b4c3d2e"
	ociCredential := []string{envOCIUsername + "=" + refusalUsername, EnvOCIPassword + "=" + refusalPassword, EnvOCIToken + "=" + refusalToken}
	ociAuthenticated := append([]string{EnvOCIAuthMode + "=" + ociAuthEnvironment, envOCIRegistry + "=registry.example",
		EnvOCIAuthRegistryGrant + "=registry.example"}, ociCredential...)
	oci := func(snapshot bool, environment ...string) func(t *testing.T) error {
		return func(t *testing.T) error {
			destination := ""
			if snapshot {
				destination = filepath.Join(t.TempDir(), "ca.pem")
			}
			return GuardOCISourceAccess(ociReference, environment, destination)
		}
	}

	gitCredential := []string{EnvGitUsername + "=" + refusalUsername, EnvGitPassword + "=" + refusalPassword}
	gitGranted := append([]string{EnvGitHostGrant + "=git.example.com"}, gitCredential...)
	sshGranted := []string{EnvGitURL + "=ssh://git@git.example.com:2222/acme/app.git", EnvGitSSHKeyFile + "=" + refusalSSHKey,
		EnvGitHostGrant + "=git.example.com:2222"}
	git := func(extra ...string) func(t *testing.T) error {
		return func(t *testing.T) error {
			executor := &scriptedExecutor{t: t}
			environment := gitFetchEnvironment("file", "db/schema.hcl", otherDigest, extra...)
			err := FetchGitSource(t.Context(), "ptah", environment, filepath.Join(t.TempDir(), "source"), t.TempDir(), executor, nil)
			if len(executor.calls) != 0 {
				t.Errorf("Ptah ran %d times before the refusal", len(executor.calls))
			}
			return err
		}
	}

	httpsCredential := []string{EnvHTTPSUsername + "=" + refusalUsername, EnvHTTPSPassword + "=" + refusalPassword}
	httpsGranted := append([]string{EnvHTTPSHostGrant + "=files.example.com"}, httpsCredential...)
	https := func(extra ...string) func(t *testing.T) error {
		return func(t *testing.T) error {
			executor := &scriptedExecutor{t: t}
			environment := httpsFetchEnvironment("TarGzip", "directory", "app/db/migrations", otherDigest, extra...)
			err := FetchHTTPSSource(t.Context(), "ptah", environment, filepath.Join(t.TempDir(), "source"), t.TempDir(), executor, nil)
			if len(executor.calls) != 0 {
				t.Errorf("Ptah ran %d times before the refusal", len(executor.calls))
			}
			return err
		}
	}

	s3Pair := []string{EnvS3AccessKeyID + "=" + refusalUsername, EnvS3SecretAccessKey + "=" + refusalPassword}
	s3Granted := append([]string{EnvS3HostGrant + "=objects.example.com"}, s3Pair...)
	s3 := func(extra ...string) func(t *testing.T) error {
		return func(t *testing.T) error {
			environment := s3FetchEnvironment("https://objects.example.com", "app/1.4.0/app.tar.gz", otherDigest, "TarGzip", "directory",
				"app/db/migrations", otherDigest, extra...)
			return FetchS3Source(t.Context(), environment, filepath.Join(t.TempDir(), "source"), t.TempDir())
		}
	}

	with := func(base []string, extra ...string) []string { return append(append([]string(nil), base...), extra...) }
	for _, row := range []struct {
		name     string
		code     string
		refuse   func(t *testing.T) error
		sentence string
	}{
		{"oci/a reference with no authority", CodeInvalidOCIAccess, func(*testing.T) error {
			return GuardOCISourceAccess("oci://", ociAuthenticated, "")
		}, "does not contain a valid authority"},
		{"oci/plain HTTP not exactly true", CodeInvalidOCIAccess, oci(false, with(ociAuthenticated, envPlainHTTP+"= true")...), "PTAH_PLAIN_HTTP must be exactly true or false"},
		{"oci/a CA flag not exactly true", CodeInvalidOCIAccess, oci(false, with(ociAuthenticated, EnvOCIHasCA+"=yes")...), "must be exactly true or false"},
		{"oci/a client certificate", CodeInvalidOCIAccess, oci(false, with(ociAuthenticated, envOCIClientCertificate+"=/credentials/tls.crt")...), "client certificates are not supported"},
		{"oci/plain HTTP with a CA", CodeInvalidOCIAccess, oci(true, with(ociAuthenticated, envPlainHTTP+"=true", EnvOCIHasCA+"=true", EnvOCICASourceFile+"="+caFile)...),
			"plain HTTP cannot be combined with a registry certificate authority"},
		{"oci/a CA path Ptah would read", CodeInvalidOCIAccess, oci(false, with(ociAuthenticated, envOCICAFile+"="+caFile)...), "must be supplied only by the runner"},
		{"oci/a CA flag without its source", CodeInvalidOCIAccess, oci(true, with(ociAuthenticated, EnvOCIHasCA+"=true")...), "source binding is incomplete"},
		{"oci/a grant without authentication", CodeInvalidOCIAccess, oci(false, EnvOCIAuthRegistryGrant+"=registry.example"), "grant was supplied without registry authentication"},
		{"oci/a credential granted for another registry", CodeInvalidOCIAccess, oci(false, with(ociAuthenticated, EnvOCIAuthRegistryGrant+"=attacker.example")...),
			"registry credential is not granted for the OCI source authority"},
		{"oci/a credential scoped to another registry", CodeInvalidOCIAccess, oci(false, with(ociAuthenticated, envOCIRegistry+"=attacker.example")...),
			"credential scope does not match the OCI source authority"},
		{"oci/an authentication mode the runner does not know", CodeInvalidOCIAccess, oci(false, with(ociAuthenticated, EnvOCIAuthMode+"=Basic")...),
			`unsupported OCI registry authentication mode "Basic"`},
		{"oci/authenticated plain HTTP without its grant", CodeInvalidOCIAccess, oci(false, with(ociAuthenticated, envPlainHTTP+"=true")...),
			"authenticated plain HTTP requires an explicit credential-owner grant"},
		{"oci/a CA pin without the CA", CodeInvalidOCIAccess, oci(false, with(ociAuthenticated, EnvOCICASHA256Grant+"="+gitTestDigest(ca))...),
			"the registry credential's Secret pins caSHA256, so the source has to select that CA in transport.caFrom"},
		{"oci/an authenticated CA without its pin", CodeInvalidOCIAccess, oci(true, with(ociAuthenticated, EnvOCIHasCA+"=true", EnvOCICASourceFile+"="+caFile)...),
			"authenticated custom CA requires a lowercase SHA-256 credential-owner grant"},
		{"oci/a CA pin without authentication", CodeInvalidOCIAccess, oci(true, EnvOCIHasCA+"=true", EnvOCICASourceFile+"="+caFile, EnvOCICASHA256Grant+"="+gitTestDigest(ca)),
			"digest grant was supplied without registry authentication"},
		{"oci/a CA that cannot be opened", CodeInvalidOCIAccess, oci(true, with(ociAuthenticated, EnvOCIHasCA+"=true", EnvOCICASourceFile+"="+missing, EnvOCICASHA256Grant+"="+gitTestDigest(ca))...),
			"open registry certificate authority"},
		{"oci/an empty CA", CodeInvalidOCIAccess, oci(true, with(ociAuthenticated, EnvOCIHasCA+"=true", EnvOCICASourceFile+"="+emptyFile, EnvOCICASHA256Grant+"="+gitTestDigest(ca))...),
			"registry certificate authority is empty"},
		{"oci/another CA", CodeInvalidOCIAccess, oci(true, with(ociAuthenticated, EnvOCIHasCA+"=true", EnvOCICASourceFile+"="+caFile, EnvOCICASHA256Grant+"="+otherDigest)...),
			"registry certificate authority bytes do not match the credential-owner grant"},
		{"oci/a CA the guard was not asked to copy", CodeInvalidOCIAccess, oci(false, with(ociAuthenticated, EnvOCIHasCA+"=true", EnvOCICASourceFile+"="+caFile, EnvOCICASHA256Grant+"="+gitTestDigest(ca))...),
			"requires an immutable snapshot"},
		{"oci/a copy asked for without a CA", CodeInvalidOCIAccess, oci(true, ociAuthenticated...), "snapshot requested without a custom CA"},

		{"git/a credential without a grant", CodeInvalidGitAccess, git(gitCredential...), "the Git credential is not granted for the source's host"},
		{"git/a credential granted for another host", CodeInvalidGitAccess, git(with(gitCredential, EnvGitHostGrant+"=attacker.example")...),
			"the Git credential is not granted for the source's host"},
		{"git/a grant that is not ASCII", CodeInvalidGitAccess, git(with(gitCredential, EnvGitHostGrant+"=git.example.com\n")...),
			`host grant is refused: the host "git.example.com\n" is not printable ASCII`},
		{"git/a grant without a credential", CodeInvalidGitAccess, git(EnvGitHostGrant + "=git.example.com"), "a Git grant was supplied without a credential"},
		{"git/a CA the owner did not grant", CodeInvalidGitAccess, git(with(gitGranted, EnvGitCASourceFile+"="+caFile)...),
			"the Git certificate authority is not the one the credential's owner granted"},
		{"git/a CA pin without the CA", CodeInvalidGitAccess, git(with(gitGranted, EnvGitCASHA256Grant+"="+gitTestDigest(ca))...),
			"the credential's Secret pins caSHA256, so the source has to select it in transport.caFrom"},
		{"git/a CA that cannot be read", CodeInvalidGitAccess, git(with(gitGranted, EnvGitCASourceFile+"="+missing, EnvGitCASHA256Grant+"="+gitTestDigest(ca))...),
			"read the Git certificate authority"},
		{"git/known hosts the owner did not grant", CodeInvalidGitAccess,
			git(with(sshGranted, EnvGitKnownHostsSourceFile+"="+knownHostsFile, EnvGitKnownHostsSHA256Grant+"="+otherDigest)...),
			"the Git known hosts is not the one the credential's owner granted"},
		{"git/an ssh source without known hosts", CodeInvalidGitAccess, git(sshGranted...), "an ssh:// Git source needs known hosts"},
		{"git/a URL that carries the credential", CodeInvalidGitAccess,
			git(EnvGitURL + "=https://" + refusalUsername + ":" + refusalPassword + "@git.example.com/acme/app.git"), "the Git source URL carries a credential"},
		{"git/an ssh URL that carries a password", CodeInvalidGitAccess,
			git(EnvGitURL + "=ssh://" + refusalUsername + ":" + refusalPassword + "@git.example.com/acme/app.git"), "the Git source URL carries a credential"},
		{"git/a URL with the credential in its query", CodeInvalidGitAccess,
			git(with(gitGranted, EnvGitURL+"=https://git.example.com/acme/app.git?token="+refusalToken)...), "the Git source URL has a query or a fragment"},

		{"https/a token beside a username", CodeInvalidHTTPSAccess, https(with(httpsGranted, EnvHTTPSToken+"="+refusalToken)...),
			"the HTTPS credential holds a token beside a username or a password"},
		{"https/a username without a password", CodeInvalidHTTPSAccess, https(EnvHTTPSHostGrant+"=files.example.com", EnvHTTPSUsername+"="+refusalUsername),
			"the HTTPS credential holds a username without a password"},
		{"https/a password without a username", CodeInvalidHTTPSAccess, https(EnvHTTPSHostGrant+"=files.example.com", EnvHTTPSPassword+"="+refusalPassword),
			"the HTTPS credential holds a password without a username"},
		{"https/a credential granted for another host", CodeInvalidHTTPSAccess, https(with(httpsCredential, EnvHTTPSHostGrant+"=attacker.example")...),
			"the HTTPS credential is not granted for the source's host"},
		{"https/a token without a grant", CodeInvalidHTTPSAccess, https(EnvHTTPSToken + "=" + refusalToken), "the HTTPS credential is not granted for the source's host"},
		{"https/a grant that is not ASCII", CodeInvalidHTTPSAccess, https(with(httpsCredential, EnvHTTPSHostGrant+"=files.example.com\n")...),
			`host grant is refused: the host "files.example.com\n" is not printable ASCII`},
		{"https/a grant without a credential", CodeInvalidHTTPSAccess, https(EnvHTTPSHostGrant + "=files.example.com"), "an HTTPS grant was supplied without a credential"},
		{"https/a CA pin without the CA", CodeInvalidHTTPSAccess, https(with(httpsGranted, EnvHTTPSCASHA256Grant+"="+gitTestDigest(ca))...),
			"the credential's Secret pins caSHA256, so the source has to select that CA in transport.caFrom"},
		{"https/a CA the owner did not grant", CodeInvalidHTTPSAccess, https(with(httpsGranted, EnvHTTPSCASourceFile+"="+caFile, EnvHTTPSCASHA256Grant+"="+otherDigest)...),
			"the HTTPS certificate authority is not the one the credential's owner granted"},
		{"https/a CA that cannot be read", CodeInvalidHTTPSAccess, https(with(httpsGranted, EnvHTTPSCASourceFile+"="+missing)...),
			"read the HTTPS certificate authority"},
		{"https/a URL that carries the credential", CodeInvalidHTTPSAccess,
			https(EnvHTTPSURL + "=https://" + refusalUsername + ":" + refusalPassword + "@files.example.com/app/1.4.0/app.tar.gz"), "the HTTPS source URL carries a credential"},
		{"https/a URL with the credential in its query", CodeInvalidHTTPSAccess,
			https(with(httpsGranted, EnvHTTPSURL+"=https://files.example.com/app/1.4.0/app.tar.gz?token="+refusalToken)...), "the HTTPS source URL has a query or a fragment"},

		{"s3/a key pair without a grant", CodeInvalidS3Access, s3(s3Pair...), "the S3 credential is not granted for the source's endpoint"},
		{"s3/a key pair granted for another endpoint", CodeInvalidS3Access, s3(with(s3Pair, EnvS3HostGrant+"=attacker.example")...),
			"the S3 credential is not granted for the source's endpoint"},
		{"s3/a key pair granted for another bucket", CodeInvalidS3Access, s3(with(s3Granted, EnvS3BucketGrant+"=orders-private")...),
			`the S3 credential is granted for the bucket "orders-private", not the source's`},
		{"s3/a grant that is not ASCII", CodeInvalidS3Access, s3(with(s3Pair, EnvS3HostGrant+"=objects.example.com\n")...),
			`host grant is refused: the host "objects.example.com\n" is not printable ASCII`},
		{"s3/an access key ID without its secret", CodeInvalidS3Access, s3(EnvS3HostGrant+"=objects.example.com", EnvS3AccessKeyID+"="+refusalUsername),
			"the S3 credential holds an access key ID without a secret access key"},
		{"s3/a secret without its access key ID", CodeInvalidS3Access, s3(EnvS3HostGrant+"=objects.example.com", EnvS3SecretAccessKey+"="+refusalPassword),
			"the S3 credential holds a secret access key without an access key ID"},
		{"s3/a session token without a key pair", CodeInvalidS3Access, s3(EnvS3HostGrant+"=objects.example.com", EnvS3SessionToken+"="+refusalToken),
			"the S3 credential holds a session token without a key pair"},
		{"s3/a grant without a key pair", CodeInvalidS3Access, s3(EnvS3HostGrant + "=objects.example.com"), "an S3 grant was supplied without a key pair"},
		{"s3/a CA pin without the CA", CodeInvalidS3Access, s3(with(s3Granted, EnvS3CASHA256Grant+"="+gitTestDigest(ca))...),
			"the credential's Secret pins caSHA256, so the source has to select that CA in transport.caFrom"},
		{"s3/a CA the owner did not grant", CodeInvalidS3Access, s3(with(s3Granted, EnvS3CASourceFile+"="+caFile, EnvS3CASHA256Grant+"="+otherDigest)...),
			"the S3 certificate authority is not the one the credential's owner granted"},
		{"s3/a CA that cannot be read", CodeInvalidS3Access, s3(with(s3Granted, EnvS3CASourceFile+"="+missing)...),
			"read the S3 certificate authority"},
		{"s3/a key that climbs", CodeInvalidS3Access, s3(with(s3Granted, EnvS3Key+"=app/../app.tar.gz")...), `".." element`},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			failure := row.refuse(t)
			var refused *AccessRefusalError
			if !errors.As(failure, &refused) {
				t.Fatalf("the check returned %v, want an access refusal", failure)
			}
			if refused.Refusal.Code != row.code || !strings.Contains(refused.Refusal.Message, row.sentence) {
				t.Fatalf("the refusal is %q, want %s naming %q", refused.Refusal, row.code, row.sentence)
			}
			record, err := EncodeAccessRefusal(refused.Refusal)
			if err != nil {
				t.Fatalf("EncodeAccessRefusal() error = %v", err)
			}
			if parsed, err := ParseAccessRefusal(string(record)); err != nil || parsed != refused.Refusal {
				t.Fatalf("ParseAccessRefusal() = %#v, %v", parsed, err)
			}
			for _, credential := range refusalCredentials {
				// The record, and the line the step logs.
				for _, text := range []string{string(record), failure.Error()} {
					if strings.Contains(text, credential) {
						t.Fatalf("%q carries the credential %q", text, credential)
					}
				}
			}
		})
	}
}

// TestTheGuardAndTheFetchSayWhatAResolveSays runs a Resolve and the step that
// checks the same access before a fetch, under the same grants, and holds the
// step's record to the Resolve frame's error: the same code and the same
// sentence. A record that drifted from the Resolve's words, or carried another
// code, would fail here.
func TestTheGuardAndTheFetchSayWhatAResolveSays(t *testing.T) {
	t.Parallel()
	const ociReference = "oci://registry.example/team/schema:stable"
	pinned := "oci://registry.example/team/schema@sha256:" + strings.Repeat("a", 64)
	for _, row := range []struct {
		name    string
		resolve []string
		step    func(t *testing.T) error
	}{
		{
			name: "an OCI credential granted for another registry",
			resolve: []string{envRequestedReference + "=" + ociReference, EnvOCIAuthMode + "=" + ociAuthEnvironment,
				envOCIRegistry + "=registry.example", EnvOCIAuthRegistryGrant + "=attacker.example"},
			step: func(*testing.T) error {
				return GuardOCISourceAccess(pinned, []string{EnvOCIAuthMode + "=" + ociAuthEnvironment,
					envOCIRegistry + "=registry.example", EnvOCIAuthRegistryGrant + "=attacker.example"}, "")
			},
		},
		{
			name: "an OCI CA pin without the CA",
			resolve: []string{envRequestedReference + "=" + ociReference, EnvOCIAuthMode + "=" + ociAuthEnvironment, envOCIRegistry + "=registry.example",
				EnvOCIAuthRegistryGrant + "=registry.example", EnvOCICASHA256Grant + "=" + "sha256:" + strings.Repeat("b", 64)},
			step: func(*testing.T) error {
				return GuardOCISourceAccess(pinned, []string{EnvOCIAuthMode + "=" + ociAuthEnvironment, envOCIRegistry + "=registry.example",
					EnvOCIAuthRegistryGrant + "=registry.example", EnvOCICASHA256Grant + "=" + "sha256:" + strings.Repeat("b", 64)}, "")
			},
		},
		{
			name:    "a Git credential granted for another host",
			resolve: []string{EnvGitPassword + "=" + refusalPassword, EnvGitHostGrant + "=attacker.example"},
			step: func(t *testing.T) error {
				environment := gitFetchEnvironment("file", "db/schema.hcl", "sha256:"+strings.Repeat("c", 64),
					EnvGitPassword+"="+refusalPassword, EnvGitHostGrant+"=attacker.example")
				return FetchGitSource(t.Context(), "ptah", environment, filepath.Join(t.TempDir(), "source"), t.TempDir(), &scriptedExecutor{t: t}, nil)
			},
		},
		{
			name:    "an HTTPS credential granted for another host",
			resolve: []string{EnvHTTPSToken + "=" + refusalToken, EnvHTTPSHostGrant + "=attacker.example"},
			step: func(t *testing.T) error {
				environment := httpsFetchEnvironment("TarGzip", "directory", "app/db/migrations", "sha256:"+strings.Repeat("c", 64),
					EnvHTTPSToken+"="+refusalToken, EnvHTTPSHostGrant+"=attacker.example")
				return FetchHTTPSSource(t.Context(), "ptah", environment, filepath.Join(t.TempDir(), "source"), t.TempDir(), &scriptedExecutor{t: t}, nil)
			},
		},
		{
			name: "an S3 key pair granted for another bucket",
			resolve: []string{EnvS3AccessKeyID + "=" + refusalUsername, EnvS3SecretAccessKey + "=" + refusalPassword,
				EnvS3HostGrant + "=objects.example.com", EnvS3BucketGrant + "=orders-private"},
			step: func(t *testing.T) error {
				environment := s3FetchEnvironment("https://objects.example.com", "app.tar.gz", "sha256:"+strings.Repeat("d", 64), "TarGzip",
					"directory", "app/db/migrations", "sha256:"+strings.Repeat("c", 64),
					EnvS3AccessKeyID+"="+refusalUsername, EnvS3SecretAccessKey+"="+refusalPassword,
					EnvS3HostGrant+"=objects.example.com", EnvS3BucketGrant+"=orders-private")
				return FetchS3Source(t.Context(), environment, filepath.Join(t.TempDir(), "source"), t.TempDir())
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var environment []string
			switch {
			case strings.HasPrefix(row.name, "a Git"):
				environment = gitResolveEnvironment(row.resolve...)
			case strings.HasPrefix(row.name, "an HTTPS"):
				environment = httpsResolveEnvironment(row.resolve...)
			case strings.HasPrefix(row.name, "an S3"):
				environment = s3ResolveEnvironment("https://objects.example.com", "app.tar.gz", "sha256:"+strings.Repeat("d", 64), row.resolve...)
			default:
				environment = withRunnerProtocol(append([]string{envOperationID + "=resolve-oci"}, row.resolve...))
			}
			executor := &scriptedExecutor{t: t}
			result := Run(t.Context(), Config{Operation: OperationResolve, Environment: environment, Executor: executor, TempDir: t.TempDir()})
			if result.Error == nil || len(executor.calls) != 0 {
				t.Fatalf("the Resolve was not refused before Ptah ran: %#v after %d calls", result, len(executor.calls))
			}
			var refused *AccessRefusalError
			if err := row.step(t); !errors.As(err, &refused) {
				t.Fatalf("the step returned %v, want an access refusal", err)
			}
			if got, want := refused.Refusal.String(), result.Error.Code+": "+result.Error.Message; got != want {
				t.Fatalf("the step's record reads %q, the Resolve's frame %q", got, want)
			}
		})
	}
}
