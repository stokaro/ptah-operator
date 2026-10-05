package e2e

import "testing"

func TestAlertPhasesDoNotReuseRetainedNames(t *testing.T) {
	infrastructure := alScopeFor("alerting")
	operations := alScopeFor("alerting-operations")
	certificates := alScopeFor("alerting-certificates")
	seen := map[string]bool{}
	for _, scope := range []alPhaseScope{infrastructure, operations, certificates} {
		for _, name := range []string{scope.monitoringNamespace, scope.stalledNamespace, scope.schemaProducer, scope.migrationProducer, scope.producerVersion} {
			if name == "" || seen[name] {
				t.Fatalf("alert phases share or omit retained identity %q", name)
			}
			seen[name] = true
		}
		for _, family := range []string{"schema", "migration"} {
			resource := alOverdueResource(scope.stalledNamespace, family)
			if resource["metadata"].(map[string]any)["namespace"] != scope.stalledNamespace {
				t.Fatal("overdue resource escaped its phase namespace")
			}
			realm := alOverdueRealm(scope.stalledNamespace, family)
			if len(realm.Spec.Namespaces) != 1 || realm.Spec.Namespaces[0] != scope.stalledNamespace {
				t.Fatal("overdue realm grants the wrong phase namespace")
			}
		}
	}
	if infrastructure.monitoringNamespace != alMonitoringNamespace {
		t.Fatal("infrastructure monitoring moved away from the upgrade observer endpoint")
	}
}
