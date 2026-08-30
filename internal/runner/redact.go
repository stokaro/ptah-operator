package runner

import (
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

const RedactionMarker = "[REDACTED]"

var urlPattern = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s"'<>]+`)

var protectedEnvironmentKeys = []string{
	EnvDatabaseURL,
	EnvDevelopmentDatabaseURL,
	EnvOCIPassword,
	EnvOCIToken,
}

type Redactor struct {
	secrets []string
}

func NewRedactor(environment []string) Redactor {
	values := environmentMap(environment)
	seen := make(map[string]struct{}, len(protectedEnvironmentKeys)*4)
	secrets := make([]string, 0, len(protectedEnvironmentKeys)*4)
	addSecret := func(value string) {
		if value == "" {
			return
		}
		for _, representation := range secretRepresentations(value) {
			if representation == "" {
				continue
			}
			if _, ok := seen[representation]; ok {
				continue
			}
			seen[representation] = struct{}{}
			secrets = append(secrets, representation)
		}
	}
	for _, key := range protectedEnvironmentKeys {
		value := values[key]
		addSecret(value)
		if key == EnvDatabaseURL || key == EnvDevelopmentDatabaseURL {
			for _, credential := range databaseURLCredentials(value) {
				addSecret(credential)
			}
		}
	}
	sort.Slice(secrets, func(i, j int) bool {
		return len(secrets[i]) > len(secrets[j])
	})
	return Redactor{secrets: secrets}
}

func secretRepresentations(value string) []string {
	representations := []string{value}
	if encoded, err := json.Marshal(value); err == nil && len(encoded) >= 2 {
		representations = append(representations, string(encoded[1:len(encoded)-1]))
	}
	representations = append(representations, url.QueryEscape(value), url.PathEscape(value))
	return representations
}

func databaseURLCredentials(rawDatabaseURL string) []string {
	parsed, err := url.Parse(rawDatabaseURL)
	if err != nil {
		return nil
	}
	credentials := make([]string, 0, 4)
	if parsed.User != nil {
		if password, present := parsed.User.Password(); present {
			credentials = append(credentials, password)
		}
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return credentials
	}
	for key, values := range query {
		normalizedKey := normalizeIdentityKey(key)
		if secretLikeKey(normalizedKey) {
			credentials = append(credentials, values...)
		}
		if normalizedKey == "options" {
			credentials = append(credentials, secretOptions(values)...)
		}
	}
	return credentials
}

func secretOptions(values []string) []string {
	var credentials []string
	assignments, err := optionAssignments(values)
	if err != nil {
		// A malformed option string is rejected by target identity validation.
		// Retain the whole value as a conservative redaction fallback.
		return append(credentials, values...)
	}
	for _, assignment := range assignments {
		if secretLikeKey(normalizeIdentityKey(assignment.key)) {
			credentials = append(credentials, assignment.value)
		}
	}
	return credentials
}

func (r Redactor) Redact(value string) string {
	for _, secret := range r.secrets {
		value = strings.ReplaceAll(value, secret, RedactionMarker)
	}
	return redactURLPasswords(value)
}

// RedactCaptured also removes a trailing prefix of a protected value when a
// bounded capture ended in the middle of that value.
func (r Redactor) RedactCaptured(value string, truncated bool) string {
	if truncated {
		for _, secret := range r.secrets {
			maximumPrefix := min(len(value), len(secret)-1)
			for prefixLength := maximumPrefix; prefixLength > 0; prefixLength-- {
				if strings.HasSuffix(value, secret[:prefixLength]) {
					value = value[:len(value)-prefixLength] + RedactionMarker
					break
				}
			}
		}
		value = redactTruncatedURLPassword(value)
	}
	return r.Redact(value)
}

func redactURLPasswords(value string) string {
	return urlPattern.ReplaceAllStringFunc(value, func(candidate string) string {
		schemeEnd := strings.Index(candidate, "://")
		if schemeEnd < 0 {
			return candidate
		}
		authorityStart := schemeEnd + 3
		authorityEnd := len(candidate)
		if offset := strings.IndexAny(candidate[authorityStart:], "/?#"); offset >= 0 {
			authorityEnd = authorityStart + offset
		}
		authority := candidate[authorityStart:authorityEnd]
		at := strings.LastIndex(authority, "@")
		if at < 0 {
			return candidate
		}
		userinfo := authority[:at]
		colon := strings.Index(userinfo, ":")
		if colon < 0 {
			return candidate
		}
		passwordStart := authorityStart + colon + 1
		passwordEnd := authorityStart + at
		return candidate[:passwordStart] + RedactionMarker + candidate[passwordEnd:]
	})
}

func redactTruncatedURLPassword(value string) string {
	locations := urlPattern.FindAllStringIndex(value, -1)
	if len(locations) == 0 {
		return value
	}
	last := locations[len(locations)-1]
	if last[1] != len(value) {
		return value
	}
	candidate := value[last[0]:last[1]]
	schemeEnd := strings.Index(candidate, "://")
	if schemeEnd < 0 {
		return value
	}
	authority := candidate[schemeEnd+3:]
	if strings.ContainsAny(authority, "/?#") || strings.Contains(authority, "@") {
		return value
	}
	colon := strings.Index(authority, ":")
	if colon < 0 {
		return value
	}
	passwordStart := last[0] + schemeEnd + 3 + colon + 1
	return value[:passwordStart] + RedactionMarker
}
