package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
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
// make an old transcript a lie about what ran. Those fields say so with
// digest:"-"; a field the scenario gains is covered unless it says the same.
//
// It is taken over what the file states, never over how the decoder holds it
// in memory: the recorder computes it in one process and -verify in another, so
// anything that depends on the process makes every recording fail. The first
// form printed the decoded structs with %#v, which writes a pointer field as its
// address, and not one recording could be verified.
func definitionDigest(s scenario) string {
	var canonical strings.Builder
	writeDeclared(&canonical, "scenario", reflect.ValueOf(s))
	sum := sha256.Sum256([]byte(canonical.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// writeDeclared writes a decoded scenario value as one line per scalar, named
// by its path of YAML keys and list indices.
//
// A pointer is followed to what it points at, so `exit: 0` reads as 0 wherever
// it was allocated. A struct writes a line of its own before its fields, which
// keeps `await: {}` apart from no await. A field left at its zero value writes
// nothing: the decoder cannot tell it from an absent key, and a field the
// recorder gains does not re-bind every recording of a scenario that never set
// it. Values are quoted, so a newline in a command cannot fake a line.
func writeDeclared(to *strings.Builder, path string, value reflect.Value) {
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			writeDeclared(to, path, value.Elem())
		}
	case reflect.Struct:
		fmt.Fprintf(to, "%s{}\n", path)
		for index := range value.NumField() {
			declared := value.Type().Field(index)
			if declared.Tag.Get("digest") == "-" {
				continue
			}
			field := value.Field(index)
			if field.IsZero() || (field.Kind() == reflect.Slice && field.Len() == 0) {
				continue
			}
			writeDeclared(to, path+"."+yamlKey(declared), field)
		}
	case reflect.Slice:
		for index := range value.Len() {
			writeDeclared(to, path+"."+strconv.Itoa(index), value.Index(index))
		}
	case reflect.String:
		fmt.Fprintf(to, "%s=%q\n", path, value.String())
	case reflect.Bool:
		fmt.Fprintf(to, "%s=%t\n", path, value.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		fmt.Fprintf(to, "%s=%d\n", path, value.Int())
	default:
		// TestTheDigestCanWriteEveryScenarioField walks the scenario type and
		// fails on a field of any other kind, so this is reached only by a
		// change that test has already refused.
		panic(fmt.Sprintf("definitionDigest cannot write %s at %s", value.Kind(), path))
	}
}

// yamlKey is the key a field is written under in a scenario file.
func yamlKey(field reflect.StructField) string {
	key, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
	return key
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
