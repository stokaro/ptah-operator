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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// A phase runs against a cluster of its own, so an artifact one of its rows
// reads has to be one the phase published. In an unpartitioned run every
// phase shares one registry, and a row that reads a tag an earlier phase left
// passes there; in its own suite it fails on its first cluster run, an hour or
// more in. #931 split the migration fault rows out of the migrations phases,
// and the first walk over what they read found the engine's own artifact and
// missed the uncertain one.
//
// The check walks every phase the suite catalog runs, from the source: the
// functions a phase's test reaches, the artifacts those functions name, and
// the ones they publish. A row that reads an artifact it does not publish in
// the same function depends on another row, and the phase declares that
// artifact in Artifacts. Each declared artifact has to be published by
// something the phase runs, and has to be read by a row that does not publish
// it.
//
// It covers the migration fixtures, which name every artifact through
// (*migrationRun).reference. The schema fixtures build their references by
// hand, and nothing here reads those. Nor does it prove that the publish runs
// before the read: a phase that publishes a shared artifact in a scenario
// after the rows that read it passes here and fails on the cluster.

// e2eArtifactFamily is how one kind of fixture names its artifacts in the
// registry: the method a row builds a reference with, and the methods that
// push one, by the index of the argument that carries the reference.
type e2eArtifactFamily struct {
	receiver   string
	builder    string
	publishers map[string]int
}

// migrationArtifacts are the migration fixtures. A row names one by the suffix
// it passes to (*migrationRun).reference: "" is the engine's own artifact,
// "-uncertain" the slow one.
var migrationArtifacts = e2eArtifactFamily{
	receiver: "migrationRun",
	builder:  "reference",
	publishers: map[string]int{
		"publish":                     2,
		"publishWithCheck":            2,
		"publishUnknownLayerArtifact": 0,
	},
}

// e2eArtifactUse is one reference a phase builds: a read, or a publish.
type e2eArtifactUse struct {
	// suffix is the argument the reference was built with, and known says it
	// was a constant the check could read.
	suffix   string
	known    bool
	position token.Position
	// scope is the declared function the reference is built in, which is the
	// unit a row publishes its own artifact in, or the package variable whose
	// initializer builds it.
	scope types.Object
}

func (u e2eArtifactUse) String() string {
	return fmt.Sprintf("%s at %s:%d", u.scope.Name(), filepath.Base(u.position.Filename), u.position.Line)
}

// e2ePhaseArtifacts is what one phase does with the family's artifacts.
type e2ePhaseArtifacts struct {
	phase  phases.Phase
	scopes int
	reads     []e2eArtifactUse
	publishes []e2eArtifactUse
	// shared are the reads whose function publishes nothing under that suffix.
	shared []e2eArtifactUse
}

func (p e2ePhaseArtifacts) sharedSuffixes() []string {
	var suffixes []string
	for _, use := range p.shared {
		suffixes = append(suffixes, use.suffix)
	}
	slices.Sort(suffixes)
	return slices.Compact(suffixes)
}

// e2eSource is test/e2e, type-checked with the e2e tag, so a method value, a
// method expression and a promoted method all resolve to the function they
// name rather than to whatever shares their name.
type e2eSource struct {
	fset      *token.FileSet
	pkg       *types.Package
	info      *types.Info
	functions map[*types.Func]*ast.FuncDecl
	// values are package-level variables' initializers, and fields every
	// expression assigned to a struct field anywhere in the package.
	values map[*types.Var]ast.Expr
	fields map[*types.Var][]ast.Expr
}

// e2eListing is what go list says about test/e2e: where it is, the files the
// e2e tag builds, and the export data of everything they import.
type e2eListing struct {
	dir     string
	files   []string
	exports map[string]string
}

// listE2EPackage runs go list once for every test that loads the source.
var listE2EPackage = sync.OnceValues(goListE2EPackage)

func goListE2EPackage() (e2eListing, error) {
	type listed struct {
		ImportPath, Dir, Export string
		GoFiles, TestGoFiles    []string
		Imports, TestImports    []string
		Error                   *struct{ Err string }
	}
	run := func(arguments ...string) ([]listed, error) {
		command := exec.Command("go", append([]string{"list", "-e", "-tags", "e2e"}, arguments...)...)
		command.Dir = ".."
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			return nil, fmt.Errorf("go list %s: %w: %s", strings.Join(arguments, " "), err, stderr.String())
		}
		var packages []listed
		for decoder := json.NewDecoder(bytes.NewReader(output)); ; {
			var p listed
			if err := decoder.Decode(&p); errors.Is(err, io.EOF) {
				return packages, nil
			} else if err != nil {
				return nil, err
			}
			if p.Error != nil {
				return nil, fmt.Errorf("go list: %s: %s", p.ImportPath, p.Error.Err)
			}
			packages = append(packages, p)
		}
	}
	self, err := run("-json=ImportPath,Dir,GoFiles,TestGoFiles,Imports,TestImports,Error", "./test/e2e")
	if err != nil {
		return e2eListing{}, err
	}
	if len(self) != 1 {
		return e2eListing{}, fmt.Errorf("go list ./test/e2e answered %d packages", len(self))
	}
	imports := slices.Concat(self[0].Imports, self[0].TestImports)
	slices.Sort(imports)
	imports = slices.Compact(imports)
	// Only what the package imports is built, for its export data: the
	// package itself is parsed here, so it need not compile into a test.
	dependencies, err := run(append([]string{"-export", "-deps", "-json=ImportPath,Export,Error"}, imports...)...)
	if err != nil {
		return e2eListing{}, err
	}
	listing := e2eListing{dir: self[0].Dir, files: slices.Concat(self[0].GoFiles, self[0].TestGoFiles), exports: map[string]string{}}
	for _, dependency := range dependencies {
		if dependency.Export != "" {
			listing.exports[dependency.ImportPath] = dependency.Export
		}
	}
	return listing, nil
}

// loadE2ESource parses and type-checks test/e2e as the e2e tag builds it, with
// overlay standing in for the files it names, by base name.
func loadE2ESource(t *testing.T, overlay map[string]string) *e2eSource {
	t.Helper()
	listing, err := listE2EPackage()
	if err != nil {
		t.Fatalf("list test/e2e: %v", err)
	}
	s := &e2eSource{
		fset:      token.NewFileSet(),
		functions: map[*types.Func]*ast.FuncDecl{},
		values:    map[*types.Var]ast.Expr{},
		fields:    map[*types.Var][]ast.Expr{},
		info: &types.Info{
			Defs:       map[*ast.Ident]types.Object{},
			Uses:       map[*ast.Ident]types.Object{},
			Types:      map[ast.Expr]types.TypeAndValue{},
			Selections: map[*ast.SelectorExpr]*types.Selection{},
		},
	}
	files := make([]*ast.File, 0, len(listing.files))
	used := map[string]bool{}
	for _, name := range listing.files {
		var source any
		if content, ok := overlay[name]; ok {
			source, used[name] = content, true
		}
		file, err := parser.ParseFile(s.fset, filepath.Join(listing.dir, name), source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
	}
	for name := range overlay {
		if !used[name] {
			t.Fatalf("the overlay replaces %s, which test/e2e does not build", name)
		}
	}
	config := types.Config{Importer: importer.ForCompiler(s.fset, "gc", func(path string) (io.ReadCloser, error) {
		export, ok := listing.exports[path]
		if !ok {
			return nil, fmt.Errorf("go list built no export data for %s", path)
		}
		return os.Open(export) //nolint:gosec // A file go list named.
	})}
	s.pkg, err = config.Check("github.com/stokaro/ptah-operator/test/e2e", s.fset, files, s.info)
	if err != nil {
		t.Fatalf("type-check test/e2e with the e2e tag: %v", err)
	}
	for _, file := range files {
		s.index(file)
	}
	if len(s.functions) == 0 {
		t.Fatal("test/e2e declares no function, so no phase could be walked")
	}
	return s
}

func (s *e2eSource) index(file *ast.File) {
	for _, declaration := range file.Decls {
		switch declaration := declaration.(type) {
		case *ast.FuncDecl:
			if function, ok := s.info.Defs[declaration.Name].(*types.Func); ok {
				s.functions[function] = declaration
			}
		case *ast.GenDecl:
			for _, spec := range declaration.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for index, name := range value.Names {
					if variable, ok := s.info.Defs[name].(*types.Var); ok && index < len(value.Values) {
						s.values[variable] = value.Values[index]
					}
				}
			}
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.CompositeLit:
			structure, ok := s.underlying(node).(*types.Struct)
			if !ok {
				return true
			}
			for index, element := range node.Elts {
				if pair, ok := element.(*ast.KeyValueExpr); ok {
					if key, ok := pair.Key.(*ast.Ident); ok {
						if field, ok := s.info.Uses[key].(*types.Var); ok && field.IsField() {
							s.fields[field] = append(s.fields[field], pair.Value)
						}
					}
				} else if index < structure.NumFields() {
					s.fields[structure.Field(index)] = append(s.fields[structure.Field(index)], element)
				}
			}
		case *ast.AssignStmt:
			if len(node.Lhs) != len(node.Rhs) {
				return true
			}
			for index, target := range node.Lhs {
				if field := s.fieldOf(target); field != nil {
					s.fields[field] = append(s.fields[field], node.Rhs[index])
				}
			}
		}
		return true
	})
}

func (s *e2eSource) underlying(expression ast.Expr) types.Type {
	if typed, ok := s.info.Types[expression]; ok && typed.Type != nil {
		return typed.Type.Underlying()
	}
	return nil
}

func (s *e2eSource) fieldOf(expression ast.Expr) *types.Var {
	selector, ok := ast.Unparen(expression).(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	if selection := s.info.Selections[selector]; selection != nil && selection.Kind() == types.FieldVal {
		return selection.Obj().(*types.Var)
	}
	return nil
}

// method is the package's method of that name on that type.
func (s *e2eSource) method(receiver, name string) (*types.Func, error) {
	object := s.pkg.Scope().Lookup(receiver)
	if object == nil {
		return nil, fmt.Errorf("test/e2e declares no type %s", receiver)
	}
	named, ok := object.Type().(*types.Named)
	if !ok {
		return nil, fmt.Errorf("%s is not a named type", receiver)
	}
	for index := range named.NumMethods() {
		if named.Method(index).Name() == name {
			return named.Method(index), nil
		}
	}
	return nil, fmt.Errorf("test/e2e declares no method (*%s).%s", receiver, name)
}

// e2eResolvedFamily is a family's methods as the type checker knows them.
type e2eResolvedFamily struct {
	e2eArtifactFamily
	builder    *types.Func
	publishers map[*types.Func]int
}

// resolve finds the family's methods, and refuses a publisher whose argument
// at the family's index is not its reference string: the check would follow
// the wrong argument and report every artifact it pushes as unpublished.
func (s *e2eSource) resolve(family e2eArtifactFamily) (e2eResolvedFamily, error) {
	resolved := e2eResolvedFamily{e2eArtifactFamily: family, publishers: map[*types.Func]int{}}
	var err error
	if resolved.builder, err = s.method(family.receiver, family.builder); err != nil {
		return resolved, err
	}
	if len(family.publishers) == 0 {
		return resolved, fmt.Errorf("(*%s).%s names artifacts that nothing publishes", family.receiver, family.builder)
	}
	for name, index := range family.publishers {
		publisher, err := s.method(family.receiver, name)
		if err != nil {
			return resolved, err
		}
		parameters := publisher.Type().(*types.Signature).Params()
		if index < 0 || index >= parameters.Len() || parameters.At(index).Name() != "reference" ||
			!types.Identical(parameters.At(index).Type(), types.Typ[types.String]) {
			return resolved, fmt.Errorf("(*%s).%s takes no reference as argument %d", family.receiver, name, index)
		}
		resolved.publishers[publisher] = index
	}
	return resolved, nil
}

// callee is the package function a call names, if it names one.
func (s *e2eSource) callee(call *ast.CallExpr) *types.Func {
	var name *ast.Ident
	switch function := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		name = function
	case *ast.SelectorExpr:
		name = function.Sel
	case *ast.IndexExpr:
		if identifier, ok := function.X.(*ast.Ident); ok {
			name = identifier
		}
	}
	if name == nil {
		return nil
	}
	function, _ := s.info.Uses[name].(*types.Func)
	if function == nil || function.Pkg() != s.pkg {
		return nil
	}
	return function.Origin()
}

// reach is the code the phase's test can run, by the function or package
// variable it is declared in: each function a reached one names, called or
// not, since a method value handed to a scenario table is how most rows are
// run, and each package variable it reads, whose initializer may hold a
// function literal. A scenario table -- a map from scenario name to body, or a
// {name, body} pair -- is followed only for the scenarios this phase declares,
// because the alerting phases share one table and each runs its own rows from
// it.
func (s *e2eSource) reach(phase phases.Phase, scenarios map[string]bool) (map[types.Object]ast.Node, error) {
	root, ok := s.pkg.Scope().Lookup(phase.Test).(*types.Func)
	if !ok {
		return nil, fmt.Errorf("phase %s: test/e2e declares no function %s", phase.Name, phase.Test)
	}
	own := map[string]bool{}
	for _, scenario := range phase.Scenarios {
		own[scenario] = true
	}
	reached := map[types.Object]ast.Node{}
	var queue []ast.Node
	add := func(function *types.Func) {
		function = function.Origin()
		if _, seen := reached[function]; seen || function.Pkg() != s.pkg {
			return
		}
		reached[function] = nil
		if declaration := s.functions[function]; declaration != nil && declaration.Body != nil {
			reached[function] = declaration.Body
			queue = append(queue, declaration.Body)
		}
	}
	var methods []*types.Func
	for function := range s.functions {
		if function.Type().(*types.Signature).Recv() != nil {
			methods = append(methods, function)
		}
	}
	otherScenario := func(expression ast.Expr) bool {
		value := s.info.Types[expression].Value
		if value == nil || value.Kind() != constant.String {
			return false
		}
		name := constant.StringVal(value)
		return scenarios[name] && !own[name]
	}
	var visit func(ast.Node) bool
	visit = func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.CompositeLit:
			switch kind := s.underlying(node).(type) {
			case *types.Map:
				if _, body := kind.Elem().Underlying().(*types.Signature); !body {
					return true
				}
				for _, element := range node.Elts {
					if pair, ok := element.(*ast.KeyValueExpr); ok && otherScenario(pair.Key) {
						continue
					}
					ast.Inspect(element, visit)
				}
				return false
			case *types.Struct:
				if !scenarioPair(kind) {
					return true
				}
				for _, element := range node.Elts {
					if pair, ok := element.(*ast.KeyValueExpr); ok {
						element = pair.Value
					}
					if otherScenario(element) {
						return false
					}
				}
			}
		case *ast.Ident:
			switch object := s.info.Uses[node].(type) {
			case *types.Func:
				signature := object.Type().(*types.Signature)
				if receiver := signature.Recv(); receiver != nil && types.IsInterface(receiver.Type()) {
					// A call through an interface reaches every method of
					// the package that could answer it.
					for _, method := range methods {
						if method.Name() == object.Name() {
							add(method)
						}
					}
					return true
				}
				add(object)
			case *types.Var:
				if _, seen := reached[object]; !seen && object.Parent() == s.pkg.Scope() {
					reached[object] = nil
					if value := s.values[object]; value != nil {
						reached[object] = value
						queue = append(queue, value)
					}
				}
			}
		}
		return true
	}
	add(root)
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		ast.Inspect(node, visit)
	}
	return reached, nil
}

// scenarioPair is the shape of a scenario table's row: a name and a body.
func scenarioPair(structure *types.Struct) bool {
	if structure.NumFields() != 2 {
		return false
	}
	var name, body bool
	for index := range structure.NumFields() {
		switch field := structure.Field(index).Type().Underlying().(type) {
		case *types.Basic:
			name = name || field.Kind() == types.String
		case *types.Signature:
			body = true
		}
	}
	return name && body
}

// artifacts sorts every reference the reached code builds into the ones it
// publishes and the ones it reads.
func (s *e2eSource) artifacts(family e2eResolvedFamily, phase phases.Phase, reached map[types.Object]ast.Node) (e2ePhaseArtifacts, error) {
	builder, publishers := family.builder, family.publishers
	result := e2ePhaseArtifacts{phase: phase, scopes: len(reached)}
	published := map[*ast.CallExpr]bool{}
	var problems []error
	use := func(call *ast.CallExpr, scope types.Object) e2eArtifactUse {
		found := e2eArtifactUse{position: s.fset.Position(call.Pos()), scope: scope}
		if len(call.Args) == 1 {
			if value := s.info.Types[call.Args[0]].Value; value != nil && value.Kind() == constant.String {
				found.suffix, found.known = constant.StringVal(value), true
			}
		}
		return found
	}
	isBuilder := func(expression ast.Expr) *ast.CallExpr {
		call, ok := ast.Unparen(expression).(*ast.CallExpr)
		if ok && s.callee(call) == builder {
			return call
		}
		return nil
	}
	scopes := slices.SortedFunc(func(yield func(types.Object) bool) {
		for scope, node := range reached {
			if node != nil && !yield(scope) {
				return
			}
		}
	}, func(a, b types.Object) int { return int(a.Pos() - b.Pos()) })
	// The publishes first, so a reference handed to one is not counted as
	// a read too.
	for _, scope := range scopes {
		function, _ := scope.(*types.Func)
		_, forwards := publishers[function]
		ast.Inspect(reached[scope], func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			index, ok := publishers[s.callee(call)]
			if !ok || index >= len(call.Args) {
				return true
			}
			sources, resolved := s.referenceSources(reached[scope], call, call.Args[index], isBuilder)
			switch {
			case resolved:
				for _, source := range sources {
					published[source] = true
					publish := use(source, scope)
					publish.position = s.fset.Position(call.Pos())
					result.publishes = append(result.publishes, publish)
				}
			case !forwards:
				problems = append(problems, fmt.Errorf(
					"phase %s: %s publishes at %s:%d a reference the check cannot trace to (*%s).%s; build it there, or in a variable or field assigned from it",
					phase.Name, scope.Name(), filepath.Base(s.fset.Position(call.Pos()).Filename), s.fset.Position(call.Pos()).Line,
					family.receiver, family.builder))
			}
			return true
		})
	}
	for _, scope := range scopes {
		ast.Inspect(reached[scope], func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || s.callee(call) != builder || published[call] {
				return true
			}
			read := use(call, scope)
			if !read.known {
				problems = append(problems, fmt.Errorf(
					"phase %s: %s reads an artifact whose name the check cannot read; pass (*%s).%s a constant",
					phase.Name, read, family.receiver, family.builder))
				return true
			}
			result.reads = append(result.reads, read)
			return true
		})
	}
	for _, read := range result.reads {
		own := slices.ContainsFunc(result.publishes, func(publish e2eArtifactUse) bool {
			return publish.scope == read.scope && publish.known && publish.suffix == read.suffix
		})
		if !own {
			result.shared = append(result.shared, read)
		}
	}
	return result, errors.Join(problems...)
}

// referenceSources is the builder calls a publisher's reference argument was
// built by: the call itself, the last assignment in the same scope before the
// publish to a local variable, or every assignment anywhere to a struct field.
func (s *e2eSource) referenceSources(
	scope ast.Node, publish *ast.CallExpr, argument ast.Expr, isBuilder func(ast.Expr) *ast.CallExpr,
) ([]*ast.CallExpr, bool) {
	if call := isBuilder(argument); call != nil {
		return []*ast.CallExpr{call}, true
	}
	if field := s.fieldOf(argument); field != nil {
		var sources []*ast.CallExpr
		for _, value := range s.fields[field] {
			if call := isBuilder(value); call != nil {
				sources = append(sources, call)
			}
		}
		return sources, len(sources) > 0
	}
	identifier, ok := ast.Unparen(argument).(*ast.Ident)
	if !ok {
		return nil, false
	}
	variable, ok := s.info.Uses[identifier].(*types.Var)
	if !ok || variable.Parent() == s.pkg.Scope() {
		return nil, false
	}
	var last ast.Expr
	var lastAt token.Pos
	assigned := func(name ast.Expr, value ast.Expr) {
		identifier, ok := name.(*ast.Ident)
		if !ok || value == nil || identifier.Pos() >= publish.Pos() || identifier.Pos() < lastAt {
			return
		}
		if s.info.Defs[identifier] == variable || s.info.Uses[identifier] == variable {
			last, lastAt = value, identifier.Pos()
		}
	}
	ast.Inspect(scope, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.AssignStmt:
			if len(node.Lhs) == len(node.Rhs) {
				for index, name := range node.Lhs {
					assigned(name, node.Rhs[index])
				}
			}
		case *ast.ValueSpec:
			if len(node.Names) == len(node.Values) {
				for index, name := range node.Names {
					assigned(name, node.Values[index])
				}
			}
		}
		return true
	})
	if call := isBuilder(last); last != nil && call != nil {
		return []*ast.CallExpr{call}, true
	}
	return nil, false
}

// e2eArtifactReport is the check over every phase the suite catalog runs.
type e2eArtifactReport struct {
	examined int
	phases   []e2ePhaseArtifacts
}

// checkE2EArtifacts walks every phase support/e2e-suites.json runs and holds
// each one's shared reads to the artifacts it declares.
func checkE2EArtifacts(s *e2eSource, family e2eArtifactFamily, catalog e2eSuiteCatalog, declared []phases.Phase) (e2eArtifactReport, error) {
	resolved, err := s.resolve(family)
	if err != nil {
		return e2eArtifactReport{}, err
	}
	definitions := map[string]phases.Phase{}
	scenarios := map[string]bool{}
	for _, phase := range declared {
		definitions[phase.Name] = phase
		for _, scenario := range phase.Scenarios {
			scenarios[scenario] = true
		}
	}
	// loadE2ESuites refuses a phase a suite prepares with and no suite runs,
	// so the phases the suites run are every phase any suite starts.
	var names []string
	for _, suite := range catalog.Suites {
		names = append(names, suite.Phases...)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	var report e2eArtifactReport
	var problems []error
	for _, name := range names {
		phase, ok := definitions[name]
		if !ok {
			problems = append(problems, fmt.Errorf("%s runs phase %s, which test/e2e/phases does not declare", e2eSuitesPath, name))
			continue
		}
		reached, err := s.reach(phase, scenarios)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		report.examined++
		found, err := s.artifacts(resolved, phase, reached)
		problems = append(problems, err)
		report.phases = append(report.phases, found)
		problems = append(problems, found.verify())
	}
	if report.examined == 0 {
		problems = append(problems, fmt.Errorf("%s runs no phase test/e2e/phases declares, so no artifact was checked", e2eSuitesPath))
	}
	return report, errors.Join(problems...)
}

// verify holds the phase's shared reads to what it declares.
func (p e2ePhaseArtifacts) verify() error {
	var problems []error
	declared := map[string]bool{}
	for _, suffix := range p.phase.Artifacts {
		declared[suffix] = true
	}
	for _, read := range p.shared {
		if !declared[read.suffix] {
			problems = append(problems, fmt.Errorf(
				"phase %s: %s reads artifact %q, which it does not publish and the phase does not declare in Artifacts",
				p.phase.Name, read, read.suffix))
		}
	}
	shared := p.sharedSuffixes()
	for _, suffix := range p.phase.Artifacts {
		if !slices.ContainsFunc(p.publishes, func(publish e2eArtifactUse) bool { return publish.known && publish.suffix == suffix }) {
			problems = append(problems, fmt.Errorf(
				"phase %s declares artifact %q, and nothing the phase runs publishes it, so its rows read what another phase left",
				p.phase.Name, suffix))
		}
		if !slices.Contains(shared, suffix) {
			problems = append(problems, fmt.Errorf(
				"phase %s declares artifact %q, which no row of it reads without publishing it",
				p.phase.Name, suffix))
		}
	}
	return errors.Join(problems...)
}

func loadArtifactCatalog(t *testing.T) e2eSuiteCatalog {
	t.Helper()
	catalog, err := loadE2ESuites(repositoryFile(t, e2eSuitesPath))
	if err != nil {
		t.Fatalf("load the acceptance suites: %v", err)
	}
	return catalog
}

func TestEveryPhasePublishesTheArtifactsItsRowsShare(t *testing.T) {
	t.Parallel()
	catalog := loadArtifactCatalog(t)
	report, err := checkE2EArtifacts(loadE2ESource(t, nil), migrationArtifacts, catalog, phases.All())
	if err != nil {
		t.Fatal(err)
	}
	// Every declared phase is one the catalog runs, so the walk reached all
	// of them, and the ones that name a migration artifact are the ones whose
	// rows read it.
	if want := len(phases.All()); report.examined != want {
		t.Fatalf("examined %d phases, want the %d test/e2e/phases declares", report.examined, want)
	}
	var naming, reads, publishes int
	for _, phase := range report.phases {
		if len(phase.reads)+len(phase.publishes) > 0 {
			naming++
		}
		reads += len(phase.reads)
		publishes += len(phase.publishes)
		t.Logf("%-30s %4d scopes %3d reads %3d publishes, shares %q",
			phase.phase.Name, phase.scopes, len(phase.reads), len(phase.publishes), phase.sharedSuffixes())
	}
	if naming == 0 || reads == 0 || publishes == 0 {
		t.Fatalf("%d phases name a migration artifact, with %d reads and %d publishes; the check examined nothing", naming, reads, publishes)
	}
	var declaring int
	for _, phase := range phases.All() {
		if len(phase.Artifacts) > 0 {
			declaring++
		}
	}
	if declaring == 0 {
		t.Fatal("no phase declares an artifact, so the check held nothing to its rows")
	}
}

// mutatedE2EFile is test/e2e's file with one thing changed, so a refusal the
// check earns is attributable to that thing.
func mutatedE2EFile(t *testing.T, name, old, new string) map[string]string {
	t.Helper()
	source, err := os.ReadFile(repositoryFile(t, filepath.Join("test", "e2e", name)))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if count := strings.Count(string(source), old); count != 1 {
		t.Fatalf("%s holds %d instances of %q, want 1", name, count, old)
	}
	return map[string]string{name: strings.Replace(string(source), old, new, 1)}
}

// withPhase is the declared phases with the named one changed.
func withPhase(t *testing.T, name string, change func(*phases.Phase)) []phases.Phase {
	t.Helper()
	declared := phases.All()
	for index := range declared {
		if declared[index].Name == name {
			change(&declared[index])
			return declared
		}
	}
	t.Fatalf("test/e2e/phases declares no phase %s", name)
	return nil
}

func TestTheArtifactCheckRefusesAPhaseThatDropsOne(t *testing.T) {
	t.Parallel()
	catalog := loadArtifactCatalog(t)
	tests := map[string]struct {
		overlay  map[string]string
		declared []phases.Phase
		// want is every message the check has to give, and only those.
		want []string
	}{
		// #931: the fault rows create their resources from the slow artifact,
		// which the migrations phase publishes in its own rows.
		"a fault phase that stops publishing the uncertain artifact": {
			overlay: mutatedE2EFile(t, "migrations_e2e_test.go",
				`			m.publish("fault-uncertain", m.fixtureDir("-uncertain"), m.reference("-uncertain"))
`, ""),
			want: []string{
				`phase migration-faults-mysql declares artifact "-uncertain", and nothing the phase runs publishes it`,
				`phase migration-faults-postgresql declares artifact "-uncertain", and nothing the phase runs publishes it`,
			},
		},
		"a fault phase that stops publishing the engine's artifact": {
			overlay: mutatedE2EFile(t, "migrations_e2e_test.go",
				`			m.publish("fault-source", m.fixtureDir(""), m.reference(""))
`, ""),
			want: []string{
				`phase migration-faults-mysql declares artifact "", and nothing the phase runs publishes it`,
				`phase migration-faults-postgresql declares artifact "", and nothing the phase runs publishes it`,
			},
		},
		"a runtime phase that stops publishing the engine's artifact": {
			overlay: mutatedE2EFile(t, "migrations_runtime_e2e_test.go",
				`			m.publish("runtime-source", m.fixtureDir(""), m.reference(""))
`, ""),
			want: []string{
				`phase migration-runtime-mysql declares artifact "", and nothing the phase runs publishes it`,
				`phase migration-runtime-postgresql declares artifact "", and nothing the phase runs publishes it`,
			},
		},
		"a shared read the phase does not declare": {
			declared: withPhase(t, "migration-faults-postgresql", func(p *phases.Phase) { p.Artifacts = []string{""} }),
			want: []string{
				`phase migration-faults-postgresql: deletionDuringApplyProof at migrations_faults_e2e_test.go:`,
				`phase migration-faults-postgresql: lostLogProof at migrations_faults_e2e_test.go:`,
				`phase migration-faults-postgresql: suspensionDuringApplyProof at migrations_faults_e2e_test.go:`,
				`phase migration-faults-postgresql: createResource at migrations_isolation_e2e_test.go:`,
			},
		},
		"a declaration no shared read needs": {
			declared: withPhase(t, "migration-faults-postgresql", func(p *phases.Phase) {
				p.Artifacts = append(slices.Clone(p.Artifacts), "-stopped")
			}),
			want: []string{
				`phase migration-faults-postgresql declares artifact "-stopped", which no row of it reads without publishing it`,
			},
		},
		"a declaration nothing publishes or reads": {
			declared: withPhase(t, "migrations-postgresql", func(p *phases.Phase) {
				p.Artifacts = append(slices.Clone(p.Artifacts), "-never")
			}),
			want: []string{
				`phase migrations-postgresql declares artifact "-never", and nothing the phase runs publishes it`,
				`phase migrations-postgresql declares artifact "-never", which no row of it reads without publishing it`,
			},
		},
		// A scenario table is followed for the phase's own scenarios. When
		// the certificate phase runs the unresolved-apply body as well, its
		// rows read the slow artifact that body publishes.
		"a scenario the phase starts running from the shared table": {
			declared: withPhase(t, "alerting-certificates", func(p *phases.Phase) {
				p.Scenarios = append(slices.Clone(p.Scenarios), "unresolved-apply")
			}),
			want: []string{
				`phase alerting-certificates: unresolvedMigrationCase at alerting_unresolved_e2e_test.go:`,
			},
		},
		"a row that reads another artifact than the one it publishes": {
			overlay: mutatedE2EFile(t, "migrations_faults_e2e_test.go",
				`m.stopRowResource(name, secret, m.reference("-stopped"),`,
				`m.stopRowResource(name, secret, m.reference("-stopped-elsewhere"),`),
			want: []string{
				`phase migration-faults-mysql: stoppedApplyProof at migrations_faults_e2e_test.go:`,
				`phase migration-faults-postgresql: stoppedApplyProof at migrations_faults_e2e_test.go:`,
			},
		},
		"a read whose artifact the check cannot name": {
			overlay: mutatedE2EFile(t, "migrations_isolation_e2e_test.go",
				`name: r.name, secret: r.secret, reference: m.reference("-uncertain"),
		coordinationKey: "e2e/isolated-node/"`,
				`name: r.name, secret: r.secret, reference: m.reference(strings.ToLower("-uncertain")),
		coordinationKey: "e2e/isolated-node/"`),
			want: []string{
				`phase migration-faults-mysql: createResource at migrations_isolation_e2e_test.go:`,
				`phase migration-faults-postgresql: createResource at migrations_isolation_e2e_test.go:`,
			},
		},
		"a publish the check cannot trace": {
			overlay: mutatedE2EFile(t, "migrations_e2e_test.go",
				`m.publish("fault-uncertain", m.fixtureDir("-uncertain"), m.reference("-uncertain"))`,
				`m.publish("fault-uncertain", m.fixtureDir("-uncertain"), strings.Clone(m.reference("-uncertain")))`),
			want: []string{
				`phase migration-faults-mysql: runMigrationFaultsPhase publishes at migrations_e2e_test.go:`,
				`phase migration-faults-mysql declares artifact "-uncertain", and nothing the phase runs publishes it`,
				`phase migration-faults-postgresql: runMigrationFaultsPhase publishes at migrations_e2e_test.go:`,
				`phase migration-faults-postgresql declares artifact "-uncertain", and nothing the phase runs publishes it`,
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			declared := test.declared
			if declared == nil {
				declared = phases.All()
			}
			_, err := checkE2EArtifacts(loadE2ESource(t, test.overlay), migrationArtifacts, catalog, declared)
			if err == nil {
				t.Fatal("the check passed")
			}
			messages := strings.Split(err.Error(), "\n")
			for _, want := range test.want {
				if !slices.ContainsFunc(messages, func(message string) bool { return strings.HasPrefix(message, want) }) {
					t.Errorf("no message starts with %q", want)
				}
			}
			for _, message := range messages {
				if !slices.ContainsFunc(test.want, func(want string) bool { return strings.HasPrefix(message, want) }) {
					t.Errorf("unexpected message %q", message)
				}
			}
		})
	}
}

// The family and the catalog are what the check reads before any source, and
// a mistake in either would leave it checking nothing.
func TestTheArtifactCheckRefusesWhatItCannotCheck(t *testing.T) {
	t.Parallel()
	source := loadE2ESource(t, nil)
	catalog := loadArtifactCatalog(t)
	withPublishers := func(publishers map[string]int) e2eArtifactFamily {
		family := migrationArtifacts
		family.publishers = publishers
		return family
	}
	tests := map[string]struct {
		family  e2eArtifactFamily
		catalog e2eSuiteCatalog
		want    string
	}{
		"a publisher whose reference is another argument": {
			family:  withPublishers(map[string]int{"publish": 1, "publishWithCheck": 2, "publishUnknownLayerArtifact": 0}),
			catalog: catalog,
			want:    "(*migrationRun).publish takes no reference as argument 1",
		},
		"a publisher past its arguments": {
			family:  withPublishers(map[string]int{"publish": 2, "publishWithCheck": 2, "publishUnknownLayerArtifact": 1}),
			catalog: catalog,
			want:    "(*migrationRun).publishUnknownLayerArtifact takes no reference as argument 1",
		},
		"a family nothing publishes": {
			family:  withPublishers(nil),
			catalog: catalog,
			want:    "(*migrationRun).reference names artifacts that nothing publishes",
		},
		"a builder the package does not declare": {
			family:  e2eArtifactFamily{receiver: "migrationRun", builder: "artifact", publishers: migrationArtifacts.publishers},
			catalog: catalog,
			want:    "test/e2e declares no method (*migrationRun).artifact",
		},
		"a catalog that runs nothing": {
			family:  migrationArtifacts,
			catalog: e2eSuiteCatalog{},
			want:    "runs no phase test/e2e/phases declares, so no artifact was checked",
		},
		"a catalog phase nothing declares": {
			family:  migrationArtifacts,
			catalog: e2eSuiteCatalog{Suites: []e2eSuite{{Name: "smoke", Phases: []string{"smoke"}}}},
			want:    "runs phase smoke, which test/e2e/phases does not declare",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := checkE2EArtifacts(source, test.family, test.catalog, phases.All())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("checkE2EArtifacts() error = %v, want one containing %q", err, test.want)
			}
		})
	}
}

// A publish counts wherever the phase reaches it: behind a method a package
// interface dispatches to, or in a function literal a package variable holds.
// Either would otherwise read as an artifact nothing publishes.
func TestTheArtifactCheckFollowsAPublishWhereverThePhaseReachesIt(t *testing.T) {
	t.Parallel()
	catalog := loadArtifactCatalog(t)
	const publish = `			m.publish("fault-uncertain", m.fixtureDir("-uncertain"), m.reference("-uncertain"))
`
	tests := map[string]struct{ call, declaration string }{
		"a method an interface dispatches to": {
			call: `			var publisher uncertainPublisher = m
			publisher.publishUncertain()
`,
			declaration: `
type uncertainPublisher interface{ publishUncertain() }

func (m *migrationRun) publishUncertain() {
` + publish + `}
`,
		},
		"a function literal a package variable holds": {
			call: `			publishUncertainArtifact(m)
`,
			declaration: `
var publishUncertainArtifact = func(m *migrationRun) {
` + publish + `}
`,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			overlay := mutatedE2EFile(t, "migrations_e2e_test.go", publish, test.call)
			overlay["migrations_e2e_test.go"] += test.declaration
			if _, err := checkE2EArtifacts(loadE2ESource(t, overlay), migrationArtifacts, catalog, phases.All()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A scenario table holds the bodies of several phases. Each phase reaches the
// bodies of its own scenarios and no other.
func TestAPhaseReachesOnlyItsOwnScenarios(t *testing.T) {
	t.Parallel()
	source := loadE2ESource(t, nil)
	scenarios := map[string]bool{}
	for _, phase := range phases.All() {
		for _, scenario := range phase.Scenarios {
			scenarios[scenario] = true
		}
	}
	for _, test := range []struct {
		phase    phases.Phase
		receiver string
		body     string
		reached  bool
	}{
		// The struct pairs runMigrationPhase appends for MySQL alone.
		{phases.MigrationsMySQL.Phase, "migrationRun", "transactionModeProof", true},
		{phases.MigrationsPostgreSQL.Phase, "migrationRun", "transactionModeProof", false},
		// The map every alerting phase picks its scenarios from.
		{phases.AlertingOperations.Phase, "alertingRun", "unresolvedApply", true},
		{phases.AlertingCertificates.Phase, "alertingRun", "unresolvedApply", false},
		{phases.Alerting.Phase, "alertingRun", "unresolvedApply", false},
	} {
		t.Run(test.phase.Name+"/"+test.body, func(t *testing.T) {
			t.Parallel()
			body, err := source.method(test.receiver, test.body)
			if err != nil {
				t.Fatal(err)
			}
			reached, err := source.reach(test.phase, scenarios)
			if err != nil {
				t.Fatal(err)
			}
			if _, found := reached[body]; found != test.reached {
				t.Fatalf("phase %s reaches (*%s).%s: %t, want %t", test.phase.Name, test.receiver, test.body, found, test.reached)
			}
		})
	}
}
