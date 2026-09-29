package e2e

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type mysqlStatementRecord struct {
	Time        string `json:"time"`
	Thread      uint64 `json:"thread"`
	Client      string `json:"client"`
	Command     string `json:"type"`
	ArgumentHex string `json:"argumentHex"`
}

const mysqlJournalTime = "2006-01-02T15:04:05.000000"

var mysqlAuditIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

func mysqlStatementJournal(raw []byte) ([]mysqlStatementRecord, error) {
	var records []mysqlStatementRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	for {
		var record struct {
			mysqlStatementRecord
			ArgumentHex *string `json:"argumentHex"`
		}
		if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, errors.New("invalid or incomplete MySQL statement journal")
		}
		if record.ArgumentHex == nil {
			return nil, errors.New("MySQL statement journal lost its argument field")
		}
		row := record.mysqlStatementRecord
		row.ArgumentHex = *record.ArgumentHex
		if _, err := time.Parse(mysqlJournalTime, row.Time); err != nil || row.Thread == 0 || row.Client == "" || row.Command == "" {
			return nil, errors.New("MySQL statement journal lost its record identity")
		}
		argument, err := hex.DecodeString(row.ArgumentHex)
		if err != nil || !utf8.Valid(argument) {
			return nil, errors.New("MySQL statement journal has invalid argument bytes")
		}
		records = append(records, row)
	}
	if len(records) == 0 {
		return nil, errors.New("MySQL statement journal is empty")
	}
	return records, nil
}

// MySQL's table journal has no sequence number. Compare complete row multisets
// to retain duplicate records and refuse any lost or rewritten entry. Do not
// subtract timestamps or counts: either can miss a replacement or another SQL.
func mysqlStatementWindow(before, after []mysqlStatementRecord) ([]mysqlStatementRecord, error) {
	if len(before) == 0 || len(after) <= len(before) {
		return nil, errors.New("MySQL SQL audit needs complete advancing snapshots")
	}
	remaining := make(map[mysqlStatementRecord]int)
	for _, row := range before {
		remaining[row]++
	}
	var window []mysqlStatementRecord
	for _, row := range after {
		if remaining[row] > 0 {
			remaining[row]--
		} else {
			window = append(window, row)
		}
	}
	for _, count := range remaining {
		if count != 0 {
			return nil, errors.New("MySQL SQL audit lost or replaced journal records")
		}
	}
	return window, nil
}

// mysqlStatementIdentity reads the supplied login and effective account from
// user_host, never from SQL text. Connect precedes authentication and has an
// empty login; subsequent commands must carry the exact authenticated account.
func mysqlStatementIdentity(value string) (login, account, host string, err error) {
	open, end := strings.Index(value, "["), strings.Index(value, "] @")
	addressStart := strings.LastIndex(value, " [")
	if open < 0 || end < open || addressStart < end || !strings.HasSuffix(value, "]") {
		return "", "", "", errors.New("MySQL SQL audit has an invalid client identity")
	}
	login, account = value[:open], value[open+1:end]
	host = value[addressStart+2 : len(value)-1]
	if host != "" {
		address, parseErr := netip.ParseAddr(host)
		if parseErr != nil {
			return "", "", "", errors.New("MySQL SQL audit has no numeric client address")
		}
		host = address.Unmap().String()
	}
	return login, account, host, nil
}

func mysqlMigrationRefusalSQL(before, after []mysqlStatementRecord, database, user string, clients map[string]migrationSQLClient) (map[string]int, error) {
	if !mysqlAuditIdentifier.MatchString(database) || len(database) > 64 ||
		!mysqlAuditIdentifier.MatchString(user) || len(user) > 32 || len(clients) == 0 {
		return nil, errors.New("MySQL refusal audit needs an isolated database, unique account and identified clients")
	}
	window, err := mysqlStatementWindow(before, after)
	if err != nil {
		return nil, err
	}
	scoped := func(row mysqlStatementRecord) (string, string, string, bool, error) {
		login, account, host, err := mysqlStatementIdentity(row.Client)
		_, knownHost := clients[host]
		return login, account, host, login == user || account == user || knownHost, err
	}
	for _, row := range before {
		login, account, _, err := mysqlStatementIdentity(row.Client)
		// A previous, unrelated Pod may have used this address before the
		// window. Only the unique account must have no earlier traffic.
		if err != nil || login == user || account == user {
			return nil, errors.New("MySQL refusal audit started after the isolated account was used")
		}
	}
	counts := make(map[string]int)
	connected, latest := make(map[uint64]string), make(map[uint64]string)
	for index, row := range window {
		login, account, host, relevant, err := scoped(row)
		if err != nil {
			return nil, err
		}
		if !relevant {
			continue
		}
		actor, found := clients[host]
		if !found || actor.jobUID == "" || actor.podUID == "" || actor.operation != "history" || account != user {
			return nil, fmt.Errorf("MySQL SQL record %d has no identified History Job, Pod and account", index+1)
		}
		if row.Time <= latest[row.Thread] {
			return nil, errors.New("MySQL SQL audit has ambiguous or reversed session ordering")
		}
		latest[row.Thread] = row.Time
		argument, err := hex.DecodeString(row.ArgumentHex)
		if err != nil || !utf8.Valid(argument) {
			return nil, errors.New("MySQL SQL record has invalid argument bytes")
		}
		sql := string(argument)
		if row.Command == "Connect" {
			if login != "" || connected[row.Thread] != "" ||
				!strings.HasPrefix(sql, user+"@") || !strings.HasSuffix(sql, " on "+database+" using TCP/IP") {
				return nil, errors.New("MySQL SQL audit connection does not bind the isolated account and database")
			}
			connected[row.Thread] = host
			continue
		}
		if login != user || connected[row.Thread] != host {
			return nil, errors.New("MySQL SQL record has no matching authenticated connection")
		}
		switch row.Command {
		case "Quit", "Close stmt", "Ping":
			if sql != "" {
				return nil, errors.New("MySQL protocol control carries unexpected argument bytes")
			}
			if row.Command == "Quit" {
				delete(connected, row.Thread)
			}
		case "Init DB":
			if sql != database {
				return nil, errors.New("MySQL History changed its selected database")
			}
		case "Query", "Prepare", "Execute":
			if !mysqlMigrationHistoryStatement(row.Command, sql, database) {
				return nil, fmt.Errorf("MySQL SQL record %d is outside the permitted history diagnostics", index+1)
			}
			if row.Command != "Prepare" {
				counts[host]++
			}
		default:
			return nil, errors.New("MySQL History sent an undeclared protocol command")
		}
	}
	if len(counts) == 0 {
		return nil, errors.New("MySQL refusal window did not observe its initial History control")
	}
	if len(connected) != 0 {
		return nil, errors.New("MySQL refusal window ended with an open History connection")
	}
	return counts, nil
}

func mysqlMigrationHistoryStatement(command, statement, database string) bool {
	if !mysqlAuditIdentifier.MatchString(database) || len(database) > 64 {
		return false
	}
	switch command {
	case "Query":
		switch statement {
		case "SELECT VERSION()", "SELECT @@SESSION.restrict_fk_on_non_standard_key", mysqlHistoryCreate,
			"SHOW CREATE TABLE `" + database + "`.`schema_migrations`",
			"SELECT version, description, state, applied, total, COALESCE(error, ''), COALESCE(error_stmt, ''), execution_time_ms, checksum, applied_at\nFROM `" + database + "`.`schema_migrations`\nORDER BY version":
			return true
		}
	case "Prepare":
		return statement == mysqlHistoryEngine || statement == mysqlHistoryVersionType || statement == mysqlHistoryColumn
	case "Execute":
		for _, template := range []string{mysqlHistoryEngine, mysqlHistoryVersionType} {
			bound := strings.Replace(template, "?", "'"+database+"'", 1)
			bound = strings.Replace(bound, "?", "'schema_migrations'", 1)
			if statement == bound {
				return true
			}
		}
		for _, column := range []string{"state", "applied", "total", "error", "error_stmt", "execution_time_ms", "checksum"} {
			bound := strings.Replace(mysqlHistoryColumn, "?", "'"+database+"'", 1)
			bound = strings.Replace(bound, "?", "'schema_migrations'", 1)
			bound = strings.Replace(bound, "?", "'"+column+"'", 1)
			if statement == bound {
				return true
			}
		}
	}
	return false
}

const mysqlHistoryEngine = `SELECT engine
FROM information_schema.tables
WHERE table_schema = ? AND table_name = ? AND table_type = 'BASE TABLE'`

const mysqlHistoryVersionType = `SELECT data_type
FROM information_schema.columns
WHERE table_schema = ? AND table_name = ? AND column_name = 'version'`

const mysqlHistoryColumn = `SELECT COUNT(*)
FROM information_schema.columns
WHERE table_schema = ? AND table_name = ? AND column_name = ?`

const mysqlHistoryCreate = "CREATE TABLE IF NOT EXISTS `schema_migrations` (\n" + `    version BIGINT PRIMARY KEY,
    description TEXT NOT NULL,
    applied_at TIMESTAMP NOT NULL,
    state VARCHAR(32) NOT NULL DEFAULT 'applied',
    applied INTEGER NOT NULL DEFAULT 1,
    total INTEGER NOT NULL DEFAULT 1,
    error TEXT NULL,
    error_stmt TEXT NULL,
    execution_time_ms BIGINT NOT NULL DEFAULT 0,
    checksum VARCHAR(64) NOT NULL DEFAULT ''
) ENGINE=InnoDB`
