package runnerharness_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/runnerharness"
	"github.com/stokaro/ptah-operator/test/runnerharness/volumes"
)

// packRoot is the stream that makes a shared source volume writable by the Pod.
func packRoot(_ string, pod runnerharness.Pod) ([]byte, error) {
	group := 0
	for _, volume := range pod.Volumes {
		if volume.Name == sourceVolumeName {
			group = volume.Group
		}
	}
	return volumes.Pack([]volumes.Entry{{Volume: sourceVolumeName, Path: ".", Directory: true, Mode: 0o770, GID: group}})
}

// TestRunnerAgainstThePinnedImage is the harness. `make runner-harness` runs it
// and `make runner-harness-write` regenerates the captures with it.
func TestRunnerAgainstThePinnedImage(t *testing.T) {
	gate, err := runnerharness.ReadGate(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if !gate.Required {
		t.Skip("the harness needs Docker: run `DOCKER_CONTEXT=<context> make runner-harness`")
	}
	l := startLab(t, gate)

	engines := []string{enginePostgres, engineMySQL}
	inputs := map[string]*faultInputs{enginePostgres: {}, engineMySQL: {}}
	rows := pullRows(t)
	prepareDifferingServer(t, l)
	t.Run("lifecycles", func(t *testing.T) {
		for _, engine := range engines {
			t.Run("schema-"+engine, func(t *testing.T) {
				t.Parallel()
				schemaLifecycle(t, l, engine, inputs[engine])
			})
			t.Run("migrations-"+engine, func(t *testing.T) {
				t.Parallel()
				migrationLifecycle(t, l, engine, inputs[engine])
			})
		}
		t.Run("verification-refusals", func(t *testing.T) {
			t.Parallel()
			verificationRefusals(t, l)
		})
		t.Run("features", func(t *testing.T) {
			t.Parallel()
			featureLifecycle(t, l)
		})
		t.Run("migration-failures", func(t *testing.T) {
			t.Parallel()
			migrationFailures(t, l)
		})
		t.Run("git", func(t *testing.T) {
			t.Parallel()
			gitLifecycles(t, l)
		})
		t.Run("differing", func(t *testing.T) {
			t.Parallel()
			differingDatabases(t, l)
		})
		t.Run("statement-errors", func(t *testing.T) {
			t.Parallel()
			statementErrors(t, l)
		})
		t.Run("unreadable-sources", func(t *testing.T) {
			t.Parallel()
			unreadableSources(t, l)
		})
		t.Run("structural-faults", func(t *testing.T) {
			t.Parallel()
			structuralFaults(t, l)
		})
		t.Run("source-boundaries", func(t *testing.T) {
			t.Parallel()
			sourceBoundaries(t, l)
		})
		t.Run("pulls", func(t *testing.T) {
			t.Parallel()
			for _, row := range rows {
				if row.rewritten {
					continue
				}
				t.Run(row.name, func(t *testing.T) {
					t.Parallel()
					runPull(t, l, row)
				})
			}
		})
	})

	// A fault needs what a lifecycle really produced.
	if inputs[enginePostgres].plan != nil && inputs[engineMySQL].plan != nil &&
		inputs[enginePostgres].planned != nil && inputs[engineMySQL].planned != nil {
		t.Run("faults", func(t *testing.T) {
			prepareFaultDatabases(t, l)
			sources := map[string]faultSources{}
			for _, engine := range engines {
				sources[engine] = faultSources{schema: l.name("fault-schema-" + engine), migration: l.name("fault-migration-" + engine)}
				schema := inputs[engine].schema
				l.seedSource(t, schema.row("seed", "", "", schema.build(schema.reads("seed", operatorv1alpha1.OperationObserve), nil)), sources[engine].schema)
				migration := inputs[engine].migration
				l.seedSource(t, migration.row("seed", "", migration.build(migration.reads("seed", operatorv1alpha1.MigrationOperationHistory), nil)), sources[engine].migration)
			}
			for _, each := range faults() {
				t.Run(each.name, func(t *testing.T) {
					t.Parallel()
					engine := enginePostgres
					if strings.HasPrefix(each.name, "my-") {
						engine = engineMySQL
					}
					faultRows(t, l, each, inputs[engine], sources[engine])
				})
			}
		})
	}

	// The rows that read a repository whose branch was rewritten stop the
	// server's other readers, so they run alone at the end.
	t.Run("rewritten", func(t *testing.T) {
		l.serveGit(t, "rewritten.json")
		for _, row := range rows {
			if row.rewritten {
				t.Run(row.name, func(t *testing.T) { runPull(t, l, row) })
			}
		}
	})

	report(t, l)
	keepFresh(t, l, gate)
	if t.Failed() {
		t.Log("a row failed, so the captures are neither compared nor written")
		return
	}
	finish(t, l, gate)
}

// fresh is what this run produced, in the form it is committed.
func fresh(t *testing.T, l *lab) runnerharness.Captures {
	t.Helper()
	executor, commit, release := pinnedExecutor(t)
	l.mu.Lock()
	defer l.mu.Unlock()
	slices.SortFunc(l.rows, func(a, b runnerharness.Row) int { return strings.Compare(a.Name, b.Name) })
	return runnerharness.Captures{
		Rows: l.rows, Documents: l.docs,
		Provenance: runnerharness.Provenance{PtahRelease: release, PtahCommit: commit, PtahImage: executor},
	}
}

// keepFresh leaves what the run printed in the directory that names, whatever
// became of it, so a failure can be read without running it again.
func keepFresh(t *testing.T, l *lab, gate runnerharness.Gate) {
	t.Helper()
	if gate.Out == "" {
		return
	}
	if err := runnerharness.WriteCaptures(gate.Out, runnerharness.NewMasker(), runnerharness.Captures{}, fresh(t, l)); err != nil {
		t.Errorf("keep the fresh captures: %v", err)
	}
}

func report(t *testing.T, l *lab) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	t.Logf("ran %d rows", l.counts.Rows)
	commands := make([]string, 0, len(l.counts.Commands))
	for command := range l.counts.Commands {
		commands = append(commands, command)
	}
	slices.Sort(commands)
	for _, command := range commands {
		t.Logf("`ptah %s`: %d runs, peak %d KiB", command, l.counts.Commands[command], l.peaks[command])
	}
}

// finish holds the run to the declared set, then to the committed captures, or
// writes them.
func finish(t *testing.T, l *lab, gate runnerharness.Gate) {
	t.Helper()
	if err := runnerharness.CheckCount(runnerharness.Declared.Operations, runnerharness.Declared.Commands, l.counts); err != nil {
		t.Errorf("the count of what ran does not match the declared set:\n%v", err)
	}
	for file, document := range l.docs {
		for _, secret := range l.secrets {
			if bytes.Contains(document, []byte(secret)) {
				t.Errorf("%s holds a credential", file)
			}
		}
	}
	for index, row := range l.rows {
		encoded := fmt.Sprintf("%+v", row)
		for _, secret := range l.secrets {
			if strings.Contains(encoded, secret) {
				t.Errorf("row %d (%s) holds a credential", index, row.Name)
			}
		}
	}
	if t.Failed() {
		return
	}
	produced := fresh(t, l)
	root := filepath.Join(repositoryRoot, filepath.FromSlash(runnerharness.CapturesDirectory))
	committed, err := runnerharness.LoadCaptures(root)
	if err != nil {
		t.Fatal(err)
	}
	// A rule counts what it masked in what this run printed, whatever the run
	// is compared with: the rows were masked as they ran, and this reads the
	// documents.
	masker := l.masker
	for _, document := range produced.Documents {
		runnerharness.CompareDocuments(masker, document, document)
	}
	changes := runnerharness.CompareCaptures(masker, committed, produced)
	if gate.Write {
		if gate.Image != "" {
			t.Fatal("a run against another image does not write captures: they would not answer the pin")
		}
		if err := runnerharness.WriteCaptures(root, masker, committed, produced); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote the captures: %d changes of shape against what was committed", len(changes))
		for _, change := range changes {
			t.Log(change.String())
		}
	} else {
		for _, change := range changes {
			t.Error(change.String())
		}
		stray, err := runnerharness.StrayFiles(root, produced)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range stray {
			t.Errorf("%s: no row produced this capture", file)
		}
		if len(changes) != 0 || len(stray) != 0 {
			t.Log("if the change is intended, run `make runner-harness-write` and review the diff")
		}
	}
	for _, rule := range masker.UnusedRules() {
		t.Errorf("the volatile rule %q masked nothing in a complete run: remove it", rule)
	}
}
