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
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-operator/hack/crdschemahistory"
)

const (
	baselineEnvironment        = "CRD_SCHEMA_BASELINE_REF"
	requireExplicitEnvironment = "CRD_SCHEMA_REQUIRE_EXPLICIT_BASELINE"
)

type options struct {
	root                    string
	candidateDirectory      string
	baselineRef             string
	requireExplicitBaseline bool
	requireExactCommit      bool
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(
	ctx context.Context,
	arguments []string,
	getenv func(string) string,
	stdout io.Writer,
) error {
	configuration, err := parseOptions(arguments, getenv)
	if err != nil {
		return err
	}
	if configuration.requireExactCommit && !exactCommit(configuration.baselineRef) {
		return fmt.Errorf("CRD schema history baseline %q is not an exact 40-character lowercase Git commit", configuration.baselineRef)
	}
	result, err := crdschemahistory.Verify(ctx, crdschemahistory.Config{
		Root:                    configuration.root,
		CandidateDirectory:      configuration.candidateDirectory,
		BaselineRef:             configuration.baselineRef,
		RequireExplicitBaseline: configuration.requireExplicitBaseline,
	})
	if err != nil {
		return err
	}
	mode := "versioned"
	if result.InitialAdoption {
		mode = "initial-adoption"
	}
	_, err = fmt.Fprintf(
		stdout,
		"CRD schema history verified: baseline=%s source=%s baseline-version=%d candidate-version=%d schema-changed=%t mode=%s\n",
		result.BaselineCommit,
		result.BaselineSource,
		result.BaselineVersion,
		result.CandidateVersion,
		result.SchemaChanged,
		mode,
	)
	if err != nil {
		return fmt.Errorf("write CRD schema history result: %w", err)
	}
	return nil
}

func parseOptions(arguments []string, getenv func(string) string) (options, error) {
	if getenv == nil {
		return options{}, errors.New("read CRD schema history options: environment reader is required")
	}
	requireExplicit, err := environmentBool(getenv(requireExplicitEnvironment))
	if err != nil {
		return options{}, fmt.Errorf("read %s: %w", requireExplicitEnvironment, err)
	}
	githubActions, err := environmentBool(getenv("GITHUB_ACTIONS"))
	if err != nil {
		return options{}, fmt.Errorf("read GITHUB_ACTIONS: %w", err)
	}
	if githubActions {
		requireExplicit = true
	}

	configuration := options{
		baselineRef:             getenv(baselineEnvironment),
		requireExplicitBaseline: requireExplicit,
		requireExactCommit:      githubActions,
	}
	flags := flag.NewFlagSet("verifycrdschemahistory", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&configuration.root, "root", ".", "repository root")
	flags.StringVar(
		&configuration.candidateDirectory,
		"candidate-directory",
		"config/crd/bases",
		"repository-relative generated CRD directory",
	)
	flags.StringVar(
		&configuration.baselineRef,
		"baseline-ref",
		configuration.baselineRef,
		"Git commit or revision used as the exact comparison baseline",
	)
	flags.BoolVar(
		&configuration.requireExplicitBaseline,
		"require-explicit-baseline",
		configuration.requireExplicitBaseline,
		"reject local automatic baseline selection",
	)
	if err := flags.Parse(arguments); err != nil {
		return options{}, fmt.Errorf("parse CRD schema history flags: %w", err)
	}
	if flags.NArg() != 0 {
		return options{}, fmt.Errorf("parse CRD schema history flags: unexpected positional arguments %q", flags.Args())
	}
	if githubActions {
		configuration.requireExplicitBaseline = true
	}
	return configuration, nil
}

func exactCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func environmentBool(raw string) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("value %q is not a boolean", raw)
	}
	return value, nil
}
