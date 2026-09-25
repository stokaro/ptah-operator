package runner

import (
	"context"
	"testing"
	"time"
)

// execution_deadline_expired cannot be produced. Both of its sites run after the
// dispatch check and after the guard that refuses an execution deadline behind
// the dispatch one, so now at or past executionNotAfter implies now at or past
// dispatchNotAfter, and the dispatch refusal answers first.
//
// That is by design rather than an oversight. The API field says what enforces
// the execution horizon -- "ExecutionNotAfter is enforced by the runner as the
// mutating child context deadline, independently of relative Job and Pod
// deadlines" -- and TestMigrationApplyExecutionDeadlineCancelsAStartedChild
// measures it there.
//
// This walks the pairs rather than stating the argument, because the day the two
// deadlines are decoupled the code becomes reachable and wants a proof of its
// own. A cluster row for a Pod that starts after its dispatch window needs
// exactly that decoupling -- the Job's relative deadline outliving the absolute
// window -- so this is the row that will tell whoever builds it.
func TestNoDeadlinePairProducesTheExecutionRefusal(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	offsets := []time.Duration{
		-2 * time.Hour, -time.Hour, -time.Second, 0, time.Second, time.Hour, 2 * time.Hour,
	}
	seen := map[string]int{}
	for _, dispatch := range offsets {
		for _, execution := range offsets {
			environment := append(migrationApplyEnvironment(t, "deadline-pair"),
				envDispatchNotAfter+"="+now.Add(dispatch).Format(time.RFC3339Nano),
				envExecutionNotAfter+"="+now.Add(execution).Format(time.RFC3339Nano),
			)
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{
				{stdout: migrationRunDocument("partial"), exitCode: 1},
			}}
			result := Run(context.Background(), Config{
				Operation:   OperationMigrationApply,
				Environment: environment,
				Executor:    executor,
				Clock:       func() time.Time { return now },
			})
			code := "accepted"
			if result.Error != nil {
				code = result.Error.Code
			}
			seen[code]++
			if code == "execution_deadline_expired" {
				t.Errorf("dispatch=%s execution=%s produced execution_deadline_expired; the code is reachable now and needs a proof of its own",
					dispatch, execution)
			}
		}
	}
	// Without this the check passes over a harness that never reached a guard at
	// all, which is the one way it could report what it is looking for as absent.
	// child_exit is a pair that passed every guard and reached the executor,
	// which is what says the walk gets past them rather than being refused
	// throughout.
	for _, wanted := range []string{"dispatch_deadline_expired", "missing_execution_deadline", "child_exit"} {
		if seen[wanted] == 0 {
			t.Errorf("no deadline pair produced %q, so this check did not reach the guards it walks: %v", wanted, seen)
		}
	}
}
