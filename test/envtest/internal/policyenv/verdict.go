package policyenv

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Verdict is what the API server answered one request, and on whose word.
type Verdict struct {
	Admitted bool
	// Policy and Binding name the admission policy that refused the request.
	// Both are empty when something else refused it: RBAC, a schema, or the
	// request itself.
	Policy  string
	Binding string
	Message string
	Err     error
}

// The API server reports the first policy that denied a request, in this form,
// whatever reason code the policy chose.
var policyDenial = regexp.MustCompile(`ValidatingAdmissionPolicy '([^']+)' with binding '([^']+)' denied request: (?s:(.*))$`)

// Decide reads the outcome of a request.
func Decide(err error) Verdict {
	if err == nil {
		return Verdict{Admitted: true}
	}
	verdict := Verdict{Err: err, Message: err.Error()}
	if match := policyDenial.FindStringSubmatch(err.Error()); match != nil {
		verdict.Policy, verdict.Binding, verdict.Message = match[1], match[2], match[3]
	}
	return verdict
}

func (verdict Verdict) String() string {
	switch {
	case verdict.Admitted:
		return "admitted"
	case verdict.Policy != "":
		return fmt.Sprintf("refused by policy %s (binding %s): %s", verdict.Policy, verdict.Binding, verdict.Message)
	default:
		return fmt.Sprintf("refused, and not by an admission policy: %v", verdict.Err)
	}
}

// Row is one request and the verdict the installed release owes it.
type Row struct {
	Name string
	// Deny names the policies a refusal may be attributed to. Empty means the
	// request must be admitted. More than one name is for requests two
	// policies refuse on purpose -- a boundary held twice so either can be
	// replaced without a gap -- and the API server reports only the first it
	// evaluated, in no fixed order.
	Deny []string
	// Message must appear in the refusal.
	Message string
	// Do sends the request.
	Do func(ctx context.Context, env *Env) error
}

// Check sends row's request and compares the verdict with what row owes.
func (env *Env) Check(ctx context.Context, row Row) error {
	return row.judge(Decide(row.Do(ctx, env)))
}

// judge is Check without the request, so the matcher itself can be shown to
// refuse the verdicts it exists to refuse.
func (row Row) judge(verdict Verdict) error {
	if len(row.Deny) == 0 {
		if verdict.Admitted {
			return nil
		}
		return fmt.Errorf("%s: want admitted; %s", row.Name, verdict)
	}
	want := "refused by " + strings.Join(row.Deny, " or ")
	if row.Message != "" {
		want += fmt.Sprintf(" with %q", row.Message)
	}
	switch {
	case verdict.Admitted:
		return fmt.Errorf("%s: want %s; the API server admitted it", row.Name, want)
	case !slices.Contains(row.Deny, verdict.Policy):
		return fmt.Errorf("%s: want %s; %s", row.Name, want, verdict)
	case !strings.Contains(verdict.Message, row.Message):
		return fmt.Errorf("%s: want %s; %s", row.Name, want, verdict)
	}
	return nil
}

// Judge exposes the matcher to the suite's own self-test.
func (row Row) Judge(verdict Verdict) error { return row.judge(verdict) }
