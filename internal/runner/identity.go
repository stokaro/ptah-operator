package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
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

var scopeQueryKeys = map[string]struct{}{
	"catalog":        {},
	"currentcatalog": {},
	"currentschema":  {},
	"database":       {},
	"db":             {},
	"dbname":         {},
	"host":           {},
	"hostaddr":       {},
	"initialcatalog": {},
	"namespace":      {},
	"port":           {},
	"role":           {},
	"schema":         {},
	"searchpath":     {},
	"service":        {},
	"socket":         {},
	"tenant":         {},
	"user":           {},
	"username":       {},
	"warehouse":      {},
}

// TargetIdentityDigest hashes a canonical credential-free target identity.
// The canonical plaintext is deliberately not exposed by this package.
func TargetIdentityDigest(rawDatabaseURL string) (string, error) {
	if rawDatabaseURL == "" {
		return "", errors.New("database URL is empty")
	}
	parsed, err := url.Parse(rawDatabaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Opaque != "" {
		return "", errors.New("database URL is invalid")
	}

	scheme := strings.ToLower(parsed.Scheme)
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	port := parsed.Port()
	if port != "" {
		portNumber, err := strconv.ParseUint(port, 10, 16)
		if err != nil || portNumber == 0 {
			return "", errors.New("database URL has an invalid port")
		}
		port = strconv.FormatUint(portNumber, 10)
	}
	if port == defaultDatabasePorts[scheme] {
		port = ""
	}
	host := hostname
	if strings.Contains(hostname, ":") && port == "" {
		host = "[" + hostname + "]"
	} else if port != "" {
		host = net.JoinHostPort(hostname, port)
	}

	username := ""
	if parsed.User != nil {
		username = parsed.User.Username()
	}

	canonicalPath := (&url.URL{Path: parsed.Path}).EscapedPath()
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", errors.New("database URL has an invalid query")
	}
	canonicalQuery := make(url.Values)
	for key, values := range query {
		normalizedKey := normalizeIdentityKey(key)
		if normalizedKey == "options" {
			canonicalQuery[normalizedKey] = append(canonicalQuery[normalizedKey], scopeOptions(values)...)
			continue
		}
		if _, ok := scopeQueryKeys[normalizedKey]; !ok || secretLikeKey(normalizedKey) {
			continue
		}
		canonicalQuery[normalizedKey] = append(canonicalQuery[normalizedKey], values...)
	}
	for key := range canonicalQuery {
		sort.Strings(canonicalQuery[key])
	}

	identity := scheme + "|" + host + "|" + canonicalPath + "|" + username + "|" + canonicalQuery.Encode()
	return sha256Digest([]byte(identity)), nil
}

func scopeOptions(values []string) []string {
	var scoped []string
	for _, value := range values {
		fields := strings.Fields(value)
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
			normalizedKey := normalizeIdentityKey(key)
			if _, keep := scopeQueryKeys[normalizedKey]; keep && !secretLikeKey(normalizedKey) {
				scoped = append(scoped, normalizedKey+"="+value)
			}
		}
	}
	return scoped
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
