package e2e

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestExcludedPolicyDocumentsRemoveOnlyTheExcludedDrop(t *testing.T) {
	t.Parallel()
	for _, dialect := range []string{"postgres", "mysql"} {
		t.Run(dialect, func(t *testing.T) {
			t.Parallel()
			add, drop := `ALTER TABLE "public"."e2e_widgets" ADD COLUMN "fault_token" text;`, `DROP TABLE "public"."e2e_excluded_policy_keep";`
			if dialect == "mysql" {
				add, drop = "ALTER TABLE `e2e_widgets` ADD COLUMN `fault_token` varchar(255) NULL;", "DROP TABLE IF EXISTS `e2e_excluded_policy_keep`;"
			}
			plans := func() (planDocument, planDocument) {
				return planDocument{FormatVersion: 1, Dialect: dialect, Destructive: ptr.To(true),
						Statements: []planStatement{{SQL: add}, {SQL: drop}}},
					planDocument{FormatVersion: 1, Dialect: dialect, Destructive: ptr.To(false), Exclude: []string{excludedPolicyTable},
						Statements: []planStatement{{SQL: add}}}
			}
			old, current := plans()
			if err := excludedPolicyDocuments(old, current); err != nil {
				t.Fatal(err)
			}
			for name, edit := range map[string]func(*planDocument, *planDocument){
				"not excluded":            func(_, n *planDocument) { n.Exclude = nil },
				"other exclusion":         func(_, n *planDocument) { n.Exclude[0] = "another_table" },
				"already excluded":        func(o, n *planDocument) { o.Exclude = n.Exclude },
				"extra exclusion":         func(_, n *planDocument) { n.Exclude = append(n.Exclude, "e2e_widgets") },
				"still destructive":       func(_, n *planDocument) { n.Destructive = ptr.To(true) },
				"unclassified original":   func(o, _ *planDocument) { o.Destructive = nil },
				"empty replacement":       func(_, n *planDocument) { n.Statements = nil },
				"drop retained":           func(o, n *planDocument) { n.Statements = o.Statements },
				"drop another table":      func(o, _ *planDocument) { o.Statements[1].SQL = "DROP TABLE another_table;" },
				"drop only in a literal":  func(o, _ *planDocument) { o.Statements[1].SQL = "SELECT 'DROP TABLE e2e_excluded_policy_keep';" },
				"drop duplicated":         func(o, _ *planDocument) { o.Statements[0] = o.Statements[1] },
				"another retained change": func(o, _ *planDocument) { o.Statements[0].SQL = "ALTER TABLE e2e_widgets ADD COLUMN other text;" },
				"SQL after the drop":      func(o, _ *planDocument) { o.Statements[1].SQL += " DELETE FROM e2e_widgets;" },
				"SQL after the addition": func(o, n *planDocument) {
					n.Statements[0].SQL += " DROP TABLE e2e_excluded_policy_keep;"
					o.Statements[0] = n.Statements[0]
				},
				"missing dialect": func(o, n *planDocument) { o.Dialect, n.Dialect = "", "" },
				"wrong format":    func(_, n *planDocument) { n.FormatVersion = 2 },
			} {
				old, current := plans()
				edit(&old, &current)
				if excludedPolicyDocuments(old, current) == nil {
					t.Errorf("accepted %s", name)
				}
			}
		})
	}
}

func TestExclusionEditNeedsANewPlanForTheSameResourceAndArtifact(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("a", 64)
	old := &ptahv1alpha1.PtahSchemaPlan{
		ObjectMeta: metav1.ObjectMeta{UID: "old-plan"},
		Spec: ptahv1alpha1.PtahSchemaPlanSpec{
			SchemaRef:   ptahv1alpha1.ImmutableObjectReference{Name: "schema", UID: "schema-uid"},
			Fingerprint: "old-fingerprint", PolicyFingerprint: "old-policy", ArtifactDigest: digest,
			TargetIdentityDigest: digest, CoordinationDigest: digest, ActualStateFingerprint: digest,
			Destructive: true, StatementCount: 2,
		},
	}
	current := old.DeepCopy()
	current.UID, current.Spec.Fingerprint, current.Spec.PolicyFingerprint = "new-plan", "new-fingerprint", "new-policy"
	current.Spec.Destructive, current.Spec.StatementCount = false, 1
	current.Spec.ActualStateFingerprint = "sha256:" + strings.Repeat("b", 64)
	if err := changedExcludedPolicyPlan(old, current); err != nil {
		t.Fatalf("refused a narrowed scope, which may have its own state fingerprint: %v", err)
	}
	for name, edit := range map[string]func(*ptahv1alpha1.PtahSchemaPlan){
		"same plan":        func(p *ptahv1alpha1.PtahSchemaPlan) { p.UID = old.UID },
		"empty UID":        func(p *ptahv1alpha1.PtahSchemaPlan) { p.UID = "" },
		"same fingerprint": func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Fingerprint = old.Spec.Fingerprint },
		"empty policy":     func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.PolicyFingerprint = "" },
		"same policy":      func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.PolicyFingerprint = old.Spec.PolicyFingerprint },
		"another resource": func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.SchemaRef.UID = "another" },
		"another artifact": func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ArtifactDigest = current.Spec.ActualStateFingerprint },
		"another database": func(p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.TargetIdentityDigest = current.Spec.ActualStateFingerprint
		},
		"another realm":  func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.CoordinationDigest = current.Spec.ActualStateFingerprint },
		"no observation": func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ActualStateFingerprint = "" },
		"still destructive": func(p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.Destructive = true
		},
		"empty selection": func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.StatementCount = 0 },
		"extra work":      func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.StatementCount = 2 },
	} {
		candidate := current.DeepCopy()
		edit(candidate)
		if changedExcludedPolicyPlan(old, candidate) == nil {
			t.Errorf("accepted %s", name)
		}
	}
	if changedExcludedPolicyPlan(nil, current) == nil || changedExcludedPolicyPlan(old, nil) == nil {
		t.Fatal("accepted a missing plan")
	}
}
