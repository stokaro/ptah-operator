package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

	canonicalPath := canonicalEscapedPath(parsed.EscapedPath())
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", errors.New("database URL has an invalid query")
	}
	canonicalQuery := make(url.Values)
	for key, values := range query {
		normalizedKey := normalizeIdentityKey(key)
		if _, exists := canonicalQuery[normalizedKey]; exists {
			return "", errors.New("database URL has ambiguous duplicate scope query keys")
		}
		if normalizedKey == "options" {
			scopedOptions, err := scopeOptions(values)
			if err != nil {
				return "", errors.New("database URL contains invalid options")
			}
			canonicalQuery[normalizedKey] = append(canonicalQuery[normalizedKey], scopedOptions...)
			continue
		}
		if _, ok := scopeQueryKeys[normalizedKey]; !ok || secretLikeKey(normalizedKey) {
			continue
		}
		canonicalQuery[normalizedKey] = append(canonicalQuery[normalizedKey], values...)
	}

	identity := scheme + "|" + host + "|" + canonicalPath + "|" + username + "|" + canonicalQuery.Encode()
	return sha256Digest([]byte(identity)), nil
}

func canonicalEscapedPath(escaped string) string {
	if escaped == "" {
		return ""
	}
	var canonical strings.Builder
	canonical.Grow(len(escaped))
	for index := 0; index < len(escaped); index++ {
		if escaped[index] != '%' || index+2 >= len(escaped) {
			canonical.WriteByte(escaped[index])
			continue
		}
		high, highOK := hexadecimalNibble(escaped[index+1])
		low, lowOK := hexadecimalNibble(escaped[index+2])
		if !highOK || !lowOK {
			canonical.WriteByte(escaped[index])
			continue
		}
		decoded := high<<4 | low
		if isUnreservedURLByte(decoded) {
			canonical.WriteByte(decoded)
		} else {
			const uppercaseHex = "0123456789ABCDEF"
			canonical.WriteByte('%')
			canonical.WriteByte(uppercaseHex[decoded>>4])
			canonical.WriteByte(uppercaseHex[decoded&0x0f])
		}
		index += 2
	}
	return canonical.String()
}

func hexadecimalNibble(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func isUnreservedURLByte(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' ||
		value == '-' || value == '.' || value == '_' || value == '~'
}

type optionAssignment struct {
	key   string
	value string
}

func scopeOptions(values []string) ([]string, error) {
	var scoped []string
	assignments, err := optionAssignments(values)
	if err != nil {
		return nil, err
	}
	for _, assignment := range assignments {
		normalizedKey := normalizeIdentityKey(assignment.key)
		if _, keep := scopeQueryKeys[normalizedKey]; keep && !secretLikeKey(normalizedKey) {
			scoped = append(scoped, normalizedKey+"="+assignment.value)
		}
	}
	return scoped, nil
}

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
	var quote rune
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
		if quote != 0 {
			if character == quote {
				quote = 0
			} else {
				field.WriteRune(character)
			}
			inField = true
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
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
	if escaped || quote != 0 {
		return nil, errors.New("unterminated option escape or quote")
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
