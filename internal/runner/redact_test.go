package runner

import (
	"strings"
	"testing"
)

func TestRedactorRemovesExactValuesAndURLPasswords(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor([]string{
		"PTAH_DB_URL=postgres://app:database-secret@db.example/app",
		"PTAH_OCI_TOKEN=registry-secret",
	})
	input := "postgres://app:database-secret@db.example/app registry-secret https://alice:url-secret@example.net/path"
	got := redactor.Redact(input)
	for _, secret := range []string{"database-secret", "registry-secret", "url-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("Redact() = %q, contains %q", got, secret)
		}
	}
}

func TestRedactorRemovesSecretPrefixAtCaptureBoundary(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor([]string{"PTAH_OCI_TOKEN=supersecret"})
	got := redactor.RedactCaptured("diagnostic: super", true)
	if strings.Contains(got, "super") || !strings.Contains(got, RedactionMarker) {
		t.Fatalf("RedactCaptured() = %q", got)
	}

	got = redactor.RedactCaptured("diagnostic: https://alice:partial-password", true)
	if strings.Contains(got, "partial-password") || !strings.Contains(got, RedactionMarker) {
		t.Fatalf("RedactCaptured(truncated URL) = %q", got)
	}
}

func TestSanitizedCapturedTextRemainsBoundedWhenMarkersExpand(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor([]string{"PTAH_OCI_TOKEN=x"})
	got, dropped := sanitizedCapturedText([]byte("xxxx"), redactor, false, 4)
	if len(got) > 4 || strings.Contains(got, "x") || dropped == 0 {
		t.Fatalf("sanitizedCapturedText() = %q, dropped %d", got, dropped)
	}
}
