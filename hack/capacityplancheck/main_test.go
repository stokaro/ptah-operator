package main

import (
	"os"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

func TestNativeMySQLPlans(t *testing.T) {
	// Both are actual plans emitted by the pinned Ptah. The old fixture says
	// safe in its metadata, but the operator raises its MODIFY COLUMN to
	// destructive; native Apply alone therefore did not qualify that input.
	for _, tc := range []struct {
		file    string
		refused bool
	}{
		{"mysql-modify-default.json", true},
		{"mysql-create-default.json", false},
	} {
		t.Run(tc.file, func(t *testing.T) {
			path := "testdata/" + tc.file
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := dataplane.DecodePlan(raw, "mysql")
			if err != nil {
				t.Fatal(err)
			}
			if plan.Destructive != tc.refused {
				t.Fatalf("unexpected operator classification: %v", plan.Destructive)
			}
			if err := check("mysql", path); (err != nil) != tc.refused {
				t.Fatalf("check = %v, refusal wanted %v", err, tc.refused)
			}
		})
	}
}
