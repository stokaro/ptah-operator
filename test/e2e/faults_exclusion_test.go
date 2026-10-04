package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestExcludedPolicyDocumentsFromNativePlans(t *testing.T) {
	t.Parallel()
	for _, engine := range []string{"postgresql", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			t.Parallel()
			read := func(scope string) planDocument {
				t.Helper()
				raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", engine+"-schema-exclusion-"+scope+"-plan.json"))
				if err != nil {
					t.Fatal(err)
				}
				var plan planDocument
				if err := json.Unmarshal(raw, &plan); err != nil {
					t.Fatal(err)
				}
				return plan
			}
			if err := excludedPolicyDocuments(read("wide"), read("narrow")); err != nil {
				t.Fatalf("refused the actual executor plans: %v", err)
			}
			for name, edit := range map[string]func(*planDocument, *planDocument){
				"comment only": func(o, _ *planDocument) {
					o.Statements[0].SQL = "-- ALTER TABLE e2e_widgets ADD COLUMN fault_token text"
				},
				"executable comment": func(_, n *planDocument) {
					n.Statements[0].SQL = "/*! DROP TABLE e2e_widgets */\n" + n.Statements[0].SQL
				},
				"statement before addition": func(_, n *planDocument) { n.Statements[0].SQL = "DELETE FROM e2e_widgets;\n" + n.Statements[0].SQL },
				"statement after addition":  func(_, n *planDocument) { n.Statements[0].SQL += "; DROP TABLE e2e_widgets" },
				"commented drop": func(o, _ *planDocument) {
					o.Statements[len(o.Statements)-1].SQL = "-- DROP TABLE e2e_excluded_policy_keep\nSELECT 1"
				},
				"extra constraint": func(o, _ *planDocument) {
					o.Statements = append(o.Statements, planStatement{SQL: `ALTER TABLE "e2e_excluded_policy_keep" DROP CONSTRAINT "another_constraint"`})
				},
				"PostgreSQL constraint on MySQL": func(o, n *planDocument) {
					o.Dialect, n.Dialect = "mysql", "mysql"
					if engine == "mysql" {
						o.Statements = append(o.Statements, planStatement{SQL: `ALTER TABLE "e2e_excluded_policy_keep" DROP CONSTRAINT "e2e_excluded_policy_keep_pkey"`})
					}
				},
			} {
				old, current := read("wide"), read("narrow")
				edit(&old, &current)
				if excludedPolicyDocuments(old, current) == nil {
					t.Errorf("accepted %s", name)
				}
			}
			if engine == "postgresql" {
				for _, from := range []string{"e2e_excluded_policy_keep_pkey", `"e2e_excluded_policy_keep"`} {
					old, current := read("wide"), read("narrow")
					old.Statements[1].SQL = strings.ReplaceAll(old.Statements[1].SQL, from, "unrelated")
					if excludedPolicyDocuments(old, current) == nil {
						t.Errorf("accepted changed primary-key removal: %s", from)
					}
				}
				old, current := read("wide"), read("narrow")
				old.Statements[0] = old.Statements[1]
				if excludedPolicyDocuments(old, current) == nil {
					t.Error("accepted duplicate constraint removal without the retained addition")
				}
			}
		})
	}
}

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
	withConstraint := old.DeepCopy()
	withConstraint.Spec.StatementCount = 3
	if err := changedExcludedPolicyPlan(withConstraint, current); err != nil {
		t.Fatalf("refused the PostgreSQL plan's separate primary-key removal: %v", err)
	}
	for _, count := range []int32{0, 1} {
		incomplete := old.DeepCopy()
		incomplete.Spec.StatementCount = count
		if changedExcludedPolicyPlan(incomplete, current) == nil {
			t.Errorf("accepted an original selection with only %d statements", count)
		}
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
