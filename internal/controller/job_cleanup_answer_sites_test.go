package controller

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A cleanup TTL write a pass makes before it acts on a finished Job has to be
// settled, or a policy that refuses the manager's TTL patch fails every pass and
// the resource never moves again (#998). The calls that make that write are
// markJobHarvested, the harvest's five minutes, and retainFailedApplyJob, the
// rule a failed or uncertain Apply keeps its log by. The table below is where each
// may be called, and what takes its answer. A new call has to be added here, which
// is the moment to decide how its answer is settled: through harvestJob or
// harvestMigrationJob for a harvest, or through one of the settle functions for
// the rest.

// cleanupAnswerSite is one call of a function that writes a Job's cleanup TTL: the
// function it is made in, the function called, and what takes the answer. The
// answer is taken by the call it is an argument of, by its name; or it is
// "return", "if" (the init of an if statement), "switch", "assign" or "bare".
type cleanupAnswerSite struct {
	in, call, answer string
}

func (site cleanupAnswerSite) String() string {
	return fmt.Sprintf("%s calls %s, and %s takes the answer", site.in, site.call, site.answer)
}

// cleanupTTLWriters are the functions whose calls cleanupAnswerSites collects.
var cleanupTTLWriters = []string{"markJobHarvested", "retainFailedApplyJob"}

// settledCleanupAnswerSites is every call of a cleanupTTLWriters function the
// package may make.
var settledCleanupAnswerSites = []cleanupAnswerSite{
	{in: "SchemaReconciler.harvestJob", call: "markJobHarvested", answer: "settleSchemaJobCleanup"},
	{in: "MigrationReconciler.harvestMigrationJob", call: "markJobHarvested", answer: "settleMigrationJobCleanup"},
	{in: "SchemaReconciler.cleanupRetiredReadOnlyJob", call: "markJobHarvested", answer: "settleJobCleanupAnswer"},
	{in: "SchemaReconciler.recoverLeaseContinuity", call: "markJobHarvested", answer: "settleJobCleanupAnswer"},
	{in: "SchemaReconciler.letGoOfReadOnlySchemaJob", call: "markJobHarvested", answer: "settleJobCleanupAnswer"},
	{in: "MigrationReconciler.letGoOfReadOnlyMigrationJob", call: "markJobHarvested", answer: "settleMigrationJobCleanupAnswer"},
	{in: "SchemaReconciler.finishUncertainApplyWithEvidenceAndBinding", call: "retainFailedApplyJob", answer: "settleSchemaJobCleanup"},
	{in: "MigrationReconciler.recordMigrationRun", call: "retainFailedApplyJob", answer: "settleMigrationJobCleanup"},
	{in: "SchemaReconciler.cleanupRetiredApplyJob", call: "retainFailedApplyJob", answer: "settleRetiredApplyJobCleanup"},
	// Its callers settle what it returns: scheduleStoppedMigrationJobTTL through
	// settleMigrationJobCleanup, and the deletion path by its own switch.
	{in: "MigrationReconciler.scheduleUnaccountedMigrationJobCleanup", call: "retainFailedApplyJob", answer: "return"},
}

func TestEveryJobCleanupAnswerIsSettledWhereItIsAsked(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	var parsed []*ast.File
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := parser.ParseFile(fileSet, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, source)
	}
	if len(parsed) == 0 {
		t.Fatal("no source file was read, so no call was checked")
	}
	sites := cleanupAnswerSites(parsed)
	if unexpected := unsettledCleanupAnswerSites(sites, settledCleanupAnswerSites); len(unexpected) > 0 {
		t.Fatalf("calls whose answer nothing in the table settles:\n%s", strings.Join(unexpected, "\n"))
	}
	if len(sites) != len(settledCleanupAnswerSites) {
		t.Fatalf("read %d calls, want the %d the table names:\n%v", len(sites), len(settledCleanupAnswerSites), sites)
	}
}

// TestTheCleanupAnswerSiteCheckRefusesAnUnsettledCall holds the check to the
// shapes a harvest took before #998, which it must refuse, and to the settled one,
// which it must accept, so a check that read no call or read every call as settled
// is found here.
func TestTheCleanupAnswerSiteCheckRefusesAnUnsettledCall(t *testing.T) {
	t.Parallel()

	const source = `package controller

func (r *SchemaReconciler) harvestJob(ctx context.Context, job *batchv1.Job) error {
	return r.settleSchemaJobCleanup(ctx, nil, job, r.markJobHarvested(ctx, job))
}

func (r *SchemaReconciler) consume(ctx context.Context, job *batchv1.Job) error {
	if err := r.markJobHarvested(ctx, job); err != nil {
		return err
	}
	r.markJobHarvested(ctx, job)
	_ = retainFailedApplyJob(ctx, r.Client, job, true, nil)
	return r.markJobHarvested(ctx, job)
}
`
	parsed, err := parser.ParseFile(token.NewFileSet(), "sample.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	sites := cleanupAnswerSites([]*ast.File{parsed})
	want := []cleanupAnswerSite{
		{in: "SchemaReconciler.harvestJob", call: "markJobHarvested", answer: "settleSchemaJobCleanup"},
		{in: "SchemaReconciler.consume", call: "markJobHarvested", answer: "if"},
		{in: "SchemaReconciler.consume", call: "markJobHarvested", answer: "bare"},
		{in: "SchemaReconciler.consume", call: "retainFailedApplyJob", answer: "assign"},
		{in: "SchemaReconciler.consume", call: "markJobHarvested", answer: "return"},
	}
	if !slices.Equal(sites, want) {
		t.Fatalf("cleanupAnswerSites() = %v, want %v", sites, want)
	}
	unexpected := unsettledCleanupAnswerSites(sites, settledCleanupAnswerSites)
	if len(unexpected) != 4 || slices.ContainsFunc(unexpected, func(line string) bool { return strings.Contains(line, "harvestJob") }) {
		t.Fatalf("unsettledCleanupAnswerSites() = %q, want the four calls in consume and not the settled harvest", unexpected)
	}
}

// unsettledCleanupAnswerSites is every site the allowed table does not name, and
// every allowed site seen other than exactly once.
func unsettledCleanupAnswerSites(sites, allowed []cleanupAnswerSite) []string {
	var unexpected []string
	seen := map[cleanupAnswerSite]int{}
	for _, site := range sites {
		if !slices.Contains(allowed, site) {
			unexpected = append(unexpected, site.String())
			continue
		}
		seen[site]++
	}
	for _, site := range allowed {
		if n := seen[site]; n > 1 {
			unexpected = append(unexpected, fmt.Sprintf("%s, %d times", site, n))
		}
	}
	return unexpected
}

// cleanupAnswerSites lists every call of a cleanupTTLWriters function in files,
// in source order.
func cleanupAnswerSites(files []*ast.File) []cleanupAnswerSite {
	var sites []cleanupAnswerSite
	for _, file := range files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			in := function.Name.Name
			if function.Recv != nil && len(function.Recv.List) == 1 {
				in = receiverTypeName(function.Recv.List[0].Type) + "." + in
			}
			var stack []ast.Node
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if node == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				if call, ok := node.(*ast.CallExpr); ok {
					if name := calledName(call.Fun); slices.Contains(cleanupTTLWriters, name) {
						sites = append(sites, cleanupAnswerSite{in: in, call: name, answer: answerTaker(call, stack)})
					}
				}
				stack = append(stack, node)
				return true
			})
		}
	}
	return sites
}

// answerTaker names what takes the value of call, whose ancestors are stack.
func answerTaker(call *ast.CallExpr, stack []ast.Node) string {
	if len(stack) == 0 {
		return "bare"
	}
	switch parent := stack[len(stack)-1].(type) {
	case *ast.CallExpr:
		if slices.Contains(parent.Args, ast.Expr(call)) {
			return calledName(parent.Fun)
		}
	case *ast.ReturnStmt:
		return "return"
	case *ast.ExprStmt:
		return "bare"
	case *ast.AssignStmt:
		if len(stack) > 1 {
			switch grandparent := stack[len(stack)-2].(type) {
			case *ast.IfStmt:
				if grandparent.Init == parent {
					return "if"
				}
			case *ast.SwitchStmt:
				if grandparent.Init == parent {
					return "switch"
				}
			}
		}
		return "assign"
	}
	return fmt.Sprintf("%T", stack[len(stack)-1])
}

// calledName is the name of the function or method an expression calls.
func calledName(fun ast.Expr) string {
	switch fun := fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

// receiverTypeName is the type a method's receiver names, without its pointer.
func receiverTypeName(receiver ast.Expr) string {
	if star, ok := receiver.(*ast.StarExpr); ok {
		receiver = star.X
	}
	if ident, ok := receiver.(*ast.Ident); ok {
		return ident.Name
	}
	return fmt.Sprintf("%T", receiver)
}
