package e2e

import (
	"errors"
	"fmt"
	"strings"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/plancontract"
	"github.com/stokaro/ptah-operator/internal/runner"
)

type planSizeCalibration struct {
	envelope, repeatedWidth int
}

// planSizeRevisionWidth is the length of the longest revision the row names,
// "oversize".
const planSizeRevisionWidth = 8

// planSizeDatabase names the database one revision of the row plans against, and
// every name on an engine has the same length. Since Ptah v0.19.0 a MySQL plan's
// scope holds the database name, so a longer name is a longer plan, and the
// calibration reads any difference between two plans as the width of one repeated
// byte. A name one byte shorter measures 5 where the serializer writes 6.
func planSizeDatabase(engine, revision string) (string, error) {
	if revision == "" || len(revision) > planSizeRevisionWidth {
		return "", fmt.Errorf("plan-size revision %q is not 1 to %d bytes", revision, planSizeRevisionWidth)
	}
	return "e2e_size_" + engine + "_" + revision + strings.Repeat("x", planSizeRevisionWidth-len(revision)), nil
}

// planSizeWidth is what one repeated byte can take in a plan: itself, or the six
// bytes of \u003c where the serializer escapes HTML-sensitive bytes.
func planSizeWidth(width int) bool {
	return width == 1 || width == 6
}

// Fingerprint digests have fixed lengths, and planSizeDatabase holds the scope's.
// Only the generated default changes between these three native plans; measure
// its serializer instead of assuming whether this Ptah build escapes
// HTML-sensitive bytes.
func calibratePlanSize(size32, size33, size32ASCII int) (planSizeCalibration, error) {
	width := size33 - size32
	base := size32 - 32*width
	if !planSizeWidth(width) || size32ASCII-size32 != 1 || base <= 0 || base >= int(plancontract.MaxExecutableBytes) ||
		size32 >= int(plancontract.MaxExecutableBytes) || size33 >= int(plancontract.MaxExecutableBytes) || size32ASCII >= int(plancontract.MaxExecutableBytes) {
		return planSizeCalibration{}, errors.New("native plans do not have the declared size-fixture serializer")
	}
	return planSizeCalibration{envelope: base, repeatedWidth: width}, nil
}

func (c planSizeCalibration) lengths(size int) (int, int, error) {
	if c.envelope <= 0 || !planSizeWidth(c.repeatedWidth) || size < c.envelope || size > int(plancontract.MaxExecutableBytes)+1 {
		return 0, 0, errors.New("plan-size calibration cannot produce the requested boundary")
	}
	return (size - c.envelope) / c.repeatedWidth, (size - c.envelope) % c.repeatedWidth, nil
}

func executablePlanAtLimit(plan *ptahv1alpha1.PtahSchemaPlan, document []byte, dialect string, repeated, suffix int) error {
	if plan == nil || int64(len(document)) != plancontract.MaxExecutableBytes || plan.Spec.Size != plancontract.MaxExecutableBytes ||
		len(plan.Spec.Chunks) != plancontract.MaxChunks || repeated <= 0 || suffix < 0 || suffix > 5 {
		return errors.New("the operational plan did not reach the exact executable byte and chunk limits")
	}
	for i, chunk := range plan.Spec.Chunks {
		if chunk.Index != int32(i) || chunk.Size != plancontract.ChunkBytes || chunk.Name == "" || !sha256Pattern.MatchString(chunk.Digest) {
			return errors.New("the maximum plan lost an exact full-size chunk")
		}
	}
	parsed, err := parsePlanDocument(document)
	statements := 1
	if dialect == "mysql" {
		statements = 100
	}
	if err != nil || parsed.FormatVersion != 1 || parsed.Dialect != dialect || parsed.Destructive == nil || *parsed.Destructive ||
		len(parsed.Statements) != statements || plan.Spec.StatementCount != int32(statements) {
		return errors.New("the maximum plan lost its native non-destructive statements")
	}
	total := 0
	for i := range statements {
		name := "e2e_plan_size_limit"
		count, ascii := repeated, suffix
		if dialect == "mysql" {
			name = fmt.Sprintf("e2e_plan_size_limit_%03d", i)
			count, ascii = repeated/statements, 0
			if i < repeated%statements {
				count++
			}
			if i == statements-1 {
				ascii = suffix
			}
		}
		// The statement opens with the table it creates, quoted the way the
		// dialect quotes it. Ptah up to v0.16.0 wrote a "-- POSTGRES TABLE:
		// <name> --" line ahead of it and v0.17.0 writes none, so the predicate
		// holds the statement itself rather than a comment the planner owns.
		quote := `"`
		if dialect == "mysql" {
			quote = "`"
		}
		opening := "CREATE TABLE " + quote + name + quote + " ("
		matches := 0
		for _, statement := range parsed.Statements {
			if strings.HasPrefix(statement.SQL, opening) && strings.Count(statement.SQL, "<") == count &&
				strings.Count(statement.SQL, "DEFAULT '"+strings.Repeat("<", count)+strings.Repeat("x", ascii)+"'") == 1 {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("maximum plan table %s does not contain its independently generated default", name)
		}
		total += count
	}
	if total != repeated {
		return errors.New("the maximum plan changed its total fixture payload")
	}
	return nil
}

func oversizedNativePlanRefused(schema *ptahv1alpha1.PtahSchema, result runner.Result, artifactDigest string) error {
	if schema == nil || schema.UID == "" || schema.Status.Source.Digest != artifactDigest || !sha256Pattern.MatchString(artifactDigest) ||
		!schema.Status.Source.Verified || schema.Status.Plan != nil || schema.Status.Phase != ptahv1alpha1.PhaseFailed ||
		result.ChildExitCode != 0 || result.Stdout != "" || result.PlanContentDigest != "" || result.PlanOutcome != "" ||
		result.MutationStarted || result.Uncertain || result.Truncation != nil || result.OperationID == "" ||
		result.CoordinationDigest != schema.Status.Target.CoordinationDigest || result.TargetIdentityDigest != schema.Status.Target.IdentityDigest ||
		!sha256Pattern.MatchString(result.CoordinationDigest) || !sha256Pattern.MatchString(result.TargetIdentityDigest) {
		return errors.New("the oversized native Plan did not fail closed under its exact source and target bindings")
	}
	want := fmt.Sprintf("plan output exceeds the configured plan limit: saved file has %d bytes; limit is %d", plancontract.MaxExecutableBytes+1, plancontract.MaxExecutableBytes)
	if result.Error == nil || result.Error.Code != "invalid_plan_output" || result.Error.Message != want {
		return errors.New("the refusal did not measure an actual saved native plan exactly one byte beyond the limit")
	}
	return nil
}
