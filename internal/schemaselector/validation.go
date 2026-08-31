// Package schemaselector validates the bounded lexical contract shared by the
// API and the execution boundary for schema scope selectors.
package schemaselector

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	MaxItems = 128
	MaxRunes = 256
)

// Validate rejects values that cannot safely be copied through status and Job
// inputs. The selected Ptah binary remains authoritative for selector grammar.
func Validate(values []string) error {
	if len(values) > MaxItems {
		return fmt.Errorf("exclude contains more than %d selectors", MaxItems)
	}
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		if !utf8.ValidString(value) || value == "" || strings.TrimSpace(value) != value || hasControlCharacter(value) {
			return fmt.Errorf("exclude selector %d has invalid whitespace or encoding", index)
		}
		if utf8.RuneCountInString(value) > MaxRunes {
			return fmt.Errorf("exclude selector %d exceeds %d characters", index, MaxRunes)
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("exclude selector %d duplicates an earlier selector", index)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func hasControlCharacter(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}
