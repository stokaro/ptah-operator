package runnerharness_test

import (
	"path"
	"testing"

	"github.com/stokaro/ptah-operator/test/e2e/httpsfixture"
	"github.com/stokaro/ptah-operator/test/runnerharness"
)

// An HTTPS pull a row writes out, so internal/runner can compare what its
// HTTPSPull builds with what the capture ran. access is the first element of
// the server's path: public, basic, bearer, or a redirect in front of one.
type httpsPullRow struct {
	name, family     string
	access, file     string
	format, selected string
	// digest is the download digest the row pins, the file's own unless set.
	digest string
	// content, when set, is the content digest a fetch holds the files to.
	content string
	refused bool
}

const (
	httpsUser        = "deploy"
	httpsPullScratch = "/fetch-work/ptah-https-fetch-capture"
	httpsPullCAFile  = "/credentials/https-ca/ca.pem"
)

func (row httpsPullRow) args(t *testing.T) []string {
	t.Helper()
	digest := row.digest
	if digest == "" {
		files, err := httpsfixture.Files()
		if err != nil {
			t.Fatal(err)
		}
		digest = httpsfixture.Digest(files[row.file])
	}
	out := httpsPullScratch + "/migrations"
	if row.family == "schema" {
		out = httpsPullScratch + "/" + path.Base(row.selected)
	}
	args := []string{row.family, "pull", "https://" + httpsHost + "/" + row.access + "/" + row.file,
		"--format", row.format, "--path", row.selected, "--expect-download-digest", digest}
	if row.content != "" {
		args = append(args, "--expect-digest", row.content)
	}
	return append(args, "--ca-file", httpsPullCAFile, "--out", out, "--json")
}

// credential is the environment a row's access takes: the run's token as the
// Basic password or as the Bearer token, and nothing for a public file.
func (row httpsPullRow) credential(l *lab) []string {
	switch access := path.Base(row.access); access {
	case "basic":
		return []string{"PTAH_HTTPS_USERNAME=" + httpsUser, "PTAH_HTTPS_PASSWORD=" + l.keys.Token}
	case "bearer":
		return []string{"PTAH_HTTPS_TOKEN=" + l.keys.Token}
	default:
		return nil
	}
}

func httpsPullRows(t *testing.T) []httpsPullRow {
	t.Helper()
	files, err := httpsfixture.Files()
	if err != nil {
		t.Fatal(err)
	}
	schemaFile, err := httpsfixture.SchemaContent(httpsfixture.SchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := httpsfixture.MigrationContent()
	if err != nil {
		t.Fatal(err)
	}
	schemaFileRow := httpsPullRow{
		family: "schema", access: "basic", file: httpsfixture.SchemaFile, format: "file", selected: httpsfixture.SchemaFile,
	}
	migrationRow := httpsPullRow{
		family: "migrations", access: "bearer", file: httpsfixture.MigrationArchive, format: "tar.gz",
		selected: httpsfixture.MigrationPath,
	}
	zipRow := httpsPullRow{
		family: "schema", access: "public", file: httpsfixture.SchemaArchive, format: "zip", selected: httpsfixture.SchemaInArchive,
	}
	named := func(row httpsPullRow, name string, edit func(*httpsPullRow)) httpsPullRow {
		row.name = name
		if edit != nil {
			edit(&row)
		}
		return row
	}
	return []httpsPullRow{
		// What a Resolve runs: the download held to its digest, and the
		// content digest reported.
		named(schemaFileRow, "schema-file-basic", nil),
		named(migrationRow, "migrations-tar-gzip-bearer", nil),
		named(zipRow, "schema-zip-public", nil),
		named(schemaFileRow, "schema-file-basic-same-origin-redirect", func(r *httpsPullRow) { r.access = "same/basic" }),
		// What the fetch before an operation runs: held to both digests.
		named(schemaFileRow, "schema-file-basic-fetch", func(r *httpsPullRow) { r.content = schemaFile }),
		named(migrationRow, "migrations-tar-gzip-bearer-fetch", func(r *httpsPullRow) { r.content = migrations }),
		// The refusals the acceptance phase reaches.
		named(zipRow, "refused-download-digest", func(r *httpsPullRow) {
			r.digest, r.refused = httpsfixture.Digest(files[httpsfixture.SchemaArchiveRepublished]), true
		}),
		named(schemaFileRow, "refused-content-digest", func(r *httpsPullRow) { r.content, r.refused = migrations, true }),
		named(schemaFileRow, "refused-redirect-other-origin", func(r *httpsPullRow) { r.access, r.refused = "away/basic", true }),
		named(migrationRow, "refused-dotdot-entry", func(r *httpsPullRow) { r.file, r.refused = httpsfixture.DotDotArchive, true }),
		named(migrationRow, "refused-symlink", func(r *httpsPullRow) { r.file, r.refused = httpsfixture.SymlinkArchive, true }),
	}
}

func runHTTPSPull(t *testing.T, l *lab, row httpsPullRow) {
	t.Helper()
	args := row.args(t)
	stdout, stderr, exit := l.pull(t, args, row.credential(l))
	name := "https-pull/" + row.name
	if row.refused != (exit != 0) {
		t.Errorf("%s: exit %d, refused=%v\nstdout: %s\nstderr: %s", name, exit, row.refused, stdout, stderr)
	}
	if err := runnerharness.DecodeDocument(args, exit, stdout); err != nil {
		t.Errorf("%s: %v\n%s", name, err, stdout)
	}
	child := runnerharness.Child{Command: runnerharness.CommandWords(args), Args: args, Exit: exit, Stderr: string(stderr)}
	if len(stdout) != 0 {
		child.Stdout = name + "/stdout"
		l.addDocument(t, name, child.Stdout, stdout)
	}
	l.addRow(runnerharness.Row{Name: name, Children: []runnerharness.Child{child}})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts.Rows++
	l.counts.Commands[child.Command]++
}
