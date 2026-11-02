package approve

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/planview"
	"github.com/stokaro/ptah-operator/internal/statusview"
)

// RenderReview writes what a person is asked to approve: the identities the
// approval will carry, then the plan they identify. A schema plan is its
// verified SQL with each statement's severity and reason. A migration plan is
// its versions and checksums and the source its SQL is read from; the SQL is
// not in the plan, and this says so rather than leaving a reader to think it
// was shown.
func RenderReview(out io.Writer, review Review) error {
	var text strings.Builder
	field := func(name, value string) {
		fmt.Fprintf(&text, "%-15s%s\n", name+":", value)
	}
	field(review.Family.Kind(), fmt.Sprintf("%s/%s, UID %s", review.Namespace, review.Name, statusview.Clean(string(review.ResourceUID))))
	field("Plan", fmt.Sprintf("%s, UID %s", statusview.Clean(review.PlanName), statusview.Clean(string(review.PlanUID))))
	field("Fingerprint", statusview.Clean(review.Fingerprint))
	text.WriteString("\n")
	if _, err := io.WriteString(out, text.String()); err != nil {
		return err
	}
	switch review.Family {
	case Migration:
		return renderMigrationPlan(out, review)
	default:
		if review.SchemaPlan == nil {
			return fmt.Errorf("the review carries no schema plan")
		}
		return planview.Render(out, *review.SchemaPlan, planview.Text)
	}
}

func renderMigrationPlan(out io.Writer, review Review) error {
	plan := review.MigrationPlan
	if plan == nil {
		return fmt.Errorf("the review carries no migration plan")
	}
	var text strings.Builder
	fmt.Fprintf(&text, "From version %d, in this order:\n", plan.Spec.CurrentVersion)
	for _, migration := range plan.Spec.Migrations {
		version := strconv.FormatInt(migration.Version, 10)
		if migration.VersionKey != "" {
			version = statusview.Clean(migration.VersionKey)
		}
		fmt.Fprintf(&text, "  %-16s %s\n", version, statusview.Clean(migration.Description))
		fmt.Fprintf(&text, "  %-16s checksum %s", "", statusview.Clean(migration.Checksum))
		if migration.Checkpoint {
			text.WriteString(", checkpoint")
		}
		if migration.TransactionMode != "" {
			fmt.Fprintf(&text, ", transaction %s", statusview.Clean(migration.TransactionMode))
		}
		text.WriteString("\n")
	}
	text.WriteString("\nA migration plan records versions and checksums. Its SQL is read from the\nsource the plan binds, and it is not shown here:\n")
	for _, line := range planSource(plan.Spec) {
		fmt.Fprintf(&text, "  %-16s %s\n", line[0]+":", line[1])
	}
	_, err := io.WriteString(out, text.String())
	return err
}

// planSource is the source a migration plan binds its SQL to, as the plan
// records it: the OCI artifact's digest, or the ConfigMap or Git revision with
// the digest of the files selected from it. An approval of the plan is an
// approval of those bytes.
func planSource(spec operatorv1alpha1.PtahMigrationPlanSpec) [][2]string {
	if source := spec.Source; source != nil {
		lines := [][2]string{
			{"Source", statusview.Clean(string(source.Kind))},
			{"Revision", statusview.Clean(source.Revision)},
			{"Content", statusview.Clean(source.ContentDigest)},
		}
		if source.Policy != nil {
			lines = append(lines, [2]string{"Source policy", statusview.Clean(source.Policy.Digest)})
		}
		return lines
	}
	if spec.ArtifactDigest != "" {
		return [][2]string{{"Source", "OCI artifact"}, {"Artifact", statusview.Clean(spec.ArtifactDigest)}}
	}
	// A contract-1 plan binds an artifact digest and a contract-2 plan a
	// source; one with neither says so rather than printing nothing.
	return [][2]string{{"Source", "none recorded"}}
}
