package policyenv

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

// Mutation weakens installed policies the way a regression in the chart would:
// a binding left out, a match widened, validations that refuse everything. A
// row that still passes while a mutation holds does not depend on what the
// mutation changed, and so does not prove what it is named for.
type Mutation struct {
	Name string
	// Policies are the policies the mutation weakens.
	Policies []string
	// Apply weakens them and returns what puts them back.
	Apply func(ctx context.Context, env *Env) (restore func(context.Context) error, err error)
	// Breaks names the rows that must fail while the mutation holds.
	Breaks []string
	// Admits makes the rows it breaks owe more than a failure: each one must be
	// admitted while the mutation holds. A mutation that writes the value a
	// refused request carries into the policy has to reverse that verdict. A
	// policy the mutation left malformed refuses everything instead, and that
	// failure proves nothing about the value.
	Admits bool
}

// The API server rebuilds its policy set from informers once a second, so a
// mutation reaches admission within about a second of the write.
const (
	settle       = 20 * time.Second
	pollInterval = 100 * time.Millisecond
)

// Prove applies mutation, waits until every row it names fails, and puts the
// policies back, waiting until those rows pass again. rows maps row names to
// rows. The error names what did not happen and what the rows saw instead.
func (env *Env) Prove(ctx context.Context, mutation Mutation, rows map[string]Row) error {
	breaks := make([]Row, 0, len(mutation.Breaks))
	for _, name := range mutation.Breaks {
		row, ok := rows[name]
		if !ok {
			return fmt.Errorf("mutation %q names row %q, which does not exist", mutation.Name, name)
		}
		breaks = append(breaks, row)
	}
	if len(breaks) == 0 {
		return fmt.Errorf("mutation %q names no row it breaks, so it measures nothing", mutation.Name)
	}
	if failures := env.failing(ctx, breaks); len(failures) != 0 {
		return fmt.Errorf("mutation %q: before it was applied, %s", mutation.Name, strings.Join(failures, "; "))
	}

	restore, err := mutation.Apply(ctx, env)
	if err != nil {
		err = fmt.Errorf("mutation %q: apply: %w", mutation.Name, err)
		if restore != nil {
			err = errors.Join(err, restore(ctx))
		}
		return err
	}
	var proved error
	deadline := time.Now().Add(settle)
	for {
		if mutation.Admits {
			refused := env.refused(ctx, breaks)
			if len(refused) == 0 {
				break
			}
			if time.Now().After(deadline) {
				proved = fmt.Errorf("mutation %q held for %s and these rows were not admitted, so it did not reverse their verdict: %s",
					mutation.Name, settle, strings.Join(refused, "; "))
				break
			}
		} else {
			failures := env.failing(ctx, breaks)
			if len(failures) == len(breaks) {
				break
			}
			if time.Now().After(deadline) {
				proved = fmt.Errorf("mutation %q held for %s and these rows still passed, so they do not depend on it: %s",
					mutation.Name, settle, strings.Join(passing(breaks, failures), ", "))
				break
			}
		}
		time.Sleep(pollInterval)
	}

	if err := restore(ctx); err != nil {
		return errors.Join(proved, fmt.Errorf("mutation %q: restore: %w", mutation.Name, err))
	}
	deadline = time.Now().Add(settle)
	for {
		failures := env.failing(ctx, breaks)
		if len(failures) == 0 {
			return proved
		}
		if time.Now().After(deadline) {
			return errors.Join(proved, fmt.Errorf("mutation %q was restored and after %s these rows still fail: %s",
				mutation.Name, settle, strings.Join(failures, "; ")))
		}
		time.Sleep(pollInterval)
	}
}

// failing checks every row and returns the failures.
func (env *Env) failing(ctx context.Context, rows []Row) []string {
	var failures []string
	for _, row := range rows {
		if err := env.Check(ctx, row); err != nil {
			failures = append(failures, err.Error())
		}
	}
	return failures
}

// refused sends every row's request and returns each one the API server did not
// admit, with its verdict.
func (env *Env) refused(ctx context.Context, rows []Row) []string {
	var refused []string
	for _, row := range rows {
		if verdict := Decide(row.Do(ctx, env)); !verdict.Admitted {
			refused = append(refused, fmt.Sprintf("%s: %s", row.Name, verdict))
		}
	}
	return refused
}

func passing(rows []Row, failures []string) []string {
	var names []string
	for _, row := range rows {
		failed := false
		for _, failure := range failures {
			if strings.HasPrefix(failure, row.Name+": ") {
				failed = true
				break
			}
		}
		if !failed {
			names = append(names, row.Name)
		}
	}
	return names
}

// WaitFor polls until every row passes, which is how the suite knows the API
// server has compiled the policies it just installed.
func (env *Env) WaitFor(ctx context.Context, rows []Row, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		failures := env.failing(ctx, rows)
		if len(failures) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("after %s: %s", within, strings.Join(failures, "; "))
		}
		time.Sleep(pollInterval)
	}
}

// DropBinding deletes the bindings of the named policies, which is what a
// chart that stopped rendering them would leave: the policies exist and judge
// nothing.
func DropBinding(policies ...string) func(context.Context, *Env) (func(context.Context) error, error) {
	return func(ctx context.Context, env *Env) (func(context.Context) error, error) {
		var dropped []*admissionregistrationv1.ValidatingAdmissionPolicyBinding
		restore := func(ctx context.Context) error {
			for _, binding := range dropped {
				if err := env.Admin.Create(ctx, cleanBinding(binding)); err != nil {
					return fmt.Errorf("recreate binding %s: %w", binding.Name, err)
				}
			}
			return nil
		}
		for _, policy := range policies {
			binding, err := env.Chart.Binding(policy)
			if err != nil {
				return restore, err
			}
			if err := env.Admin.Delete(ctx, cleanBinding(binding)); err != nil {
				return restore, fmt.Errorf("delete binding %s: %w", binding.Name, err)
			}
			dropped = append(dropped, binding)
		}
		return restore, nil
	}
}

// WidenMatch removes the policy's match conditions, so it judges every
// request its resource rules match rather than the identities and objects it
// was written for.
func WidenMatch(policy string) func(context.Context, *Env) (func(context.Context) error, error) {
	return editPolicy(policy, func(spec *admissionregistrationv1.ValidatingAdmissionPolicySpec) {
		spec.MatchConditions = nil
	})
}

// RefuseEverything replaces the policy's validations with one that refuses
// every request the policy matches. A row that stays admitted under it was
// never inside the policy's scope, whatever its name says.
func RefuseEverything(policy string) func(context.Context, *Env) (func(context.Context) error, error) {
	return editPolicy(policy, func(spec *admissionregistrationv1.ValidatingAdmissionPolicySpec) {
		spec.Validations = []admissionregistrationv1.Validation{{
			Expression: "false",
			Message:    "the suite's mutation refused a request this policy matched",
		}}
		spec.AuditAnnotations = nil
	})
}

// SetVariables rewrites the named variables of the policy, which is how a
// chart that inlined another release's values would render it. A row refused
// for carrying another release's value is admitted once the policy carries
// that value itself, and that is what shows the variable is what refused it.
func SetVariables(policy string, expressions map[string]string) func(context.Context, *Env) (func(context.Context) error, error) {
	return func(ctx context.Context, env *Env) (func(context.Context) error, error) {
		rendered, err := env.renderedPolicy(policy)
		if err != nil {
			return nil, err
		}
		restore := func(ctx context.Context) error { return env.putPolicy(ctx, policy, rendered.Spec) }
		spec := *rendered.Spec.DeepCopy()
		set := map[string]bool{}
		for index := range spec.Variables {
			if expression, ok := expressions[spec.Variables[index].Name]; ok {
				spec.Variables[index].Expression = expression
				set[spec.Variables[index].Name] = true
			}
		}
		for name := range expressions {
			if !set[name] {
				return restore, fmt.Errorf("policy %s has no variable %s", policy, name)
			}
		}
		return restore, env.putPolicy(ctx, policy, spec)
	}
}

func editPolicy(
	name string,
	edit func(*admissionregistrationv1.ValidatingAdmissionPolicySpec),
) func(context.Context, *Env) (func(context.Context) error, error) {
	return func(ctx context.Context, env *Env) (func(context.Context) error, error) {
		rendered, err := env.renderedPolicy(name)
		if err != nil {
			return nil, err
		}
		restore := func(ctx context.Context) error { return env.putPolicy(ctx, name, rendered.Spec) }
		spec := *rendered.Spec.DeepCopy()
		edit(&spec)
		return restore, env.putPolicy(ctx, name, spec)
	}
}

func (env *Env) putPolicy(ctx context.Context, name string, spec admissionregistrationv1.ValidatingAdmissionPolicySpec) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live := &admissionregistrationv1.ValidatingAdmissionPolicy{}
		if err := env.Admin.Get(ctx, types.NamespacedName{Name: name}, live); err != nil {
			return err
		}
		live.Spec = *spec.DeepCopy()
		return env.Admin.Update(ctx, live)
	})
}
