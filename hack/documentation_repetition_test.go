package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	fencedBlock  = regexp.MustCompile("(?s)```.*?```")
	sentenceEnd  = regexp.MustCompile(`(?:[.:;]) `)
	collapseText = regexp.MustCompile(`\s+`)
)

// minimumRepeatedSentence is the length above which an exactly repeated
// sentence is a mistake rather than a turn of phrase. Sixty characters is
// longer than any heading, table cell or short instruction the site repeats on
// purpose, and shorter than the sentence this check was written for.
const minimumRepeatedSentence = 60

// A published page said the same two sentences twice in a row.
//
// It is the kind of defect nothing catches: the build is fine, the links
// resolve, and a reader skims past it having lost a little trust. Measured
// over the whole site when it was written, this rule matched that passage and
// nothing else -- so it is a check rather than a style opinion.
func TestNoDocumentationPageSaysTheSameSentenceTwice(t *testing.T) {
	t.Parallel()
	pages := documentationPages(t)
	if len(pages) < 10 {
		t.Fatalf("read %d documentation pages; the site has more than that", len(pages))
	}
	for _, path := range pages {
		content, err := os.ReadFile(path) //nolint:gosec // A path this test walked under the repository.
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, repeated := range repeatedSentences(string(content)) {
			t.Errorf("%s says this twice: %q", filepath.Base(path), repeated)
		}
	}
}

// repeatedSentences returns the prose sentences a page states more than once.
//
// Code blocks are excluded because a command repeated in two procedures is
// two procedures, and table rows because a column of repeated verdicts is a
// table doing its job.
func repeatedSentences(page string) []string {
	page = fencedBlock.ReplaceAllString(page, " ")
	var prose []string
	for _, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		prose = append(prose, line)
	}
	text := collapseText.ReplaceAllString(strings.Join(prose, " "), " ")
	counts := map[string]int{}
	for _, sentence := range sentenceEnd.Split(text, -1) {
		sentence = strings.TrimSpace(sentence)
		if len(sentence) < minimumRepeatedSentence {
			continue
		}
		counts[sentence]++
	}
	var repeated []string
	for sentence, count := range counts {
		if count > 1 {
			repeated = append(repeated, sentence)
		}
	}
	sort.Strings(repeated)
	return repeated
}

// documentationPages lists every published Markdown page.
func documentationPages(t *testing.T) []string {
	t.Helper()
	root := repositoryFile(t, filepath.Join("docs", "site", "src", "content", "docs"))
	var pages []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".md") {
			pages = append(pages, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the documentation: %v", err)
	}
	sort.Strings(pages)
	return pages
}
