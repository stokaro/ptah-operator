package main

import (
	"go/ast"
)

// Two gates ask the same question of the source: which API kind does this
// variable hold? One reads it to say whose status a write belongs to, the other
// to say whose Conditions a call appends to. The answer is the same shape in
// both, so it is written once here rather than twice with a chance of being
// fixed in one place only.

// declaredAPIKind finds the API kind a name holds inside one function, from its
// parameters or from where the body assigns it an API type.
func declaredAPIKind(function *ast.FuncDecl, name string) (string, bool) {
	if function.Type.Params != nil {
		for _, field := range function.Type.Params.List {
			for _, parameter := range field.Names {
				if parameter.Name == name {
					if kind, ok := operatorType(field.Type); ok {
						return kind, true
					}
				}
			}
		}
	}
	kind, found := "", false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for index, target := range assignment.Lhs {
			identifier, ok := target.(*ast.Ident)
			if !ok || identifier.Name != name || index >= len(assignment.Rhs) {
				continue
			}
			if resolved, ok := operatorType(assignment.Rhs[index]); ok {
				kind, found = resolved, true
			}
		}
		return !found
	})
	return kind, found
}

// operatorType reads an API kind out of a type expression or a literal of one.
func operatorType(expression ast.Expr) (string, bool) {
	switch node := expression.(type) {
	case *ast.StarExpr:
		return operatorType(node.X)
	case *ast.UnaryExpr:
		return operatorType(node.X)
	case *ast.CompositeLit:
		return operatorType(node.Type)
	case *ast.SelectorExpr:
		return operatorConstant(node)
	}
	return "", false
}

// operatorConstant reads the name of an operatorv1alpha1 selector.
func operatorConstant(expression ast.Expr) (string, bool) {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	identifier, ok := selector.X.(*ast.Ident)
	if !ok || identifier.Name != "operatorv1alpha1" {
		return "", false
	}
	return selector.Sel.Name, true
}
