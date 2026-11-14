// Package sourcehost compares the host a Git or HTTPS source names with a
// host something else grants: the host key of a credential Secret, or an
// entry of a source policy's allowedGitHosts or allowedHTTPSHosts.
//
// A host is compared as ASCII. url.Parse accepts UTF-8 in a host and decodes
// a percent-escaped one, and Go's HTTP transport dials such a host by its
// IDNA form: https://g%C4%B0thub.com/, "g", U+0130 and "thub.com", dials
// xn--github-qyd.com. A comparison that folds Unicode case reads that host as
// github.com, since strings.ToLower maps U+0130 to "i", and holds a grant for
// one name to a server under another. So a host that holds a byte outside
// printable ASCII is refused, and an internationalized name is written in its
// xn-- form.
package sourcehost

import (
	"errors"
	"fmt"
	"net/url"
)

// ErrNotASCII is the refusal of a host that holds a byte outside printable
// ASCII, matched with errors.Is.
var ErrNotASCII = errors.New("write an internationalized name in its xn-- form")

// Authority is the host[:port] a source URL names, as url.Parse reads it,
// percent escapes decoded. It refuses a URL url.Parse refuses and a host that
// is not printable ASCII.
func Authority(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if err := CheckASCII(parsed.Host); err != nil {
		return "", err
	}
	return parsed.Host, nil
}

// CheckASCII refuses a host[:port] that holds a byte outside printable
// ASCII, naming it with every such byte escaped.
func CheckASCII(authority string) error {
	if !printableASCII(authority) {
		return fmt.Errorf("the host %+q is not printable ASCII: %w", authority, ErrNotASCII)
	}
	return nil
}

// Equal reports whether two authorities are the same host[:port], byte for
// byte but for the case of ASCII letters. An authority that is empty, or
// holds a byte outside printable ASCII, equals nothing, itself included.
func Equal(left, right string) bool {
	if left == "" || len(left) != len(right) || !printableASCII(left) || !printableASCII(right) {
		return false
	}
	for i := range len(left) {
		if lowerASCII(left[i]) != lowerASCII(right[i]) {
			return false
		}
	}
	return true
}

func printableASCII(value string) bool {
	for i := range len(value) {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func lowerASCII(b byte) byte {
	if 'A' <= b && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}
