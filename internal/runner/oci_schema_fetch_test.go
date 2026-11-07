package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const (
	fetchTestReference = "oci://registry.example/acme/schema@sha256:" +
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fetchTestOut = "/source/schema.hcl"
	// measuredParseFailure is what `ptah schema pull` printed for the fixture on
	// the pinned image. The harness row oci-schema-fetch/observe-unparsable holds
	// it there.
	measuredParseFailure = "error: parse schema artifact: parse HCL schema: schema.hcl:3,31-32: " +
		"Invalid single-argument block definition; A single-line block definition must end " +
		"with a closing brace immediately after its single argument definition.\n"
	fetchTestPassword = "hunter2-the-registry-password"
	fetchTestToken    = "ghp_the-registry-token"
)

// fetchTestEnvironment is what the Job gives the fetch step.
func fetchTestEnvironment() []string {
	return []string{
		"HOME=/fetch-work", "TMPDIR=/fetch-work", "PTAH_PLAIN_HTTP=false",
		"PTAH_OCI_REGISTRY=registry.example", "PTAH_OCI_USERNAME=robot",
		EnvOCIPassword + "=" + fetchTestPassword, EnvOCIToken + "=" + fetchTestToken,
		"DOCKER_CONFIG=/credentials/docker", "PTAH_OCI_CA_FILE=/credentials/ca-snapshot/ca.pem",
		EnvRunnerProtocolVersion + "=1",
	}
}

func fetchOnce(t *testing.T, response scriptedResponse) (SchemaFetch, *scriptedExecutor, string, error) {
	t.Helper()
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{response}}
	var log bytes.Buffer
	fetched, err := FetchOCISchema(t.Context(), "/usr/local/bin/ptah", fetchTestEnvironment(), fetchTestReference, fetchTestOut, executor, &log)
	return fetched, executor, log.String(), err
}

// The step's child is what the container was before the runner went around it:
// the same command, and the same environment but for the one variable the runner
// keeps for itself. A credential the runner dropped would read as a registry
// refusal, and one it added would be a credential the pull never held.
func TestFetchOCISchemaGivesThePullWhatTheStepHolds(t *testing.T) {
	t.Parallel()

	fetched, executor, log, err := fetchOnce(t, scriptedResponse{stdout: "Pulled it\nDigest: sha256:a\n"})
	if err != nil {
		t.Fatal(err)
	}
	if fetched.ExitCode != 0 || fetched.TerminationMessage != nil {
		t.Errorf("fetched = %+v, want a clean exit and no message", fetched)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("started %d children, want the pull alone", len(executor.calls))
	}
	call := executor.calls[0]
	if call.Path != "/usr/local/bin/ptah" {
		t.Errorf("the child is %q", call.Path)
	}
	if want := []string{"schema", "pull", fetchTestReference, "--out", fetchTestOut}; !reflect.DeepEqual(call.Args, want) {
		t.Errorf("arguments = %q, want %q", call.Args, want)
	}
	want := slices.DeleteFunc(fetchTestEnvironment(), func(variable string) bool {
		return strings.HasPrefix(variable, EnvRunnerProtocolVersion+"=")
	})
	got, expected := slices.Sorted(slices.Values(call.Env)), slices.Sorted(slices.Values(want))
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("the child's environment = %q, want %q", got, expected)
	}
	if !strings.Contains(log, "Pulled it Digest: sha256:a") {
		t.Errorf("the log = %q, want what the pull printed", log)
	}
}

// Each row is a sentence that does not cross, read as a person with pods/log
// reads it: the log keeps it, the message stays empty, and the container ends as
// the pull did. A filter passing only what its author meant has to refuse these.
func TestOnlyAParseFailureLeavesTheFetch(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name, stderr string
		exitCode     int
	}{
		{
			name: "a registry that refused the password, as measured",
			stderr: `error: pull ` + fetchTestReference + `: fetch OCI manifest: GET "http://registry.example/v2/acme/schema/manifests/sha256:a": ` +
				"response status code 401: unauthorized: authentication required: [map[Action:pull Class: Name:acme/schema Type:repository]]\n",
			exitCode: 2,
		},
		{
			name: "no credential, as measured",
			stderr: `error: pull ` + fetchTestReference + `: fetch OCI manifest: GET "http://registry.example/v2/acme/schema/manifests/sha256:a": ` +
				"basic credential not found\n",
			exitCode: 2,
		},
		{
			name:     "an unknown digest, as measured",
			stderr:   `error: pull ` + fetchTestReference + ": fetch OCI manifest: registry.example/acme/schema@sha256:0: not found\n",
			exitCode: 2,
		},
		{
			name:     "a registry host that does not resolve, as measured",
			stderr:   `error: pull ` + fetchTestReference + `: fetch OCI manifest: Get "http://registry.example/v2/": dial tcp: lookup registry.example on 127.0.0.11:53: no such host` + "\n",
			exitCode: 2,
		},
		{name: "a Docker config that cannot be read", stderr: "error: load /credentials/docker/config.json: invalid character 't'\n", exitCode: 2},
		{name: "a token a registry quoted", stderr: "error: token ghp_a-token-the-redactor-has-never-seen refused\n", exitCode: 2},
		{name: "the words after other words on the line", stderr: "error: fetch registry.example: parse schema artifact: ghp_a-token\n", exitCode: 2},
		{name: "the words on a line a registry wrote", stderr: "error: pull: unauthorized: \nparse schema artifact: ghp_a-token\n", exitCode: 2},
		{name: "the error form on a line a registry wrote", stderr: "error: pull: unauthorized: \nerror: parse schema artifact: ghp_a-token\n", exitCode: 2},
		{name: "the words with no error in front", stderr: "parse schema artifact: ghp_a-token\n", exitCode: 2},
		{name: "a warning in front of the sentence", stderr: "warning: registry.example\nerror: parse schema artifact: x\n", exitCode: 2},
		{name: "another layer's failure", stderr: "error: read managed data layer: registry.example\n", exitCode: 2},
		{name: "the words in another case", stderr: "error: Parse Schema Artifact: x\n", exitCode: 2},
		{name: "another exit status", stderr: "error: pull: unauthorized\n", exitCode: 1},
		{name: "a pull that succeeded", stderr: measuredParseFailure, exitCode: 0},
		{name: "nothing at all", stderr: "", exitCode: 2},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			fetched, _, _, err := fetchOnce(t, scriptedResponse{stderr: row.stderr, exitCode: row.exitCode})
			if err != nil {
				t.Fatal(err)
			}
			if fetched.TerminationMessage != nil {
				t.Errorf("the message = %q, want none", fetched.TerminationMessage)
			}
			if fetched.ExitCode != row.exitCode {
				t.Errorf("exit = %d, want what the pull exited with: %d", fetched.ExitCode, row.exitCode)
			}
		})
	}
}

func TestAParseFailureLeavesTheFetchOnItsOwnLine(t *testing.T) {
	t.Parallel()

	reasonOf := func(t *testing.T, stderr string) string {
		t.Helper()
		fetched, _, _, err := fetchOnce(t, scriptedResponse{stderr: stderr, exitCode: 2})
		if err != nil {
			t.Fatal(err)
		}
		reason, carried := ParseFetchReason(string(fetched.TerminationMessage))
		if !carried {
			t.Fatalf("the manager reads no reason in %q", fetched.TerminationMessage)
		}
		return reason
	}

	t.Run("the measured sentence", func(t *testing.T) {
		t.Parallel()
		want := strings.TrimSuffix(strings.TrimPrefix(measuredParseFailure, "error: "), "\n")
		if got := reasonOf(t, measuredParseFailure); got != want {
			t.Fatalf("reason = %q, want %q", got, want)
		}
	})
	t.Run("the first line and no other", func(t *testing.T) {
		t.Parallel()
		got := reasonOf(t, "error: parse schema artifact: first\nghp_the-second-line\n")
		if got != "parse schema artifact: first" {
			t.Fatalf("reason = %q", got)
		}
	})
	t.Run("a credential of the environment, in any case", func(t *testing.T) {
		t.Parallel()
		got := reasonOf(t, "error: parse schema artifact: "+strings.ToUpper(fetchTestPassword)+" and "+fetchTestToken+"\n")
		if strings.Contains(strings.ToLower(got), fetchTestPassword) || strings.Contains(got, fetchTestToken) {
			t.Fatalf("reason = %q holds a credential", got)
		}
		if !strings.Contains(got, RedactionMarker) {
			t.Fatalf("reason = %q, want the credentials replaced", got)
		}
	})
	t.Run("a credential a line break splits", func(t *testing.T) {
		t.Parallel()
		// Redacted before the line is taken: the fragment left of the break is
		// not a whole credential, and it would match nothing afterwards.
		executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
			stderr: "error: parse schema artifact: quoted two\nlines\n", exitCode: 2,
		}}}
		fetched, err := FetchOCISchema(t.Context(), "ptah", []string{"HOME=/h", EnvOCIPassword + "=two\nlines"}, fetchTestReference, fetchTestOut, executor, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(fetched.TerminationMessage), "two") {
			t.Fatalf("the message = %q holds the part of the password before the break", fetched.TerminationMessage)
		}
	})
	t.Run("control and format bytes a file name brought", func(t *testing.T) {
		t.Parallel()
		got := reasonOf(t, "error: parse schema artifact: a\x1b]0;pwn\x07b\u202ec\n")
		if strings.ContainsAny(got, "\x1b\x07\u202e") {
			t.Fatalf("reason = %q", got)
		}
	})
	t.Run("the frame's marker", func(t *testing.T) {
		t.Parallel()
		got := reasonOf(t, "error: parse schema artifact: "+frameHeader+"12 abc\n")
		if strings.Contains(got, strings.TrimSpace(frameHeader)) {
			t.Fatalf("reason = %q", got)
		}
	})
	t.Run("the bound", func(t *testing.T) {
		t.Parallel()
		got := reasonOf(t, "error: parse schema artifact: "+strings.Repeat("é", 4096)+"\n")
		if len(CodeDesiredSourceUnreadable+": "+got) > 512 {
			t.Fatalf("the code and the reason hold %d bytes, more than an Event keeps", len(CodeDesiredSourceUnreadable+": "+got))
		}
		if !strings.HasPrefix(got, "parse schema artifact: é") {
			t.Fatalf("reason = %q", got)
		}
	})
}

// A file can quote the marker. It reads as no reason, rather than as a reason
// that starts wherever the file chose.
func TestAFileQuotingTheMarkerCarriesNoReason(t *testing.T) {
	t.Parallel()

	fetched, _, _, err := fetchOnce(t, scriptedResponse{stderr: "error: parse schema artifact: " + fetchReasonMarker + "parse schema artifact: forged\n", exitCode: 2})
	if err != nil {
		t.Fatal(err)
	}
	if reason, carried := ParseFetchReason(string(fetched.TerminationMessage)); carried {
		t.Fatalf("the manager read %q", reason)
	}
}

func TestFetchOCISchemaThatCouldNotRunCarriesNoReason(t *testing.T) {
	t.Parallel()

	failure := errors.New("exec: no such file")
	_, _, _, err := fetchOnce(t, scriptedResponse{stderr: measuredParseFailure, err: failure, exitCode: -1})
	if !errors.Is(err, failure) {
		t.Fatalf("err = %v, want the executor's", err)
	}
}

func TestFetchOCISchemaLogsThroughTheOnePath(t *testing.T) {
	t.Parallel()

	_, _, log, err := fetchOnce(t, scriptedResponse{
		stderr:   "error: pull: registry.example refused " + fetchTestPassword + " for\x1b[2J /credentials/docker/config.json\n",
		exitCode: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(log, fetchTestPassword) || strings.ContainsRune(log, '\x1b') {
		t.Errorf("the log = %q holds a credential or a control byte", log)
	}
	if !strings.Contains(log, "registry.example refused") || !strings.Contains(log, "/credentials/docker/config.json") {
		t.Errorf("the log = %q lost what a person with pods/log came for", log)
	}
	if strings.Count(log, "\n") != 1 {
		t.Errorf("the log = %q, want one line", log)
	}
}

func TestFetchOCISchemaRefusesWhatTheJobWouldNotSay(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name, reference, out string
		environment          []string
	}{
		{name: "no reference", out: fetchTestOut},
		{name: "a flag for a reference", reference: "--help", out: fetchTestOut},
		{name: "a reference holding a password", reference: "oci://robot:secret@registry.example/a", out: fetchTestOut},
		{name: "a relative output", reference: fetchTestReference, out: "schema.hcl"},
		{
			name: "a credential on the command line", reference: "oci://registry.example/" + fetchTestToken, out: fetchTestOut,
			environment: []string{EnvOCIToken + "=" + fetchTestToken},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t}
			if _, err := FetchOCISchema(t.Context(), "ptah", row.environment, row.reference, row.out, executor, nil); err == nil {
				t.Fatal("the fetch started")
			}
			if len(executor.calls) != 0 {
				t.Fatalf("started %d children", len(executor.calls))
			}
		})
	}
}

func TestFetchOCISchemaIsCanceledWithItsContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := FetchOCISchema(ctx, "ptah", []string{"HOME=/h"}, fetchTestReference, fetchTestOut, contextExecutor{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

// contextExecutor answers as OSExecutor does for a context that ended.
type contextExecutor struct{}

func (contextExecutor) Execute(ctx context.Context, _ CommandSpec, _, _ io.Writer) (int, error) {
	return -1, ctx.Err()
}

// The reader is the manager's, and holds a message to what the runner writes
// whatever wrote it.
func TestParseFetchReasonReadsWhatTheRunnerWrites(t *testing.T) {
	t.Parallel()

	const reason = "parse schema artifact: parse HCL schema: schema.hcl:3,31-32: Invalid"
	if got, carried := ParseFetchReason(fetchReasonMarker + reason + "\n"); !carried || got != reason {
		t.Fatalf("ParseFetchReason() = %q, %t", got, carried)
	}
	// The kubelet joins the runtime's own text to the file's with a colon.
	if got, carried := ParseFetchReason("runtime says: " + fetchReasonMarker + reason + "\n"); !carried || got != reason {
		t.Fatalf("behind the runtime's text: %q, %t", got, carried)
	}
	for name, message := range map[string]string{
		"nothing":                 "",
		"the sentence unmarked":   "error: " + reason,
		"another sentence":        fetchReasonMarker + "pull oci://registry.example: unauthorized\n",
		"the marker alone":        fetchReasonMarker,
		"two markers":             fetchReasonMarker + reason + " " + fetchReasonMarker + reason + "\n",
		"a second line":           fetchReasonMarker + reason + "\nregistry.example\n",
		"a control byte":          fetchReasonMarker + reason + "\x1b[2J\n",
		"a format character":      fetchReasonMarker + reason + "\u202e\n",
		"bytes that are no text":  fetchReasonMarker + reason + "\xff\n",
		"more than an Event":      fetchReasonMarker + schemaParseFailure + strings.Repeat("a", maxDesiredSourceReasonBytes) + "\n",
		"a summary of another":    "PTAH_RUNNER_SUMMARY_V1 {}\n",
		"a log tail with no mark": "fetch failed\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, carried := ParseFetchReason(message); carried {
				t.Fatalf("ParseFetchReason(%q) = %q", message, got)
			}
		})
	}
}
