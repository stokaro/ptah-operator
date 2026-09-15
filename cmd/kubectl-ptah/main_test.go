package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// What the command answers before it reaches a cluster. Each of these is a
// reader's mistake, and each has a status a script can act on.
func TestRunUsagePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		arguments []string
		status    int
		says      string
	}{
		{name: "no arguments", arguments: nil, status: exitUsage, says: "usage: kubectl ptah plan"},
		{name: "help", arguments: []string{"--help"}, status: exitOK, says: "usage: kubectl ptah plan"},
		{name: "version", arguments: []string{"--version"}, status: exitOK, says: "kubectl-ptah"},
		{
			name:      "another command",
			arguments: []string{"schema", "storefront"},
			status:    exitUsage,
			says:      `unknown command "schema"`,
		},
		{
			name:      "no schema named",
			arguments: []string{"plan"},
			status:    exitUsage,
			says:      "name one PtahSchema",
		},
		{
			name:      "two schemas named",
			arguments: []string{"plan", "storefront", "warehouse"},
			status:    exitUsage,
			says:      "name one PtahSchema",
		},
		{
			// They are two different plans, and picking one for the reader
			// would answer a question they did not ask.
			name:      "both plans at once",
			arguments: []string{"plan", "storefront", "--current", "--applied"},
			status:    exitUsage,
			says:      "ask for one",
		},
		{
			name:      "an output format that does not exist",
			arguments: []string{"plan", "storefront", "-o", "yaml"},
			status:    exitUsage,
			says:      "unknown output format",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer

			status := run(context.Background(), test.arguments, &stdout, &stderr)

			if status != test.status {
				t.Fatalf("run(%v) = %d, want %d\n%s%s", test.arguments, status, test.status, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String()+stderr.String(), test.says) {
				t.Fatalf("run(%v) did not say %q:\n%s%s", test.arguments, test.says, stdout.String(), stderr.String())
			}
		})
	}
}

// A usage failure writes nothing to stdout that a pipe would take for a plan.
func TestRunWritesNoPlanWhenItRefusesTheCommandLine(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer

	status := run(context.Background(), []string{"plan", "storefront", "-o", "yaml"}, &stdout, &stderr)

	if status == exitOK {
		t.Fatal("run() accepted an output format it does not have")
	}
	if stdout.Len() != 0 {
		t.Fatalf("run() wrote %q to stdout while refusing the command line", stdout.String())
	}
}

// The help says which plan the command reads without one of the two flags.
func TestUsageStatesTheDefaultSelection(t *testing.T) {
	t.Parallel()
	var help bytes.Buffer

	usage(&help)

	if !strings.Contains(help.String(), "--current              the plan the operator would run next (the default)") {
		t.Fatalf("the help does not say which plan is the default:\n%s", help.String())
	}
	if !strings.Contains(help.String(), "log of what each statement did") {
		t.Fatalf("the help does not say what applied is not:\n%s", help.String())
	}
}

// A reader writes the schema first and the flags after it, the way every
// kubectl command accepts them. The standard library's flag package stops at
// the first argument that is not a flag, so this is a property of the parser
// this command uses rather than of the arguments.
func TestRunReadsFlagsWrittenAfterTheSchemaName(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer

	// Two selections after the name: refused for being two, which is only
	// visible if both were read at all.
	status := run(context.Background(), []string{"plan", "storefront", "--current", "--applied"}, &stdout, &stderr)

	if status != exitUsage {
		t.Fatalf("run() = %d, want %d\n%s%s", status, exitUsage, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "ask for one") {
		t.Fatalf("run() did not read the flags after the name:\n%s", stderr.String())
	}
}

// `kubectl ptah plan --help` is a question, not a mistake.
func TestRunAnswersTheHelpUnderThePlanCommand(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer

	status := run(context.Background(), []string{"plan", "--help"}, &stdout, &stderr)

	if status != exitOK {
		t.Fatalf("run(plan --help) = %d, want %d", status, exitOK)
	}
	if !strings.Contains(stdout.String()+stderr.String(), "usage: kubectl ptah plan") {
		t.Fatalf("run(plan --help) printed no usage:\n%s%s", stdout.String(), stderr.String())
	}
}

func TestMigrationRefusesAnAmbiguousCommandLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		arguments []string
		want      int
	}{
		{name: "no migration named", arguments: []string{"migration"}, want: exitUsage},
		{name: "two migrations named", arguments: []string{"migration", "orders", "invoices"}, want: exitUsage},
		{name: "unknown format", arguments: []string{"migration", "orders", "-o", "yaml"}, want: exitUsage},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if status := run(context.Background(), test.arguments, &stdout, &stderr); status != test.want {
				t.Fatalf("status = %d, want %d (%s)", status, test.want, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("a refused command line printed %q", stdout.String())
			}
		})
	}
}

func TestUsageNamesBothCommands(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if status := run(context.Background(), []string{"help"}, &stdout, &stderr); status != exitOK {
		t.Fatalf("status = %d", status)
	}
	for _, want := range []string{"kubectl ptah plan", "kubectl ptah migration"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("usage does not name %q:\n%s", want, stdout.String())
		}
	}
}
