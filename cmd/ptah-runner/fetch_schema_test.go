package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/runner"
)

const (
	fetchTestReference = "oci://registry.example/acme/schema@sha256:" +
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fetchTestOut      = "/source/schema.hcl"
	fetchTestPassword = "the-registry-password"
)

// fakePtah writes a program standing where Ptah stands in the executor image,
// which records what it was started with and answers as the row asks.
func fakePtah(t *testing.T, stderr string, exit int) (program, argumentsFile, environmentFile string) {
	t.Helper()
	directory := t.TempDir()
	program = filepath.Join(directory, "ptah")
	argumentsFile = filepath.Join(directory, "arguments")
	environmentFile = filepath.Join(directory, "environment")
	script := "#!/bin/sh\n" +
		"for argument in \"$@\"; do printf '%s\\n' \"$argument\" >> '" + argumentsFile + "'; done\n" +
		"env > '" + environmentFile + "'\n" +
		"printf '%s\\n' '" + stderr + "' >&2\n" +
		"exit " + strconv.Itoa(exit) + "\n"
	if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return program, argumentsFile, environmentFile
}

// mountedTerminationLog is the file the kubelet mounts into every container.
func mountedTerminationLog(t *testing.T) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "termination-log")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func fetchEnvironment(extra ...string) []string {
	return append([]string{
		"HOME=/fetch-work", "PTAH_OCI_REGISTRY=registry.example", "PTAH_OCI_USERNAME=robot",
		runner.EnvOCIPassword + "=" + fetchTestPassword, runnerProtocol,
	}, extra...)
}

func fetchArguments(program string) []string {
	return []string{"--fetch-oci-schema", fetchTestReference, "--fetch-oci-schema-to", fetchTestOut, "--ptah-binary", program}
}

// The Pod's fetch step, end to end: the arguments a Job writes, a real process,
// and the file the kubelet copies into status.
func TestRunFetchesASchemaAndReportsAParseFailure(t *testing.T) {
	t.Parallel()

	const measured = "error: parse schema artifact: parse HCL schema: schema.hcl:3,31-32: Invalid single-argument block definition"
	program, argumentsFile, environmentFile := fakePtah(t, measured, 2)
	terminationLog := mountedTerminationLog(t)
	var stdout, stderr bytes.Buffer
	if exit := run(context.Background(), fetchArguments(program), &stdout, &stderr, fetchEnvironment(), terminationLog); exit != 2 {
		t.Fatalf("exit = %d, want what the pull exited with; stderr = %q", exit, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q: a fetch writes no frame", stdout.String())
	}
	if !strings.Contains(stderr.String(), measured) {
		t.Errorf("the log = %q does not hold the sentence", stderr.String())
	}
	message, err := os.ReadFile(terminationLog)
	if err != nil {
		t.Fatal(err)
	}
	reason, carried := runner.ParseFetchReason(string(message))
	if want := strings.TrimPrefix(measured, "error: "); !carried || reason != want {
		t.Fatalf("the manager reads %q, %t in %q, want %q", reason, carried, message, want)
	}

	arguments, err := os.ReadFile(argumentsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{"schema", "pull", fetchTestReference, "--out", fetchTestOut}, "\n") + "\n"
	if string(arguments) != want {
		t.Errorf("the child's arguments = %q, want %q", arguments, want)
	}
	environment, err := os.ReadFile(environmentFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(environment), "\n")
	for _, variable := range []string{"PTAH_OCI_USERNAME=robot", runner.EnvOCIPassword + "=" + fetchTestPassword} {
		if !slices.Contains(lines, variable) {
			t.Errorf("the child does not hold %s", variable)
		}
	}
	if strings.Contains(string(environment), runner.EnvRunnerProtocolVersion) {
		t.Error("the child was told the runner's protocol")
	}
}

func TestRunFetchLeavesARegistryRefusalInTheLog(t *testing.T) {
	t.Parallel()

	const refused = `error: pull oci://registry.example/a: fetch OCI manifest: GET "http://registry.example/v2/": response status code 401: unauthorized`
	program, _, _ := fakePtah(t, refused, 2)
	terminationLog := mountedTerminationLog(t)
	var stdout, stderr bytes.Buffer
	if exit := run(context.Background(), fetchArguments(program), &stdout, &stderr, fetchEnvironment(), terminationLog); exit != 2 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), refused) {
		t.Errorf("the log = %q does not hold what the registry said", stderr.String())
	}
	if message, err := os.ReadFile(terminationLog); err != nil || len(message) != 0 {
		t.Fatalf("the termination message = %q, %v, want nothing", message, err)
	}
}

// A file the kubelet did not mount costs the reason and nothing else.
func TestRunFetchEndsAsThePullDidWithNoTerminationLog(t *testing.T) {
	t.Parallel()

	program, _, _ := fakePtah(t, "error: parse schema artifact: x", 2)
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "not-mounted")
	if exit := run(context.Background(), fetchArguments(program), &stdout, &stderr, fetchEnvironment(), missing); exit != 2 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "could not write the termination message") {
		t.Errorf("stderr = %q does not say the message was lost", stderr.String())
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("the runner created the file (stat error %v)", err)
	}
}

func TestRunFetchRefusesBeforeThePullStarts(t *testing.T) {
	t.Parallel()

	program, argumentsFile, _ := fakePtah(t, "", 0)
	nextProtocol := runner.EnvRunnerProtocolVersion + "=" + strconv.Itoa(runner.ProtocolVersion+1)
	for _, row := range []struct {
		name        string
		arguments   []string
		environment []string
		wantStderr  string
	}{
		{"no output file", []string{"--fetch-oci-schema", fetchTestReference, "--ptah-binary", program}, fetchEnvironment(), "schema fetch mode"},
		{"no reference", []string{"--fetch-oci-schema-to", fetchTestOut, "--ptah-binary", program}, fetchEnvironment(), "schema fetch mode"},
		{"another mode's flag", append(fetchArguments(program), "--operation", "observe"), fetchEnvironment(), "schema fetch mode"},
		{"an argument left over", append(fetchArguments(program), "extra"), fetchEnvironment(), "schema fetch mode"},
		{"a Job built for another protocol", fetchArguments(program), []string{nextProtocol}, runner.CodeRunnerProtocolMismatch},
		{"a Job naming no protocol", fetchArguments(program), []string{"HOME=/fetch-work"}, runner.CodeRunnerProtocolMismatch},
		{"a relative output file", []string{"--fetch-oci-schema", fetchTestReference, "--fetch-oci-schema-to", "schema.hcl", "--ptah-binary", program}, fetchEnvironment(), "absolute"},
	} {
		t.Run(row.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if exit := run(context.Background(), row.arguments, &stdout, &stderr, row.environment, ""); exit != 2 {
				t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
			}
			if !strings.Contains(stderr.String(), row.wantStderr) {
				t.Errorf("stderr = %q, want %q", stderr.String(), row.wantStderr)
			}
			if _, err := os.Stat(argumentsFile); !os.IsNotExist(err) {
				t.Fatalf("the pull started (stat error %v)", err)
			}
		})
	}
}
