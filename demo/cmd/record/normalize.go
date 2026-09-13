package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// normalizer rewrites the identifiers a lab generates, and refuses to publish a
// credential.
//
// The two are one pass because both read the same environment, and because a
// transcript is written once: what a reader sees is the output of this, and
// there is no second place where either decision could be made differently.
type normalizer struct {
	rules   []rewrite
	secrets []string
}

type rewrite struct {
	pattern     *regexp.Regexp
	replacement string
}

// newNormalizer builds the rewrites from the lab's own names.
//
// Built, not written down: a run identifier is in the cluster name, both
// namespaces, the Helm release and the registry authority, and a list of
// literals here would be a list that goes stale the first time the bootstrap
// names something differently.
func newNormalizer(live lab) (normalizer, error) {
	built := normalizer{rules: append([]rewrite(nil), technicalIdentifiers...)}

	// Longest first: the test namespace is a substring of the registry
	// authority, and rewriting the short one first would leave the long one
	// half-rewritten.
	// The chart derives several object names from this one: a suffix is added
	// and the whole is truncated to fit a 63-character label. So the pattern
	// matches a prefix of the name and keeps whatever the chart appended,
	// which is what tells a cert-rotator Deployment from the controller's.
	if name, ok := live.values["E2E_CONTROLLER_NAME"]; ok && name != "" {
		built.rules = append(built.rules, rewrite{
			pattern:     regexp.MustCompile(prefixPattern(name, 24) + `((?:-[a-z0-9]+)*)`),
			replacement: "ptah-operator$1",
		})
	}

	for _, replacement := range []struct{ variable, stable string }{
		{"E2E_KIND_CLUSTER_NAME", "ptah-demo"},
		{"E2E_OPERATOR_NAMESPACE", "ptah-system"},
		{"E2E_TEST_NAMESPACE", "demo"},
		{"E2E_FOREIGN_NAMESPACE", "demo-other"},
		{"E2E_HELM_RELEASE", "ptah"},
	} {
		value, ok := live.values[replacement.variable]
		if !ok || value == "" {
			continue
		}
		built.rules = append(built.rules, rewrite{
			pattern:     regexp.MustCompile(regexp.QuoteMeta(value)),
			replacement: replacement.stable,
		})
	}

	secrets, err := labSecrets(live)
	if err != nil {
		return normalizer{}, err
	}
	built.secrets = secrets
	return built, nil
}

// technicalIdentifiers are the rewrites that hold for any lab.
//
// Only identifiers, and only for display. A UID, a generated Pod suffix and a
// timestamp differ every run, and leaving them in would make two recordings of
// the same scenario differ in ways that mean nothing. What is never rewritten
// is anything that carries meaning: the order of events, the plan, the
// fingerprint an approval names, or the reason on a condition.
var technicalIdentifiers = []rewrite{
	{regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "<uid>"},
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z\b`), "<time>"},
}

// labSecrets reads the values that must never appear in a transcript.
//
// They are read from the credential files the bootstrap wrote, so the list is
// the lab's actual credentials rather than a pattern that hopes to match them.
func labSecrets(live lab) ([]string, error) {
	var secrets []string
	for _, variable := range []string{
		"E2E_REGISTRY_CREDENTIALS_FILE",
		"E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE",
	} {
		path, ok := live.values[variable]
		if !ok || path == "" {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read the lab credentials named by %s: %w", variable, err)
		}
		var credentials map[string]any
		if err := json.Unmarshal(content, &credentials); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		for name, value := range credentials {
			text, ok := value.(string)
			if !ok || text == "" {
				continue
			}
			// The username and the database name are not credentials, and they
			// appear in the transcript on purpose: a reader has to see which
			// identity the operator connected as.
			if name == "username" || name == "database" {
				continue
			}
			secrets = append(secrets, text)
		}
	}
	return secrets, nil
}

// apply rewrites one piece of text for publication.
func (n normalizer) apply(text string) string {
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		for _, rule := range n.rules {
			line = replaceKeepingColumns(line, rule)
		}
		lines[index] = line
	}
	return strings.Join(lines, "\n")
}

// replaceKeepingColumns applies one rewrite and leaves a padded table aligned.
//
// kubectl pads a column to its widest value, so shortening a name inside one
// moves every column after it. Where the replacement sits in a column -- the
// text after it is a space -- it is padded back to the width it replaced, and
// the columns stay where the command put them. Nothing but spaces is added,
// trailing ones are dropped, and a name in prose is left exactly as it was.
func replaceKeepingColumns(line string, rule rewrite) string {
	var built strings.Builder
	rest := line
	for {
		match := rule.pattern.FindStringIndex(rest)
		if match == nil {
			built.WriteString(rest)
			return strings.TrimRight(built.String(), " ")
		}
		replacement := rule.pattern.ReplaceAllString(rest[match[0]:match[1]], rule.replacement)
		built.WriteString(rest[:match[0]])
		built.WriteString(replacement)
		rest = rest[match[1]:]

		padding := (match[1] - match[0]) - len(replacement)
		if padding > 0 && strings.HasPrefix(rest, " ") {
			built.WriteString(strings.Repeat(" ", padding))
		}
	}
}

// audit refuses text carrying a lab credential.
//
// A recording is committed and published. The operator exists to keep a
// database password out of the places a schema change would otherwise carry it,
// and a demonstration of that which leaked one would be the exact failure it
// claims to prevent. So this is a refusal, never a redaction: a transcript that
// reached a password reached it through a command the scenario asked for, and
// the repair is the scenario.
func (n normalizer) audit(where, text string) error {
	for _, secret := range n.secrets {
		if strings.Contains(text, secret) {
			return fmt.Errorf(
				"%s printed one of the lab's credentials; a recording is published, so the "+
					"step has to stop asking for it rather than have it removed here", where)
		}
	}
	return nil
}

// prefixPattern matches the whole of a name and every prefix of it at least
// floor characters long, longest first.
//
// Nested optional groups rather than a list of alternatives: a regular
// expression prefers the leftmost longest match through a nested group, so the
// full name wins over its truncation wherever both would match.
func prefixPattern(name string, floor int) string {
	if len(name) <= floor {
		return regexp.QuoteMeta(name)
	}
	tail := ""
	for index := len(name) - 1; index >= floor; index-- {
		tail = "(?:" + regexp.QuoteMeta(string(name[index])) + tail + ")?"
	}
	return regexp.QuoteMeta(name[:floor]) + tail
}
