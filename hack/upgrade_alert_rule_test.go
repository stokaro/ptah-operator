package main

import (
	"os/exec"
	"testing"
)

func TestExternalUpgradeAlertRules(t *testing.T) {
	command := exec.Command(promtoolOrSkip(t), "test", "rules", "rules.test.yaml")
	command.Dir = "upgradealert"
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("external upgrade alert rules: %v\n%s", err, output)
	}
}
