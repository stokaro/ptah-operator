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
// a binding left out, a match widened, a parameter reference pointed at
// nothing. A row that still passes while a mutation holds does not depend on
// what the mutation changed, and so does not prove what it is named for.
type Mutation struct {
	Name string
	// Policies are the policies the mutation weakens.
	Policies []string
	// Apply weakens them and returns what puts them back.
	Apply func(ctx context.Context, env *Env) (restore func(context.Context) error, err error)
	// Breaks names the rows that must fail while the mutation holds.
	Breaks []string
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
	var last []string
	deadline := time.Now().Add(settle)
	for {
		failures := env.failing(ctx, breaks)
		if len(failures) == len(breaks) {
			break
		}
		last = passing(breaks, failures)
		if time.Now().After(deadline) {
			proved = fmt.Errorf("mutation %q held for %s and these rows still passed, so they do not depend on it: %s",
				mutation.Name, settle, strings.Join(last, ", "))
			break
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
// server has compiled the policies it just installed and synced the
// parameters they read.
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

// RedirectParameters points the policy's binding at a parameter that does
// not exist and tells the API server to admit when it is missing: a paramRef
// that fails open.
func RedirectParameters(policy string) func(context.Context, *Env) (func(context.Context) error, error) {
	return func(ctx context.Context, env *Env) (func(context.Context) error, error) {
		rendered, err := env.Chart.Binding(policy)
		if err != nil {
			return nil, err
		}
		allow := admissionregistrationv1.AllowAction
		restore := func(ctx context.Context) error { return env.putBinding(ctx, rendered.Name, rendered.Spec) }
		spec := *rendered.Spec.DeepCopy()
		if spec.ParamRef == nil {
			return restore, fmt.Errorf("binding %s has no paramRef to redirect", rendered.Name)
		}
		spec.ParamRef.Name = "policyenv-missing-parameter"
		spec.ParamRef.ParameterNotFoundAction = &allow
		return restore, env.putBinding(ctx, rendered.Name, spec)
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

func (env *Env) putBinding(ctx context.Context, name string, spec admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
		if err := env.Admin.Get(ctx, types.NamespacedName{Name: name}, live); err != nil {
			return err
		}
		live.Spec = *spec.DeepCopy()
		return env.Admin.Update(ctx, live)
	})
}
