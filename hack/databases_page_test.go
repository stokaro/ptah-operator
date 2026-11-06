package main

import (
	"fmt"
	"regexp"
	"testing"
)

const databasesPage = "docs/site/src/content/docs/support/databases.md"

var databasesPageExecutor = regexp.MustCompile(`Ptah (v[0-9]+\.[0-9]+\.[0-9]+), the executor`)

// databasesPageNamesBuild holds the page's one statement of which executor it
// describes to the build the catalog pins.
func databasesPageNamesBuild(page, build string) error {
	named := databasesPageExecutor.FindAllStringSubmatch(page, -1)
	if len(named) != 1 {
		return fmt.Errorf("names the executor it describes %d times, want once", len(named))
	}
	if named[0][1] != build {
		return fmt.Errorf("describes %s, and the catalog pins %s; measure the page again against that build", named[0][1], build)
	}
	return nil
}

// What a login cannot see was measured on one executor build, and another build
// can answer differently. Moving the pin has to send somebody back to the page,
// because nothing else ties the two and the page would still read as true.
func TestTheDatabasesPageDescribesThePinnedExecutor(t *testing.T) {
	t.Parallel()
	page := readDocumentationPage(t, databasesPage)
	if err := databasesPageNamesBuild(page, verifiedPtahRelease(t)); err != nil {
		t.Errorf("%s %v", databasesPage, err)
	}
}

func TestTheDatabasesPageCheckRefusesAnotherBuild(t *testing.T) {
	t.Parallel()
	const build = "v1.2.3"
	accepted := "This section describes Ptah v1.2.3, the executor this operator build pins."
	if err := databasesPageNamesBuild(accepted, build); err != nil {
		t.Fatalf("the page naming the pinned build was refused: %v", err)
	}
	for name, page := range map[string]string{
		"a page naming no build":   "This section describes the executor this operator build pins.",
		"a page naming an old one": "This section describes Ptah v1.2.2, the executor this operator build pins.",
		"a version elsewhere only": "Ptah v1.2.3 added this. The executor this operator build pins is another.",
		"a page naming two builds": accepted + " It describes Ptah v1.2.2, the executor before it.",
	} {
		if err := databasesPageNamesBuild(page, build); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
