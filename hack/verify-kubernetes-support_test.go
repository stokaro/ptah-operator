// Copyright 2026 The Ptah Operator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadAndValidateManifestFreshness(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		lastVerified string
		now          string
		wantError    string
	}{
		{
			name:         "exact maximum age is accepted",
			lastVerified: "2026-08-31",
			now:          "2026-10-05",
		},
		{
			name:         "older verification is stale",
			lastVerified: "2026-08-31",
			now:          "2026-10-06",
			wantError:    "maximum age is 35 days",
		},
		{
			name:         "future verification is rejected",
			lastVerified: "2026-08-31",
			now:          "2026-08-30",
			wantError:    "is after validation date",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "kubernetes.json")
			manifest := fmt.Sprintf(`{
  "schemaVersion": 1,
  "policy": "upstream-active-minors",
  "windowSize": 3,
  "lastVerified": %q,
  "kindVersion": "v0.32.0",
  "releases": [
    {"minor": "1.35", "nodeImage": "kindest/node:v1.35.5@sha256:%s"},
    {"minor": "1.36", "nodeImage": "kindest/node:v1.36.1@sha256:%s"},
    {"minor": "1.37", "nodeImage": "kindest/node:v1.37.0@sha256:%s"}
  ]
}
`, test.lastVerified, strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64))
			if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
				t.Fatalf("write manifest: %v", err)
			}
			now, err := time.Parse("2006-01-02", test.now)
			if err != nil {
				t.Fatalf("parse test date: %v", err)
			}
			_, _, err = loadAndValidateManifest(path, now)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("loadAndValidateManifest() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("loadAndValidateManifest() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestValidationDateRejectsNonDate(t *testing.T) {
	t.Parallel()
	if _, err := validationDate("2026-8-31"); err == nil || !strings.Contains(err.Error(), "YYYY-MM-DD") {
		t.Fatalf("validationDate() error = %v, want strict date error", err)
	}
}
