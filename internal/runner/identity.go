package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

var defaultDatabasePorts = map[string]string{
	"cockroach":   "26257",
	"cockroachdb": "26257",
	"mariadb":     "3306",
	"mssql":       "1433",
	"mysql":       "3306",
	"postgres":    "5432",
	"postgresql":  "5432",
	"sqlserver":   "1433",
}

// TargetIdentityDigest hashes a canonical credential-free route identity used
// to reject target redirects between observation and Apply. Cross-resource
// mutation serialization uses the separately declared coordination realm.
func TargetIdentityDigest(rawDatabaseURL string) (string, error) {
	if rawDatabaseURL == "" {
		return "", errors.New("database URL is empty")
	}
	if looksLikeMySQLNetworkDSN(rawDatabaseURL) {
		return mysqlNetworkTargetIdentityDigest(rawDatabaseURL)
	}
	parsed, err := url.Parse(rawDatabaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Opaque != "" {
		return "", errors.New("database URL is invalid")
	}
	scheme, route, database, executionContext, err := effectiveDatabaseTarget(parsed)
	if err != nil {
		return "", err
	}
	// The coordination realm intentionally unifies aliases and credentials for
	// serialization. This per-plan guard is narrower: it also binds the
	// credential-free session context that can change how exact SQL resolves.
	identity := scheme + "\x00" + route + "\x00" + database + "\x00" + executionContext
	return sha256Digest([]byte(identity)), nil
}

func effectiveDatabaseTarget(parsed *url.URL) (scheme, route, database, executionContext string, err error) {
	scheme = canonicalDatabaseScheme(parsed.Scheme)
	if scheme != "postgres" && scheme != "mysql" {
		return "", "", "", "", errors.New("database URL uses an unsupported scheme")
	}
	host := parsed.Hostname()
	port := parsed.Port()
	// Ptah's conventional MySQL URL conversion currently forwards a bracketed
	// IPv6 host without a port to the driver as an already-bracketed address.
	// The driver then adds its default port with net.JoinHostPort, producing an
	// unusable double-bracket address. Network-form DSNs do not have this
	// conversion ambiguity, and conventional URLs remain safe with an explicit
	// port.
	if scheme == "mysql" && strings.Contains(host, ":") && port == "" {
		return "", "", "", "", errors.New("MySQL IPv6 URLs must include an explicit port")
	}
	if scheme == "mysql" && mysqlConventionalPathChangesDriverParsing(parsed.EscapedPath()) {
		return "", "", "", "", errors.New("MySQL URL database name contains an unsupported escaped delimiter")
	}
	database, err = databaseFromEscapedPath(parsed.EscapedPath())
	if err != nil {
		return "", "", "", "", err
	}
	query, err := normalizedQuery(parsed.RawQuery)
	if err != nil {
		return "", "", "", "", err
	}
	if scheme == "postgres" {
		if value, ok, valueErr := singleQueryValue(query, "host"); valueErr != nil {
			return "", "", "", "", valueErr
		} else if ok {
			if value == "" {
				return "", "", "", "", errors.New("database URL contains an empty host override")
			}
			host = value
		}
		if value, ok, valueErr := singleQueryValue(query, "port"); valueErr != nil {
			return "", "", "", "", valueErr
		} else if ok {
			if value == "" {
				return "", "", "", "", errors.New("database URL contains an empty port override")
			}
			port = value
		}
		databaseAliases := 0
		for _, key := range []string{"dbname", "database"} {
			value, ok, valueErr := singleQueryValue(query, key)
			if valueErr != nil {
				return "", "", "", "", valueErr
			}
			if ok {
				databaseAliases++
				if value == "" {
					return "", "", "", "", errors.New("database URL contains an empty database override")
				}
				database = value
			}
		}
		if databaseAliases > 1 {
			return "", "", "", "", errors.New("database URL contains ambiguous database aliases")
		}
	}
	if database == "" {
		return "", "", "", "", errors.New("database URL must identify a database")
	}
	route, err = canonicalDatabaseRoute(host, port, defaultDatabasePorts[scheme])
	if err != nil {
		return "", "", "", "", err
	}
	executionContext, err = databaseExecutionContext(scheme, parsed.User, query)
	if err != nil {
		return "", "", "", "", err
	}
	return scheme, route, database, executionContext, nil
}

func mysqlConventionalPathChangesDriverParsing(escapedPath string) bool {
	lower := strings.ToLower(escapedPath)
	// Ptah currently decodes a conventional URL path before constructing the
	// driver's DSN. Encoded slash/question-mark bytes then become DSN syntax,
	// and an encoded percent can trigger a second decode. Reject those forms
	// until the conversion preserves the escaped path byte-for-byte.
	return strings.Contains(lower, "%2f") || strings.Contains(lower, "%3f") || strings.Contains(lower, "%25")
}

// These are the only URL parameters excluded from the execution-context
// digest. They are credential bytes or connection timing controls. TLS mode,
// authentication requirements, certificate paths, protocol policy, and every
// unknown server runtime parameter remain bound. This is deliberately a small
// denylist so rotating a password does not invalidate a plan while weakening
// peer authentication always does.
var postgresConnectionOnlyIdentityKeys = map[string]struct{}{
	"connect_timeout": {},
	"password":        {},
	"sslpassword":     {},
}

var mysqlConnectionOnlyIdentityKeys = map[string]struct{}{
	"checkConnLiveness": {},
	"maxAllowedPacket":  {},
	"readTimeout":       {},
	"timeout":           {},
	"writeTimeout":      {},
}

// mysqlAllowedDriverParameters contains only driver-side settings that do not
// become arbitrary server SET statements. Unknown keys are deliberately
// rejected: the MySQL driver treats them as session variables and sends their
// values as SQL during connection setup, which would make Observe and Plan
// capable of mutating state before approval.
var mysqlAllowedDriverParameters = map[string]struct{}{
	"allowCleartextPasswords":  {},
	"allowFallbackToPlaintext": {},
	"allowNativePasswords":     {},
	"allowOldPasswords":        {},
	"checkConnLiveness":        {},
	"clientFoundRows":          {},
	"collation":                {},
	"columnsWithAlias":         {},
	"compress":                 {},
	"connectionAttributes":     {},
	"interpolateParams":        {},
	"loc":                      {},
	"maxAllowedPacket":         {},
	"parseTime":                {},
	"readTimeout":              {},
	"rejectReadOnly":           {},
	"serverPubKey":             {},
	"timeTruncate":             {},
	"timeout":                  {},
	"tls":                      {},
	"writeTimeout":             {},
}

// databaseExecutionContext binds URL-visible session and connection-security
// semantics without binding password bytes or connection timing controls. It
// is hashed as part of the target identity and is never exposed as plaintext.
func databaseExecutionContext(scheme string, user *url.Userinfo, query map[string][]string) (string, error) {
	username := ""
	if user != nil {
		username = user.Username()
	}
	connectionOnly := mysqlConnectionOnlyIdentityKeys
	if scheme == "postgres" {
		connectionOnly = postgresConnectionOnlyIdentityKeys
		if value, ok, err := singleQueryValue(query, "user"); err != nil {
			return "", err
		} else if ok {
			username = value
		}
	} else if err := validateMySQLDriverParameters(query); err != nil {
		return "", err
	}
	parameters := make(url.Values)
	for key, values := range query {
		if executionContextKeyRepresentedElsewhere(scheme, key) {
			continue
		}
		if _, skip := connectionOnly[key]; skip {
			continue
		}
		if len(values) != 1 {
			return "", fmt.Errorf("database URL has multiple %s runtime values", key)
		}
		parameters[key] = append([]string(nil), values...)
	}
	return "user=" + url.QueryEscape(username) + "&" + parameters.Encode(), nil
}

func validateMySQLDriverParameters(query map[string][]string) error {
	for key, values := range query {
		if key == "multiStatements" {
			return errors.New("MySQL DSN multi-statement execution is not allowed")
		}
		if _, allowed := mysqlAllowedDriverParameters[key]; !allowed {
			return errors.New("MySQL DSN contains an unsupported server session parameter")
		}
		if len(values) != 1 {
			return errors.New("MySQL DSN contains repeated driver parameters")
		}
	}
	return nil
}

func executionContextKeyRepresentedElsewhere(scheme, key string) bool {
	if scheme != "postgres" {
		return false
	}
	switch key {
	case "host", "port", "dbname", "database":
		return true
	case "user":
		return true
	default:
		return false
	}
}

func databaseFromEscapedPath(escapedPath string) (string, error) {
	escaped := strings.TrimPrefix(escapedPath, "/")
	if escaped == "" {
		return "", nil
	}
	if strings.Contains(escaped, "/") {
		return "", errors.New("database URL path contains multiple unescaped segments")
	}
	database, err := url.PathUnescape(escaped)
	if err != nil || strings.ContainsRune(database, '\x00') {
		return "", errors.New("database URL has an invalid database name")
	}
	return database, nil
}

func normalizedQuery(rawQuery string) (map[string][]string, error) {
	parsed, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, errors.New("database URL has an invalid query")
	}
	result := make(map[string][]string, len(parsed))
	for key, values := range parsed {
		result[key] = values
	}
	return result, nil
}

func singleQueryValue(query map[string][]string, key string) (string, bool, error) {
	values, ok := query[key]
	if !ok {
		return "", false, nil
	}
	if len(values) != 1 {
		return "", false, fmt.Errorf("database URL has multiple %s query values", key)
	}
	return values[0], true, nil
}

func canonicalDatabaseRoute(host, port, defaultPort string) (string, error) {
	host = strings.TrimSpace(host)
	if strings.Contains(host, ",") || strings.Contains(port, ",") {
		return "", errors.New("database URL names multiple endpoints; a single lock target is required")
	}
	effectivePort := port
	if effectivePort == "" {
		effectivePort = defaultPort
	}
	if effectivePort != "" {
		portNumber, parseErr := strconv.ParseUint(effectivePort, 10, 16)
		if parseErr != nil || portNumber == 0 {
			return "", errors.New("database URL has an invalid port")
		}
		effectivePort = strconv.FormatUint(portNumber, 10)
	}
	if strings.HasPrefix(host, "/") {
		if strings.ContainsRune(host, '\x00') {
			return "", errors.New("database URL has an invalid unix socket path")
		}
		// PostgreSQL derives the socket filename from both the directory and
		// the effective port (for example .s.PGSQL.5432). Bind both so two
		// distinct servers cannot share a target identity.
		return "unix:path=" + url.QueryEscape(host) + "&port=" + effectivePort, nil
	}
	host, err := canonicalDatabaseHost(host)
	if err != nil {
		return "", err
	}
	port = effectivePort
	if port == defaultPort {
		port = ""
	}
	if port != "" {
		return net.JoinHostPort(host, port), nil
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]", nil
	}
	return host, nil
}

func canonicalDatabaseHost(host string) (string, error) {
	if strings.TrimSpace(host) != host {
		return "", errors.New("database URL has an invalid host")
	}
	host = strings.ToLower(host)
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	if host == "" {
		return "", errors.New("database URL has an empty host")
	}
	if address := net.ParseIP(host); address != nil {
		return address.String(), nil
	}
	return host, nil
}

func canonicalDatabaseScheme(scheme string) string {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "postgresql", "pgx":
		return "postgres"
	case "mariadb":
		return "mysql"
	default:
		return strings.ToLower(strings.TrimSpace(scheme))
	}
}

func looksLikeMySQLNetworkDSN(raw string) bool {
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "mysql://") || strings.HasPrefix(lower, "mariadb://") {
		lower = lower[strings.Index(lower, "://")+3:]
	}
	return strings.HasPrefix(lower, "tcp(") || strings.HasPrefix(lower, "unix(") ||
		strings.Contains(lower, "@tcp(") || strings.Contains(lower, "@unix(")
}

type mysqlNetworkDSN struct {
	endpoint  string
	database  string
	username  string
	password  string
	queryText string
}

// mysqlNetworkTargetIdentityDigest handles the network(address) syntax used
// by the Go MySQL driver and accepted by Ptah. It binds the physical route,
// database, user, and every execution-relevant driver or server parameter;
// authentication secrets and connection-liveness tuning stay outside the
// redirect guard.
func mysqlNetworkTargetIdentityDigest(raw string) (string, error) {
	parsed, err := parseMySQLNetworkDSN(raw)
	if err != nil {
		return "", err
	}
	query, err := normalizedQuery(parsed.queryText)
	if err != nil {
		return "", errors.New("MySQL network DSN has an invalid query")
	}
	executionContext, err := databaseExecutionContext("mysql", url.User(parsed.username), query)
	if err != nil {
		return "", err
	}
	identity := "mysql\x00" + parsed.endpoint + "\x00" + parsed.database + "\x00" + executionContext
	return sha256Digest([]byte(identity)), nil
}

func parseMySQLNetworkDSN(raw string) (mysqlNetworkDSN, error) {
	dsn := raw
	if separator := strings.Index(dsn, "://"); separator >= 0 {
		scheme := canonicalDatabaseScheme(dsn[:separator])
		if scheme != "mysql" {
			return mysqlNetworkDSN{}, errors.New("database URL is invalid")
		}
		dsn = dsn[separator+3:]
	}
	password := ""
	username := ""
	scoped := dsn
	if at := strings.LastIndex(dsn, "@"); at >= 0 {
		if at == 0 {
			return mysqlNetworkDSN{}, errors.New("MySQL network DSN has empty user information")
		}
		userInfo := dsn[:at]
		scoped = dsn[at+1:]
		var err error
		username, password, err = mysqlDSNUserInfo(userInfo)
		if err != nil {
			return mysqlNetworkDSN{}, err
		}
	}

	open := strings.IndexByte(scoped, '(')
	close := strings.Index(scoped, ")/")
	if open <= 0 || close <= open+1 {
		return mysqlNetworkDSN{}, errors.New("MySQL network DSN has an invalid endpoint")
	}
	network := strings.ToLower(scoped[:open])
	address := scoped[open+1 : close]
	databaseAndParams := scoped[close+2:]
	escapedDatabase, queryText, _ := strings.Cut(databaseAndParams, "?")
	if escapedDatabase == "" || strings.Contains(escapedDatabase, "/") {
		return mysqlNetworkDSN{}, errors.New("MySQL network DSN has an invalid database name")
	}
	database, err := url.PathUnescape(escapedDatabase)
	if err != nil || database == "" || strings.ContainsRune(database, '\x00') {
		return mysqlNetworkDSN{}, errors.New("MySQL network DSN has an invalid database name")
	}

	endpoint := ""
	switch network {
	case "tcp":
		endpoint, err = canonicalTCPAddress(address, "3306")
	case "unix":
		if !strings.HasPrefix(address, "/") || strings.ContainsRune(address, '\x00') {
			return mysqlNetworkDSN{}, errors.New("MySQL unix DSN has an invalid socket path")
		}
		endpoint = "unix:" + address
	default:
		return mysqlNetworkDSN{}, errors.New("MySQL network DSN uses an unsupported network")
	}
	if err != nil {
		return mysqlNetworkDSN{}, err
	}
	return mysqlNetworkDSN{
		endpoint: endpoint, database: database, username: username,
		password: password, queryText: queryText,
	}, nil
}

func mysqlDSNUserInfo(userInfo string) (username, password string, err error) {
	username = userInfo
	if before, after, ok := strings.Cut(userInfo, ":"); ok {
		username = before
		password = after
	}
	if username == "" {
		return "", "", errors.New("MySQL network DSN has an invalid username")
	}
	return username, password, nil
}

func canonicalTCPAddress(address, defaultPort string) (string, error) {
	hostname := address
	port := ""
	if host, parsedPort, err := net.SplitHostPort(address); err == nil {
		hostname, port = host, parsedPort
	} else if strings.Count(address, ":") != 0 {
		candidate := strings.TrimPrefix(strings.TrimSuffix(address, "]"), "[")
		if parsedAddress := net.ParseIP(candidate); parsedAddress != nil {
			hostname = parsedAddress.String()
		} else {
			return "", errors.New("MySQL TCP DSN has an invalid endpoint")
		}
	}
	hostname, err := canonicalDatabaseHost(hostname)
	if err != nil {
		return "", errors.New("MySQL TCP DSN has an invalid host")
	}
	if port != "" {
		portNumber, err := strconv.ParseUint(port, 10, 16)
		if err != nil || portNumber == 0 {
			return "", errors.New("MySQL TCP DSN has an invalid port")
		}
		port = strconv.FormatUint(portNumber, 10)
	}
	if port == defaultPort {
		port = ""
	}
	if port == "" {
		if strings.Contains(hostname, ":") {
			return "[" + hostname + "]", nil
		}
		return hostname, nil
	}
	return net.JoinHostPort(hostname, port), nil
}

type optionAssignment struct {
	key   string
	value string
}

// optionAssignments is used only to discover values that need log redaction.
// Identity hashing binds the exact decoded options string and never depends on
// this parser. PostgreSQL options split on whitespace, with backslash as the
// only escape; quote characters are ordinary data.
func optionAssignments(values []string) ([]optionAssignment, error) {
	var assignments []optionAssignment
	for _, value := range values {
		fields, err := splitOptionFields(value)
		if err != nil {
			return nil, err
		}
		for index := 0; index < len(fields); index++ {
			assignment := ""
			switch {
			case fields[index] == "-c" && index+1 < len(fields):
				index++
				assignment = fields[index]
			case strings.HasPrefix(fields[index], "-c"):
				assignment = strings.TrimPrefix(fields[index], "-c")
			}
			key, value, ok := strings.Cut(assignment, "=")
			if !ok {
				continue
			}
			assignments = append(assignments, optionAssignment{key: key, value: value})
		}
	}
	return assignments, nil
}

func splitOptionFields(value string) ([]string, error) {
	var fields []string
	var field strings.Builder
	escaped := false
	inField := false
	flush := func() {
		if inField {
			fields = append(fields, field.String())
			field.Reset()
			inField = false
		}
	}
	for _, character := range value {
		if escaped {
			field.WriteRune(character)
			inField = true
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			inField = true
			continue
		}
		if unicode.IsSpace(character) {
			flush()
			continue
		}
		field.WriteRune(character)
		inField = true
	}
	if escaped {
		return nil, errors.New("unterminated option escape")
	}
	flush()
	return fields, nil
}

func normalizeIdentityKey(key string) string {
	key = strings.ToLower(key)
	return strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(key)
}

func secretLikeKey(normalizedKey string) bool {
	for _, marker := range []string{"password", "passwd", "pwd", "secret", "token", "credential", "apikey", "privatekey", "accesskey", "clientkey"} {
		if strings.Contains(normalizedKey, marker) {
			return true
		}
	}
	return false
}

func sha256Digest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}
