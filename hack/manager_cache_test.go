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
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// internal/managercache is tested against itself, which says what the options
// do and nothing about whether the manager runs with them. This is the other
// half: the manager's options are written once, in one composite literal, and
// dropping either field there is a one-line change that no other test notices
// and that puts eight-mebibyte plan chunks back in the manager's memory.
const managerMainPath = "../cmd/manager/main.go"

// The fields the manager must set, and what each one has to be set to.
var managerCacheFields = map[string]string{
	"Cache":  "managercache.Options",
	"Client": "managercache.ClientOptions",
}

func TestTheManagerRunsWithTheBoundedCache(t *testing.T) {
	t.Parallel()

	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, managerMainPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", managerMainPath, err)
	}

	options := managerOptionsLiteral(t, file)
	found := map[string]string{}
	for _, element := range options.Elts {
		pair, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := pair.Key.(*ast.Ident)
		if !ok {
			continue
		}
		if _, wanted := managerCacheFields[key.Name]; !wanted {
			continue
		}
		call, ok := pair.Value.(*ast.CallExpr)
		if !ok {
			t.Fatalf("%s is set to %T rather than to a call", key.Name, pair.Value)
		}
		found[key.Name] = callName(call)
	}

	for field, want := range managerCacheFields {
		switch actual, ok := found[field]; {
		case !ok:
			t.Fatalf("the manager sets no %s, so it caches whole ConfigMaps including this operator's plan chunks", field)
		case actual != want:
			t.Fatalf("the manager sets %s to %s(), want %s()", field, actual, want)
		}
	}
}

// managerOptionsLiteral finds the ctrl.Options the manager is built from.
func managerOptionsLiteral(t *testing.T, file *ast.File) *ast.CompositeLit {
	t.Helper()

	var options *ast.CompositeLit
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}
		selector, ok := literal.Type.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		identifier, ok := selector.X.(*ast.Ident)
		if !ok || identifier.Name != "ctrl" || selector.Sel.Name != "Options" {
			return true
		}
		options = literal
		return false
	})
	if options == nil {
		t.Fatalf("%s builds no ctrl.Options, so this gate is measuring nothing", managerMainPath)
	}
	return options
}

// callName renders a call's function as package.Function, and anything it does
// not recognize as its own source text, so a failure names what it found.
func callName(call *ast.CallExpr) string {
	switch function := call.Fun.(type) {
	case *ast.SelectorExpr:
		if identifier, ok := function.X.(*ast.Ident); ok {
			return identifier.Name + "." + function.Sel.Name
		}
		return function.Sel.Name
	case *ast.Ident:
		return function.Name
	default:
		return fmt.Sprintf("an expression of type %T", call.Fun)
	}
}
