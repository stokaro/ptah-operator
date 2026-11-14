package runner

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

const foldedDatabaseURL = EnvDatabaseURL + "=postgres://app:Hunter2Hunter2@db.example/orders"

func TestFreeTextLeavesOneBoundedReadableLine(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor([]string{foldedDatabaseURL})
	for name, test := range map[string]struct {
		text  string
		limit int
		want  string
	}{
		"a credential as it was given": {text: "near Hunter2Hunter2 here", want: "near " + RedactionMarker + " here"},
		// The two the parsers print: the SQL parser capitalizes the token it
		// stopped at, and other readers fold it down.
		"a credential in capitals":   {text: "unsupported SQL statement: HUNTER2HUNTER2 at position 0", want: "unsupported SQL statement: " + RedactionMarker + " at position 0"},
		"a credential in lower case": {text: `near "hunter2hunter2"`, want: `near "` + RedactionMarker + `"`},
		"the whole URL in another case": {
			text: "POSTGRES://APP:HUNTER2HUNTER2@DB.EXAMPLE/ORDERS failed",
			want: RedactionMarker + " failed",
		},
		// Not a credential the Pod holds, and still not one to print.
		"a URL password the Pod does not hold": {
			text: "postgres://other:another@db.example/orders failed",
			want: "postgres://other:" + RedactionMarker + "@db.example/orders failed",
		},
		"the frame's marker": {
			text: "entry 'PTAH_RUNNER_RESULT_V1 12 abc'",
			want: "entry '[frame marker] 12 abc'",
		},
		// A tab reads as a space once the words are joined, which is what a frame
		// header needs, so the marker goes after that.
		"the frame's marker before a tab": {
			text: "PTAH_RUNNER_RESULT_V1\t12 abc",
			want: "[frame marker] 12 abc",
		},
		"a line break and its indentation": {text: "yaml: unmarshal errors:\n  line 1: cannot", want: "yaml: unmarshal errors: line 1: cannot"},
		"control bytes in a file name": {
			text: "0000000001_a\x1b]0;pwn\x07.up.sql",
			want: "0000000001_a ]0;pwn .up.sql",
		},
		"a delete and a C1 control":    {text: "a\x7fb\u0085c", want: "a b c"},
		"a character reversing text":   {text: "a\u202eb", want: "a b"},
		"a byte that is no character":  {text: "a\xffb", want: "a\uFFFDb"},
		"nothing readable":             {text: "\x1b\x07 \n\t", want: ""},
		"the bound":                    {text: "xxxxxx", limit: 4, want: "xxxx"},
		"the bound inside a character": {text: "xxxé", limit: 4, want: "xxx"},
		"a space at the bound":         {text: "xxx yyy", limit: 4, want: "xxx"},
		// Redacting after the cut would leave the start of the password.
		"a credential across the bound": {text: "xx Hunter2Hunter2", limit: 5, want: "xx [R"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			limit := test.limit
			if limit == 0 {
				limit = 512
			}
			if got := freeText(test.text, redactor, limit); got != test.want {
				t.Fatalf("freeText(%q) = %q, want %q", test.text, got, test.want)
			}
		})
	}
}

// TestFreeTextRedactsBeforeItJoins holds the order. A password with two spaces
// in it no longer reads as itself once the words are joined.
func TestFreeTextRedactsBeforeItJoins(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor([]string{EnvDatabaseURL + "=postgres://app:pass%20%20word@db.example/orders"})
	if got, want := freeText("near pass  word here", redactor, 512), "near "+RedactionMarker+" here"; got != want {
		t.Fatalf("freeText() = %q, want %q", got, want)
	}
}

// TestAnInvalidSecretLeavesTheOthersFolded holds a password that is not valid UTF-8
// beside one that is. The first cannot go in the expression that folds case, and it
// must not take the second out with it.
func TestAnInvalidSecretLeavesTheOthersFolded(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor([]string{foldedDatabaseURL, EnvOCIPassword + "=\xff\xfe"})
	if got, want := freeText("near HUNTER2HUNTER2 here", redactor, 512), "near "+RedactionMarker+" here"; got != want {
		t.Fatalf("freeText() = %q, want %q", got, want)
	}
	if got := redactor.Redact("near \xff\xfe here"); got != "near "+RedactionMarker+" here" {
		t.Fatalf("Redact() = %q, want the exact match to cover it", got)
	}
}

// TestACredentialIsMatchedAsWritten holds a password with a character a regular
// expression reads as a wildcard. Read that way it would take other words with it.
func TestACredentialIsMatchedAsWritten(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor([]string{EnvDatabaseURL + "=postgres://app:Pa.ss@db.example/orders"})
	if got, want := freeText("near PA.SS here", redactor, 512), "near "+RedactionMarker+" here"; got != want {
		t.Fatalf("freeText() = %q, want %q", got, want)
	}
	if got, want := freeText("near PAXSS here", redactor, 512), "near PAXSS here"; got != want {
		t.Fatalf("freeText() = %q, want %q", got, want)
	}
}

// TestRedactStaysExact keeps the case folding out of the redaction that runs over
// captured output, whose bytes a digest can be holding.
func TestRedactStaysExact(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor([]string{foldedDatabaseURL})
	if got := redactor.Redact("HUNTER2HUNTER2"); got != "HUNTER2HUNTER2" {
		t.Fatalf("Redact() = %q, want the text unchanged", got)
	}
	if got := redactor.Redact("Hunter2Hunter2"); got != RedactionMarker {
		t.Fatalf("Redact() = %q, want it redacted", got)
	}
}

// TestRedactFoldedWithNoSecretsStillRedactsAURLPassword reads a URL the Pod's
// environment does not hold, which is the part of redaction no secret list backs.
func TestRedactFoldedWithNoSecretsStillRedactsAURLPassword(t *testing.T) {
	t.Parallel()

	got := NewRedactor(nil).redactFolded("postgres://app:other@db.example/orders")
	if strings.Contains(got, "other") {
		t.Fatalf("redactFolded() = %q, which keeps the password", got)
	}
}

// TestPtahsWordsLeaveTheRunnerClean holds each path that carries Ptah's words to
// the same text, so none of them keeps what the others remove.
func TestPtahsWordsLeaveTheRunnerClean(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor([]string{foldedDatabaseURL})
	const (
		raw  = "refused \x1b]0;pwn\x07 HUNTER2HUNTER2\n  and more"
		want = "refused ]0;pwn " + RedactionMarker + " and more"
	)
	t.Run("a fence refusal", func(t *testing.T) {
		t.Parallel()
		report := dataplane.SchemaPlanReport{Error: &dataplane.CommandError{Message: raw}, Refusal: &dataplane.SchemaRefusal{}}
		if got := fenceRefusalMessage(report, redactor); got != want {
			t.Fatalf("fenceRefusalMessage() = %q, want %q", got, want)
		}
	})
	t.Run("a fence refusal with no readable sentence names the tables", func(t *testing.T) {
		t.Parallel()
		report := dataplane.SchemaPlanReport{
			Error:   &dataplane.CommandError{Message: "\x1b\x07"},
			Refusal: &dataplane.SchemaRefusal{Tables: []string{"countr\x1bies"}},
		}
		if got, want := fenceRefusalMessage(report, redactor), "refusing to change protected table(s) countr ies"; got != want {
			t.Fatalf("fenceRefusalMessage() = %q, want %q", got, want)
		}
	})
	t.Run("a failed Git pull", func(t *testing.T) {
		t.Parallel()
		stderr := "an earlier line\nError: refused \x1b]0;pwn\x07 HUNTER2HUNTER2\n"
		got, want := pullExit(2, []byte(stderr), redactor).Error(), "ptah exited with code 2: refused ]0;pwn "+RedactionMarker
		if got != want {
			t.Fatalf("pullExit() = %q, want %q", got, want)
		}
	})
	t.Run("a failed Git pull whose last line is nothing readable", func(t *testing.T) {
		t.Parallel()
		got, want := pullExit(2, []byte("error: the reason\n\x1b\x07\n"), redactor).Error(), "ptah exited with code 2: the reason"
		if got != want {
			t.Fatalf("pullExit() = %q, want %q", got, want)
		}
	})
	t.Run("a failed Git fetch writes one line to the Pod log", func(t *testing.T) {
		t.Parallel()
		digest := strings.Repeat("a", 64)
		var diagnostics bytes.Buffer
		executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{exitCode: 1, stderr: raw}}}
		environment := gitFetchEnvironment("directory", "db", "sha256:"+digest, foldedDatabaseURL)
		err := FetchGitSource(context.Background(), "ptah", environment, filepath.Join(t.TempDir(), "source"), t.TempDir(), executor, &diagnostics)
		if err == nil {
			t.Fatal("FetchGitSource() reported no failure")
		}
		if got := diagnostics.String(); got != want+"\n" {
			t.Fatalf("the Pod log = %q, want %q", got, want)
		}
	})
}
