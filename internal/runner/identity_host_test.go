package runner_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcehost"
)

// stokaro/ptah-operator#1084. strings.ToLower reads U+0130 as "i" and the
// Kelvin sign as "k", so each URL below used to give the target identity of
// the ASCII URL beside it while its driver dials another name. A plan, an
// approval or a proof bound to the ASCII host would then pass for this one.
// Each is refused instead, with the host escaped and the xn-- form named, and
// the ASCII URL beside it still gives its identity.
func TestTargetIdentityDigestRefusesAHostOutsidePrintableASCII(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		url  string
		// twin is the URL whose identity this one used to give.
		twin string
		// host is how the refusal prints the host.
		host string
	}{
		{
			name: "U+0130",
			url:  "postgres://app@db.\u0130nternal/accounts",
			twin: "postgres://app@db.internal/accounts",
			host: `"db.\u0130nternal"`,
		},
		{
			name: "U+0130 percent-escaped",
			url:  "postgres://app@db.%C4%B0nternal:5432/accounts",
			twin: "postgres://app@db.internal/accounts",
			host: `"db.\u0130nternal"`,
		},
		{
			name: "the Kelvin sign",
			url:  "postgres://app@\u212Aeys.internal/accounts",
			twin: "postgres://app@keys.internal/accounts",
			host: `"\u212aeys.internal"`,
		},
		{
			name: "U+0130 in the PostgreSQL host override",
			url:  "postgres://app@ignored/accounts?host=db.%C4%B0nternal",
			twin: "postgres://app@ignored/accounts?host=db.internal",
			host: `"db.\u0130nternal"`,
		},
		{
			name: "a Unicode space strings.TrimSpace removes",
			url:  "postgres://app@ignored/accounts?host=db.internal%C2%A0",
			twin: "postgres://app@ignored/accounts?host=db.internal",
			host: `"db.internal\u00a0"`,
		},
		{
			name: "U+0130 in a zone",
			url:  "postgres://app@[fe80::1%25\u0130th0]/accounts",
			twin: "postgres://app@[fe80::1%25ith0]/accounts",
			host: `"fe80::1%\u0130th0"`,
		},
		{
			name: "U+0130 in a MySQL URL",
			url:  "mysql://app@db.\u0130nternal:3307/accounts",
			twin: "mysql://app@db.internal:3307/accounts",
			host: `"db.\u0130nternal"`,
		},
		{
			name: "U+0130 in a MySQL network DSN",
			url:  "mysql://app@tcp(db.\u0130nternal:3306)/accounts",
			twin: "mysql://app@tcp(db.internal:3306)/accounts",
			host: `"db.\u0130nternal"`,
		},
		{
			name: "the Kelvin sign in a network DSN with no scheme",
			url:  "tcp(\u212Aeys.internal)/accounts",
			twin: "tcp(keys.internal)/accounts",
			host: `"\u212aeys.internal"`,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if _, err := runner.TargetIdentityDigest(row.twin); err != nil {
				t.Fatalf("TargetIdentityDigest(%q), the ASCII URL: %v", row.twin, err)
			}
			digest, err := runner.TargetIdentityDigest(row.url)
			if !errors.Is(err, sourcehost.ErrNotASCII) {
				t.Fatalf("TargetIdentityDigest(%q) = %q, %v, want the refusal of a host outside printable ASCII", row.url, digest, err)
			}
			message := err.Error()
			for _, want := range []string{"the database URL: the host " + row.host + " is not printable ASCII", "xn-- form"} {
				if !strings.Contains(message, want) {
					t.Fatalf("the refusal %q does not say %q", message, want)
				}
			}
			if strings.ContainsFunc(message, func(r rune) bool { return r > 0x7e }) {
				t.Fatalf("the refusal %q prints the host unescaped", message)
			}
		})
	}
}

// The check refuses only what lies outside printable ASCII. A host in
// capitals is the same target as in lowercase, in each form a URL can name
// it, and an internationalized name written in its xn-- form is a host of its
// own.
func TestTargetIdentityDigestFoldsTheCaseOfAnASCIIHost(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name        string
		left, right string
		same        bool
	}{
		{name: "a URL", left: "postgres://app@DB.Internal/accounts", right: "postgres://app@db.internal/accounts", same: true},
		{name: "the host override", left: "postgres://app@ignored/accounts?host=DB.INTERNAL", right: "postgres://app@db.internal/accounts", same: true},
		{name: "a MySQL URL", left: "mysql://app@Db.Internal:3307/accounts", right: "mysql://app@db.internal:3307/accounts", same: true},
		{name: "a network DSN", left: "mysql://app@tcp(DB.INTERNAL:3306)/accounts", right: "mysql://app@tcp(db.internal)/accounts", same: true},
		{name: "an xn-- name", left: "postgres://app@XN--BCHER-KVA.example/accounts", right: "postgres://app@xn--bcher-kva.example/accounts", same: true},
		{name: "an xn-- name is not its ASCII look-alike", left: "postgres://app@xn--bcher-kva.example/accounts", right: "postgres://app@bucher.example/accounts"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			left, err := runner.TargetIdentityDigest(row.left)
			if err != nil {
				t.Fatalf("TargetIdentityDigest(%q): %v", row.left, err)
			}
			right, err := runner.TargetIdentityDigest(row.right)
			if err != nil {
				t.Fatalf("TargetIdentityDigest(%q): %v", row.right, err)
			}
			if (left == right) != row.same {
				t.Fatalf("TargetIdentityDigest(%q) = %s and TargetIdentityDigest(%q) = %s, want the same target: %t",
					row.left, left, row.right, right, row.same)
			}
		})
	}
}
