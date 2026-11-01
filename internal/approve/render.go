package approve

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-operator/internal/planview"
	"github.com/stokaro/ptah-operator/internal/statusview"
)

// RenderReview writes what a person is asked to approve: the identities the
// approval will carry, then the plan they identify. A schema plan is its
// verified SQL with each statement's severity and reason. A migration plan is
// its versions and checksums; its SQL lives in the artifact, and this says so
// rather than leaving a reader to think it was shown.
func RenderReview(out io.Writer, review Review) error {
	var text strings.Builder
	field := func(name, value string) {
		fmt.Fprintf(&text, "%-14s%s\n", name+":", value)
	}
	kind := "PtahSchema"
	if review.Family == Migration {
		kind = "PtahMigration"
	}
	field(kind, fmt.Sprintf("%s/%s, UID %s", review.Namespace, review.Name, review.ResourceUID))
	field("Plan", fmt.Sprintf("%s, UID %s", review.PlanName, review.PlanUID))
	field("Fingerprint", review.Fingerprint)
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
	text.WriteString("\nA migration plan records versions and checksums. The SQL is in the artifact,\nand it is not shown here.\n")
	_, err := io.WriteString(out, text.String())
	return err
}
