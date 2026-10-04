package runner

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestDurablePlanPreservesBytesWithoutProcessKey(t *testing.T) {
	for _, size := range []int{int(DefaultMaxPlanBytes), int(DefaultMaxPlanBytes) + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			plan := exactSizePlanDocument(t, size)
			executor := &scriptedExecutor{t: t, responses: stablePlanResponses(t, plan)}
			var diagnostics bytes.Buffer
			result := Run(t.Context(), Config{Operation: OperationPlan, DurableResult: true, Environment: withRunnerProtocol(environmentWithout(databaseEnvironment("durable-plan"), EnvPlanSealPublicKey, EnvSealedPlanJobName)), Executor: executor, Diagnostics: &diagnostics, TempDir: t.TempDir()})
			if size > int(DefaultMaxPlanBytes) {
				if result.Error == nil || result.Error.Code != "invalid_plan_output" || result.Stdout != "" {
					t.Fatalf("oversized plan accepted: error=%#v stdout bytes=%d", result.Error, len(result.Stdout))
				}
				return
			}
			if result.Error != nil || result.Stdout != plan || result.PlanContentDigest != fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(plan))) {
				t.Fatalf("durable plan changed: error=%#v bytes=%d digest=%s", result.Error, len(result.Stdout), result.PlanContentDigest)
			}
			if bytes.Contains(diagnostics.Bytes(), []byte("CREATE TABLE")) {
				t.Fatalf("plan leaked to diagnostics: %s", diagnostics.Bytes())
			}
			encoded, err := EncodeSummary(result)
			if err != nil {
				t.Fatal(err)
			}
			summary, err := ParseSummaryFor(string(encoded), OperationPlan, result.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			framed, err := EncodeResult(result)
			if err != nil {
				t.Fatal(err)
			}
			legacy, err := ParseSummaryFor(string(framed.Summary), OperationPlan, result.OperationID)
			if err != nil || summary.FrameDigest != legacy.FrameDigest {
				t.Fatalf("summary digest differs by transport: %v", err)
			}
		})
	}
}
