package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
)

// These are disposable acceptance databases. Keep their raw SQL in the
// database container, away from the published container logs and artifacts.
func databaseAuditArgs(engine string) []any {
	switch engine {
	case "postgresql":
		return []any{"-c", "logging_collector=on", "-c", "log_destination=jsonlog",
			"-c", "log_directory=/tmp/ptah-sql-audit", "-c", "log_filename=statements.log",
			"-c", "log_rotation_age=0", "-c", "log_rotation_size=0",
			"-c", "log_min_error_statement=error", "-c", "log_hostname=off", "-c", "log_timezone=UTC"}
	case "mysql":
		// log-raw also records statements the password rewriter cannot parse.
		return []any{"--log-output=TABLE", "--log-raw"}
	default:
		panic("unsupported database audit engine")
	}
}

// sqlAuditCounts counts received SQL records, including failed statements.
// A protocol message containing several statements can produce one record:
// zero records proves zero statements, but a positive count is not a count
// of individual SQL commands. No SQL classification is used to excuse a read.
type sqlAuditCounts struct {
	clients map[string]int64
	records int64
}

// The in-memory fields stay private, but retained evidence must carry the
// actual counts rather than the empty object encoding/json otherwise emits.
func (counts sqlAuditCounts) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Clients map[string]int64 `json:"clients"`
		Records int64            `json:"records"`
	}{Clients: counts.clients, Records: counts.records})
}

// Keep the refusal strict while distinguishing a shorter read from a
// changed prefix. SQL journals can contain credentials, so diagnostics expose
// only lengths, the first differing offset and digests, never journal bytes.
func postgresAuditPrefixError(before, after []byte) error {
	if bytes.HasPrefix(after, before) {
		return nil
	}
	compared := min(len(before), len(after))
	mismatch := 0
	for mismatch < compared && before[mismatch] == after[mismatch] {
		mismatch++
	}
	return fmt.Errorf("PostgreSQL SQL audit was truncated or replaced: previousBytes=%d currentBytes=%d firstMismatchOffset=%d previousSHA256=%x currentPrefixSHA256=%x",
		len(before), len(after), mismatch, sha256.Sum256(before), sha256.Sum256(after[:compared]))
}

func postgresAuditCounts(raw []byte, marker string) (sqlAuditCounts, bool, error) {
	counts := sqlAuditCounts{clients: map[string]int64{}}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	found := false
	for {
		var row struct {
			Host      string `json:"remote_host"`
			Message   string `json:"message"`
			Statement string `json:"statement"`
		}
		if err := decoder.Decode(&row); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			// Never quote a server log: it can contain fixture credentials.
			return counts, false, errors.New("invalid or incomplete PostgreSQL audit JSON")
		}
		if row.Message == "statement: SELECT '"+marker+"'" && marker != "" {
			found = true
		}
		statement := strings.HasPrefix(row.Message, "statement: ") ||
			(strings.HasPrefix(row.Message, "execute ") && strings.Contains(row.Message, ": ")) || row.Statement != ""
		if !statement {
			continue
		}
		counts.records++
		if row.Host == "" { // Unix socket used during database initialization.
			continue
		}
		address, err := netip.ParseAddr(row.Host)
		if err != nil {
			return counts, false, errors.New("PostgreSQL SQL record has no numeric client address")
		}
		counts.clients[address.Unmap().String()]++
	}
	if counts.records == 0 {
		return counts, false, errors.New("PostgreSQL SQL audit is empty")
	}
	return counts, found, nil
}

// MySQL returns only user_host and aggregate counts from its general log.
// SQL text never leaves that server. The last bracket pair is the client IP;
// usernames and reverse DNS names before it are not used as identities.
func mysqlAuditCounts(raw []byte) (sqlAuditCounts, error) {
	counts := sqlAuditCounts{clients: map[string]int64{}}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	for {
		var row struct {
			Client string `json:"client"`
			Count  int64  `json:"count"`
		}
		if err := decoder.Decode(&row); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return counts, errors.New("invalid MySQL audit counts")
		}
		start := strings.LastIndex(row.Client, " [")
		if row.Count <= 0 || start < 0 || !strings.HasSuffix(row.Client, "]") {
			return counts, errors.New("invalid MySQL audit client or count")
		}
		counts.records += row.Count
		host := row.Client[start+2 : len(row.Client)-1]
		if host == "" { // A Unix socket, never a runner Pod.
			continue
		}
		address, err := netip.ParseAddr(host)
		if err != nil {
			return counts, errors.New("MySQL SQL record has no numeric client address")
		}
		counts.clients[address.Unmap().String()] += row.Count
	}
	if counts.records == 0 {
		return counts, errors.New("MySQL SQL audit is empty")
	}
	return counts, nil
}

func sqlAuditDelta(before, after sqlAuditCounts, clientIP string) (int64, error) {
	address, err := netip.ParseAddr(clientIP)
	if err != nil || before.records == 0 || after.records == 0 {
		return 0, errors.New("SQL audit needs a numeric Pod IP and nonempty snapshots")
	}
	if after.records < before.records {
		return 0, errors.New("SQL audit lost server records")
	}
	for host, count := range before.clients {
		if after.clients[host] < count {
			return 0, errors.New("SQL audit lost client records")
		}
	}
	host := address.Unmap().String()
	delta := after.clients[host] - before.clients[host]
	if delta < 0 {
		return 0, errors.New("SQL audit counter decreased")
	}
	return delta, nil
}
