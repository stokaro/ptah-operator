package e2e

import (
	"maps"
	"testing"
)

func TestLifecycleSQLQuiescenceRequiresTheOriginalControlAndEveryRemoteClient(t *testing.T) {
	t.Parallel()
	const apply = "10.244.0.9"
	before := sqlAuditCounts{records: 8, clients: map[string]int64{apply: 3, "10.244.0.8": 2, "127.0.0.1": 3}}
	after := sqlAuditCounts{records: 10, clients: map[string]int64{apply: 3, "10.244.0.8": 2, "127.0.0.1": 5}}
	if err := lifecycleSQLQuiescent(before, after, apply); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*sqlAuditCounts, *sqlAuditCounts, *string){
		"empty baseline":           func(b, _ *sqlAuditCounts, _ *string) { *b = sqlAuditCounts{} },
		"no original control":      func(b, _ *sqlAuditCounts, _ *string) { delete(b.clients, apply) },
		"replayed by original Pod": func(_, a *sqlAuditCounts, _ *string) { a.clients[apply]++ },
		"new Pod address":          func(_, a *sqlAuditCounts, _ *string) { a.clients["10.244.0.10"] = 1 },
		"another known client":     func(_, a *sqlAuditCounts, _ *string) { a.clients["10.244.0.8"]++ },
		"lost original records":    func(_, a *sqlAuditCounts, _ *string) { a.clients[apply]-- },
		"lost unrelated client":    func(_, a *sqlAuditCounts, _ *string) { delete(a.clients, "10.244.0.8") },
		"decreased total":          func(_, a *sqlAuditCounts, _ *string) { a.records = 1 },
		"invalid client":           func(_, a *sqlAuditCounts, _ *string) { a.clients["unknown"] = 1 },
		"another loopback address": func(_, a *sqlAuditCounts, _ *string) { a.clients["127.0.0.2"] = 1 },
		"loopback control":         func(_, _ *sqlAuditCounts, host *string) { *host = "127.0.0.1" },
	} {
		t.Run(name, func(t *testing.T) {
			b, a, host := before, after, apply
			b.clients, a.clients = maps.Clone(before.clients), maps.Clone(after.clients)
			mutate(&b, &a, &host)
			if err := lifecycleSQLQuiescent(b, a, host); err == nil {
				t.Fatal("invalid lifecycle SQL window passed")
			}
		})
	}
}
