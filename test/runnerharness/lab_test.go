package runnerharness_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/planseal"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/e2e/gitfixture"
	"github.com/stokaro/ptah-operator/test/e2e/gitrepo"
	"github.com/stokaro/ptah-operator/test/runnerharness"
	"github.com/stokaro/ptah-operator/test/runnerharness/recording"
	"github.com/stokaro/ptah-operator/test/runnerharness/volumes"
)

const (
	repositoryRoot = "../.."

	gitHost = "e2e-git.ptah-test.svc"

	postgresHost = "harness-postgres"
	mysqlHost    = "harness-mysql"
	registryHost = "harness-registry:5000"

	missingHost = "harness-no-such-host.invalid"

	dockerTimeout = 5 * time.Minute
)

// docker runs the docker client against one named context.
type docker struct {
	context string
}

// run returns standard output, standard error and the exit status, and an error
// only when the command could not run or timed out. A value in env reaches the
// client's environment, so a container's --env NAME finds it without the value
// being on a command line.
//
// A host with many other clients drops SSH connections while they are being set
// up, and `docker run` opens several. A command that lost one of them tries again
// when it ran nothing: a container that was created and never started is removed,
// and one that started is a failure, since running it again could apply twice.
func (d docker) run(ctx context.Context, stdin []byte, env []string, args ...string) (stdout, stderr []byte, exit int, err error) {
	for attempt := range 5 {
		stdout, stderr, exit, err = d.runOnce(ctx, stdin, env, args...)
		if err != nil || exit == 0 || !bytes.Contains(stderr, []byte("kex_exchange_identification")) || !d.ranNothing(ctx, args) {
			return stdout, stderr, exit, err
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return stdout, stderr, exit, err
}

// ranNothing reads what became of a `docker run` that lost its connection, and
// clears the way for another try.
func (d docker) ranNothing(ctx context.Context, args []string) bool {
	flag := slices.Index(args, "--name")
	if args[0] != "run" || flag < 0 || flag+1 >= len(args) {
		return true
	}
	name := args[flag+1]
	state, _, exit, err := d.runOnce(ctx, nil, nil, "inspect", "--format", "{{.State.Status}}", name)
	switch {
	case err != nil:
		return false
	case exit != 0:
		return true
	case strings.TrimSpace(string(state)) != "created":
		return false
	}
	_, _, exit, err = d.runOnce(ctx, nil, nil, "rm", "--force", name)
	return err == nil && exit == 0
}

func (d docker) runOnce(ctx context.Context, stdin []byte, env []string, args ...string) (stdout, stderr []byte, exit int, err error) {
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", append([]string{"--context", d.context}, args...)...) // #nosec G204 -- the harness's own command
	command.Env = append(os.Environ(), env...)
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	var outBuffer, errBuffer bytes.Buffer
	command.Stdout, command.Stderr = &outBuffer, &errBuffer
	err = command.Run()
	var exitError *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return nil, nil, -1, fmt.Errorf("docker %s timed out after %s: %s", args[0], dockerTimeout, errBuffer.Bytes())
	case errors.As(err, &exitError):
		return outBuffer.Bytes(), errBuffer.Bytes(), exitError.ExitCode(), nil
	case err != nil:
		return nil, nil, -1, err
	}
	return outBuffer.Bytes(), errBuffer.Bytes(), 0, nil
}

// must runs a command that has to succeed and returns its standard output.
func (d docker) must(t *testing.T, env []string, args ...string) string {
	t.Helper()
	stdout, stderr, exit, err := d.run(context.Background(), nil, env, args...)
	if err != nil {
		t.Fatalf("docker %s: %v", strings.Join(args, " "), err)
	}
	if exit != 0 {
		t.Fatalf("docker %s exited %d: %s", strings.Join(args, " "), exit, stderr)
	}
	return strings.TrimSpace(string(stdout))
}

// quiet runs a removal, which may find nothing to remove.
func (d docker) quiet(t *testing.T, args ...string) {
	t.Helper()
	if _, _, _, err := d.run(context.Background(), nil, nil, args...); err != nil {
		t.Logf("docker %v: %v", args, err)
	}
}

// lab is everything a run stands up on one Docker host: one network, the
// databases, the registry, the Git server and the volumes every Pod shares.
type lab struct {
	docker  docker
	prefix  string
	network string
	images  labImages

	databasePassword string
	keys             gitfixture.Keys
	seal             planseal.KeyPair
	secrets          []string

	// masker masks everything the run records, and counts what each rule did.
	masker *runnerharness.Masker

	// pulled names the images the host did not hold, which go with the run.
	pulled []string

	sequence atomic.Int64
	// slots bounds the containers running at once.
	slots chan struct{}

	shapeOnce sync.Once
	shape     runnerharness.Container

	mu       sync.Mutex
	rows     []runnerharness.Row
	docs     map[string][]byte
	peaks    map[string]int64
	counts   runnerharness.Ran
	failures int
}

type labImages struct {
	executor, postgres, mysql, registry, tools string
}

func (l *lab) name(part string) string { return l.prefix + "-" + part }

// pinnedExecutor reads the image and commit support/ptah.json pins for edge.
func pinnedExecutor(t *testing.T) (image, commit, release string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot, "support", "ptah.json"))
	if err != nil {
		t.Fatal(err)
	}
	image, commit, release, err = runnerharness.PinnedExecutor(data)
	if err != nil {
		t.Fatal(err)
	}
	return image, commit, release
}

// drivenImages reads the database and registry images the lifecycle driver pins,
// so the harness runs what acceptance runs and holds no second copy of a digest.
func drivenImages(t *testing.T) (postgres, mysql, registry string) {
	t.Helper()
	driver, err := os.ReadFile(filepath.Join(repositoryRoot, "hack", "e2e-kind.sh"))
	if err != nil {
		t.Fatal(err)
	}
	postgres, mysql, registry, err = runnerharness.DrivenImages(driver)
	if err != nil {
		t.Fatal(err)
	}
	return postgres, mysql, registry
}

func randomHex(t *testing.T, size int) string {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(data)
}

// startLab stands up the lab, and removes everything it made when the test ends,
// whatever the outcome. Everything is named under one prefix, and removed by
// name. A run that dies before its cleanup is cleared by its janitor.
func startLab(t *testing.T, gate runnerharness.Gate) *lab {
	t.Helper()
	executor, commit, _ := pinnedExecutor(t)
	if gate.Image != "" {
		executor, commit = gate.Image, ""
	}
	postgres, mysql, registry := drivenImages(t)
	prefix := gate.Prefix
	if prefix == "" {
		prefix = "ptah-runner-harness"
	}
	// Random even under a chosen prefix: the janitor of an earlier run with the
	// same prefix may still be clearing it.
	prefix += "-" + randomHex(t, 4)
	keys, err := gitfixture.NewKeys(gitHost, gitHost, gitHost+".cluster.local")
	if err != nil {
		t.Fatal(err)
	}
	seal, err := planseal.Generate()
	if err != nil {
		t.Fatal(err)
	}
	// Each `docker run` over SSH opens several connections, and a shared host
	// drops them past a limit it does not say.
	slots := 8
	if gate.Context != "default" {
		slots = 4
	}
	l := &lab{
		docker: docker{context: gate.Context}, prefix: prefix,
		network: prefix + "-net",
		images:  labImages{executor: executor, postgres: postgres, mysql: mysql, registry: registry},
		keys:    keys, seal: seal,
		databasePassword: randomHex(t, 16),
		slots:            make(chan struct{}, slots),
		masker:           runnerharness.NewMasker(),
		docs:             map[string][]byte{},
		peaks:            map[string]int64{},
		counts:           runnerharness.Ran{Operations: map[runnerharness.OperationCase]int{}, Commands: map[string]int{}},
	}
	l.secrets = []string{l.databasePassword, keys.Token, "PRIVATE KEY"}
	for _, block := range [][]byte{keys.KeyPEM, keys.HostKeyPEM, keys.ClientKeyPEM} {
		for line := range strings.SplitSeq(string(block), "\n") {
			if line != "" && !strings.HasPrefix(line, "-----") {
				l.secrets = append(l.secrets, line)
			}
		}
	}
	architecture := l.docker.must(t, nil, "version", "--format", "{{.Server.Arch}}")
	l.docker.sweepExpired(t.Logf, time.Now())

	start := time.Now()
	l.pulled = l.missingImages(t, executor, postgres, mysql, registry)
	work := l.startCleanup(t)
	var pulls sync.WaitGroup
	for _, image := range l.pulled {
		pulls.Go(func() { l.pullImage(t, image) })
	}
	pulls.Wait()
	if t.Failed() {
		t.FailNow()
	}
	t.Logf("images ready in %s", time.Since(start).Round(time.Millisecond))
	if commit != "" {
		version := l.docker.must(t, nil, "run", "--rm", "--pull", "never", "--name", l.name("version"), "--network", "none", executor, "version")
		if !strings.Contains(version, "Commit: "+commit) {
			t.Fatalf("the image says %q, and the catalog pins commit %s", version, commit)
		}
	}
	l.images.tools = l.buildTools(t, architecture, work)
	t.Logf("tools built after %s", time.Since(start).Round(time.Millisecond))

	// A new volume mounted over a directory the image holds starts with that
	// directory's files, owners and modes.
	l.docker.must(t, nil, "run", "--rm", "--pull", "never", "--name", l.name("seed"), "--network", "none", "--user", "0:0",
		"--mount", "type=volume,src="+l.name("runner")+",dst=/runner",
		"--mount", "type=volume,src="+l.name("fixtures")+",dst=/fixtures",
		"--mount", "type=volume,src="+l.name("credentials")+",dst=/credentials",
		"--mount", "type=volume,src="+l.name("tls")+",dst=/tls",
		"--mount", "type=volume,src="+l.name("desired-source")+",dst=/desired-source",
		"--entrypoint", "/recorder", l.images.tools, "--prepare-volumes", "/runner")

	l.startDatabases(t)
	l.startRegistry(t)
	l.serveGit(t, "main.json")
	t.Logf("lab ready after %s", time.Since(start).Round(time.Millisecond))
	return l
}

// missingImages names the images the host does not hold. One the run pulls is
// removed with the run, so a shared host keeps what it held and nothing more.
func (l *lab) missingImages(t *testing.T, images ...string) []string {
	t.Helper()
	var missing []string
	for _, image := range images {
		_, _, exit, err := l.docker.run(context.Background(), nil, nil, "image", "inspect", image)
		if err != nil {
			t.Fatalf("look for %s: %v", image, err)
		}
		if exit != 0 {
			missing = append(missing, image)
		}
	}
	return missing
}

func (l *lab) pullImage(t *testing.T, image string) {
	t.Helper()
	if _, stderr, exit, err := l.docker.run(context.Background(), nil, nil, "pull", "--quiet", image); err != nil || exit != 0 {
		t.Errorf("pull %s: exit %d, %v: %s", image, exit, err, stderr)
	}
}

// startCleanup arranges for everything the run is about to make to go away, and
// creates the first of it: the directory the tools image is built in, which it
// returns, and the network, which records the run for whoever has to clear it.
// The network goes last, so a run that is cleared halfway can still be found.
func (l *lab) startCleanup(t *testing.T) string {
	t.Helper()
	work, err := os.MkdirTemp("", runWorkPattern(l.prefix))
	if err != nil {
		t.Fatal(err)
	}
	janitor, err := startJanitor(janitorOrder{
		Context: l.docker.context, Prefix: l.prefix, Pulled: l.pulled, Work: work,
		Settle: dockerTimeout, Interval: janitorInterval,
	})
	if err != nil {
		t.Fatal(errors.Join(err, os.RemoveAll(work)))
	}
	t.Cleanup(func() {
		err := errors.Join(os.RemoveAll(work), l.docker.removeRunObjects(l.prefix))
		if err == nil {
			err = l.docker.removeRunRecord(l.prefix, l.pulled)
		}
		if err != nil {
			t.Logf("this run's janitor takes over: %v", err)
			return
		}
		if err := janitor.dismiss(); err != nil {
			t.Logf("dismiss this run's janitor: %v", err)
		}
	})
	record, err := runnerharness.RunRecord{Prefix: l.prefix, Expires: runExpiry(t), Pulled: l.pulled}.Label()
	if err != nil {
		t.Fatal(err)
	}
	l.docker.must(t, nil, "network", "create", "--internal", "--label", runnerharness.RunLabel+"="+record, l.network)
	return work
}

func writeTree(t *testing.T, file string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil { // #nosec G301 -- an image's build context
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode passes through the umask, and the containers' user is
	// not the file's owner.
	if err := os.Chmod(file, mode); err != nil { // #nosec G302 -- an image's build context
		t.Fatal(err)
	}
}

// copyTree copies a fixture directory into the build context, world readable.
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	if err := filepath.WalkDir(from, func(file string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(from, file)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(file) // #nosec G304 -- the repository's own fixtures
		if err != nil {
			return err
		}
		writeTree(t, filepath.Join(to, relative), data, 0o444)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// controlBytesDirectory holds a migration whose file name sets a terminal's title,
// which `ptah migrations push` and `pull` carry through a registry. The name is
// written here rather than committed, so that no checkout holds it.
const controlBytesDirectory = "migrations-control-bytes"

func writeControlByteMigrations(t *testing.T, directory string) {
	t.Helper()
	const name = "0000000001_a\x1b]0;pwn\x07"
	writeTree(t, filepath.Join(directory, name+".up.sql"), []byte("-- +ptah no_transaction=sometimes\nCREATE TABLE a (id bigint);\n"), 0o444)
	writeTree(t, filepath.Join(directory, name+".down.sql"), []byte("DROP TABLE a;\n"), 0o444)
}

// buildTools builds the one image the lab adds: the recorder, the Git server,
// the fixture sources and the keys generated for this run. It holds no shell.
func (l *lab) buildTools(t *testing.T, architecture, work string) string {
	t.Helper()
	for binary, source := range map[string]string{
		"recorder": "./recorder", "runner/ptah-runner": "./recorder", "e2e-git-server": "../e2e/gitserver",
		"unparsable-schema": "../../hack/unparsableschemafixture",
	} {
		output := filepath.Join(work, binary)
		build := exec.CommandContext(t.Context(), "go", "build", "-o", output, source) // #nosec G204 -- the harness's own command
		build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+architecture, "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v: %s", source, err, out)
		}
	}
	fixtures := filepath.Join(work, "fixtures")
	sources := filepath.Join("testdata", "sources")
	copyTree(t, sources, fixtures)
	copyTree(t, filepath.Join(sources, "desired-source"), filepath.Join(work, "desired-source"))
	writeControlByteMigrations(t, filepath.Join(work, "desired-source", controlBytesDirectory))
	e2eFixtures := filepath.Join(repositoryRoot, "testdata", "e2e")
	for _, name := range []string{
		"postgresql-v1.sql", "postgresql-v2.sql", "mysql-v1.sql", "mysql-v2.sql", "extension-postgresql-v1.sql",
		"verification-policy-digest-pin.yaml", "verification-policy-migrations.yaml",
	} {
		data, err := os.ReadFile(filepath.Join(e2eFixtures, name))
		if err != nil {
			t.Fatal(err)
		}
		writeTree(t, filepath.Join(fixtures, name), data, 0o444)
	}
	copyTree(t, filepath.Join(e2eFixtures, "reference", "v1"), filepath.Join(fixtures, "reference"))
	copyTree(t, filepath.Join(e2eFixtures, "migrations", "postgresql"), filepath.Join(fixtures, "migrations-postgresql"))
	copyTree(t, filepath.Join(e2eFixtures, "migrations", "mysql"), filepath.Join(fixtures, "migrations-mysql"))
	copyTree(t, filepath.Join(e2eFixtures, "migrations", "postgresql-checkpoint"), filepath.Join(fixtures, "migrations-checkpoint-postgresql"))
	copyTree(t, filepath.Join(e2eFixtures, "migrations", "postgresql-stopped"), filepath.Join(fixtures, "migrations-stopped-postgresql"))

	for name, content := range map[string][]byte{
		"keys/tls.crt":        l.keys.CertificatePEM,
		"keys/tls.key":        l.keys.KeyPEM,
		"keys/token":          []byte(l.keys.Token),
		"keys/ssh_host_key":   l.keys.HostKeyPEM,
		"keys/authorized_key": l.keys.AuthorizedKey,
		// The paths the fetch container's credentials and trust have in a Job,
		// and the known hosts that pin another key.
		"credentials/git/ssh-key":                       l.keys.ClientKeyPEM,
		"credentials/git-ca/ca.pem":                     l.keys.CAPEM,
		"credentials/git-known-hosts/known_hosts":       l.keys.KnownHosts,
		"credentials/git-known-hosts-other/known_hosts": l.keys.OtherKnownHosts,
	} {
		writeTree(t, filepath.Join(work, name), content, 0o444)
	}
	for name, spec := range map[string]func() (string, error){
		"spec/main.json":      func() (string, error) { return gitfixture.SpecDocument(gitfixture.RepositorySpec(2)) },
		"spec/rewritten.json": func() (string, error) { return gitfixture.SpecDocument(gitfixture.RewrittenSpec()) },
	} {
		document, err := spec()
		if err != nil {
			t.Fatal(err)
		}
		writeTree(t, filepath.Join(work, name), []byte(document), 0o444)
	}
	certificate, key := selfSignedWithoutNames(t)
	writeTree(t, filepath.Join(work, "tls", "server.crt"), certificate, 0o444)
	// PostgreSQL refuses a key anyone but its owner and group can read.
	writeTree(t, filepath.Join(work, "tls", "server.key"), key, 0o640)
	writeTree(t, filepath.Join(work, "Dockerfile"), []byte(`FROM scratch
COPY recorder /recorder
COPY runner/ /runner/
COPY e2e-git-server /e2e-git-server
COPY unparsable-schema /unparsable-schema
COPY fixtures/ /fixtures/
COPY spec/ /spec/
COPY keys/ /keys/
COPY credentials/ /credentials/
COPY --chown=0:70 tls/ /tls/
COPY desired-source/ /desired-source/
`), 0o644)
	tag := l.prefix + "-tools:run"
	l.docker.must(t, nil, "build", "--quiet", "--tag", tag, work)
	return tag
}

// selfSignedWithoutNames is a certificate for the TLS fault: self-signed and
// naming no host, so a client that verifies refuses it.
func selfSignedWithoutNames(t *testing.T) (certificate, key []byte) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: postgresHost},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, private.Public(), private)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func (l *lab) startDatabases(t *testing.T) {
	t.Helper()
	l.docker.must(t, []string{"POSTGRES_PASSWORD=" + l.databasePassword}, "run", "--detach", "--pull", "never",
		"--name", l.name("postgres"), "--network", l.network, "--network-alias", postgresHost,
		"--env", "POSTGRES_PASSWORD",
		// Most rows make a database, and CREATE DATABASE also writes its
		// template to the WAL, which stays until a checkpoint, so the space a
		// run takes grows with its rows.
		"--tmpfs", "/var/lib/postgresql/data:rw,size=1g",
		"--mount", "type=volume,src="+l.name("tls")+",dst=/tls,readonly",
		l.images.postgres,
		"-c", "ssl=on", "-c", "ssl_cert_file=/tls/server.crt", "-c", "ssl_key_file=/tls/server.key",
		"-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off")
	l.docker.must(t, []string{"MYSQL_ROOT_PASSWORD=" + l.databasePassword}, "run", "--detach", "--pull", "never",
		"--name", l.name("mysql"), "--network", l.network, "--network-alias", mysqlHost,
		"--env", "MYSQL_ROOT_PASSWORD",
		"--tmpfs", "/var/lib/mysql:rw,size=1g,uid=999,gid=999",
		l.images.mysql,
		"--innodb-doublewrite=0", "--sync-binlog=0", "--innodb-flush-log-at-trx-commit=0")
	var ready sync.WaitGroup
	for _, probe := range []struct {
		name string
		args []string
		env  []string
	}{
		{l.name("postgres"), []string{"pg_isready", "-U", "postgres", "-h", "127.0.0.1"}, nil},
		{l.name("mysql"), []string{"mysql", "-h127.0.0.1", "-uroot", "-e", "SELECT 1"}, []string{"MYSQL_PWD=" + l.databasePassword}},
	} {
		ready.Go(func() {
			deadline := time.Now().Add(3 * time.Minute)
			for time.Now().Before(deadline) {
				args := []string{"exec"}
				for _, variable := range probe.env {
					name, _, _ := strings.Cut(variable, "=")
					args = append(args, "--env", name)
				}
				args = append(args, probe.name)
				_, _, exit, err := l.docker.run(context.Background(), nil, probe.env, append(args, probe.args...)...)
				if err == nil && exit == 0 {
					return
				}
				time.Sleep(time.Second)
			}
			logs, stderr, _, _ := l.docker.run(context.Background(), nil, nil, "logs", "--tail", "40", probe.name)
			t.Errorf("%s did not become ready: %s%s", probe.name, logs, stderr)
		})
	}
	ready.Wait()
	if t.Failed() {
		t.FailNow()
	}
}

func (l *lab) startRegistry(t *testing.T) {
	t.Helper()
	l.docker.must(t, nil, "run", "--detach", "--pull", "never",
		"--name", l.name("registry"), "--network", l.network, "--network-alias", strings.Split(registryHost, ":")[0],
		"--tmpfs", "/var/lib/registry:rw,size=256m", l.images.registry)
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		_, _, exit, err := l.docker.run(context.Background(), nil, nil, "exec", l.name("registry"),
			"wget", "-q", "-O", "/dev/null", "http://127.0.0.1:5000/v2/")
		if err == nil && exit == 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	logs, stderr, _, _ := l.docker.run(context.Background(), nil, nil, "logs", "--tail", "40", l.name("registry"))
	t.Fatalf("the registry did not become ready: %s%s", logs, stderr)
}

// sql runs statements in a database, as a person editing it by hand would.
func (l *lab) sql(t *testing.T, engine, database string, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		var args []string
		var env []string
		switch engine {
		case enginePostgres:
			args = []string{"exec", l.name("postgres"), "psql", "-U", "postgres", "-d", database, "-v", "ON_ERROR_STOP=1", "-c", statement}
		case engineMySQL:
			env = []string{"MYSQL_PWD=" + l.databasePassword}
			args = []string{"exec", "--env", "MYSQL_PWD", l.name("mysql"), "mysql", "-uroot", "--database", database, "-e", statement}
		default:
			t.Fatalf("no engine %q", engine)
		}
		l.docker.must(t, env, args...)
	}
}

func (l *lab) createDatabase(t *testing.T, engine, database string) {
	t.Helper()
	switch engine {
	case enginePostgres:
		l.sql(t, engine, "postgres", "CREATE DATABASE "+database)
	case engineMySQL:
		l.sql(t, engine, "mysql", "CREATE DATABASE "+database)
	}
}

func (l *lab) databaseURL(engine, database string) string {
	switch engine {
	case engineMySQL:
		return "mysql://root:" + l.databasePassword + "@" + mysqlHost + ":3306/" + database
	default:
		return "postgres://postgres:" + l.databasePassword + "@" + postgresHost + ":5432/" + database + "?sslmode=disable"
	}
}

// publish runs `ptah <family> push` from the fixture volume and returns the
// reference. A row reads what this wrote; nothing here is recorded.
func (l *lab) publish(t *testing.T, family, repository, version string, source ...string) string {
	t.Helper()
	reference := "oci://" + registryHost + "/" + repository + ":" + version
	args := append([]string{family, "push", reference}, source...)
	args = append(args, "--version", version, "--plain-http")
	stdout, stderr, exit := l.ptah(t, args, nil, nil, "fixtures", "/fixtures")
	if exit != 0 {
		t.Fatalf("publish %s exited %d: %s%s", reference, exit, stdout, stderr)
	}
	return reference
}

// rowSpec is one row: a Job and what the run has to show of it.
type rowSpec struct {
	name      string
	operation runner.Operation
	source    runnerharness.Source
	engine    string
	job       *batchv1.Job
	cluster   runnerharness.Cluster
	// wantCode is the frame's error code; empty means no error.
	wantCode string
	// anyOutcome records whichever code the frame carries, for a row whose
	// reading is the point, so that a release that moves it reaches the diff
	// rather than stopping the run that would have written it.
	anyOutcome bool
	// wantRefusal is a word the refusal of a fetch has to hold; set, the row
	// ends at that fetch and writes no frame.
	wantRefusal string
	// documents names where a command's document is stored, for the documents
	// the decoders' tests read by a path of their own.
	documents map[string]string
	// savedPlan names where the plan a Plan operation saved is stored.
	savedPlan string
	// appendArgs goes on the end of every child's arguments.
	appendArgs []string
	// sharedSource names a volume already filled: the row reads it and runs none
	// of the Job's init containers. A fault row has nothing to learn from
	// fetching the same artifact again.
	sharedSource string
}

// rowOutcome is what the manager would have read of a row.
type rowOutcome struct {
	result  runner.Result
	plan    []byte
	refusal string
	// logs is what the Pod's containers wrote to their logs.
	logs []byte
	// children is what each Ptah command printed, before any masking.
	children []recording.Child
}

const operationContainer = "ptah"

// runRow runs a row's Pod and records it. It fails the test on anything the row
// does not expect, and returns what the next row needs.
func (l *lab) runRow(t *testing.T, row rowSpec) rowOutcome {
	t.Helper()
	pod, err := runnerharness.Translate(row.job, row.cluster)
	if err != nil {
		t.Fatalf("%s: %v", row.name, err)
	}
	var children []recording.Child
	var frame, logs []byte
	var refusal string
	summaryError := ""
	finished := false
	containers := pod.Containers
	if row.sharedSource != "" {
		containers = containers[len(containers)-1:]
	}
	l.withVolumes(t, row.name, pod, row.sharedSource, func(mounts map[string]podVolume) {
		for _, container := range containers {
			args := []string(nil)
			if container.Name == operationContainer {
				for _, extra := range row.appendArgs {
					args = append(args, "--append-arg", extra)
				}
			}
			ran, stderr, ok := l.runContainer(t, row.name, container, pod.InstallTo, mounts, args)
			children = append(children, ran.Children...)
			logs = append(logs, stderr...)
			if ran.Refusal != "" {
				refusal = ran.Refusal
			}
			if ran.SummaryError != "" {
				summaryError = ran.SummaryError
			}
			frame = ran.Frame
			if !ok {
				if refusal == "" {
					t.Errorf("%s: container %s stopped the Pod: %s", row.name, container.Name, stderr)
				}
				return
			}
		}
		finished = true
	})
	outcome := rowOutcome{refusal: refusal, logs: logs, children: children}
	record := runnerharness.Row{
		Name: row.name, Operation: string(row.operation), Source: row.source, Engine: row.engine,
		Refusal: l.masker.Text(refusal),
	}
	l.count(row, children)
	for index, child := range children {
		if err := runnerharness.DecodeDocument(child.Args, child.Exit, child.Stdout); err != nil {
			t.Errorf("%s: `ptah %s` exited %d and printed a document the decoder refuses: %v\n%s\n%s",
				row.name, runnerharness.CommandWords(child.Args), child.Exit, err, child.Stdout, child.Stderr)
		}
		record.Children = append(record.Children, l.recordChild(t, row, index, child))
	}
	if row.wantRefusal != "" {
		if !strings.Contains(refusal, row.wantRefusal) {
			t.Errorf("%s: the fetch refused with %q, want %q", row.name, refusal, row.wantRefusal)
		}
		l.addRow(record)
		return outcome
	}
	if summaryError != "" {
		t.Errorf("%s: the result has no termination summary: %s", row.name, summaryError)
	}
	if !finished {
		l.addRow(record)
		t.FailNow()
	}
	logs = append(logs, frame...)
	result, err := runner.ParseResultFor(logs, row.operation, operationID(row.name))
	if err != nil {
		t.Errorf("%s: the manager's parser refused the frame: %v\n%s", row.name, err, logs)
		l.addRow(record)
		t.FailNow()
	}
	outcome.result = result
	switch {
	case row.anyOutcome:
	case row.wantCode == "" && result.Error != nil:
		t.Errorf("%s: the frame says %s: %s\n%s", row.name, result.Error.Code, result.Error.Message, childrenText(children))
	case row.wantCode != "" && (result.Error == nil || result.Error.Code != row.wantCode):
		t.Errorf("%s: the frame says %+v, want the code %s\n%s", row.name, result.Error, row.wantCode, childrenText(children))
	}
	if result.PlanOutcome == runner.PlanOutcomeChanges {
		plan, err := l.seal.OpenPlan(result.Stdout, planseal.Envelope{OperationID: result.OperationID, JobName: planJobName(row.job)})
		if err != nil {
			t.Errorf("%s: the manager's key does not open the sealed plan: %v", row.name, err)
		} else {
			if _, err := dataplane.DecodePlan(plan, row.engine); err != nil {
				t.Errorf("%s: the plan the runner sealed does not decode: %v\n%s", row.name, err, plan)
			}
			outcome.plan = plan
			record.SavedPlan = row.savedPlan
			if record.SavedPlan == "" {
				record.SavedPlan = runnerharness.DocumentPath(row.name, len(record.Children), "saved plan", plan)
			}
			l.addDocument(t, row.name, record.SavedPlan, plan)
		}
		// The ciphertext is different every time, and was just opened.
		result.Stdout = "<sealed plan>"
	}
	record.Result = l.maskedResult(t, result)
	l.addRow(record)
	if t.Failed() {
		t.FailNow()
	}
	return outcome
}

// childrenText is what a failed row's children printed, for its message.
func childrenText(children []recording.Child) string {
	var text strings.Builder
	for _, child := range children {
		fmt.Fprintf(&text, "`ptah %s` exited %d\nstdout: %s\nstderr: %s\n", strings.Join(child.Args, " "), child.Exit, child.Stdout, child.Stderr)
	}
	return text.String()
}

func planJobName(job *batchv1.Job) string {
	for _, container := range job.Spec.Template.Spec.Containers {
		for _, variable := range container.Env {
			if variable.Name == runner.EnvSealedPlanJobName {
				return variable.Value
			}
		}
	}
	return ""
}

// maskedResult is the frame's payload as the transcript stores it.
func (l *lab) maskedResult(t *testing.T, result runner.Result) any {
	t.Helper()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return l.masker.Value(value)
}

func (l *lab) recordChild(t *testing.T, row rowSpec, index int, child recording.Child) runnerharness.Child {
	t.Helper()
	if child.Error != "" {
		t.Errorf("%s: `ptah %s` could not be run: %s", row.name, runnerharness.CommandWords(child.Args), child.Error)
	}
	command := runnerharness.CommandWords(child.Args)
	record := runnerharness.Child{Command: command, Exit: child.Exit, Stderr: l.masker.Text(string(child.Stderr))}
	for _, argument := range child.Args {
		record.Args = append(record.Args, l.masker.Text(argument))
	}
	if len(child.Stdout) != 0 {
		document, err := runnerharness.Scrub(child.Stdout)
		if err != nil {
			t.Errorf("%s: %v", row.name, err)
			document = child.Stdout
		}
		// A plan reads twice. The path a row names holds the first read, which
		// the decoders' tests read; the second is compared like any other.
		record.Stdout = row.documents[command]
		if record.Stdout == "" || l.hasDocument(record.Stdout) {
			record.Stdout = runnerharness.DocumentPath(row.name, index, command, document)
		}
		l.addDocument(t, row.name, record.Stdout, document)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if child.PeakKiB > l.peaks[command] {
		l.peaks[command] = child.PeakKiB
	}
	return record
}

func (l *lab) hasDocument(file string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, taken := l.docs[file]
	return taken
}

func (l *lab) addDocument(t *testing.T, row, file string, document []byte) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, taken := l.docs[file]; taken {
		t.Errorf("%s: another row already stored %s", row, file)
		return
	}
	l.docs[file] = slices.Clone(document)
}

func (l *lab) addRow(row runnerharness.Row) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, row)
}

// count notes what a row ran, whatever became of it.
func (l *lab) count(row rowSpec, children []recording.Child) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts.Rows++
	if row.operation != "" {
		l.counts.Operations[runnerharness.OperationCase{Operation: row.operation, Source: row.source}]++
	}
	for _, child := range children {
		l.counts.Commands[runnerharness.CommandWords(child.Args)]++
	}
}

// withVolumes makes a Pod's volumes and hands over each one's mount flags. A
// memory volume one container alone mounts is a tmpfs of its size. A volume two
// containers share, or one holding files, is a named volume filled before any
// container starts. The runner's volume is the one every Pod reads.
func (l *lab) withVolumes(t *testing.T, row string, pod runnerharness.Pod, sharedSource string, run func(map[string]podVolume)) {
	t.Helper()
	l.slots <- struct{}{}
	defer func() { <-l.slots }()

	mountsOf := map[string]int{}
	for _, container := range pod.Containers {
		for _, mount := range container.Mounts {
			mountsOf[mount.Volume]++
		}
	}
	mounts := map[string]podVolume{}
	var named []string
	var entries []volumes.Entry
	suffix := strconv.FormatInt(l.sequence.Add(1), 10)
	prepare := []string{"run", "--interactive", "--rm", "--pull", "never", "--name", l.name("fill-" + suffix), "--network", "none", "--user", "0:0"}
	for _, volume := range pod.Volumes {
		switch {
		case volume.Name == sourceVolumeName && sharedSource != "":
			mounts[volume.Name] = podVolume{named: sharedSource}
			continue
		case volume.Name == runnerVolumeName:
			mounts[volume.Name] = podVolume{named: l.name("runner")}
			continue
		case volume.Empty && mountsOf[volume.Name] == 1:
			mounts[volume.Name] = podVolume{tmpfs: fmt.Sprintf("rw,exec,size=%d,mode=0770,uid=0,gid=%d", volume.SizeBytes, volume.Group)}
			continue
		}
		name := l.name("pod-" + suffix + "-" + volume.Name)
		named = append(named, name)
		mounts[volume.Name] = podVolume{named: name}
		prepare = append(prepare, "--mount", "type=volume,src="+name+",dst=/volumes/"+volume.Name)
		if volume.Empty {
			entries = append(entries, volumes.Entry{Volume: volume.Name, Path: ".", Directory: true, Mode: 0o770, GID: volume.Group})
			continue
		}
		entries = append(entries, volumes.Entry{Volume: volume.Name, Path: ".", Directory: true, Mode: 0o750, GID: volume.Group})
		entries = append(entries, volume.Files...)
	}
	defer func() {
		if len(named) != 0 {
			l.docker.quiet(t, append([]string{"volume", "rm", "--force"}, named...)...)
		}
	}()
	if len(named) != 0 {
		stream, err := volumes.Pack(entries)
		if err != nil {
			t.Fatalf("%s: %v", row, err)
		}
		prepare = append(prepare, "--entrypoint", "/recorder", l.images.tools, "--prepare-volumes", "/volumes")
		if _, stderr, exit, err := l.docker.run(context.Background(), stream, nil, prepare...); err != nil || exit != 0 {
			t.Fatalf("%s: fill the volumes: exit %d, %v: %s", row, exit, err, stderr)
		}
	}
	run(mounts)
}

// podVolume is how one of a Pod's volumes is mounted: a tmpfs with these options,
// or a named Docker volume.
type podVolume struct{ tmpfs, named string }

// resolver is the mount flags of a container's volumes.
func resolver(mounts map[string]podVolume) func(runnerharness.Mount) ([]string, error) {
	return func(mount runnerharness.Mount) ([]string, error) {
		volume, known := mounts[mount.Volume]
		switch {
		case !known:
			return nil, fmt.Errorf("no volume %s", mount.Volume)
		case volume.tmpfs != "":
			return []string{"--tmpfs", mount.Path + ":" + volume.tmpfs}, nil
		}
		value := "type=volume,src=" + volume.named + ",dst=" + mount.Path
		if mount.ReadOnly {
			value += ",readonly"
		}
		return []string{"--mount", value}, nil
	}
}

const (
	runnerVolumeName = "runner"
	sourceVolumeName = "schema-source"
)

// runContainer runs one container of a Pod and reads what it did. The recorder
// prints a transcript; any other container is a Ptah command the Job runs
// directly, and what it printed is its record.
func (l *lab) runContainer(t *testing.T, row string, container runnerharness.Container, installTo string, mounts map[string]podVolume, extra []string) (recording.Transcript, []byte, bool) {
	t.Helper()
	name := l.name("c-" + strconv.FormatInt(l.sequence.Add(1), 10))
	args, err := container.RunArgs(name, l.network, resolver(mounts), extra...)
	if err != nil {
		t.Fatalf("%s: %v", row, err)
	}
	stdout, stderr, exit, err := l.docker.run(context.Background(), nil, container.SecretEnv(), args...)
	if err != nil {
		t.Fatalf("%s: run %s: %v", row, container.Name, err)
	}
	if exit >= 125 {
		// Docker's own failures are not Ptah's refusals.
		t.Fatalf("%s: docker exited %d for %s: %s", row, exit, container.Name, stderr)
	}
	if container.Command[0] != installTo {
		child := recording.Child{Args: slices.Clone(container.Args), Stdout: stdout, Stderr: stderr, Exit: exit}
		return recording.Transcript{Children: []recording.Child{child}}, nil, exit == 0
	}
	var transcript recording.Transcript
	if err := json.Unmarshal(stdout, &transcript); err != nil {
		t.Fatalf("%s: read what %s recorded: %v: %s%s", row, container.Name, err, stdout, stderr)
	}
	return transcript, stderr, exit == 0
}

// serveGit runs the Git server over one of its specs, listening on 443 and 22
// itself because a Docker network maps no ports: URLs carry no port, and the
// known-hosts line matches. It returns when a pull that must succeed does.
func (l *lab) serveGit(t *testing.T, spec string) {
	t.Helper()
	server := l.name("git")
	l.docker.quiet(t, "rm", "--force", server)
	l.docker.must(t, nil, "run", "--detach", "--pull", "never", "--name", server,
		"--network", l.network, "--network-alias", gitHost,
		"--read-only", "--user", "65532:65532", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--entrypoint", "/e2e-git-server", l.images.tools,
		"-spec", "/spec/"+spec, "-repository", gitfixture.Repository,
		"-https-listen", ":443", "-tls-certificate", "/keys/tls.crt", "-tls-key", "/keys/tls.key", "-token-file", "/keys/token",
		"-ssh-listen", ":22", "-ssh-host-key", "/keys/ssh_host_key", "-ssh-authorized-key", "/keys/authorized_key")
	probe := []string{"schema", "pull", "git+https://" + gitHost + "/" + gitfixture.Repository, "--path", gitfixture.SchemaPath,
		"--branch", "main", "--ca-file", "/credentials/git-ca/ca.pem", "--out", "/fetch-work/ptah-git-fetch-capture/schema.sql", "--json"}
	for range 30 {
		_, stderr, exit := l.pull(t, probe, []string{"PTAH_GIT_PASSWORD=" + l.keys.Token})
		if exit == 0 {
			return
		}
		t.Logf("the Git server is not ready yet: %s", stderr)
		time.Sleep(time.Second)
	}
	logs, _, _, _ := l.docker.run(context.Background(), nil, nil, "logs", server)
	t.Fatalf("the Git server did not become ready: %s", logs)
}

// pull runs Ptah's pull directly with the arguments a row wrote out.
func (l *lab) pull(t *testing.T, args, secrets []string) (stdout, stderr []byte, exit int) {
	t.Helper()
	return l.ptah(t, args, secrets, nil, "credentials", "/credentials")
}

// ptah runs one Ptah command in a container with the shape of a Job's fetch
// container, reading one shared volume, and a writable volume at its HOME. Its
// volume holds what the row reads: the trust and credentials at the paths a Job's
// projections have, or the fixtures an artifact is published from.
func (l *lab) ptah(t *testing.T, args, secrets []string, literals []runnerharness.EnvVar, volume, mountedAt string) (stdout, stderr []byte, exit int) {
	t.Helper()
	l.slots <- struct{}{}
	defer func() { <-l.slots }()
	const workVolume, workPath = "fetch-work", "/fetch-work"
	shape := l.fetchShape(t)
	shape.Command = []string{"/usr/local/bin/ptah"}
	shape.Args = args
	shape.Mounts = []runnerharness.Mount{
		{Volume: workVolume, Path: workPath},
		{Volume: volume, Path: mountedAt, ReadOnly: true},
	}
	shape.Env = append([]runnerharness.EnvVar{{Name: "HOME", Value: workPath}, {Name: "TMPDIR", Value: workPath}}, literals...)
	for _, variable := range secrets {
		key, value, _ := strings.Cut(variable, "=")
		shape.Env = append(shape.Env, runnerharness.EnvVar{Name: key, Value: value, Secret: true})
	}
	name := l.name("ptah-" + strconv.FormatInt(l.sequence.Add(1), 10))
	dockerArgs, err := shape.RunArgs(name, l.network, resolver(map[string]podVolume{
		workVolume: {tmpfs: "rw,exec,size=67108864,mode=0770,uid=0,gid=65532"},
		volume:     {named: l.name(volume)},
	}))
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, exit, err = l.docker.run(context.Background(), nil, shape.SecretEnv(), dockerArgs...)
	if err != nil {
		t.Fatalf("ptah %s: %v", strings.Join(args, " "), err)
	}
	if exit >= 125 {
		t.Fatalf("docker exited %d for `ptah %s`: %s", exit, strings.Join(args, " "), stderr)
	}
	return stdout, stderr, exit
}

// fetchShape is the fetch container of a Job the builder made: its user, its
// read-only root file system and its dropped capabilities are the ones a pull
// runs under.
func (l *lab) fetchShape(t *testing.T) runnerharness.Container {
	t.Helper()
	l.shapeOnce.Do(func() {
		run := l.newSchema(t, "fetch-shape", enginePostgres, l.databaseURL(enginePostgres, "harness_absent"),
			operatorv1alpha1.SourceSpec{Git: l.gitHTTPS(gitfixture.SchemaPath, operatorv1alpha1.GitRef{Branch: "main"})})
		tip, content := fetchShapeRevision(t)
		run.schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{Git: &operatorv1alpha1.GitSourceRevision{
			URL: run.schema.Spec.Desired.Git.URL, Ref: run.schema.Spec.Desired.Git.Ref, Path: run.schema.Spec.Desired.Git.Path,
			Commit: tip, ContentDigest: content,
		}}
		job := run.build(run.reads("fetch-shape", operatorv1alpha1.OperationObserve), nil)
		pod, err := runnerharness.Translate(job, l.cluster("postgres://harness@harness.invalid/harness", nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		index := slices.IndexFunc(pod.Containers, func(container runnerharness.Container) bool { return container.Name == gitFetchContainer })
		if index < 0 {
			t.Fatalf("the Job has no %s container", gitFetchContainer)
		}
		l.shape = pod.Containers[index]
	})
	return l.shape
}

const gitFetchContainer = "fetch-git-source"

func fetchShapeRevision(t *testing.T) (commit, content string) {
	t.Helper()
	built, err := gitrepo.Build(gitfixture.RepositorySpec(2))
	if err != nil {
		t.Fatal(err)
	}
	content, err = gitfixture.SchemaContent(1)
	if err != nil {
		t.Fatal(err)
	}
	return built.Commit(2).String(), content
}
