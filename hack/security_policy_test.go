package main

import (
	"regexp"
	"strings"
	"testing"
)

// A reporter who has found a way across one of the operator's boundaries has
// two choices: a private channel, or the public tracker. Which one they take
// is decided by what the project tells them, and the project told them
// nothing: there was no policy, and no page named a way to report at all.
//
// These hold the policy to being reachable from where a reporter actually
// stands -- the issue chooser and the contributing page -- because a policy
// nobody is shown is the same as no policy.

const (
	securityPolicy   = "SECURITY.md"
	issueChooser     = ".github/ISSUE_TEMPLATE/config.yml"
	contributingPage = "CONTRIBUTING.md"
)

var privateChannel = regexp.MustCompile(`[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}|https://github\.com/[^\s)]+/security/advisories`)

// The policy names somewhere to send a report.
func TestTheSecurityPolicyNamesAChannel(t *testing.T) {
	t.Parallel()
	policy := readDocumentationPage(t, securityPolicy)
	if !privateChannel.MatchString(policy) {
		t.Fatalf("%s names no address and no advisory page, so it tells a reporter to work it out", securityPolicy)
	}
}

// The issue chooser offers it. This is the screen a reporter is on when they
// decide, and a policy it does not list is one they do not see.
func TestTheIssueChooserOffersTheSecurityPolicy(t *testing.T) {
	t.Parallel()
	chooser := readDocumentationPage(t, issueChooser)
	if !strings.Contains(chooser, securityPolicy) {
		t.Fatalf("%s offers no link to %s, so the chooser sends a vulnerability to the public tracker with everything else",
			issueChooser, securityPolicy)
	}
}

// And the page about opening an issue says the same.
func TestContributingSendsAVulnerabilityToThePolicy(t *testing.T) {
	t.Parallel()
	contributing := readDocumentationPage(t, contributingPage)
	if !strings.Contains(contributing, "("+securityPolicy+")") {
		t.Fatalf("%s does not link to %s, so the page about filing an issue does not say which reports are not filed",
			contributingPage, securityPolicy)
	}
}
