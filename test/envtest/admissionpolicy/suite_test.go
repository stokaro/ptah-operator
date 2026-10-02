package admissionpolicy_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// catalog is every row and mutation the suite holds the installed release to.
type catalog struct {
	rows      []policyenv.Row
	mutations []policyenv.Mutation
}

func (c *catalog) row(row policyenv.Row)                { c.rows = append(c.rows, row) }
func (c *catalog) mutation(mutation policyenv.Mutation) { c.mutations = append(c.mutations, mutation) }

// groups contribute the rows and mutations of one family of policies each.
var groups = []func(*testing.T, *catalog){
	controllerRows,
	resultJobRows,
	applyPolicyRows,
	managerStateRows,
}

func buildCatalog(t *testing.T) *catalog {
	t.Helper()
	c := &catalog{}
	for _, group := range groups {
		group(t, c)
	}
	return c
}

// policy resolves the stable prefix of a policy name to the name the chart
// renders, which carries a release digest.
func policy(t *testing.T, prefix string) string {
	t.Helper()
	found, err := env.Chart.Policy(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return found.Name
}

// TestAdmissionPolicies holds every installed policy to its rows, then proves
// each row measures the policy it names: under a mutation of that policy --
// its binding dropped, its match widened, its validations replaced -- the row
// has to fail, and pass again once the policy is restored.
//
// The mutations run one at a time and only after every row has passed: a
// mutation changes what every request sees, and a row that already failed
// would make its failure under a mutation mean nothing.
func TestAdmissionPolicies(t *testing.T) {
	plane.Require(t)
	c := buildCatalog(t)
	requireCoverage(t, c)

	// The API server compiles freshly installed policies on its own
	// schedule, so the first verdicts wait for it. A row still failing after
	// the wait fails below, by name.
	ctx := context.Background()
	if err := env.WaitFor(ctx, c.rows, time.Minute); err != nil {
		t.Logf("the rows did not all settle after the install: %v", err)
	}
	passed := t.Run("rows", func(t *testing.T) {
		for _, row := range c.rows {
			t.Run(row.Name, func(t *testing.T) {
				t.Parallel()
				if err := env.Check(ctx, row); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
	if !passed {
		t.Fatal("a row failed, so no mutation can be measured against the rows")
	}

	rows := make(map[string]policyenv.Row, len(c.rows))
	for _, row := range c.rows {
		rows[row.Name] = row
	}
	t.Run("mutations", func(t *testing.T) {
		for _, mutation := range c.mutations {
			t.Run(mutation.Name, func(t *testing.T) {
				if err := env.Prove(ctx, mutation, rows); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
}

// requireCoverage refuses a catalog that leaves an installed policy unproven:
// with no row it refuses for its own purpose, or no mutation that would show a
// row depends on it. A policy the chart adds without rows fails here rather
// than passing unnoticed.
func requireCoverage(t *testing.T, c *catalog) {
	t.Helper()
	if len(env.Chart.Policies) == 0 {
		t.Fatal("the chart installs no policies, so the catalog covers nothing")
	}
	installed := map[string]bool{}
	for _, installedPolicy := range env.Chart.Policies {
		installed[installedPolicy.Name] = true
	}

	names := map[string]bool{}
	refused := map[string]int{}
	for _, row := range c.rows {
		if names[row.Name] {
			t.Errorf("two rows are named %q", row.Name)
		}
		names[row.Name] = true
		for _, name := range row.Deny {
			if !installed[name] {
				t.Errorf("row %q expects a refusal by %s, which the chart does not install", row.Name, name)
			}
			refused[name]++
		}
	}
	mutated := map[string]int{}
	mutations := map[string]bool{}
	for _, mutation := range c.mutations {
		if mutations[mutation.Name] {
			t.Errorf("two mutations are named %q", mutation.Name)
		}
		mutations[mutation.Name] = true
		for _, name := range mutation.Policies {
			if !installed[name] {
				t.Errorf("mutation %q weakens %s, which the chart does not install", mutation.Name, name)
			}
			mutated[name]++
		}
		for _, row := range mutation.Breaks {
			if !names[row] {
				t.Errorf("mutation %q breaks row %q, which the catalog does not have", mutation.Name, row)
			}
		}
	}

	for _, installedPolicy := range env.Chart.Policies {
		name := installedPolicy.Name
		if refused[name] == 0 {
			t.Errorf("policy %s has no row it refuses for its own purpose", name)
		}
		if mutated[name] == 0 {
			t.Errorf("policy %s has no mutation that shows a row depends on it", name)
		}
	}
	if slices.ContainsFunc(c.rows, func(row policyenv.Row) bool { return row.Do == nil }) {
		t.Error("a row sends no request")
	}
}
