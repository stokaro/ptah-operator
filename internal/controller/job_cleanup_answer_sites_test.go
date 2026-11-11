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
// the resource never moves again (#998). The functions that make that write are
// scheduleJobCleanup, the patch itself, and the three that call it:
// markJobHarvested, the harvest's five minutes; retainFailedApplyJob, the rule a
// failed or uncertain Apply keeps its log by; and
// scheduleUnaccountedMigrationJobCleanup, that rule for a migration run nobody
// accounted for. The table below is where each may be named, and what takes its
// answer. A new call has to be added here, which is the moment to decide how its
// answer is settled: through harvestJob or harvestMigrationJob for a harvest, or
// through one of the settle functions for the rest.

// cleanupAnswerSite is one use of a function that writes a Job's cleanup TTL: the
// function it is made in, the function used, and what takes the answer. The
// answer is taken by the call it is an argument of, by its name; or it is
// "return", "if" (the init of an if statement), "switch", "assign" or "bare". A
// function used as a value rather than called is the answer "value": nothing here
// can say what takes the answer of the call made through it.
type cleanupAnswerSite struct {
	in, call, answer string
}

func (site cleanupAnswerSite) String() string {
	if site.answer == valueUse {
		return fmt.Sprintf("%s uses %s as a value, so the answer of its call is taken out of sight", site.in, site.call)
	}
	return fmt.Sprintf("%s calls %s, and %s takes the answer", site.in, site.call, site.answer)
}

// valueUse is the answer of a site that names a cleanupTTLWriters function
// without calling it.
const valueUse = "value"

// cleanupTTLWriters are the functions whose uses cleanupAnswerSites collects.
var cleanupTTLWriters = []string{
	"scheduleJobCleanup",
	"markJobHarvested",
	"retainFailedApplyJob",
	"scheduleUnaccountedMigrationJobCleanup",
}

// settledCleanupAnswerSites is every use of a cleanupTTLWriters function the
// package may make. A writer that returns the answer of another hands it to its
// own callers, whose sites are rows here as well.
var settledCleanupAnswerSites = []cleanupAnswerSite{
	{in: "SchemaReconciler.markJobHarvested", call: "scheduleJobCleanup", answer: "return"},
	{in: "MigrationReconciler.markJobHarvested", call: "scheduleJobCleanup", answer: "return"},
	// The short TTL's answer is returned; the long TTL's refusal is what the
	// short one is asked for after, and any other answer to it is returned.
	{in: "retainFailedApplyJob", call: "scheduleJobCleanup", answer: "return"},
	{in: "retainFailedApplyJob", call: "scheduleJobCleanup", answer: "assign"},
	{in: "retainFailedApplyJob", call: "scheduleJobCleanup", answer: "if"},
	{in: "SchemaReconciler.harvestJob", call: "markJobHarvested", answer: "settleSchemaJobCleanup"},
	{in: "MigrationReconciler.harvestMigrationJob", call: "markJobHarvested", answer: "settleMigrationJobCleanup"},
	{in: "SchemaReconciler.cleanupRetiredReadOnlyJob", call: "markJobHarvested", answer: "settleJobCleanupAnswer"},
	{in: "SchemaReconciler.recoverLeaseContinuity", call: "markJobHarvested", answer: "settleJobCleanupAnswer"},
	{in: "SchemaReconciler.letGoOfReadOnlySchemaJob", call: "markJobHarvested", answer: "settleJobCleanupAnswer"},
	{in: "MigrationReconciler.letGoOfReadOnlyMigrationJob", call: "markJobHarvested", answer: "settleMigrationJobCleanupAnswer"},
	{in: "SchemaReconciler.finishUncertainApplyWithEvidenceAndBinding", call: "retainFailedApplyJob", answer: "settleSchemaJobCleanup"},
	{in: "MigrationReconciler.recordMigrationRun", call: "retainFailedApplyJob", answer: "settleMigrationJobCleanup"},
	{in: "SchemaReconciler.cleanupRetiredApplyJob", call: "retainFailedApplyJob", answer: "settleRetiredApplyJobCleanup"},
	{in: "MigrationReconciler.scheduleUnaccountedMigrationJobCleanup", call: "retainFailedApplyJob", answer: "return"},
	{in: "MigrationReconciler.scheduleStoppedMigrationJobTTL", call: "scheduleUnaccountedMigrationJobCleanup", answer: "settleMigrationJobCleanup"},
	// The deletion path settles the answer by its own cases: a Job that is gone
	// stays nowhere, a conflict on the claim's own Job retains the resource, and
	// any other answer names the Job in a Warning.
	{in: "MigrationReconciler.retireUncertainMigrationApply", call: "scheduleUnaccountedMigrationJobCleanup", answer: "switch"},
}

func TestEveryJobCleanupAnswerIsSettledWhereItIsAsked(t *testing.T) {
	t.Parallel()

	sites := cleanupAnswerSites(packageSources(t))
	if unexpected := unsettledCleanupAnswerSites(sites, settledCleanupAnswerSites); len(unexpected) > 0 {
		t.Fatalf("uses whose answer nothing in the table settles:\n%s", strings.Join(unexpected, "\n"))
	}
	if len(sites) != len(settledCleanupAnswerSites) {
		t.Fatalf("read %d uses, want the %d the table names:\n%v", len(sites), len(settledCleanupAnswerSites), sites)
	}
}

// TestTheCleanupAnswerSiteCheckRefusesAnUnsettledCall holds the check to the
// shapes a harvest took before #998 and to the ones the review of #1001 found it
// let through, which it must refuse, and to the settled one, which it must accept,
// so a check that read no call or read every call as settled is found here.
func TestTheCleanupAnswerSiteCheckRefusesAnUnsettledCall(t *testing.T) {
	t.Parallel()

	const source = `package controller

var harvestAtPackageScope = (*SchemaReconciler).markJobHarvested

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

func (r *SchemaReconciler) schedule(ctx context.Context, job *batchv1.Job) error {
	return scheduleJobCleanup(ctx, r.Client, job, jobCleanupTTLSeconds)
}

func (r *MigrationReconciler) unaccounted(ctx context.Context, m *operatorv1alpha1.PtahMigration, job *batchv1.Job) error {
	return r.scheduleUnaccountedMigrationJobCleanup(ctx, m, job)
}

func (r *SchemaReconciler) throughAValue(ctx context.Context, job *batchv1.Job) error {
	harvest := r.markJobHarvested
	return harvest(ctx, job)
}

func (r *SchemaReconciler) throughParentheses(ctx context.Context, job *batchv1.Job) error {
	return (r.markJobHarvested)(ctx, job)
}
`
	parsed, err := parser.ParseFile(token.NewFileSet(), "sample.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	sites := cleanupAnswerSites([]*ast.File{parsed})
	want := []cleanupAnswerSite{
		{in: "package scope", call: "markJobHarvested", answer: valueUse},
		{in: "SchemaReconciler.harvestJob", call: "markJobHarvested", answer: "settleSchemaJobCleanup"},
		{in: "SchemaReconciler.consume", call: "markJobHarvested", answer: "if"},
		{in: "SchemaReconciler.consume", call: "markJobHarvested", answer: "bare"},
		{in: "SchemaReconciler.consume", call: "retainFailedApplyJob", answer: "assign"},
		{in: "SchemaReconciler.consume", call: "markJobHarvested", answer: "return"},
		{in: "SchemaReconciler.schedule", call: "scheduleJobCleanup", answer: "return"},
		{in: "MigrationReconciler.unaccounted", call: "scheduleUnaccountedMigrationJobCleanup", answer: "return"},
		{in: "SchemaReconciler.throughAValue", call: "markJobHarvested", answer: valueUse},
		{in: "SchemaReconciler.throughParentheses", call: "markJobHarvested", answer: valueUse},
	}
	if !slices.Equal(sites, want) {
		t.Fatalf("cleanupAnswerSites() =\n%v\nwant\n%v", sites, want)
	}
	unexpected := unsettledCleanupAnswerSites(sites, settledCleanupAnswerSites)
	if len(unexpected) != len(want)-1 || slices.ContainsFunc(unexpected, func(line string) bool { return strings.Contains(line, "harvestJob") }) {
		t.Fatalf("unsettledCleanupAnswerSites() = %q, want every use but the settled harvest", unexpected)
	}
}

// harvestingResultPass is the function that consumes a read-only operation's
// result, and resultPassJobUses is everything in it that may be handed the Job:
// the one harvest at its top, and the pending observation an Apply's success
// records the Job in. Every other exit retries or re-observes with no Job, so a
// refused cleanup is asked for and warned about once per pass (#1002).
const harvestingResultPass = "SchemaReconciler.consumeResultWithTransport"

var resultPassJobUses = []string{"harvestJob", "pendingObservationFor"}

func TestAResultPassHandsItsJobToOneHarvest(t *testing.T) {
	t.Parallel()

	uses, found := jobUsesIn(packageSources(t), harvestingResultPass)
	if !found {
		t.Fatalf("no function %s was read, so no use of its Job was checked", harvestingResultPass)
	}
	if !slices.Equal(uses, resultPassJobUses) {
		t.Fatalf("%s hands its Job to %q, want only %q", harvestingResultPass, uses, resultPassJobUses)
	}
}

// TestTheResultPassJobCheckRefusesASecondHarvest holds the check to the shape the
// pass had before #1002, a retry handed the Job it had already harvested, and to
// a Job that leaves through a value rather than an argument.
func TestTheResultPassJobCheckRefusesASecondHarvest(t *testing.T) {
	t.Parallel()

	const source = `package controller

func (r *SchemaReconciler) consumeResultWithTransport(ctx context.Context, schema *operatorv1alpha1.PtahSchema, job *batchv1.Job) (ctrl.Result, error) {
	if err := r.harvestJob(ctx, schema, job); err != nil {
		return ctrl.Result{}, err
	}
	if mismatched {
		return r.retryOperation(ctx, schema, job, errMismatch)
	}
	held := job
	pending, err := pendingObservationFor(schema, operation, job, outcome, nil, podUIDs, podCount)
	return r.retryOperation(ctx, schema, nil, errMismatch)
}
`
	parsed, err := parser.ParseFile(token.NewFileSet(), "sample.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	uses, found := jobUsesIn([]*ast.File{parsed}, harvestingResultPass)
	if !found {
		t.Fatal("the sample's result pass was not found")
	}
	if want := []string{"harvestJob", "retryOperation", "*ast.AssignStmt", "pendingObservationFor"}; !slices.Equal(uses, want) {
		t.Fatalf("jobUsesIn() = %q, want %q", uses, want)
	}
}

// packageSources parses every non-test Go file of the package.
func packageSources(t *testing.T) []*ast.File {
	t.Helper()
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
		t.Fatal("no source file was read, so no use was checked")
	}
	return parsed
}

// jobUsesIn names what takes each use of the identifier job in the body of the
// function named in: the callee when job is an argument, and the node's type
// otherwise. found is whether the function was read.
func jobUsesIn(files []*ast.File, in string) (uses []string, found bool) {
	for _, file := range files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil || functionName(function) != in {
				continue
			}
			found = true
			walkWithParents(function.Body, func(node ast.Node, stack []ast.Node) {
				ident, ok := node.(*ast.Ident)
				if !ok || ident.Name != "job" || len(stack) == 0 {
					return
				}
				parent := stack[len(stack)-1]
				if call, ok := parent.(*ast.CallExpr); ok && slices.Contains(call.Args, ast.Expr(ident)) {
					uses = append(uses, calledName(call.Fun))
					return
				}
				uses = append(uses, fmt.Sprintf("%T", parent))
			})
		}
	}
	return uses, found
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

// cleanupAnswerSites lists every use of a cleanupTTLWriters function in files, in
// source order: each call, and each time one is named without being called.
func cleanupAnswerSites(files []*ast.File) []cleanupAnswerSite {
	var sites []cleanupAnswerSite
	for _, file := range files {
		for _, declaration := range file.Decls {
			in, body := "package scope", ast.Node(declaration)
			if function, ok := declaration.(*ast.FuncDecl); ok {
				if function.Body == nil {
					continue
				}
				in, body = functionName(function), function.Body
			}
			walkWithParents(body, func(node ast.Node, stack []ast.Node) {
				if call, ok := node.(*ast.CallExpr); ok {
					if name := calledName(call.Fun); slices.Contains(cleanupTTLWriters, name) {
						sites = append(sites, cleanupAnswerSite{in: in, call: name, answer: answerTaker(call, stack)})
					}
					return
				}
				if name, named := writerNamedAsValue(node, stack); named {
					sites = append(sites, cleanupAnswerSite{in: in, call: name, answer: valueUse})
				}
			})
		}
	}
	return sites
}

// writerNamedAsValue reports a cleanupTTLWriters function that node names
// anywhere but where a call names what it calls. A selector is read once, as the
// selector rather than again as its selected name.
func writerNamedAsValue(node ast.Node, stack []ast.Node) (string, bool) {
	var name string
	switch node := node.(type) {
	case *ast.SelectorExpr:
		name = node.Sel.Name
	case *ast.Ident:
		if len(stack) > 0 {
			if selector, ok := stack[len(stack)-1].(*ast.SelectorExpr); ok && selector.Sel == node {
				return "", false
			}
		}
		name = node.Name
	default:
		return "", false
	}
	if !slices.Contains(cleanupTTLWriters, name) {
		return "", false
	}
	if len(stack) > 0 {
		if call, ok := stack[len(stack)-1].(*ast.CallExpr); ok && call.Fun == node {
			return "", false
		}
	}
	return name, true
}

// walkWithParents calls visit for every node under root with the nodes above it,
// the nearest last.
func walkWithParents(root ast.Node, visit func(node ast.Node, stack []ast.Node)) {
	var stack []ast.Node
	ast.Inspect(root, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		visit(node, stack)
		stack = append(stack, node)
		return true
	})
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

// functionName is a function's name, with its receiver type for a method.
func functionName(function *ast.FuncDecl) string {
	if function.Recv != nil && len(function.Recv.List) == 1 {
		return receiverTypeName(function.Recv.List[0].Type) + "." + function.Name.Name
	}
	return function.Name.Name
}

// calledName is the name of the function or method an expression calls.
func calledName(fun ast.Expr) string {
	switch fun := fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	case *ast.FuncLit:
		return "a function literal"
	}
	return fmt.Sprintf("%T", fun)
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
