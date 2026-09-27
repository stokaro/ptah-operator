package admissionpolicy_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// A mutation that writes a value into a policy counts as proof only when it
// turns the row that carries that value from refused to admitted. One that
// leaves the policy unable to evaluate refuses the row as well, and a row that
// merely stops matching its refusal would read as proof of the value.
//
// The value here compiles, and the API server cannot fold it into a constant,
// so it stores the policy; it fails when a request evaluates it, because no
// Job is named with a number. The policy then refuses the Job for the error
// rather than for the image.
func TestAMutationThatBreaksThePolicyIsNoProofOfTheValue(t *testing.T) {
	plane.Require(t)
	c := buildCatalog(t)
	ctx := context.Background()
	if err := env.WaitFor(ctx, c.rows, time.Minute); err != nil {
		t.Fatalf("the rows did not settle after the install: %v", err)
	}
	rows := make(map[string]policyenv.Row, len(c.rows))
	for _, row := range c.rows {
		rows[row.Name] = row
	}
	jobGuard := policy(t, "ptah-operator-job-write-guard-")
	mutation := policyenv.Mutation{
		Name: "Job write guard carries a value that fails to evaluate", Policies: []string{jobGuard},
		Apply:  policyenv.SetVariables(jobGuard, map[string]string{"releaseControllerImage": `string(int(object.metadata.name))`}),
		Breaks: []string{"manager dispatches a Job stamped with another manager's image"},
		Admits: true,
	}
	err := env.Prove(ctx, mutation, rows)
	if err == nil || !strings.Contains(err.Error(), "were not admitted") {
		t.Fatalf("Prove() = %v, want a mutation that broke the policy refused as proof", err)
	}
	// The same mutation read as an ordinary failure passes, which is the
	// false proof Admits exists to refuse.
	mutation.Admits = false
	if err := env.Prove(ctx, mutation, rows); err != nil {
		t.Fatalf("Prove() without Admits = %v, want the broken policy to fail the row", err)
	}
}
