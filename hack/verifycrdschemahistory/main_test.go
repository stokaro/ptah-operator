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
	"context"
	"io"
	"strings"
	"testing"
)

func TestParseOptionsRequiresExplicitBaselineInGitHubActions(t *testing.T) {
	t.Parallel()

	configuration, err := parseOptions([]string{"-require-explicit-baseline=false"}, func(name string) string {
		if name == "GITHUB_ACTIONS" {
			return "true"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if !configuration.requireExplicitBaseline {
		t.Fatal("GitHub Actions did not require an explicit CRD history baseline")
	}
	if !configuration.requireExactCommit {
		t.Fatal("GitHub Actions did not require an exact CRD history commit")
	}
	if configuration.baselineRef != "" {
		t.Fatalf("baselineRef = %q, want empty value that fails closed during verification", configuration.baselineRef)
	}
}

func TestRunRejectsMissingAndSymbolicGitHubBaselineBeforeRepositoryAccess(t *testing.T) {
	t.Parallel()

	for name, baseline := range map[string]string{"missing": "", "symbolic": "HEAD^"} {
		baseline := baseline
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := run(context.Background(), nil, func(name string) string {
				switch name {
				case "GITHUB_ACTIONS":
					return "true"
				case baselineEnvironment:
					return baseline
				default:
					return ""
				}
			}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "not an exact 40-character lowercase Git commit") {
				t.Fatalf("run() error = %v, want exact CI baseline rejection", err)
			}
		})
	}
}

func TestExactCommit(t *testing.T) {
	t.Parallel()

	if !exactCommit(strings.Repeat("a", 40)) {
		t.Fatal("exactCommit() rejected a lowercase 40-character commit")
	}
	for _, value := range []string{
		"HEAD^",
		strings.Repeat("a", 39),
		strings.Repeat("a", 41),
		strings.Repeat("A", 40),
		strings.Repeat("g", 40),
	} {
		if exactCommit(value) {
			t.Errorf("exactCommit(%q) succeeded", value)
		}
	}
}

func TestParseOptionsUsesEnvironmentBaselineAndAllowsFlagOverride(t *testing.T) {
	t.Parallel()

	getenv := func(name string) string {
		switch name {
		case baselineEnvironment:
			return strings.Repeat("1", 40)
		case requireExplicitEnvironment:
			return "true"
		default:
			return ""
		}
	}
	configuration, err := parseOptions([]string{"-baseline-ref", strings.Repeat("2", 40)}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.baselineRef != strings.Repeat("2", 40) || !configuration.requireExplicitBaseline {
		t.Fatalf("configuration = %+v", configuration)
	}
}

func TestParseOptionsRejectsMalformedEnvironmentBoolean(t *testing.T) {
	t.Parallel()

	_, err := parseOptions(nil, func(name string) string {
		if name == requireExplicitEnvironment {
			return "sometimes"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), requireExplicitEnvironment) {
		t.Fatalf("parseOptions() error = %v, want malformed environment rejection", err)
	}
}
