package e2e

import (
	"errors"
	"net/netip"
)

// While the original Apply is blocked inside its statement, no remote
// client should submit more SQL. Count every received statement, including
// failed SQL, across the server rather than just the known Pod address:
// another Pod must not escape the audit by acquiring another address.
// Only the harness's exact loopback address may keep inspecting the barrier.
func lifecycleSQLQuiescent(before, after sqlAuditCounts, originalClient string) error {
	address, err := netip.ParseAddr(originalClient)
	if err != nil || address.IsLoopback() || address.Unmap().String() != originalClient || before.clients[originalClient] == 0 {
		return errors.New("lifecycle SQL audit has no original remote Apply control")
	}
	if before.records <= 0 || after.records < before.records {
		return errors.New("lifecycle SQL audit lost its server record inventory")
	}
	for host, count := range before.clients {
		if count <= 0 || after.clients[host] < count {
			return errors.New("lifecycle SQL audit lost a previously observed client record")
		}
	}
	for host, count := range after.clients {
		client, err := netip.ParseAddr(host)
		if err != nil || client.Unmap().String() != host || count <= 0 {
			return errors.New("lifecycle SQL audit has an invalid client inventory")
		}
		if host != "127.0.0.1" && count != before.clients[host] {
			return errors.New("lifecycle transition received additional remote SQL while the original Apply was held")
		}
	}
	return nil
}
