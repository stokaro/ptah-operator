package e2e

import (
	"errors"
	"strings"
)

// These fixtures use both mysql://user:password@host/database and Ptah's
// mysql://user:password@tcp(host:port)/database form. net/url does not parse
// the latter. Replace only the known fixture user, preserving the password,
// transport, address, database and query bytes exactly.
func replaceSchemaMySQLAuditUser(raw, password, user string) (string, error) {
	if password == "" || !mysqlAuditIdentifier.MatchString(user) || len(user) > 32 || user == mysqlUser {
		return "", errors.New("MySQL audit needs a distinct fixture account and a nonempty credential")
	}
	route, found := strings.CutPrefix(raw, "mysql://"+mysqlUser+":"+password+"@")
	if !found || route == "" {
		return "", errors.New("MySQL audit target does not use the expected fixture credential")
	}
	return "mysql://" + user + ":" + password + "@" + route, nil
}
