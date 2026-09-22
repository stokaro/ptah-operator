package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/plancontract"
)

var (
	documentedFence      = regexp.MustCompile("(?s)```.*?```")
	documentedKibibytes  = regexp.MustCompile(`(\d+) KiB`)
	documentedMebibytes  = regexp.MustCompile(`(\d+) MiB`)
	documentedChunkCount = regexp.MustCompile(`(?i)\b(sixteen|\d+) chunks\b`)
	documentedSentence   = regexp.MustCompile(`(?:[.;:]) `)
)

// spelledNumbers are the counts the prose writes as words.
var spelledNumbers = map[string]int{
	"eight": 8, "sixteen": 16, "thirty-two": 32, "sixty-four": 64,
}

// The plan byte limits are stated on six pages, and they are one contract.
//
// Six copies of a number is six places for it to drift, and the drift is
// invisible: a page that says a plan may be 4 MiB reads exactly as
// authoritative as the one that says 8. The limits are compiled in
// internal/plancontract, so the pages are held to them rather than to each
// other.
//
// The three are not independent. MaxChunks is derived from the other two, so
// pinning the chunk size and the chunk count pins the ceiling to within one
// chunk, and the ceiling is checked directly where a sentence states it.
func TestTheDocumentedPlanLimitsAreTheCompiledOnes(t *testing.T) {
	t.Parallel()
	wantChunk := fmt.Sprintf("%d KiB", plancontract.ChunkBytes>>10)
	wantCeiling := fmt.Sprintf("%d MiB", plancontract.MaxExecutableBytes>>20)
	checked := 0
	for _, path := range documentationPages(t) {
		content, err := os.ReadFile(path) //nolint:gosec // A path this test walked under the repository.
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		page := documentedFence.ReplaceAllString(string(content), " ")
		page = strings.Join(strings.Fields(page), " ")
		name := path[strings.LastIndex(path, "/")+1:]

		for _, match := range documentedChunkCount.FindAllStringSubmatch(page, -1) {
			checked++
			count, err := chunkCount(match[1])
			if err != nil {
				t.Errorf("%s states %q chunks, which is not a count: %v", name, match[1], err)
				continue
			}
			if count != plancontract.MaxChunks {
				t.Errorf("%s states %d chunks, and the operator compiles %d",
					name, count, plancontract.MaxChunks)
			}
		}

		// A size named in a sentence about chunks is a plan limit. Every
		// other figure on the site is measuring something else -- a caBundle,
		// a cache, a retention budget -- and says so in its own sentence,
		// which is why the sentence rather than the page is the unit.
		for _, sentence := range documentedSentence.Split(page, -1) {
			if !strings.Contains(strings.ToLower(sentence), "chunk") {
				continue
			}
			for _, match := range documentedKibibytes.FindAllString(sentence, -1) {
				checked++
				if match != wantChunk {
					t.Errorf("%s states a chunk of %s, and the operator compiles %s", name, match, wantChunk)
				}
			}
			for _, match := range documentedMebibytes.FindAllString(sentence, -1) {
				checked++
				if match != wantCeiling {
					t.Errorf("%s states a plan ceiling of %s beside its chunks, and the operator compiles %s",
						name, match, wantCeiling)
				}
			}
		}
	}
	if checked < 8 {
		t.Fatalf("checked %d stated limits; the site states at least eight", checked)
	}
}

func chunkCount(value string) (int, error) {
	if spelled, found := spelledNumbers[strings.ToLower(value)]; found {
		return spelled, nil
	}
	return strconv.Atoi(value)
}
