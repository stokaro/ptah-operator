package runner

import (
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
	seen := make(map[string]struct{}, len(protectedEnvironmentKeys))
	secrets := make([]string, 0, len(protectedEnvironmentKeys))
	for _, key := range protectedEnvironmentKeys {
		value := values[key]
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		secrets = append(secrets, value)
	}
	sort.Slice(secrets, func(i, j int) bool {
		return len(secrets[i]) > len(secrets[j])
	})
	return Redactor{secrets: secrets}
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
