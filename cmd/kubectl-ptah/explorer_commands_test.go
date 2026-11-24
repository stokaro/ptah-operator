package main

import (
	"bytes"
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
)

// explorerData is the lifecycle explorer's content on the documentation site.
// Its stages, objects and audit sources show readers kubectl ptah commands to
// copy.
const explorerData = "../../docs/site/src/lifecycle-explorer.mjs"

var (
	// explorerCommand is a `command:` value in single quotes that is a
	// kubectl ptah command, as the explorer writes every one.
	explorerCommand = regexp.MustCompile(`command:\s*'(kubectl ptah [^'\\]*)'`)
	// anyExplorerCommand is a `command:` value that starts with kubectl ptah
	// in any quoting, so a command written another way is counted, not missed.
	anyExplorerCommand = regexp.MustCompile("command:\\s*['\"`]kubectl ptah ")
)

// parses runs a command as the plugin would with --help after it. Parsing
// stops there, before any client is built, so a verb the plugin does not have
// or a flag the verb does not define is the only thing that can fail it. A
// flag's value is not checked: the plugin refuses `-o yaml` after parsing, and
// with --help it never gets there.
func parses(command string) (int, string) {
	arguments := append(strings.Fields(strings.TrimPrefix(command, "kubectl ptah ")), "--help")
	var stdout, stderr bytes.Buffer
	status := run(context.Background(), arguments, &stdout, &stderr)
	return status, stderr.String()
}

// Every kubectl ptah command the lifecycle explorer shows is one this plugin
// parses: its verb exists and each flag it passes is one that verb defines.
// The explorer is where a reader copies them from, and a flag renamed here
// would otherwise leave the page showing one that fails.
func TestExplorerCommandsParse(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile(explorerData)
	if err != nil {
		t.Fatal(err)
	}
	matches := explorerCommand.FindAllStringSubmatch(string(content), -1)
	if len(matches) == 0 {
		t.Fatalf("read no kubectl ptah command from %s", explorerData)
	}
	if written := len(anyExplorerCommand.FindAllString(string(content), -1)); written != len(matches) {
		t.Fatalf("%s writes %d kubectl ptah commands and %d were read: write each in single quotes", explorerData, written, len(matches))
	}
	seen := map[string]bool{}
	for _, match := range matches {
		command := match[1]
		if seen[command] {
			continue
		}
		seen[command] = true
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			if status, stderr := parses(command); status != exitOK {
				t.Errorf("exit %d: %s", status, strings.TrimSpace(stderr))
			}
		})
	}
}

// The check refuses what it exists to catch, and admits a command that holds.
func TestExplorerCommandCheckRefuses(t *testing.T) {
	t.Parallel()
	for name, command := range map[string]string{
		"a verb the plugin does not have": "kubectl ptah acknowledge migration/orders -n application",
		"a flag no verb defines":          "kubectl ptah schema application -n application --detail",
		"another verb's flag":             "kubectl ptah history schema/application --applied -n application",
	} {
		if status, _ := parses(command); status == exitOK {
			t.Errorf("%s: %q parsed", name, command)
		}
	}
	if status, stderr := parses("kubectl ptah plan application --applied -n application"); status != exitOK {
		t.Errorf("a supported command was refused with exit %d: %s", status, stderr)
	}
	if got := explorerCommand.FindAllStringSubmatch("inspect: [{ command: 'kubectl ptah schema a -n b', says: 'x' }]", -1); len(got) != 1 {
		t.Errorf("the pattern read %v", got)
	}
	if got := anyExplorerCommand.FindAllString("command: `kubectl ptah schema a`, command: \"kubectl ptah plan a\"", -1); len(got) != 2 {
		t.Errorf("the count read %v", got)
	}
}
