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
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// Every kind this operator ships declares exactly one short name today, so the
// other two shapes would reach a reader for the first time on the day a kind
// changes. They are written here instead.
func TestKubectlNamesReadsAsASentenceForEveryShapeOfCRDNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		names apiextensionsv1.CustomResourceDefinitionNames
		want  string
	}{
		{
			name:  "no short name",
			names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "ptahschemas"},
			want:  "`kubectl` knows it as `ptahschemas`.",
		},
		{
			name: "one short name",
			names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "ptahschemas", ShortNames: []string{"ptahs"},
			},
			want: "`kubectl` knows it as `ptahschemas`, or `ptahs` for short.",
		},
		{
			name: "several short names",
			names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "ptahschemas", ShortNames: []string{"ptahs", "psch"},
			},
			want: "`kubectl` knows it as `ptahschemas`, and by the short names `ptahs`, `psch`.",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := kubectlNames(test.names); got != test.want {
				t.Fatalf("kubectlNames() = %q, want %q", got, test.want)
			}
		})
	}
}

// The names on the page have to be the ones a cluster answers to, so they are
// read from the CRD rather than written beside it. This is the check that the
// reading happens at all: a generator that hard-coded them would pass every
// test above and still publish the wrong name for a kind that renamed one.
func TestTheHeaderNamesComeFromTheCRD(t *testing.T) {
	t.Parallel()

	crd := &apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "operator.ptah.run",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind: "PtahSchema", Plural: "renamed-plural", ShortNames: []string{"renamed-short"},
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1alpha1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object"},
				},
			}},
		},
	}
	page, _, err := render(crd, "## Examples\n\nnone.")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{"`renamed-plural`", "`renamed-short`"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the page does not carry %s, so the header is not reading the CRD", want)
		}
	}
}
