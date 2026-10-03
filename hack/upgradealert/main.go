// upgradealert observes one declared upgrade from outside the Helm release.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type observer struct {
	mu        sync.RWMutex
	state     State
	path      string
	watching  bool
	jobs      kubernetes.Interface
	resources client.Client
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	return runWithListener(ctx, args, net.Listen)
}

func runWithListener(ctx context.Context, args []string, listenSocket func(string, string) (net.Listener, error)) error {
	if len(args) == 0 || (args[0] != "prepare" && args[0] != "serve" && args[0] != "inspect") {
		return errors.New("usage: upgradealert inspect --state PATH; prepare|serve --state PATH --kubeconfig PATH [--context NAME]; prepare also requires --intent, --chart and --values")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	statePath := flags.String("state", "", "durable state file outside the upgraded release")
	kubeconfig := flags.String("kubeconfig", "", "explicit kubeconfig path")
	kubecontext := flags.String("context", "", "kubeconfig context")
	intentPath := flags.String("intent", "", "declared transaction JSON")
	chart := flags.String("chart", "", "exact packaged candidate chart")
	values := flags.String("values", "", "exact values file retained for retry")
	listen := flags.String("listen", "127.0.0.1:9812", "metrics and readiness HTTP address")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if args[0] == "inspect" {
		if flags.NArg() != 0 || *statePath == "" || *kubeconfig != "" || *intentPath != "" || *chart != "" || *values != "" {
			return errors.New("inspect requires only a state path")
		}
		return inspectState(*statePath, os.Stdout)
	}
	if flags.NArg() != 0 || *statePath == "" || *kubeconfig == "" {
		return errors.New("an explicit state path and kubeconfig are required")
	}
	lock, err := lockState(*statePath)
	if err != nil {
		return err
	}
	defer lock.Close()
	loading := &clientcmd.ClientConfigLoadingRules{ExplicitPath: *kubeconfig}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, &clientcmd.ConfigOverrides{CurrentContext: *kubecontext}).ClientConfig()
	if err != nil {
		return err
	}
	config.Timeout = 10 * time.Second
	// HTTP client timeouts include streaming response bodies. Let a quiet
	// watch reach its server-side segment boundary and receive bookmarks.
	jobConfig := rest.CopyConfig(config)
	jobConfig.Timeout = hookWatchTimeout + config.Timeout
	jobs, err := kubernetes.NewForConfig(jobConfig)
	if err != nil {
		return err
	}
	if args[0] == "prepare" {
		if *intentPath == "" || *chart == "" || *values == "" {
			return errors.New("prepare requires intent, packaged chart and values paths")
		}
		if _, err := os.Lstat(*statePath); !os.IsNotExist(err) {
			return errors.New("refusing to replace an existing transaction; retry uses the same state")
		}
		file, err := os.Open(*intentPath)
		if err != nil {
			return err
		}
		defer file.Close()
		var intent Intent
		raw, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		if err != nil {
			return err
		}
		if len(raw) > 1<<20 {
			return errors.New("upgrade intent exceeds its bound")
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&intent); err != nil {
			return errors.New("invalid upgrade intent JSON")
		}
		if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
			return errors.New("upgrade intent has trailing content")
		}
		if err := intent.validate(); err != nil {
			return err
		}
		for _, input := range []struct{ path, want string }{{*chart, intent.ChartDigest}, {*values, intent.ValuesDigest}} {
			got, err := fileDigest(input.path)
			if err != nil {
				return err
			}
			if got != input.want {
				return errors.New("candidate chart or values do not match the declared transaction")
			}
		}
		started := time.Now().UTC()
		listCtx, stopList := context.WithTimeout(ctx, config.Timeout)
		defer stopList()
		list, err := jobs.BatchV1().Jobs(intent.Namespace).List(listCtx, metav1.ListOptions{FieldSelector: "metadata.name=" + intent.HookJob})
		if err != nil {
			return fmt.Errorf("establish hook watch boundary: %w", err)
		}
		s := State{Version: 1, Intent: intent, StartedAt: started, Deadline: started.Add(upgradeDeadline), ResourceVersion: list.ResourceVersion}
		if len(list.Items) > 1 {
			return errors.New("hook inventory is ambiguous")
		}
		if len(list.Items) == 1 {
			s.BaselineJobUID = string(list.Items[0].UID)
		}
		return saveState(*statePath, s)
	}
	if *intentPath != "" || *chart != "" || *values != "" {
		return errors.New("serve reads the prepared transaction; it cannot replace candidate inputs")
	}
	s, err := loadState(*statePath)
	if err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientscheme.AddToScheme, ptahv1.AddToScheme, apiextensionsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			return err
		}
	}
	resources, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	o := &observer{state: s, path: *statePath, jobs: jobs, resources: resources}
	listener, err := listenSocket("tcp", *listen)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", o.metrics)
	mux.HandleFunc("/readyz", o.ready)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() {
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
	}()
	watchDone := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() { defer workers.Done(); watchDone <- o.watch(ctx) }()
	defer func() { cancel(); workers.Wait() }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-done:
		return err
	case err := <-watchDone:
		return err
	}
}

func (o *observer) snapshot() State {
	o.mu.RLock()
	defer o.mu.RUnlock()
	s := o.state
	s.Attempts = append([]Attempt(nil), s.Attempts...)
	return s
}
func (o *observer) persist(s State) error {
	if err := saveState(o.path, s); err != nil {
		return fmt.Errorf("retain upgrade evidence: %w", err)
	}
	o.mu.Lock()
	o.state = s
	o.mu.Unlock()
	return nil
}
func (o *observer) setWatching(v bool) { o.mu.Lock(); o.watching = v; o.mu.Unlock() }
func (o *observer) observationReady() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.watching && !o.state.HistoryLost
}
func (o *observer) ready(w http.ResponseWriter, r *http.Request) {
	if !o.observationReady() {
		http.Error(w, "hook observation is not synchronized", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}
func (o *observer) metrics(w http.ResponseWriter, r *http.Request) {
	s := o.snapshot()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	labels := fmt.Sprintf("operator_namespace=%q,release=%q", s.Intent.Namespace, s.Intent.Release)
	pending, failed, gap, ready := 0, 0, 0, 0
	if o.observationReady() {
		ready = 1
	}
	fmt.Fprintf(w, "# TYPE ptah_operator_upgrade_observer_ready gauge\nptah_operator_upgrade_observer_ready{%s} %d\n", labels, ready)
	if s.RecoveredAt == nil {
		pending = 1
	}
	if s.FailedAt != nil {
		failed = 1
	}
	if s.HistoryLost {
		gap = 1
	}
	fmt.Fprintf(w, "# TYPE ptah_operator_upgrade_pending gauge\nptah_operator_upgrade_pending{%s} %d\n", labels, pending)
	fmt.Fprintf(w, "# TYPE ptah_operator_upgrade_failed gauge\nptah_operator_upgrade_failed{%s} %d\n", labels, failed)
	fmt.Fprintf(w, "# TYPE ptah_operator_upgrade_deadline_seconds gauge\nptah_operator_upgrade_deadline_seconds{%s} %.9f\n", labels, float64(s.Deadline.UnixNano())/1e9)
	fmt.Fprintf(w, "# TYPE ptah_operator_upgrade_history_lost gauge\nptah_operator_upgrade_history_lost{%s} %d\n", labels, gap)
}

// Inspection reads one atomic state snapshot without competing with its writer.
// The document contains identities and digests, never kubeconfig credentials.
func inspectState(path string, w io.Writer) error {
	s, err := loadState(path)
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(s)
}
