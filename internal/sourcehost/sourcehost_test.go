package sourcehost_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/sourcehost"
)

// dottedCapitalI is U+0130, which strings.ToLower maps to "i" and the IDNA
// mapping Go's transport dials by does not.
const dottedCapitalI = "\u0130"

func TestAuthorityRefusesAHostThatIsNotPrintableASCII(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, url, want string
	}{
		{name: "a name", url: "https://files.example.com/app.tar.gz", want: "files.example.com"},
		{name: "a name in capitals", url: "https://Files.Example.COM/app.tar.gz", want: "Files.Example.COM"},
		{name: "a port", url: "https://files.example.com:8443/app.tar.gz", want: "files.example.com:8443"},
		{name: "an IPv6 address", url: "https://[2001:db8::1]:8443/app.tar.gz", want: "[2001:db8::1]:8443"},
		{name: "an ssh user", url: "ssh://git@git.example.com:2222/acme/app.git", want: "git.example.com:2222"},
		{name: "an xn-- name", url: "https://xn--github-qyd.com/app.tar.gz", want: "xn--github-qyd.com"},
		{name: "U+0130", url: "https://g" + dottedCapitalI + "thub.com/app.tar.gz"},
		{name: "U+0130 escaped", url: "https://g%C4%B0thub.com/app.tar.gz"},
		{name: "U+0130 over ssh", url: "ssh://git@g" + dottedCapitalI + "thub.com/acme/app.git"},
		{name: "the Kelvin sign", url: "https://\u212aelvin.example/app.tar.gz"},
		{name: "a byte that is not UTF-8", url: "https://files%FF.example.com/app.tar.gz"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got, err := sourcehost.Authority(row.url)
			if row.want == "" {
				if !errors.Is(err, sourcehost.ErrNotASCII) || !strings.Contains(err.Error(), "xn-- form") {
					t.Fatalf("Authority(%q) = %q, %v, want a refusal that names the xn-- form", row.url, got, err)
				}
				if strings.ContainsFunc(err.Error(), func(r rune) bool { return r > 0x7e }) {
					t.Fatalf("the refusal %q prints the host unescaped", err)
				}
				return
			}
			if err != nil || got != row.want {
				t.Fatalf("Authority(%q) = %q, %v, want %q", row.url, got, err, row.want)
			}
		})
	}
	if _, err := sourcehost.Authority("https://files example.com/app.tar.gz"); err == nil {
		t.Fatal("Authority() read a host with a space, which url.Parse refuses")
	}
}

func TestEqualComparesASCIIWithoutCase(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name        string
		left, right string
		want        bool
	}{
		{name: "the same name", left: "github.com", right: "github.com", want: true},
		{name: "another case", left: "GitHub.COM", right: "github.com", want: true},
		{name: "the same port", left: "git.example.com:2222", right: "GIT.example.com:2222", want: true},
		{name: "another name", left: "github.com", right: "gitlab.com"},
		{name: "a port on one side", left: "github.com:443", right: "github.com"},
		{name: "a trailing dot", left: "github.com.", right: "github.com"},
		{name: "U+0130 on the left", left: "g" + dottedCapitalI + "thub.com", right: "github.com"},
		{name: "U+0130 on the right", left: "github.com", right: "g" + dottedCapitalI + "thub.com"},
		{name: "U+0130 on both sides", left: "g" + dottedCapitalI + "thub.com", right: "g" + dottedCapitalI + "thub.com"},
		// strings.EqualFold folds the Kelvin sign to "k".
		{name: "the Kelvin sign", left: "\u212aelvin.example", right: "kelvin.example"},
		{name: "empty", left: "", right: ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := sourcehost.Equal(row.left, row.right); got != row.want {
				t.Fatalf("Equal(%+q, %+q) = %v, want %v", row.left, row.right, got, row.want)
			}
		})
	}
}
