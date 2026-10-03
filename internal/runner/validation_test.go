package runner

import (
	"errors"
	"testing"
)

func TestDecodedResultRequiresProtocolAndExactOperation(t *testing.T) {
	valid := Result{ProtocolVersion: ProtocolVersion, Operation: OperationApply, OperationID: "apply-1", ChildExitCode: -1,
		Error: &ResultError{Code: "refused", Message: "refused before dispatch"}}
	if err := ValidateResultFor(valid, OperationApply, "apply-1"); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Result){
		"missing protocol":         func(r *Result) { r.ProtocolVersion = 0 },
		"foreign protocol":         func(r *Result) { r.ProtocolVersion++ },
		"foreign operation":        func(r *Result) { r.Operation = OperationPlan },
		"foreign id":               func(r *Result) { r.OperationID = "apply-2" },
		"success without mutation": func(r *Result) { r.Error = nil; r.ChildExitCode = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			r := valid
			change(&r)
			if err := ValidateResultFor(r, OperationApply, "apply-1"); !errors.Is(err, ErrMalformedFrame) {
				t.Fatalf("invalid result accepted: %v", err)
			}
		})
	}
	if err := ValidateResultFor(valid, "", "apply-1"); !errors.Is(err, ErrMalformedFrame) {
		t.Fatal("missing expected operation accepted")
	}
	if err := ValidateResultFor(valid, OperationApply, ""); !errors.Is(err, ErrMalformedFrame) {
		t.Fatal("missing expected id accepted")
	}
}
