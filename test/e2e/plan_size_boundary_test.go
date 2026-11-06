package e2e

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPlanSizeCalibration(t *testing.T) {
	t.Parallel()
	// Independent fixture measurements, including both possible native JSON
	// encodings, must reach the declared 8 MiB and one-byte-over boundaries.
	for _, width := range []int{1, 6} {
		c, err := calibratePlanSize(600+32*width, 600+33*width, 601+32*width)
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range []int{8388608, 8388609} {
			repeated, suffix, err := c.lengths(target)
			if err != nil || 600+repeated*width+suffix != target || suffix < 0 || suffix >= width {
				t.Fatal("native calibration missed the exact independently declared byte limit")
			}
		}
	}
	for _, row := range [][3]int{{0, 1, 1}, {600, 600, 601}, {600, 607, 601}, {600, 606, 602}, {10, 16, 11}, {8388608, 8388614, 8388609}} {
		if _, err := calibratePlanSize(row[0], row[1], row[2]); err == nil {
			t.Fatal("accepted absent, changed, or oversized calibration measurements")
		}
	}
	// The measurement a database name one byte shorter than the first gives: a
	// width no serializer writes, which would ask the generator for a default
	// past what its columns hold.
	if _, err := calibratePlanSize(600+32*6, 600+33*6-1, 601+32*6); err == nil {
		t.Fatal("accepted a width of 5, which only a difference between database names can give")
	}
	for _, c := range []planSizeCalibration{{}, {100, 0}, {100, 7}, {-1, 6}, {100, 5}, {100, 2}} {
		if _, _, err := c.lengths(8388608); err == nil {
			t.Fatal("an invalid native calibration produced a boundary fixture")
		}
	}
	if _, _, err := (planSizeCalibration{600, 6}).lengths(8388610); err == nil {
		t.Fatal("produced an undeclared oversize case")
	}
}

// A plan's scope holds a MySQL database's name, so the five databases the row
// plans against have to agree in length for a difference between plans to measure
// the default alone.
func TestPlanSizeDatabasesHaveOneLength(t *testing.T) {
	t.Parallel()
	revisions := []string{"small", "plus", "ascii", "oversize", "maximum"}
	for _, engine := range []string{"postgresql", "mysql"} {
		seen := map[string]bool{}
		for _, revision := range revisions {
			name, err := planSizeDatabase(engine, revision)
			if err != nil {
				t.Fatalf("%s %s: %v", engine, revision, err)
			}
			want := len("e2e_size_") + len(engine) + len("_") + planSizeRevisionWidth
			if len(name) != want || len(name) > 63 {
				t.Errorf("%s has %d bytes, want %d and a name both engines accept", name, len(name), want)
			}
			if seen[name] {
				t.Errorf("two revisions share the database %s", name)
			}
			seen[name] = true
		}
		if len(seen) != len(revisions) {
			t.Fatalf("read %d names for %d revisions", len(seen), len(revisions))
		}
	}
	for _, revision := range []string{"", "ninebytes"} {
		if name, err := planSizeDatabase("mysql", revision); err == nil {
			t.Errorf("the revision %q was named %s, and it cannot have the length the others have", revision, name)
		}
	}
}

func TestPlanSizeExecutableBoundary(t *testing.T) {
	t.Parallel()
	for _, dialect := range []string{"postgres", "mysql"} {
		t.Run(dialect, func(t *testing.T) {
			t.Parallel()
			small, err := os.ReadFile("../../testdata/e2e/readings/plan-size-small-" + dialect + ".json")
			if err != nil {
				t.Fatal(err)
			}
			base := len(small) - 32*6
			repeated, suffix := (8388608-base)/6, (8388608-base)%6
			count := 1
			if dialect == "mysql" {
				count = 100
			}
			index := 0
			defaults := regexp.MustCompile(`DEFAULT '[^']*'`)
			document := defaults.ReplaceAllFunc(small, func([]byte) []byte {
				n, x := repeated, suffix
				if dialect == "mysql" {
					n, x = repeated/count, 0
					if index < repeated%count {
						n++
					}
					if index == count-1 {
						x = suffix
					}
				}
				index++
				return []byte("DEFAULT '" + strings.Repeat(`\u003c`, n) + strings.Repeat("x", x) + "'")
			})
			if index != count || len(document) != 8388608 {
				t.Fatalf("native fixture expansion lost its measured structure: defaults=%d bytes=%d", index, len(document))
			}
			plan := &ptahv1alpha1.PtahSchemaPlan{Spec: ptahv1alpha1.PtahSchemaPlanSpec{Size: 8388608, StatementCount: int32(count)}}
			for i := range 16 {
				plan.Spec.Chunks = append(plan.Spec.Chunks, ptahv1alpha1.PlanChunkReference{
					Name: fmt.Sprintf("native-chunk-%d", i), Index: int32(i), Size: 524288, Digest: muDigest,
				})
			}
			if err := executablePlanAtLimit(plan, document, dialect, repeated, suffix); err != nil {
				t.Fatalf("the native executable boundary was refused: %v", err)
			}
			for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchemaPlan){
				"smaller size":          func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Size-- },
				"missing chunk":         func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Chunks = p.Spec.Chunks[:15] },
				"partial chunk":         func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Chunks[15].Size-- },
				"duplicate index":       func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Chunks[1].Index = 0 },
				"wrong digest":          func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Chunks[0].Digest = "sha256:00" },
				"wrong statement count": func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.StatementCount++ },
			} {
				t.Run(name, func(t *testing.T) {
					p := plan.DeepCopy()
					mutate(p)
					if executablePlanAtLimit(p, document, dialect, repeated, suffix) == nil {
						t.Fatalf("accepted %s", name)
					}
				})
			}
			for name, altered := range map[string][]byte{
				"one byte over":              append(bytes.Clone(document), '\n'),
				"one byte short":             document[:len(document)-1],
				"comment instead of default": bytes.Replace(document, []byte("DEFAULT"), []byte("COMMENT"), 1),
				"another table":              bytes.Replace(document, []byte("e2e_plan_size_limit"), []byte("e2e_plan_size_limiz"), 1),
			} {
				t.Run(name, func(t *testing.T) {
					if executablePlanAtLimit(plan, altered, dialect, repeated, suffix) == nil {
						t.Fatalf("accepted %s", name)
					}
				})
			}
		})
	}
}

func TestPlanSizeOversizedNativeRefusal(t *testing.T) {
	t.Parallel()
	schema := &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{UID: "original-schema"}}
	schema.Status.Phase = ptahv1alpha1.PhaseFailed
	schema.Status.Source.Digest, schema.Status.Source.Verified = muDigest, true
	schema.Status.Target.IdentityDigest, schema.Status.Target.CoordinationDigest = muDigest, muDigest
	result := runner.Result{OperationID: muOperation, TargetIdentityDigest: muDigest, CoordinationDigest: muDigest,
		Error: &runner.ResultError{Code: "invalid_plan_output", Message: "plan output exceeds the configured plan limit: saved file has 8388609 bytes; limit is 8388608"}}
	if err := oversizedNativePlanRefused(schema, result, muDigest); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*runner.Result){
		"parser refusal":     func(r *runner.Result) { r.Error.Message = "invalid schema" },
		"unmeasured refusal": func(r *runner.Result) { r.Error.Message = "plan output exceeds the configured plan limit" },
		"two bytes over":     func(r *runner.Result) { r.Error.Message = strings.Replace(r.Error.Message, "8388609", "8388610", 1) },
		"truncated result":   func(r *runner.Result) { r.Truncation = &runner.TruncationMetadata{Stdout: true} },
		"different target":   func(r *runner.Result) { r.TargetIdentityDigest = "sha256:00" },
		"mutation started":   func(r *runner.Result) { r.MutationStarted = true },
		"child failed":       func(r *runner.Result) { r.ChildExitCode = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			copy := result
			errCopy := *result.Error
			copy.Error = &errCopy
			mutate(&copy)
			if oversizedNativePlanRefused(schema, copy, muDigest) == nil {
				t.Fatalf("accepted %s", name)
			}
		})
	}
}
