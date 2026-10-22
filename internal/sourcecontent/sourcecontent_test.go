package sourcecontent_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

type fixtureFile struct {
	Path       string `json:"path"`
	PathBase64 string `json:"pathBase64"`
	Base64     string `json:"base64"`
}

type generate struct {
	Count      int    `json:"count"`
	PathFormat string `json:"pathFormat"`
	Data       string `json:"data"`
}

type fixtureCase struct {
	Name     string        `json:"name"`
	Files    []fixtureFile `json:"files"`
	Generate *generate     `json:"generate"`
	Manifest string        `json:"manifest"`
	Digest   string        `json:"digest"`
	Refusal  string        `json:"refusal"`
}

type fixtures struct {
	Accepted  []fixtureCase `json:"accepted"`
	Generated []fixtureCase `json:"generated"`
	Refused   []fixtureCase `json:"refused"`
}

func readFixtures(t *testing.T) fixtures {
	t.Helper()
	body, err := os.ReadFile("../../testdata/sourcecontent/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases fixtures
	if err := json.Unmarshal(body, &cases); err != nil {
		t.Fatal(err)
	}
	// A table that stopped matching would pass every row it no longer has.
	if len(cases.Accepted) < 10 || len(cases.Refused) < 14 || len(cases.Generated) < 1 {
		t.Fatalf("read %d accepted, %d generated and %d refused cases", len(cases.Accepted), len(cases.Generated), len(cases.Refused))
	}
	return cases
}

func (c fixtureCase) files(t *testing.T) []sourcecontent.File {
	t.Helper()
	if c.Generate != nil {
		files := make([]sourcecontent.File, c.Generate.Count)
		for i := range files {
			files[i] = sourcecontent.File{Path: fmt.Sprintf(c.Generate.PathFormat, i), Data: []byte(c.Generate.Data)}
		}
		return files
	}
	files := make([]sourcecontent.File, len(c.Files))
	for i, file := range c.Files {
		data, err := base64.StdEncoding.DecodeString(file.Base64)
		if err != nil {
			t.Fatal(err)
		}
		path := file.Path
		if file.PathBase64 != "" {
			raw, err := base64.StdEncoding.DecodeString(file.PathBase64)
			if err != nil {
				t.Fatal(err)
			}
			path = string(raw)
		}
		files[i] = sourcecontent.File{Path: path, Data: data}
	}
	return files
}

func TestDigestMatchesEveryAcceptedCase(t *testing.T) {
	t.Parallel()
	cases := readFixtures(t)
	for _, c := range cases.Accepted {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			files := c.files(t)
			manifest, err := sourcecontent.Manifest(files)
			if err != nil {
				t.Fatalf("Manifest refused the case: %v", err)
			}
			if string(manifest) != c.Manifest {
				t.Fatalf("manifest\n%s\nwant\n%s", manifest, c.Manifest)
			}
			digest, err := sourcecontent.Digest(files)
			if err != nil || digest != c.Digest {
				t.Fatalf("Digest() = %q, %v; want %q", digest, err, c.Digest)
			}
			// The order files arrive in is not part of the content.
			slices.Reverse(files)
			if reversed, err := sourcecontent.Digest(files); err != nil || reversed != c.Digest {
				t.Fatalf("reversed input gave %q, %v; want %q", reversed, err, c.Digest)
			}
		})
	}
	for _, c := range cases.Generated {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			digest, err := sourcecontent.Digest(c.files(t))
			if err != nil || digest != c.Digest {
				t.Fatalf("Digest() = %q, %v; want %q", digest, err, c.Digest)
			}
		})
	}
}

func TestDigestRefusesEveryRefusedCase(t *testing.T) {
	t.Parallel()
	refusals := map[string]error{
		"empty":          sourcecontent.ErrEmpty,
		"too-many-files": sourcecontent.ErrTooManyFiles,
		"invalid-path":   sourcecontent.ErrInvalidPath,
		"duplicate":      sourcecontent.ErrDuplicate,
		"path-conflict":  sourcecontent.ErrPathConflict,
	}
	cases := readFixtures(t)
	seen := map[string]bool{}
	for _, c := range cases.Refused {
		want, known := refusals[c.Refusal]
		if !known {
			t.Fatalf("case %q names the unknown refusal %q", c.Name, c.Refusal)
		}
		seen[c.Refusal] = true
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			digest, err := sourcecontent.Digest(c.files(t))
			if !errors.Is(err, want) {
				t.Fatalf("Digest() = %q, %v; want %v", digest, err, want)
			}
			if digest != "" {
				t.Fatalf("a refused source still got the digest %q", digest)
			}
		})
	}
	for refusal := range refusals {
		if !seen[refusal] {
			t.Errorf("no case is refused as %q", refusal)
		}
	}
}

// One changed byte, a renamed file or a moved one is other content.
func TestDigestNamesTheBytesAndTheirPaths(t *testing.T) {
	t.Parallel()
	base := []sourcecontent.File{{Path: "a/schema.sql", Data: []byte("CREATE TABLE t (id int);\n")}}
	digest, err := sourcecontent.Digest(base)
	if err != nil {
		t.Fatal(err)
	}
	for name, files := range map[string][]sourcecontent.File{
		"one byte":       {{Path: "a/schema.sql", Data: []byte("CREATE TABLE t (id int);\r\n")}},
		"another name":   {{Path: "a/schema.hcl", Data: base[0].Data}},
		"another folder": {{Path: "b/schema.sql", Data: base[0].Data}},
		"one more file":  {base[0], {Path: "a/seed.sql", Data: nil}},
	} {
		other, err := sourcecontent.Digest(files)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if other == digest {
			t.Errorf("%s kept the digest %q", name, digest)
		}
	}
}
