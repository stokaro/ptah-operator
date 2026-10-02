package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// RBAC and the code that needs it are edited in different files, months apart,
// and neither one fails when they disagree. A missing grant is a refusal in a
// cluster and nowhere else; an extra grant is a privilege nobody can see is
// unused -- the manager was allowed to write a PtahMigrationPlan's status, and
// nothing has ever written one.
//
// These hold the shipped ClusterRole and the status writes to each other.

const (
	chartRoles        = "charts/ptah-operator/templates/rbac.yaml"
	securityGroup     = "operator.ptah.run"
	statusSubresource = "/status"
)

var (
	ruleAPIGroups = regexp.MustCompile(`^\s*- apiGroups: \[(.*)\]\s*$`)
	ruleResources = regexp.MustCompile(`^\s*resources: \[(.*)\]\s*$`)
	ruleVerbs     = regexp.MustCompile(`^\s*verbs: \[(.*)\]\s*$`)
	quotedItem    = regexp.MustCompile(`"([^"]+)"`)
)

// Every kind whose status the operator writes is granted that write.
func TestEveryStatusWriteIsGranted(t *testing.T) {
	t.Parallel()
	granted := grantedStatusWrites(t)
	for _, kind := range writtenStatusKinds(t) {
		resource := strings.ToLower(kind) + "s" + statusSubresource
		if !granted[resource] {
			t.Errorf("the operator writes a %s status and the ClusterRole grants no write on %s, which is a refusal in a cluster and nowhere else",
				kind, resource)
		}
	}
}

// And every status write the ClusterRole grants is one the operator makes.
func TestEveryGrantedStatusWriteIsUsed(t *testing.T) {
	t.Parallel()
	used := map[string]bool{}
	for _, kind := range writtenStatusKinds(t) {
		used[strings.ToLower(kind)+"s"+statusSubresource] = true
	}
	for resource := range grantedStatusWrites(t) {
		if !used[resource] {
			t.Errorf("the ClusterRole grants a write on %s and nothing writes that status, so the operator holds a privilege it does not use",
				resource)
		}
	}
}

// Default installations grant no Secret access. Durable delivery adds only
// CREATE for admission-validated projections; neither mode may read credentials.
func TestTheShippedRolesGrantOnlyDeclaredSecretPermission(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		values := []string{}
		if enabled {
			values = append(values, "resultDelivery.enabled=true")
		}
		roles := renderedClusterRoles(t, values...)
		if len(roles) == 0 {
			t.Fatal("no shipped roles were examined")
		}
		grants := 0
		for _, role := range roles {
			for _, rule := range role.Rules {
				for _, resource := range rule.Resources {
					if resource == "*" {
						t.Fatalf("%s grants every resource", role.Name)
					}
					if !strings.HasPrefix(resource, "secrets") {
						continue
					}
					grants++
					if !enabled || role.Name != approverRelease+"-ptah-operator" || resource != "secrets" ||
						len(rule.Verbs) != 1 || rule.Verbs[0] != "create" ||
						len(rule.APIGroups) != 1 || rule.APIGroups[0] != "" {
						t.Fatalf("durable=%v: %s has an undeclared Secret grant: %#v", enabled, role.Name, rule)
					}
				}
			}
		}
		want := 0
		if enabled {
			want = 1
		}
		if grants != want {
			t.Fatalf("durable=%v: found %d Secret grants, want %d", enabled, grants, want)
		}
	}
}

// grantedStatusWrites reads the status subresources the operator's own group
// grants a write on.
func grantedStatusWrites(t *testing.T) map[string]bool {
	t.Helper()
	granted := map[string]bool{}
	lines := strings.Split(readDocumentationPage(t, chartRoles), "\n")
	group, resources := "", []string(nil)
	for _, line := range lines {
		if match := ruleAPIGroups.FindStringSubmatch(line); match != nil {
			group, resources = firstQuoted(match[1]), nil
			continue
		}
		if match := ruleResources.FindStringSubmatch(line); match != nil {
			resources = allQuoted(match[1])
			continue
		}
		match := ruleVerbs.FindStringSubmatch(line)
		if match == nil || group != securityGroup {
			continue
		}
		if !writes(allQuoted(match[1])) {
			resources = nil
			continue
		}
		for _, resource := range resources {
			if strings.HasSuffix(resource, statusSubresource) {
				granted[resource] = true
			}
		}
		resources = nil
	}
	if len(granted) == 0 {
		t.Fatalf("%s grants no status write in %s, so these checks compare against nothing", chartRoles, securityGroup)
	}
	return granted
}

func writes(verbs []string) bool {
	for _, verb := range verbs {
		switch verb {
		case "update", "patch", "create", "*":
			return true
		}
	}
	return false
}

func firstQuoted(list string) string {
	items := allQuoted(list)
	if len(items) == 0 {
		return ""
	}
	return items[0]
}

func allQuoted(list string) []string {
	var items []string
	for _, match := range quotedItem.FindAllStringSubmatch(list, -1) {
		items = append(items, match[1])
	}
	return items
}

// writtenStatusKinds reads which kinds the operator writes the status of. A
// write whose object this cannot resolve to an API kind fails the check rather
// than shrinking the set, because a short set turns the grant check into one
// that always passes.
func writtenStatusKinds(t *testing.T) []string {
	t.Helper()
	kinds := map[string]bool{}
	var unresolved []string
	fileSet := token.NewFileSet()
	for _, root := range []string{"internal", "cmd"} {
		base := repositoryFile(t, root)
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			file, parseErr := parser.ParseFile(fileSet, path, nil, 0)
			if parseErr != nil {
				t.Fatalf("parse %s: %v", path, parseErr)
			}
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if !ok || function.Body == nil {
					continue
				}
				ast.Inspect(function.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok || !isStatusWrite(call) || len(call.Args) < 2 {
						return true
					}
					name, ok := call.Args[1].(*ast.Ident)
					if !ok {
						unresolved = append(unresolved, fileSet.Position(call.Pos()).String())
						return true
					}
					kind, ok := declaredAPIKind(function, name.Name)
					if !ok {
						unresolved = append(unresolved, fileSet.Position(call.Pos()).String()+" writes "+name.Name)
						return true
					}
					kinds[kind] = true
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		t.Fatalf("these status writes do not name an API kind this can read, so the set of written statuses is incomplete: %v", unresolved)
	}
	if len(kinds) == 0 {
		t.Fatal("no status write was found, so these checks would call every grant unused")
	}
	var written []string
	for kind := range kinds {
		written = append(written, kind)
	}
	sort.Strings(written)
	return written
}

// isStatusWrite reports whether the call is Status().Update or Status().Patch.
func isStatusWrite(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (selector.Sel.Name != "Update" && selector.Sel.Name != "Patch") {
		return false
	}
	inner, ok := selector.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	innerSelector, ok := inner.Fun.(*ast.SelectorExpr)
	return ok && innerSelector.Sel.Name == "Status"
}
