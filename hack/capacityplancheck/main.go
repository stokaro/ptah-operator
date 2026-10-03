// capacityplancheck applies the operator's SQL safety classification to a
// capacity fixture before the native verifier executes it.
package main

import (
	"fmt"
	"os"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: capacityplancheck <dialect> <plan.json>")
		os.Exit(2)
	}
	if err := check(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func check(dialect, path string) error {
	raw, err := os.ReadFile(path)
	if err == nil {
		var plan dataplane.PlanFile
		plan, err = dataplane.DecodePlan(raw, dialect)
		if err == nil && (plan.Destructive || len(plan.PrivilegeChanges) != 0) {
			err = fmt.Errorf("capacity input requires destructive or privilege-change permission")
		}
	}
	return err
}
