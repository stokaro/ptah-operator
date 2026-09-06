package runner

import (
	"encoding/json"
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

func TestRedactorRemovesStandaloneAndEscapedCredentialsFromDatabaseURLs(t *testing.T) {
	t.Parallel()

	databaseURL := "postgres://app:db%40password@db.example/app?password=query%40password&schema=public"
	token := "registry\"token\\line\n"
	redactor := NewRedactor([]string{
		EnvDatabaseURL + "=" + databaseURL,
		EnvOCIToken + "=" + token,
	})
	escapedToken, err := json.Marshal(token)
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Join([]string{
		"db@password",
		"query@password",
		"postgres://app:db%40password@db.example/app?schema=public&password=query%40password",
		string(escapedToken[1 : len(escapedToken)-1]),
	}, " ")
	got := redactor.Redact(input)
	for _, secret := range []string{"db@password", "db%40password", "query@password", "query%40password", string(escapedToken[1 : len(escapedToken)-1])} {
		if strings.Contains(got, secret) {
			t.Fatalf("Redact() retained credential representation %q in %q", secret, got)
		}
	}
}

func TestRedactorRemovesMySQLNetworkDSNCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		databaseURL string
		secrets     []string
	}{
		{
			name:        "TCP",
			databaseURL: "mysql://app:tcp%40secret%2Fvalue@tcp(db.example:3306)/app?password=query%40secret",
			secrets:     []string{"tcp@secret/value", "tcp%40secret%2Fvalue", "query@secret", "query%40secret"},
		},
		{
			name:        "unix socket",
			databaseURL: "app:unix%20secret@unix(/var/run/mysql.sock)/app?token=query%2Ftoken",
			secrets:     []string{"unix secret", "unix%20secret", "unix+secret", "query/token", "query%2Ftoken"},
		},
		{
			name:        "raw driver password delimiters",
			databaseURL: "app:p?%@/)ss@tcp(db.example:3306)/app?parseTime=true",
			secrets:     []string{"p?%@/)ss", "p%3F%25%40%2F%29ss"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			redactor := NewRedactor([]string{EnvDatabaseURL + "=" + test.databaseURL})
			input := strings.Join(append([]string{test.databaseURL}, test.secrets...), " ")
			got := redactor.Redact(input)
			for _, secret := range append(test.secrets, test.databaseURL) {
				if strings.Contains(got, secret) {
					t.Fatalf("Redact() retained MySQL credential representation %q in %q", secret, got)
				}
			}
			if !strings.Contains(got, RedactionMarker) {
				t.Fatalf("Redact() = %q, want redaction marker", got)
			}
		})
	}
}
