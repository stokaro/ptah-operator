package runnerharness_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/test/runnerharness"
)

// A run that passes its -timeout panics, and one that is killed dies where it
// stands, so nothing in the test binary can clear what the run made: t.Cleanup
// does not run. The janitor is a second process, started from the same binary,
// that holds the read end of a pipe to the run. Only a run that cleared up after
// itself writes to the pipe. Whatever else ends the run closes it, and the
// janitor then clears the run from outside.
//
// The janitor is not the run's child. The process the run starts starts the
// janitor and exits, so the janitor sits in a session of its own under init: a
// signal to the run's process group, the loss of its terminal, and a kill of
// the run's whole process tree all miss it.
const (
	// janitorVariable makes the test binary the janitor, reading its order from
	// the value.
	janitorVariable = "RUNNER_HARNESS_JANITOR"
	// janitorDetachedVariable marks the second of the janitor's two processes,
	// the one that does the work.
	janitorDetachedVariable = "RUNNER_HARNESS_JANITOR_DETACHED"
	// janitorDismissal is what a run that cleared up after itself says.
	janitorDismissal = "cleared"
	// janitorInterval is how long the janitor waits between passes.
	janitorInterval = 15 * time.Second
	// diesVariable makes the test binary a run that starts its janitor and waits
	// to be killed, for TestJanitorClearsARunThatWasKilled.
	diesVariable = "RUNNER_HARNESS_RUN_THAT_DIES"
)

// janitorOrder is what one run's janitor has to clear.
type janitorOrder struct {
	Context string `json:"context"`
	Prefix  string `json:"prefix"`
	// Pulled names the images the run pulled.
	Pulled []string `json:"pulled,omitempty"`
	// Work is the local directory the run builds its tools image in. It holds
	// the keys generated for the run.
	Work string `json:"work"`
	// Settle is how long the janitor keeps looking. The docker clients a run
	// started outlive it, and one still on its way to the daemon creates what it
	// was asked to after the first pass has looked.
	Settle time.Duration `json:"settle"`
	// Interval is how long it waits between passes.
	Interval time.Duration `json:"interval"`
	// Report names a file the janitor writes its outcome to as it leaves:
	// dismissed, cleared, or failed and why. Its output goes nowhere, so that
	// `go test` does not wait for it, and the tests read this instead.
	Report string `json:"report,omitempty"`
}

func TestMain(m *testing.M) {
	if order := os.Getenv(janitorVariable); order != "" {
		os.Exit(janitorMain(order))
	}
	if order := os.Getenv(diesVariable); order != "" {
		runThatDies(order)
		return
	}
	os.Exit(m.Run())
}

func janitorMain(encoded string) int {
	var order janitorOrder
	if err := json.Unmarshal([]byte(encoded), &order); err != nil {
		return 1
	}
	if os.Getenv(janitorDetachedVariable) == "" {
		return detachJanitor()
	}
	dismissed, err := runJanitor(order, os.Stdin)
	outcome := "cleared"
	switch {
	case err != nil:
		outcome = "failed: " + err.Error()
	case dismissed:
		outcome = "dismissed"
	}
	if order.Report != "" {
		_ = os.WriteFile(order.Report, []byte(outcome), 0o600)
	}
	if err != nil {
		// What it could not clear stays on the record the next run reads.
		return 1
	}
	return 0
}

// detachJanitor starts the janitor proper on the same pipe and leaves, so that
// init adopts it. The janitor stays in the session startJanitor made.
func detachJanitor() int {
	self, err := os.Executable()
	if err != nil {
		return 1
	}
	command := exec.Command(self) // #nosec G204 -- this test binary
	command.Env = append(os.Environ(), janitorDetachedVariable+"=1")
	command.Stdin = os.Stdin
	if err := command.Start(); err != nil {
		return 1
	}
	return 0
}

// janitor is the run's end of the pipe.
type janitor struct {
	pipe *os.File
}

// startJanitor starts the run's janitor and returns once it is detached from the
// run. It inherits none of the run's output, so `go test` does not wait for it.
func startJanitor(order janitorOrder) (*janitor, error) {
	encoded, err := json.Marshal(order)
	if err != nil {
		return nil, fmt.Errorf("write the janitor's order: %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find the test binary: %w", err)
	}
	// Not StdinPipe: Wait closes that, and the run has to hold its end until it
	// ends.
	read, write, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("open the janitor's pipe: %w", err)
	}
	command := exec.Command(self) // #nosec G204 -- this test binary
	command.Env = append(os.Environ(), janitorVariable+"="+string(encoded))
	command.Stdin = read
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = command.Start()
	if closeErr := read.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = command.Wait()
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("start the janitor: %w", err), write.Close())
	}
	return &janitor{pipe: write}, nil
}

// dismiss tells the janitor the run cleared up after itself.
func (j *janitor) dismiss() error {
	_, writeErr := io.WriteString(j.pipe, janitorDismissal)
	return errors.Join(writeErr, j.pipe.Close())
}

// runJanitor waits for the run to end, and clears it unless the run said it had.
func runJanitor(order janitorOrder, run io.Reader) (dismissed bool, err error) {
	said, _ := io.ReadAll(run)
	if string(said) == janitorDismissal {
		return true, nil
	}
	removeWork := os.RemoveAll(order.Work)
	d := docker{context: order.Context}
	settled := time.Now().Add(order.Settle)
	var passes error
	// A second pass whatever the budget: the first can start before a client
	// that was already on its way.
	for pass := 0; pass < 2 || time.Now().Before(settled); pass++ {
		if pass > 0 {
			time.Sleep(order.Interval)
		}
		passes = d.removeRunObjects(order.Prefix)
	}
	// The record goes only with everything it names. If it stays, the next run
	// on this daemon finds it once it expires.
	if passes != nil {
		return false, errors.Join(removeWork, passes)
	}
	return false, errors.Join(removeWork, d.removeRunRecord(order.Prefix, order.Pulled))
}

// runThatDies is a run as far as its janitor can tell, until it is killed.
func runThatDies(encoded string) {
	var order janitorOrder
	if err := json.Unmarshal([]byte(encoded), &order); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if _, err := startJanitor(order); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Println("started")
	request := bufio.NewReader(os.Stdin)
	if line, _ := request.ReadString('\n'); line == "spawn\n" {
		if err := exec.Command("sleep", "3600").Start(); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println("spawned")
	}
	time.Sleep(time.Hour)
}

// check runs a command that has to succeed and returns its standard output.
func (d docker) check(args ...string) (string, error) {
	stdout, stderr, exit, err := d.run(context.Background(), nil, nil, args...)
	if err != nil {
		return "", fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	if exit != 0 {
		return "", fmt.Errorf("docker %s exited %d: %s", strings.Join(args, " "), exit, stderr)
	}
	return string(stdout), nil
}

// runObjectFilter matches the names of a run's containers and volumes.
func runObjectFilter(prefix string) string {
	return "name=^" + regexp.QuoteMeta(prefix) + "-"
}

// removeRunObjects removes a run's containers, volumes and tools image, by the
// prefix that names them.
func (d docker) removeRunObjects(prefix string) error {
	removeListed := func(remove, list []string) error {
		listed, err := d.check(list...)
		if err != nil {
			return err
		}
		names := strings.Fields(listed)
		if len(names) == 0 {
			return nil
		}
		_, err = d.check(append(remove, names...)...)
		return err
	}
	filter := runObjectFilter(prefix)
	containers := removeListed([]string{"rm", "--force", "--volumes"}, []string{"ps", "--all", "--quiet", "--filter", filter})
	volumes := removeListed([]string{"volume", "rm", "--force"}, []string{"volume", "ls", "--quiet", "--filter", filter})
	_, tools := d.check("image", "rm", "--force", prefix+"-tools:run")
	return errors.Join(containers, volumes, tools)
}

// removeRunRecord removes the images a run pulled and its network, which is the
// record of the run and so goes last.
func (d docker) removeRunRecord(prefix string, pulled []string) error {
	for _, image := range pulled {
		// It may never have arrived, and another run may be using it.
		_, _, _, _ = d.run(context.Background(), nil, nil, "image", "rm", image)
	}
	network := prefix + "-net"
	listed, err := d.check("network", "ls", "--quiet", "--filter", "name=^"+regexp.QuoteMeta(network)+"$")
	if err != nil || strings.TrimSpace(listed) == "" {
		return err
	}
	_, err = d.check("network", "rm", network)
	return err
}

// sweepExpired clears the runs whose janitors could not: the machine went down,
// or the daemon could not be reached. A record that has not expired may be a live
// run's, and one this code cannot read may be anybody's.
func (d docker) sweepExpired(logf func(string, ...any), now time.Time) {
	listed, err := d.check("network", "ls", "--filter", "label="+runnerharness.RunLabel, "--format", `{{.Label "`+runnerharness.RunLabel+`"}}`)
	if err != nil {
		logf("list the records of earlier runs: %v", err)
		return
	}
	for line := range strings.Lines(listed) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		record, err := runnerharness.ParseRunRecord(line)
		if err != nil {
			logf("leave a network labeled %s alone: %v", runnerharness.RunLabel, err)
			continue
		}
		if !record.Expired(now) {
			continue
		}
		logf("clearing what the run %s left behind", record.Prefix)
		if err := d.removeRunObjects(record.Prefix); err != nil {
			logf("clear the run %s: %v", record.Prefix, err)
			continue
		}
		if err := d.removeRunRecord(record.Prefix, record.Pulled); err != nil {
			logf("remove the record of the run %s: %v", record.Prefix, err)
		}
	}
}

// runExpiry is when nothing of a run can still be in use: its binary panics at
// the test's deadline, and its janitor stops looking a Settle later.
func runExpiry(t *testing.T) time.Time {
	t.Helper()
	deadline, bounded := t.Deadline()
	if !bounded {
		return time.Now().Add(24 * time.Hour)
	}
	return deadline.Add(2 * dockerTimeout)
}

// A stub stands in for the docker client, so these rows run wherever `go test`
// does. What the daemon does with the commands, a real run killed partway shows.
const dockerStubScript = `#!/bin/sh
printf '%s\n' "$*" >> "$HARNESS_STUB/calls"
shift 2
case "$1 $2" in
"ps --all")
	if [ -e "$HARNESS_STUB/unreachable" ]; then
		echo "the daemon cannot be reached" >&2
		exit 1
	fi
	pass=$(grep -c -e ' ps --all ' "$HARNESS_STUB/calls")
	cat "$HARNESS_STUB/containers-$pass" 2>/dev/null
	;;
"volume ls") cat "$HARNESS_STUB/volumes" 2>/dev/null ;;
"network ls") cat "$HARNESS_STUB/networks" 2>/dev/null ;;
esac
exit 0
`

// awaitReport waits for the janitor to leave and returns what it said.
func awaitReport(t *testing.T, file string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		report, err := os.ReadFile(file) // #nosec G304 -- the test's own file
		if err == nil && len(report) != 0 {
			return string(report)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the janitor did not leave within 2m: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// killTree kills a process and everything below it, as a supervisor that stops
// a job does.
func killTree(t *testing.T, root int) {
	t.Helper()
	listed, err := exec.Command("ps", "-A", "-o", "pid=", "-o", "ppid=").Output()
	if err != nil {
		t.Fatal(err)
	}
	children := map[int][]int{}
	for line := range strings.Lines(string(listed)) {
		var pid, parent int
		if _, err := fmt.Sscan(line, &pid, &parent); err == nil {
			children[parent] = append(children[parent], pid)
		}
	}
	tree := []int{root}
	for index := 0; index < len(tree); index++ {
		tree = append(tree, children[tree[index]]...)
	}
	if len(tree) < 2 {
		t.Fatalf("process %d has nothing below it, so killing its tree shows nothing", root)
	}
	for _, pid := range tree {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// dockerStub puts the stub first on the PATH, serving the named listings, and
// returns a function that reads what it was called with.
func dockerStub(t *testing.T, listings map[string]string) (calls func() []string, directory string) {
	t.Helper()
	directory = t.TempDir()
	writeTree(t, filepath.Join(directory, "bin", "docker"), []byte(dockerStubScript), 0o755)
	for name, content := range listings {
		writeTree(t, filepath.Join(directory, name), []byte(content), 0o644)
	}
	t.Setenv("HARNESS_STUB", directory)
	t.Setenv("PATH", filepath.Join(directory, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		data, err := os.ReadFile(filepath.Join(directory, "calls"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}, directory
}

func TestJanitorClearsARunThatWasKilled(t *testing.T) {
	const prefix = "ptah-runner-harness-dead"
	// The second listing is a container created after the first pass looked, by
	// a client the run started and did not live to stop.
	calls, directory := dockerStub(t, map[string]string{
		"containers-1": "first\n",
		"containers-2": "late\n",
		"volumes":      prefix + "-fixtures\n",
		"networks":     "1a2b3c\n",
	})
	work := filepath.Join(directory, "work")
	writeTree(t, filepath.Join(work, "keys", "token"), []byte("generated for the run"), 0o444)
	report := filepath.Join(directory, "report")
	order, err := json.Marshal(janitorOrder{
		Context: "stub", Prefix: prefix, Pulled: []string{"registry.invalid/pulled"}, Work: work, Interval: time.Millisecond,
		Report: report,
	})
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run := exec.Command(self) // #nosec G204 -- this test binary
	run.Env = append(os.Environ(), diesVariable+"="+string(order))
	// The run leads a process group, which is what a terminal or a runner signals.
	run.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := run.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := run.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewReader(stdout)
	started, err := lines.ReadString('\n')
	if err != nil || started != "started\n" {
		t.Fatalf("the run said %q, %v, want it to have started its janitor", started, err)
	}
	if got := calls(); got != nil {
		t.Fatalf("the janitor called docker while the run was alive: %q", got)
	}
	// A child that sleeps on, as a docker client the run started does.
	if _, err := io.WriteString(stdin, "spawn\n"); err != nil {
		t.Fatal(err)
	}
	if spawned, err := lines.ReadString('\n'); err != nil || spawned != "spawned\n" {
		t.Fatalf("the run said %q, %v, want it to have started a child", spawned, err)
	}
	killTree(t, run.Process.Pid)
	if err := syscall.Kill(-run.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatal(err)
	}
	if err := run.Wait(); err == nil {
		t.Fatal("the run exited cleanly, want it killed")
	}

	const client = "--context stub "
	const list = client + "ps --all --quiet --filter name=^" + prefix + "-"
	const listVolumes = client + "volume ls --quiet --filter name=^" + prefix + "-"
	const removeVolume = client + "volume rm --force " + prefix + "-fixtures"
	const removeTools = client + "image rm --force " + prefix + "-tools:run"
	const removeRecord = client + "network rm " + prefix + "-net"
	want := []string{
		list, client + "rm --force --volumes first", listVolumes, removeVolume, removeTools,
		list, client + "rm --force --volumes late", listVolumes, removeVolume, removeTools,
		client + "image rm registry.invalid/pulled",
		client + "network ls --quiet --filter name=^" + prefix + "-net$",
		removeRecord,
	}
	if outcome := awaitReport(t, report); outcome != "cleared" {
		t.Fatalf("the janitor said %q, want cleared; it called %q", outcome, calls())
	}
	if got := calls(); !slices.Equal(got, want) {
		t.Fatalf("the janitor called:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := os.Stat(work); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the run's build context is still there: %v", err)
	}
}

func TestJanitorLeavesARunThatClearedUpAfterItself(t *testing.T) {
	calls, directory := dockerStub(t, map[string]string{"containers-1": "first\n"})
	work := filepath.Join(directory, "work")
	writeTree(t, filepath.Join(work, "keys", "token"), []byte("generated for the run"), 0o444)
	report := filepath.Join(directory, "report")
	j, err := startJanitor(janitorOrder{Context: "stub", Prefix: "ptah-runner-harness-done", Work: work, Interval: time.Millisecond, Report: report})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.dismiss(); err != nil {
		t.Fatal(err)
	}
	if outcome := awaitReport(t, report); outcome != "dismissed" {
		t.Fatalf("the janitor said %q, want dismissed", outcome)
	}
	if got := calls(); got != nil {
		t.Fatalf("the janitor called docker for a run that cleared up after itself: %q", got)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatalf("the janitor touched the run's directory: %v", err)
	}
}

// The record is how the next run finds what this one left. It goes only with
// everything it names.
func TestJanitorKeepsTheRecordOfARunItCouldNotClear(t *testing.T) {
	calls, directory := dockerStub(t, map[string]string{"unreachable": ""})
	report := filepath.Join(directory, "report")
	j, err := startJanitor(janitorOrder{Context: "stub", Prefix: "ptah-runner-harness-stuck", Interval: time.Millisecond, Report: report})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.pipe.Close(); err != nil {
		t.Fatal(err)
	}
	if outcome := awaitReport(t, report); !strings.Contains(outcome, "the daemon cannot be reached") {
		t.Fatalf("the janitor could not reach the daemon and said %q, want it to say so", outcome)
	}
	got := calls()
	if len(got) == 0 {
		t.Fatal("the janitor did not try to clear the run")
	}
	if index := slices.IndexFunc(got, func(call string) bool { return strings.Contains(call, " network ") }); index >= 0 {
		t.Fatalf("the janitor reached the record of a run it could not clear: %q", got)
	}
}

func TestSweepClearsOnlyExpiredRecordsItCanRead(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	label := func(prefix string, expires time.Time) string {
		t.Helper()
		value, err := runnerharness.RunRecord{Prefix: prefix, Expires: expires, Pulled: []string{"registry.invalid/" + prefix}}.Label()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	calls, _ := dockerStub(t, map[string]string{"networks": strings.Join([]string{
		label("ptah-runner-harness-dead", now.Add(-time.Minute)),
		label("ptah-runner-harness-live", now.Add(time.Minute)),
		`{"prefix":".*","expires":"2026-11-07T11:00:00Z"}`,
		"not a record",
	}, "\n") + "\n"})
	var logged []string
	docker{context: "stub"}.sweepExpired(func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}, now)

	got := calls()
	if !slices.Contains(got, "--context stub network rm ptah-runner-harness-dead-net") {
		t.Fatalf("the sweep did not clear the expired run: %q", got)
	}
	if !slices.Contains(got, "--context stub image rm registry.invalid/ptah-runner-harness-dead") {
		t.Fatalf("the sweep left the image the expired run pulled: %q", got)
	}
	for _, call := range got {
		if strings.Contains(call, "ptah-runner-harness-live") || strings.Contains(call, ".*") || strings.Contains(call, "not a record") {
			t.Errorf("the sweep touched what it has no claim on: %q", call)
		}
	}
	refused := 0
	for _, line := range logged {
		if strings.HasPrefix(line, "leave a network labeled") {
			refused++
		}
	}
	if refused != 2 {
		t.Errorf("the sweep reported %d records it could not read, want 2: %q", refused, logged)
	}
}

// The daemon reads this filter as a regular expression over every name it holds,
// and a shared host holds other people's.
func TestRunObjectFilterMatchesOnlyTheRunsOwnNames(t *testing.T) {
	t.Parallel()

	const prefix = "ptah-op-878-a.b"
	filter, found := strings.CutPrefix(runObjectFilter(prefix), "name=")
	if !found {
		t.Fatalf("the filter is %q, want a name filter", runObjectFilter(prefix))
	}
	pattern := regexp.MustCompile(filter)
	for name, want := range map[string]bool{
		"ptah-op-878-a.b-postgres":   true,
		"ptah-op-878-a.b-pod-1-work": true,
		"ptah-op-878-a.b":            false,
		"ptah-op-878-a.b2-postgres":  false,
		"ptah-op-878-axb-postgres":   false,
		"x-ptah-op-878-a.b-postgres": false,
	} {
		if got := pattern.MatchString(name); got != want {
			t.Errorf("the filter %q on %q = %t, want %t", filter, name, got, want)
		}
	}
}
