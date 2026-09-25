package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// definitionDigest binds a recording to the scenario it claims to be.
//
// A recording already names the scenario's id and the commit it ran from.
// Neither answers the question a reader has: is this transcript still what the
// scenario says? An id survives any edit to the commands underneath it, and a
// commit moves for every change in the repository, so checking by commit means
// checking the repository out and reading the diff.
//
// The digest covers what a reader watches and what licensed publishing the
// output: the commands, what each step waited for, what it expected, and which
// streams reached the transcript. It deliberately excludes the title, the
// tagline, the narration and the tags, because rewording a sentence does not
// make an old transcript a lie about what ran.
func definitionDigest(s scenario) string {
	var canonical strings.Builder
	fmt.Fprintf(&canonical, "id\x00%s\n", s.ID)
	for _, command := range s.Reset {
		fmt.Fprintf(&canonical, "reset\x00%s\n", command)
	}
	for index, one := range s.Steps {
		fmt.Fprintf(&canonical, "step\x00%d\n", index)
		fmt.Fprintf(&canonical, "run\x00%s\n", one.Run)
		fmt.Fprintf(&canonical, "retry\x00%s\n", one.Retry)
		fmt.Fprintf(&canonical, "sync\x00%s\n", one.Sync)
		for _, stream := range one.Show {
			fmt.Fprintf(&canonical, "show\x00%s\n", stream)
		}
		fmt.Fprintf(&canonical, "await\x00%s\n", describeForDigest(one.Await))
		fmt.Fprintf(&canonical, "expect\x00%s\n", describeExpectationForDigest(one.Expect))
	}
	sum := sha256.Sum256([]byte(canonical.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func describeForDigest(o *observation) string {
	if o == nil {
		return ""
	}
	return fmt.Sprintf("%#v", *o)
}

func describeExpectationForDigest(e *expectation) string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%#v", *e)
}

// staleRecordings names every recording whose scenario has changed under it,
// and every recording naming a scenario that no longer exists.
//
// A recording that predates the digest carries none, and is reported as
// unbound rather than as matching: "nobody checked" and "checked and agreed"
// are the distinction this whole binding exists to make.
func staleRecordings(record runRecord, current []scenario) []string {
	digests := map[string]string{}
	for _, one := range current {
		digests[one.ID] = definitionDigest(one)
	}
	var findings []string
	for _, recorded := range record.Scenarios {
		want, defined := digests[recorded.ID]
		switch {
		case !defined:
			findings = append(findings, fmt.Sprintf(
				"%s: the recording names a scenario this tree no longer declares", recorded.ID))
		case recorded.DefinitionDigest == "":
			findings = append(findings, fmt.Sprintf(
				"%s: the recording carries no definition digest, so nothing has checked it against the scenario",
				recorded.ID))
		case recorded.DefinitionDigest != want:
			findings = append(findings, fmt.Sprintf(
				"%s: the scenario changed under the recording; re-record it or keep the old definition beside it",
				recorded.ID))
		}
	}
	return findings
}

// verifyRecordings reports every recording in a run record that no longer
// represents its scenario. It runs nothing and needs no lab, which is the
// point: the question "is this transcript still true" should not cost a
// cluster to answer.
func verifyRecordings(path string, current []scenario, diagnostics io.Writer) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read the run record: %w", err)
	}
	record := runRecord{}
	if err := json.Unmarshal(contents, &record); err != nil {
		return fmt.Errorf("read the run record: %w", err)
	}
	if len(record.Scenarios) == 0 {
		return fmt.Errorf("the run record holds no recording, so nothing was verified")
	}
	findings := staleRecordings(record, current)
	if len(findings) == 0 {
		fmt.Fprintf(diagnostics, "record: %d recordings still represent their scenarios\n", len(record.Scenarios))
		return nil
	}
	for _, finding := range findings {
		fmt.Fprintf(diagnostics, "record: %s\n", finding)
	}
	return fmt.Errorf("%d of %d recordings no longer represent their scenario", len(findings), len(record.Scenarios))
}
