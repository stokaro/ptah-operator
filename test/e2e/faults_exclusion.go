package e2e

import (
	"errors"
	"regexp"
	"slices"
	"strings"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const excludedPolicyTable = "e2e_excluded_policy_keep"

// These patterns describe the two statements of this controlled fixture, not
// an allowlist for arbitrary SQL received by the database.
var (
	exclusionQualifier = `(?:(?:public|"public"|` + "`public`" + `|e2e_exclusion_policy|"e2e_exclusion_policy"|` + "`e2e_exclusion_policy`" + `)\.)?`
	exclusionDrop      = regexp.MustCompile(`(?i)^\s*DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?` + exclusionQualifier +
		`(?:e2e_excluded_policy_keep|"e2e_excluded_policy_keep"|` + "`e2e_excluded_policy_keep`" + `)(?:\s+(?:CASCADE|RESTRICT))?\s*;?\s*$`)
	exclusionAdd = regexp.MustCompile(`(?i)^\s*ALTER\s+TABLE\s+` + exclusionQualifier +
		`(?:e2e_widgets|"e2e_widgets"|` + "`e2e_widgets`" + `)\s+ADD\s+(?:COLUMN\s+)?` +
		`(?:fault_token|"fault_token"|` + "`fault_token`" + `)\s+(?:text|varchar\s*\(255\))(?:\s+NULL)?\s*;?\s*$`)
)

func changedExcludedPolicyPlan(old, current *ptahv1alpha1.PtahSchemaPlan) error {
	if old == nil || current == nil || old.UID == "" || current.UID == "" || old.UID == current.UID {
		return errors.New("the exclusion edit has no distinct immutable plans")
	}
	a, b := old.Spec, current.Spec
	if a.Fingerprint == "" || b.Fingerprint == "" || a.Fingerprint == b.Fingerprint ||
		a.PolicyFingerprint == "" || b.PolicyFingerprint == "" || a.PolicyFingerprint == b.PolicyFingerprint {
		return errors.New("the exclusion edit reused the original approval or policy decision")
	}
	if a.SchemaRef.Name == "" || a.SchemaRef.UID == "" || a.SchemaRef != b.SchemaRef ||
		!sha256Pattern.MatchString(a.ArtifactDigest) || a.ArtifactDigest != b.ArtifactDigest ||
		!sha256Pattern.MatchString(a.TargetIdentityDigest) || a.TargetIdentityDigest != b.TargetIdentityDigest ||
		!sha256Pattern.MatchString(a.CoordinationDigest) || a.CoordinationDigest != b.CoordinationDigest ||
		!sha256Pattern.MatchString(a.ActualStateFingerprint) || !sha256Pattern.MatchString(b.ActualStateFingerprint) {
		return errors.New("the exclusion edit has unrelated or incomplete input bindings")
	}
	// A scope edit can change the native state fingerprint. Database equality
	// is checked directly around the edit, independently of that fingerprint.
	if !a.Destructive || b.Destructive || a.StatementCount != 2 || b.StatementCount != 1 {
		return errors.New("the exclusion edit did not remove exactly the destructive statement")
	}
	return nil
}

func excludedPolicyDocuments(old, current planDocument) error {
	if old.FormatVersion != 1 || current.FormatVersion != 1 || old.Dialect != current.Dialect ||
		(old.Dialect != "postgres" && old.Dialect != "mysql") ||
		len(old.Exclude) != 0 || !slices.Equal(current.Exclude, []string{excludedPolicyTable}) ||
		!isTrue(old.Destructive) || !isFalse(current.Destructive) || len(old.Statements) != 2 || len(current.Statements) != 1 {
		return errors.New("the native plans do not carry the original and narrowed scopes")
	}
	kept := strings.TrimSpace(current.Statements[0].SQL)
	if !exclusionAdd.MatchString(kept) {
		return errors.New("the narrowed plan does not only add the fixture column")
	}
	drops, retained := 0, 0
	for _, statement := range old.Statements {
		switch {
		case exclusionDrop.MatchString(statement.SQL):
			drops++
		case strings.TrimSpace(statement.SQL) == kept:
			retained++
		}
	}
	if drops != 1 || retained != 1 {
		return errors.New("the original plan did not drop the excluded table and add the same fixture column")
	}
	return nil
}
