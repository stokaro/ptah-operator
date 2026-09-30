package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

type lifecycleSQLBackend struct {
	PID          int    `json:"pid"`
	Client       string `json:"client"`
	Database     string `json:"database"`
	SessionStart string `json:"sessionStart"`
}

// External Docker databases see the kind node's address after masquerading.
// Identify the one backend waiting on this barrier, rather than treating a
// node-wide count as a Pod identity. The journal control also binds its PID
// and session start before any zero-delta comparison is accepted.
func lifecycleSQLBackendQuery() string {
	return "SELECT json_build_object('pid', activity.pid, 'client', host(activity.client_addr), 'database', activity.datname, " +
		"'sessionStart', to_char(activity.backend_start AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS') || ' UTC') " +
		"FROM pg_locks AS waiting JOIN pg_locks AS held USING (locktype, database, classid, objid, objsubid) " +
		"JOIN pg_stat_activity AS holder ON holder.pid = held.pid JOIN pg_stat_activity AS activity ON activity.pid = waiting.pid " +
		"WHERE held.locktype = 'advisory' AND held.granted AND NOT waiting.granted AND waiting.pid <> held.pid " +
		"AND holder.application_name = '" + predecessorApplyBarrierApplication + "' AND activity.datname = current_database() " +
		"AND activity.state = 'active' AND activity.query = '" + lifecycleSQLControlStatement() + "'"
}

func lifecycleSQLControlStatement() string {
	return fmt.Sprintf("SELECT pg_advisory_lock(%d)", predecessorApplyBarrierKey)
}

func lifecycleSQLBackendForPod(raw []byte, pod *corev1.Pod, database string) (lifecycleSQLBackend, error) {
	var backend lifecycleSQLBackend
	if err := json.Unmarshal(raw, &backend); err != nil || backend.PID <= 0 || database == "" || backend.Database != database {
		return backend, errors.New("lifecycle SQL audit did not identify one exact barrier backend")
	}
	address, err := netip.ParseAddr(backend.Client)
	if err != nil || address.IsLoopback() || address.Unmap().String() != backend.Client || pod == nil || pod.UID == "" ||
		pod.Status.Phase != corev1.PodRunning || (backend.Client != pod.Status.PodIP && backend.Client != pod.Status.HostIP) {
		return backend, errors.New("lifecycle SQL backend does not use the original Pod or node address")
	}
	if _, err := time.Parse("2006-01-02 15:04:05 UTC", backend.SessionStart); err != nil {
		return backend, errors.New("lifecycle SQL backend has no dated session identity")
	}
	return backend, nil
}

func lifecycleSQLBackendControl(raw []byte, backend lifecycleSQLBackend) error {
	if backend.PID <= 0 || backend.Client == "" || backend.Database == "" || backend.SessionStart == "" {
		return errors.New("lifecycle SQL control has no backend identity")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	found := false
	for {
		var row struct {
			PID          int    `json:"pid"`
			Client       string `json:"remote_host"`
			Database     string `json:"dbname"`
			SessionStart string `json:"session_start"`
			Message      string `json:"message"`
		}
		if err := decoder.Decode(&row); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return errors.New("lifecycle SQL control has an incomplete statement journal")
		}
		statement := ""
		if strings.HasPrefix(row.Message, "statement: ") {
			statement = strings.TrimPrefix(row.Message, "statement: ")
		} else if strings.HasPrefix(row.Message, "execute ") {
			_, statement, _ = strings.Cut(row.Message, ": ")
		}
		if row.PID == backend.PID && row.Client == backend.Client && row.Database == backend.Database &&
			row.SessionStart == backend.SessionStart && statement == lifecycleSQLControlStatement() {
			found = true
		}
	}
	if !found {
		return errors.New("lifecycle SQL journal has no received barrier statement from the exact backend session")
	}
	return nil
}

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
