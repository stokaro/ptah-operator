package runner_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"testing"

	"github.com/stokaro/ptah-operator/internal/runner"
)

var updateTargetIdentities = flag.Bool("update", false, "rewrite testdata/target-identities.json from what TargetIdentityDigest computes now")

const targetIdentityGolden = "testdata/target-identities.json"

// goldenTargetIdentity is one URL and the target identity it gives.
type goldenTargetIdentity struct {
	URL    string `json:"url"`
	Digest string `json:"digest"`
}

// targetIdentityURLs are URLs whose target identity a stored plan, approval or
// unresolved run may already hold. A change that moves one of them leaves
// every such record naming a target its own URL no longer gives: the plan is
// refused at Apply, the approval binds nothing, and a proof stops as a read of
// another target. So each is pinned to the digest it gives, and a change to
// that digest arrives as a diff of testdata/target-identities.json.
//
// The set covers both engines, every scheme alias, names in capitals, IPv4,
// IPv6 compressed, expanded, IPv4-mapped and with a zone, default, explicit
// and zero-padded ports, the PostgreSQL host and port overrides, unix sockets
// in both engines' forms, the MySQL network DSN with and without its scheme,
// a percent sign the MySQL driver dials as written, and bytes outside ASCII
// where the identity binds them exactly: a socket path, a database name, a
// username and a session parameter.
//
// After an intended change, rewrite the file and review its diff:
//
//	go test ./internal/runner -run TestTargetIdentityDigestKeepsEveryPinnedIdentity -update
var targetIdentityURLs = []string{
	"postgres://app:secret@db.example/accounts",
	"postgres://app:secret@DB.Example:5432/accounts",
	"postgres://app@db.example:5433/accounts",
	"postgres://app@db.example:05433/accounts",
	"postgresql://app@db.example/accounts",
	"pgx://app@db.example/accounts",
	"POSTGRES://app@db.example/accounts",
	"postgres://app@db.example./accounts",
	"postgres://app@db_primary.example/accounts",
	"postgres://app@localhost/accounts",
	"postgres://app@127.0.0.1/accounts",
	"postgres://app@010.0.0.1/accounts",
	"postgres://app@[2001:db8::1]/accounts",
	"postgres://app@[2001:0DB8:0:0:0:0:0:1]:5432/accounts",
	"postgres://app@[2001:db8::1]:6432/accounts",
	"postgres://app@[::ffff:192.0.2.1]/accounts",
	"postgres://app@[fe80::1%25eth0]/accounts",
	"postgres://app@[FE80::1%25ETH0]:5432/accounts",
	"postgres://app@ignored/accounts?host=DB.Example&port=6543",
	"postgres://app@ignored/accounts?host=%20db.example%20",
	"postgres://app@ignored/accounts?host=%5B2001:db8::1%5D",
	"postgres://app@ignored/accounts?host=2001:DB8::1",
	"postgres://app@ignored/accounts?host=%2Fvar%2Frun%2Fpostgresql",
	"postgres://app@ignored/accounts?host=%2Fvar%2Frun%2Fpostgresql&port=5433",
	"postgres://app@ignored/accounts?host=%2FVar%2FRun%2FPG",
	"postgres://app@ignored/accounts?host=%2Fvar%2Frun%2F%C4%B0nternal",
	"postgres:///accounts?host=%2Ftmp",
	"postgres://app@db.example/%C4%B0nventory",
	"postgres://%C4%B0nventory@db.example/accounts",
	"postgres://app@db.example/accounts?application_name=%C4%B0nventory",
	"mysql://app@db.example/accounts",
	"mysql://app@DB.Example:3307/accounts",
	"mariadb://app@db.example:3306/accounts",
	"mysql://app@127.0.0.1:3306/accounts",
	"mysql://app@[2001:db8::1]:3307/accounts",
	"mysql://app:secret@tcp(DB.EXAMPLE:3306)/accounts",
	"mariadb://app@tcp(db.example:3307)/accounts",
	"tcp(DB.EXAMPLE)/accounts",
	"app@tcp(db.example:03306)/accounts",
	"mysql://app@tcp([2001:DB8::1]:3307)/accounts",
	"tcp(::1)/accounts",
	"mysql://app@tcp([fe80::1%eth0]:3306)/accounts",
	"mysql://app@tcp(db.%C4%B0nternal:3306)/accounts",
	"mysql://app@unix(/var/run/mysqld/mysqld.sock)/accounts",
	"unix(/var/run/mysqld/\u0130nternal.sock)/accounts",
	"mysql://app@tcp(db.example)/%C4%B0nventory",
}

// TestTargetIdentityDigestKeepsEveryPinnedIdentity holds each URL above to the
// target identity it gave when it was pinned, and the file to exactly those
// URLs in that order, so an empty or stale file cannot pass.
func TestTargetIdentityDigestKeepsEveryPinnedIdentity(t *testing.T) {
	t.Parallel()
	got := make([]goldenTargetIdentity, 0, len(targetIdentityURLs))
	for _, databaseURL := range targetIdentityURLs {
		digest, err := runner.TargetIdentityDigest(databaseURL)
		if err != nil {
			t.Fatalf("TargetIdentityDigest(%q): %v", databaseURL, err)
		}
		got = append(got, goldenTargetIdentity{URL: databaseURL, Digest: digest})
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(got); err != nil {
		t.Fatal(err)
	}
	encoded := buffer.Bytes()
	if *updateTargetIdentities {
		if err := os.WriteFile(targetIdentityGolden, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	content, err := os.ReadFile(targetIdentityGolden)
	if err != nil {
		t.Fatalf("read the pinned identities: %v", err)
	}
	var want []goldenTargetIdentity
	if err := json.Unmarshal(content, &want); err != nil {
		t.Fatalf("decode %s: %v", targetIdentityGolden, err)
	}
	if len(want) != len(targetIdentityURLs) {
		t.Fatalf("%s pins %d identities, want one for each of the %d URLs", targetIdentityGolden, len(want), len(targetIdentityURLs))
	}
	for i, pinned := range want {
		if pinned.URL != got[i].URL {
			t.Fatalf("%s entry %d is %q, want %q", targetIdentityGolden, i, pinned.URL, got[i].URL)
		}
		if pinned.Digest != got[i].Digest {
			t.Errorf("TargetIdentityDigest(%q) = %s, pinned %s", pinned.URL, got[i].Digest, pinned.Digest)
		}
	}
	if !bytes.Equal(content, encoded) {
		t.Errorf("%s is not what -update writes; rerun with -update and review the diff", targetIdentityGolden)
	}
}
