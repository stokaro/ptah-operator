package e2e

import (
	"net/url"
	"strings"
	"testing"
)

func TestSchemaMySQLAuditPreservesTheFixtureDSN(t *testing.T) {
	t.Parallel()
	fixture := deriveFixtureCredentials("audit-regression")
	// This is the real lifecycle input that the former URL parser rejected.
	if _, err := url.Parse(fixture.mysqlURL); err == nil {
		t.Fatal("the fixture no longer exercises the network DSN regression")
	}
	const user = "audit_0123456789abcdef"
	for _, original := range []string{
		fixture.mysqlURL,
		fixture.mysqlURL + "?parseTime=true&timeout=5s&charset=utf8mb4",
		"mysql://" + mysqlUser + ":" + fixture.mysqlPassword + "@mysql.test:3306/another_db?timeout=5s",
	} {
		got, err := replaceSchemaMySQLAuditUser(original, fixture.mysqlPassword, user)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(original, "mysql://"+mysqlUser+":", "mysql://"+user+":", 1)
		if got != want {
			t.Fatal("audit account replacement changed other target bytes")
		}
	}
	for _, row := range []struct{ raw, password, user string }{
		{"", fixture.mysqlPassword, user},
		{fixture.mysqlURL, "", user},
		{fixture.mysqlURL, "incorrect", user},
		{fixture.mysqlURL, fixture.mysqlPassword, ""},
		{fixture.mysqlURL, fixture.mysqlPassword, mysqlUser},
		{fixture.mysqlURL, fixture.mysqlPassword, "other:password@host"},
		{fixture.mysqlURL, fixture.mysqlPassword, strings.Repeat("x", 33)},
		{strings.Replace(fixture.mysqlURL, "mysql://", "postgres://", 1), fixture.mysqlPassword, user},
		{"mysql://" + mysqlUser + ":" + fixture.mysqlPassword + "@", fixture.mysqlPassword, user},
	} {
		got, err := replaceSchemaMySQLAuditUser(row.raw, row.password, row.user)
		if err == nil || got != "" {
			t.Fatal("unexpected fixture credential or audit account passed")
		}
		if strings.Contains(err.Error(), fixture.mysqlPassword) {
			t.Fatal("the refusal exposed the fixture credential")
		}
	}
}
