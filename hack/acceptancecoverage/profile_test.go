package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A declared profile is the only way a disposition other than "Not assessed"
// reaches the record, so every refusal below is a way the record could
// otherwise say more than the deployment did.

const completeProfile = `{
  "managerImageDigest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
  "runnerImageDigest": "sha256:2222222222222222222222222222222222222222222222222222222222222222",
  "executorImageDigest": "sha256:3333333333333333333333333333333333333333333333333333333333333333",
  "chartDigest": "sha256:4444444444444444444444444444444444444444444444444444444444444444",
  "installationValues": "values-production.yaml at rev 9",
  "operatingTargets": "p99 reconcile under 4s, 200 resources",
  "recoveryObjectives": "database RPO 5m/RTO 1h; operator state RPO 24h/RTO 30m",
  "runEvidence": "run 36030331644, jobs 1.35/1.36/1.37 lifecycle",
  "requirements": {
    "PA-01": {"disposition": "Accepted for the stated profile", "evidence": "run 36030331644 coverage table"}
  }
}`

func writeProfile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestACompleteProfileIsRead(t *testing.T) {
	t.Parallel()

	declared, err := readProfile(writeProfile(t, completeProfile))
	if err != nil {
		t.Fatalf("a complete profile was refused: %v", err)
	}
	if got := declared.verdictFor("PA-01").Disposition; got != dispositionAccepted {
		t.Fatalf("PA-01 read as %q", got)
	}
	// Every requirement the profile does not name is not assessed. That is what
	// an empty profile means for every row, and what a partial one means for
	// the rest.
	if got := declared.verdictFor("PA-07").Disposition; got != dispositionNotAssessed {
		t.Fatalf("a requirement the profile does not name read as %q", got)
	}
	if missing := declared.unfilled(); len(missing) != 0 {
		t.Fatalf("a complete profile reported %v unfilled", missing)
	}
}

func TestAProfileThatWouldOverstateTheRecordIsRefused(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		edit    func(string) string
		refusal string
	}{
		{
			// A tag names bytes that can be replaced. The record binds results
			// to bytes that cannot.
			name: "a tag where a digest belongs",
			edit: func(s string) string {
				return strings.Replace(s, "sha256:1111111111111111111111111111111111111111111111111111111111111111", "v0.1.0", 1)
			},
			refusal: "not a lowercase sha256 digest",
		},
		{
			name:    "a requirement this record does not carry",
			edit:    func(s string) string { return strings.Replace(s, `"PA-01"`, `"PA-13"`, 1) },
			refusal: "not a requirement this record carries",
		},
		{
			name:    "a verdict the issue does not define",
			edit:    func(s string) string { return strings.Replace(s, dispositionAccepted, "Probably fine", 1) },
			refusal: "which is not one of",
		},
		{
			// The line #242 draws: a pass is evidence, not an opinion.
			name: "accepted with no evidence",
			edit: func(s string) string {
				return strings.Replace(s, `"evidence": "run 36030331644 coverage table"`, `"evidence": ""`, 1)
			},
			refusal: "accepted with no evidence",
		},
		{
			// "An unfilled capacity or recovery target leaves acceptance
			// incomplete" -- so it cannot sit beside an accepted requirement.
			name: "accepted while a recovery objective is unfilled",
			edit: func(s string) string {
				return strings.Replace(s, `"database RPO 5m/RTO 1h; operator state RPO 24h/RTO 30m"`, `""`, 1)
			},
			refusal: "leaves recoveryObjectives unfilled",
		},
		{
			// A misspelled field would otherwise leave the value unfilled and
			// the record would print "to be supplied" beside a profile whose
			// author believes they supplied it.
			name:    "a field name the profile does not have",
			edit:    func(s string) string { return strings.Replace(s, `"chartDigest"`, `"chartDigests"`, 1) },
			refusal: "read the acceptance profile",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			_, err := readProfile(writeProfile(t, row.edit(completeProfile)))
			if err == nil {
				t.Fatal("the profile was accepted")
			}
			if !strings.Contains(err.Error(), row.refusal) {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
		})
	}
}

// An absent profile leaves every row not assessed and every candidate value to
// be supplied, which is what the record said before a profile could be read at
// all.
func TestTheRecordWithoutAProfileAssessesNothing(t *testing.T) {
	t.Parallel()

	coverage, err := buildCoverage("../..", "edge")
	if err != nil {
		t.Fatal(err)
	}
	page := coverage.recordMarkdown("../..", nil)
	if count := strings.Count(page, "| Not assessed |"); count != len(requirements) {
		t.Fatalf("%d of %d requirements read as not assessed", count, len(requirements))
	}
	if !strings.Contains(page, "_to be supplied: the digest the release publishes_") {
		t.Fatal("the record claimed an image digest it was never given")
	}
}

// With a profile the record carries the disposition and the evidence beside it,
// because a verdict whose evidence is not written down is the thing #242
// refuses to accept.
func TestTheRecordShowsTheEvidenceBehindADisposition(t *testing.T) {
	t.Parallel()

	declared, err := readProfile(writeProfile(t, completeProfile))
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := buildCoverage("../..", "edge")
	if err != nil {
		t.Fatal(err)
	}
	page := coverage.recordMarkdown("../..", declared)
	if !strings.Contains(page, dispositionAccepted+" — run 36030331644 coverage table") {
		t.Fatal("the record shows a disposition without the evidence it rests on")
	}
	if !strings.Contains(page, "sha256:1111111111111111111111111111111111111111111111111111111111111111") {
		t.Fatal("the record does not name the manager digest the profile declared")
	}
	if strings.Count(page, "| Not assessed |") != len(requirements)-1 {
		t.Fatal("the profile changed more rows than it named")
	}
}

// An exclusion is the cheapest way to turn a failing requirement into a
// passing record, so #242 fences it: narrow the use, name an owner and a
// review date, say how the excluded configuration is prevented or detected,
// and never waive the four protections. These are that fence.
func TestAnExclusionThatWouldWaiveAProtectionIsRefused(t *testing.T) {
	t.Parallel()

	valid := `,"exclusions":[{"requirement":"PA-09","scope":"no alerting in the trial profile",` +
		`"owner":"platform team","reviewBy":"2027-03-01","detection":"install refuses without a receiver"}]`

	for _, row := range []struct {
		name    string
		body    string
		refusal string
	}{
		{
			name:    "a requirement that carries unauthorized mutation",
			body:    strings.Replace(valid, `"PA-09"`, `"PA-02"`, 1),
			refusal: "carries unauthorized mutation",
		},
		{
			name:    "a requirement that carries unsafe replay",
			body:    strings.Replace(valid, `"PA-09"`, `"PA-03"`, 1),
			refusal: "unsafe replay or overlap",
		},
		{
			name:    "a requirement that carries credential disclosure",
			body:    strings.Replace(valid, `"PA-09"`, `"PA-05"`, 1),
			refusal: "credential disclosure",
		},
		{
			name:    "an exclusion with no owner",
			body:    strings.Replace(valid, `"owner":"platform team"`, `"owner":""`, 1),
			refusal: "names no owner",
		},
		{
			name:    "a review date that is not a date",
			body:    strings.Replace(valid, `"2027-03-01"`, `"when we get to it"`, 1),
			refusal: "never comes back for review",
		},
		{
			name:    "a requirement this record does not carry",
			body:    strings.Replace(valid, `"PA-09"`, `"PA-99"`, 1),
			refusal: "not a requirement this record carries",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			body := strings.Replace(completeProfile, "\n}", row.body+"\n}", 1)
			if _, err := readProfile(writeProfile(t, body)); err == nil {
				t.Fatal("the exclusion was accepted")
			} else if !strings.Contains(err.Error(), row.refusal) {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
		})
	}
}

// "Every applicable requirement must pass." A decision is the one place a
// record can say that once, about everything, so it is the one most worth
// refusing.
func TestADecisionTheRequirementsDoNotSupportIsRefused(t *testing.T) {
	t.Parallel()

	accepted := `,"decision":"Accepted for the stated profile"`

	t.Run("accepted with eleven requirements unassessed", func(t *testing.T) {
		t.Parallel()

		body := strings.Replace(completeProfile, "\n}", accepted+"\n}", 1)
		_, err := readProfile(writeProfile(t, body))
		if err == nil {
			t.Fatal("the decision was accepted")
		}
		if !strings.Contains(err.Error(), "every applicable requirement must pass") {
			t.Fatalf("refused for the wrong reason: %v", err)
		}
		if !strings.Contains(err.Error(), "PA-02") {
			t.Fatalf("the refusal does not name what is outstanding: %v", err)
		}
	})

	t.Run("a decision the issue does not define", func(t *testing.T) {
		t.Parallel()

		body := strings.Replace(completeProfile, "\n}", `,"decision":"Shipped"`+"\n}", 1)
		_, err := readProfile(writeProfile(t, body))
		if err == nil || !strings.Contains(err.Error(), "which is not one of") {
			t.Fatalf("a decision outside the three was not refused: %v", err)
		}
	})

	t.Run("rejected needs nothing, because it claims nothing", func(t *testing.T) {
		t.Parallel()

		body := strings.Replace(completeProfile, "\n}", `,"decision":"Rejected"`+"\n}", 1)
		if _, err := readProfile(writeProfile(t, body)); err != nil {
			t.Fatalf("a rejection was refused: %v", err)
		}
	})
}
