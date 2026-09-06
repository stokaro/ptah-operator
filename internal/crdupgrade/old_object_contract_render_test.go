package crdupgrade_test

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// TestOldObjectContractsNeverReadTheIncomingObject renders the chart and, for
// every ValidatingAdmissionPolicy validation that negates a delete
// predicate and goes on, requires the expression never to read object: a DELETE carries
// none, so the contract it evaluates has to be written over oldObject.
//
// The old-object contracts are derived from the create-time contract by
// rewriting object to oldObject. A reference the rewrite misses evaluates
// dyn(null).spec, the validation errors, and the policy denies with
// failurePolicy Fail: Helm could not replace a hook Job on any retried
// upgrade because the hook parent contract refused to let it delete the
// previous one, with "no such key: spec". The rewrite recognized
// object.spec and not dyn(object).spec, and nothing asserted the property
// the rewrite exists for.
func TestOldObjectContractsNeverReadTheIncomingObject(t *testing.T) {
	t.Parallel()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for old-object contract render tests")
	}
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	objects := parentOriginRenderObjects(t, helm, filepath.Join(repository, "charts", "ptah-operator"))
	// A negated delete predicate leaves the rest of the expression to be
	// evaluated on a DELETE. `variables.isDelete || (...)` is the other shape:
	// it answers a DELETE before reading object, and may read it after.
	deleteBranch := regexp.MustCompile(`!variables\.[A-Za-z]*Delete[A-Za-z]*|!\([^()]*variables\.[A-Za-z]*Delete[A-Za-z]*`)
	incoming := regexp.MustCompile(`dyn\(object\)|(^|[^A-Za-z])object\.`)
	checked := 0
	for key, object := range objects {
		if object["kind"] != "ValidatingAdmissionPolicy" {
			continue
		}
		spec := object["spec"].(map[string]any)
		validations, _ := spec["validations"].([]any)
		for index, validation := range validations {
			expression := validation.(map[string]any)["expression"].(string)
			if !deleteBranch.MatchString(expression) {
				continue
			}
			checked++
			if location := incoming.FindStringIndex(expression); location != nil {
				start := max(0, location[0]-60)
				t.Errorf("%s validations[%d] branches on a delete predicate and reads object: ...%s...", key, index, expression[start:min(len(expression), location[1]+40)])
			}
		}
	}
	if checked == 0 {
		t.Fatal("no rendered validation branches on a delete predicate; the predicate spelling no longer matches the chart")
	}
	t.Logf("checked %d delete-branch validations", checked)
}
