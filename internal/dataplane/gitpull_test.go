package dataplane_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

const (
	gitReference = "git+https://git.example.com/acme/app.git"
	gitCommit    = "3f1c2b0a9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a"
)

func gitPullReport(t *testing.T) (string, string) {
	t.Helper()
	data := []byte("CREATE TABLE orders (id int);\n")
	digest, err := sourcecontent.Digest([]sourcecontent.File{{Path: "0001_create.up.sql", Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf(`{"source":"git","reference":%q,"commit":%q,"path":"db/migrations","content_digest":%q,`+
		`"files":[{"path":"0001_create.up.sql","size":%d,"sha256":%q}],"written":["/work/migrations"]}`,
		gitReference, gitCommit, digest, len(data), hex.EncodeToString(sum[:])), digest
}

func TestDecodeGitPullHoldsTheReportToItsPull(t *testing.T) {
	t.Parallel()
	report, digest := gitPullReport(t)
	decoded, err := dataplane.DecodeGitPull([]byte(report), gitReference, "db/migrations")
	if err != nil || decoded.Commit != gitCommit || decoded.ContentDigest != digest {
		t.Fatalf("DecodeGitPull() = %#v, %v", decoded, err)
	}
	for name, mutate := range map[string]func(string) string{
		"an OCI report":           func(r string) string { return strings.Replace(r, `"source":"git"`, `"source":"oci"`, 1) },
		"another repository":      func(r string) string { return strings.Replace(r, "acme/app", "acme/other", 1) },
		"another path":            func(r string) string { return strings.Replace(r, `"path":"db/migrations"`, `"path":"db"`, 1) },
		"an abbreviated commit":   func(r string) string { return strings.Replace(r, gitCommit, gitCommit[:12], 1) },
		"no files":                func(r string) string { return r[:strings.Index(r, `"files"`)] + `"files":[],"written":[]}` },
		"a digest the files miss": func(r string) string { return strings.Replace(r, digest, "sha256:"+strings.Repeat("0", 64), 1) },
		"a file's other size":     func(r string) string { return strings.Replace(r, `"size":30`, `"size":31`, 1) },
		"two documents":           func(r string) string { return r + r },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mutated := mutate(report)
			if mutated == report {
				t.Fatal("the mutation changed nothing")
			}
			if _, err := dataplane.DecodeGitPull([]byte(mutated), gitReference, "db/migrations"); err == nil {
				t.Fatalf("DecodeGitPull accepted %s", mutated)
			}
		})
	}
}
