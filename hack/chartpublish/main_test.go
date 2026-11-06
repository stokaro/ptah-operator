package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/hack/chartstamp"
	"github.com/stokaro/ptah-operator/hack/chartstamp/chartstamptest"
)

const (
	image    = "ghcr.io/stokaro/ptah-operator@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	executor = "ghcr.io/stokaro/ptah@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	revision = "1111111111111111111111111111111111111111"
	version  = "0.2.10"
	token    = "registry-token-that-must-not-appear"
	actor    = "release-bot"
	repoPath = "charts/ptah-operator"
)

// fixture is one release: a packaged chart, a manifest that records its stamp, and
// a registry that reads for anyone and writes for one credential.
type fixture struct {
	chart, manifest string
	digest          string
	registry        *chartstamptest.Registry
	stdout          *bytes.Buffer
	outputs         string
	commands        []string
	// answer is what a child program says, by the joined command line.
	answer func(command string, attempt int) (string, error)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	directory := t.TempDir()
	f := &fixture{
		chart:    filepath.Join(directory, "ptah-operator-"+version+".tgz"),
		manifest: filepath.Join(directory, "release-manifest.txt"),
		outputs:  filepath.Join(directory, "outputs"),
		registry: chartstamptest.NewRegistry(t),
		stdout:   &bytes.Buffer{},
		answer: func(string, int) (string, error) {
			return `{"isDraft":false,"isImmutable":true}`, nil
		},
	}
	f.registry.Username, f.registry.Password = actor, token
	if err := os.WriteFile(f.chart, chartstamptest.PackagedChart(t, version), 0o600); err != nil {
		t.Fatal(err)
	}
	f.writeManifest(t, unrecordedManifest)
	if err := f.call(t, "record", "-chart", f.chart); err != nil {
		t.Fatal(err)
	}
	f.digest = strings.TrimPrefix(strings.TrimSpace(f.stdout.String()), chartstamp.Repository+"@")
	f.stdout.Reset()
	return f
}

const unrecordedManifest = "version=" + version + "\nsource-repository=stokaro/ptah-operator\nsource-ref=refs/tags/v" + version + "\n" +
	"source-sha=" + revision + "\nimage=" + image + "\nexecutor=" + executor + "\nexecutor-ptah-version=v0.18.0\n" +
	"chart-asset-sha256=abc\nclient-assets=x\n"

func (f *fixture) writeManifest(t *testing.T, document string) {
	t.Helper()
	if err := os.WriteFile(f.manifest, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
}

// call runs one subcommand the way the workflow does, with a credential in the
// environment, against this fixture's registry.
func (f *fixture) call(t *testing.T, subcommand string, extra ...string) error {
	t.Helper()
	return f.callWithEnv(t, map[string]string{"GH_TOKEN": token, "GITHUB_ACTOR": actor, "GITHUB_OUTPUT": f.outputs}, subcommand, extra...)
}

func (f *fixture) callWithEnv(t *testing.T, env map[string]string, subcommand string, extra ...string) error {
	t.Helper()
	attempts := map[string]int{}
	p := program{
		stdout: f.stdout,
		getenv: func(key string) string { return env[key] },
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			command := name + " " + strings.Join(args, " ")
			f.commands = append(f.commands, command)
			attempts[command]++
			output, err := f.answer(command, attempts[command])
			return []byte(output), err
		},
		wait: func(time.Duration) {},
	}
	args := append([]string{subcommand, "-manifest", f.manifest, "-repository", f.registry.Host() + "/" + repoPath, "-plain-http"}, extra...)
	return p.dispatch(context.Background(), args)
}

func assertError(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want one naming %q", err, want)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the error carries the token: %v", err)
	}
}

func TestRecordAndStamp(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	recorded, err := os.ReadFile(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	reference := chartstamp.Repository + "@" + f.digest
	if !strings.Contains(string(recorded), "chart-asset-sha256=abc\nchart-oci="+reference+"\nclient-assets=x\n") {
		t.Fatalf("the manifest reads:\n%s", recorded)
	}
	assertError(t, f.call(t, "record", "-chart", f.chart), "already records")

	output := t.TempDir()
	assertError(t, f.call(t, "stamp", "-chart", f.chart, "-output", output), "")
	if got := strings.TrimSpace(f.stdout.String()); got != reference {
		t.Errorf("stamp printed %q, want %q", got, reference)
	}
	if _, err := os.Stat(filepath.Join(output, "ptah-operator-"+version+".tgz")); err != nil {
		t.Errorf("the stamped chart was not written: %v", err)
	}
	f.writeManifest(t, strings.Replace(string(recorded), f.digest, "sha256:"+strings.Repeat("0", 64), 1))
	assertError(t, f.call(t, "stamp", "-chart", f.chart), "the chart stamps to")
}

func TestStage(t *testing.T) {
	t.Parallel()
	t.Run("pushes by digest with the token in the environment, and names no tag", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		for range 2 { // A rerun resumes.
			assertError(t, f.call(t, "stage", "-chart", f.chart), "")
		}
		if tags := f.registry.Tags(repoPath); len(tags) != 0 {
			t.Errorf("staging wrote the tags %v", tags)
		}
		outputs, err := os.ReadFile(f.outputs)
		if err != nil {
			t.Fatal(err)
		}
		target := f.registry.Host() + "/" + repoPath
		for _, want := range []string{"repository=" + target + "\n", "digest=" + f.digest + "\n", "reference=" + target + "@" + f.digest + "\n"} {
			if !strings.Contains(string(outputs), want) {
				t.Errorf("the step outputs lack %q:\n%s", want, outputs)
			}
		}
		if strings.Contains(f.stdout.String()+string(outputs), token) {
			t.Error("the token reached the step's output")
		}
	})
	for name, row := range map[string]struct {
		manifest func(string) string
		env      map[string]string
		want     string
	}{
		"a digest the chart does not stamp to": {
			manifest: func(document string) string {
				return strings.Replace(document, "chart-oci="+chartstamp.Repository+"@sha256:", "chart-oci="+chartstamp.Repository+"@sha256:0", 1)
			},
			env:  map[string]string{"GH_TOKEN": token, "GITHUB_ACTOR": actor},
			want: "the chart stamps to",
		},
		"a manifest that records no chart": {
			manifest: func(string) string { return unrecordedManifest },
			env:      map[string]string{"GH_TOKEN": token, "GITHUB_ACTOR": actor},
			want:     "records no chart-oci",
		},
		"no token":         {env: map[string]string{"GITHUB_ACTOR": actor}, want: "credential not found"},
		"a wrong token":    {env: map[string]string{"GH_TOKEN": "other", "GITHUB_ACTOR": actor}, want: "401"},
		"a token, no user": {env: map[string]string{"GH_TOKEN": token}, want: "no registry user"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			if row.manifest != nil {
				document, err := os.ReadFile(f.manifest)
				if err != nil {
					t.Fatal(err)
				}
				f.writeManifest(t, row.manifest(string(document)))
			}
			assertError(t, f.callWithEnv(t, row.env, "stage", "-chart", f.chart), row.want)
			if blobs := f.registry.Blobs(); blobs != 0 {
				t.Errorf("the refused push left %d blobs", blobs)
			}
		})
	}
}

func TestVerify(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		private bool
		staged  bool
		answer  func(string, int) (string, error)
		want    string
	}{
		"a public chart with its signature and attestation": {staged: true},
		"attestations that trail the write": {staged: true, answer: func(command string, attempt int) (string, error) {
			if strings.HasPrefix(command, "gh attestation") && attempt < 3 {
				return "", errors.New("no attestations found")
			}
			return "", nil
		}},
		"a signature that never verifies": {staged: true, want: "no matching signature", answer: func(command string, _ int) (string, error) {
			if strings.HasPrefix(command, "cosign") {
				return "", errors.New("no matching signature")
			}
			return "", nil
		}},
		"an attestation that never verifies": {staged: true, want: "no attestations", answer: func(command string, _ int) (string, error) {
			if strings.HasPrefix(command, "gh attestation") {
				return "", errors.New("no attestations")
			}
			return "", nil
		}},
		// What a first release meets, and the reason this check signs in to nothing.
		"a package still private":            {staged: true, private: true, want: "made public"},
		"a chart the registry does not hold": {want: "without signing in"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			if row.staged {
				assertError(t, f.call(t, "stage", "-chart", f.chart), "")
			}
			f.registry.Private = row.private
			if row.answer != nil {
				f.answer = row.answer
			}
			assertError(t, f.call(t, "verify"), row.want)
			if row.private && len(f.commands) != 0 {
				t.Errorf("it went on to %v after a read it could not make", f.commands)
			}
			if row.want != "" {
				return
			}
			reference := f.registry.Host() + "/" + repoPath + "@" + f.digest
			for _, want := range []string{
				"cosign verify --certificate-identity https://github.com/stokaro/ptah-operator/.github/workflows/release.yml@refs/tags/v" + version +
					" --certificate-oidc-issuer https://token.actions.githubusercontent.com " + reference,
				"gh attestation verify oci://" + reference + " --repo stokaro/ptah-operator --source-ref refs/tags/v" + version +
					" --source-digest " + revision + " --signer-workflow stokaro/ptah-operator/.github/workflows/release.yml",
			} {
				if !slices.Contains(f.commands, want) {
					t.Errorf("it ran %q, and not %q", f.commands, want)
				}
			}
			for _, command := range f.commands {
				if strings.Contains(command, token) {
					t.Errorf("the token reached a command line: %s", command)
				}
			}
		})
	}
}

func TestTag(t *testing.T) {
	t.Parallel()
	const releaseView = "gh release view v" + version + " --repo stokaro/ptah-operator --json isDraft,isImmutable"
	state := func(body string) func(string, int) (string, error) {
		return func(string, int) (string, error) { return body, nil }
	}
	for name, row := range map[string]struct {
		answer  func(string, int) (string, error)
		private bool
		env     map[string]string
		want    string
		tagged  bool
	}{
		"a published immutable release": {tagged: true},
		"a release list that trails the publication": {tagged: true, answer: func(_ string, attempt int) (string, error) {
			if attempt < 3 {
				return "", errors.New("release not found")
			}
			return `{"isDraft":false,"isImmutable":true}`, nil
		}},
		"an immutable draft":       {answer: state(`{"isDraft":true,"isImmutable":true}`), want: "not a published immutable release"},
		"a draft":                  {answer: state(`{"isDraft":true,"isImmutable":false}`), want: "not a published immutable release"},
		"a mutable release":        {answer: state(`{"isDraft":false,"isImmutable":false}`), want: "not a published immutable release"},
		"a state it cannot read":   {answer: state(`not json`), want: "read the state"},
		"a release it cannot find": {answer: func(string, int) (string, error) { return "", errors.New("release not found") }, want: "release not found"},
		"no credential":            {env: map[string]string{}, want: "credential not found"},
		// Tagged, and a reader still cannot have it, so the run has not finished.
		"a package still private": {private: true, tagged: true, want: "without signing in"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			assertError(t, f.call(t, "stage", "-chart", f.chart), "")
			f.registry.Private = row.private
			if row.answer != nil {
				f.answer = row.answer
			}
			env := map[string]string{"GH_TOKEN": token, "GITHUB_ACTOR": actor}
			if row.env != nil {
				env = row.env
			}
			assertError(t, f.callWithEnv(t, env, "tag"), row.want)
			if f.commands[0] != releaseView {
				t.Errorf("it first ran %q", f.commands[0])
			}
			if got := slices.Contains(f.registry.Tags(repoPath), version); got != row.tagged {
				t.Errorf("version tag present: %t, want %t (tags %v)", got, row.tagged, f.registry.Tags(repoPath))
			}
		})
	}

	t.Run("a rerun finds the tag done, and another digest is refused", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		assertError(t, f.call(t, "stage", "-chart", f.chart), "")
		assertError(t, f.call(t, "tag"), "")
		assertError(t, f.call(t, "tag"), "")

		document, err := os.ReadFile(f.manifest)
		if err != nil {
			t.Fatal(err)
		}
		other := strings.ReplaceAll(string(document), "executor-ptah-version=v0.18.0", "executor-ptah-version=v0.19.0")
		other = other[:strings.Index(other, "chart-oci=")] + other[strings.Index(other, "client-assets="):]
		f.writeManifest(t, other)
		assertError(t, f.call(t, "record", "-chart", f.chart), "")
		assertError(t, f.call(t, "stage", "-chart", f.chart), "")
		assertError(t, f.call(t, "tag"), "refusing to move it")
	})
}

func TestDispatchRefusesWhatItCannotRun(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	p := program{stdout: f.stdout, getenv: func(string) string { return "" }}
	for name, args := range map[string][]string{
		"no subcommand":      nil,
		"another subcommand": {"publish", "-manifest", f.manifest},
		"no manifest":        {"tag"},
		"an unknown flag":    {"tag", "-manifest", f.manifest, "-force"},
	} {
		if err := p.dispatch(context.Background(), args); err == nil {
			t.Errorf("%s: dispatch accepted %v", name, args)
		}
	}
	f.writeManifest(t, unrecordedManifest)
	assertError(t, f.call(t, "tag"), "chart-oci")
	assertError(t, f.call(t, "verify"), "chart-oci")
	f.writeManifest(t, strings.Replace(unrecordedManifest, "version=", "release=", 1))
	assertError(t, f.call(t, "stamp", "-chart", f.chart), "no version record")
	assertError(t, f.call(t, "stamp", "-chart", filepath.Join(t.TempDir(), "absent.tgz")), "no version record")
}
