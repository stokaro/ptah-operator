package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

var (
	schemaResource        = schema.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahschemas"}
	migrationResource     = schema.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahmigrations"}
	schemaPlanResource    = schema.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahschemaplans"}
	migrationPlanResource = schema.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahmigrationplans"}
	planChunkResource     = schema.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahschemaplanchunks"}
	approvalGVR           = schema.GroupVersionResource{Group: "operator.ptah.run", Version: "v1alpha1", Resource: "ptahmigrationapprovals"}
)

// sample is one reading of everything the report is built from.
type sample struct {
	Incomplete        []string                  `json:"incomplete,omitempty"`
	At                time.Time                 `json:"at"`
	PodsPending       int                       `json:"podsPending"`
	PodsRunning       int                       `json:"podsRunning"`
	Resources         int                       `json:"resources"`
	Converged         int                       `json:"converged"`
	ObservationAgeMax time.Duration             `json:"observationAgeMax"`
	OverdueMax        time.Duration             `json:"overdueMax"`
	Plans             int                       `json:"plans"`
	Chunks            int                       `json:"chunks"`
	ChunkBytes        int64                     `json:"chunkBytes"`
	Managers          map[string]managerReading `json:"managers"`
	APIServer         *apiReading               `json:"apiServer,omitempty"`
}

// managerReading is one manager Pod's own account of itself.
type managerReading struct {
	RSSBytes        float64            `json:"rssBytes"`
	CPUSeconds      float64            `json:"cpuSeconds"`
	WorkqueueDepth  map[string]float64 `json:"workqueueDepth"`
	ThrottleSeconds float64            `json:"throttleSeconds"`
	Requests429     float64            `json:"requests429"`
	QueueWait       histogram          `json:"queueWait"`
}

// apiReading is what the API server counted about the operator's admission.
type apiReading struct {
	Rejected  float64   `json:"rejected"`
	Admission histogram `json:"admission"`
}

// jobRecord follows one operation Job from creation to its end.
type jobRecord struct {
	Name      string     `json:"name"`
	Operation string     `json:"operation"`
	Family    string     `json:"family"`
	Resource  string     `json:"resource"`
	Created   time.Time  `json:"created"`
	Started   *time.Time `json:"started,omitempty"`
	Finished  *time.Time `json:"finished,omitempty"`
	Failed    bool       `json:"failed"`
}

type sampler struct {
	clientset         kubernetes.Interface
	dynamic           dynamic.Interface
	namespace         string
	operatorNamespace string
	selector          string
	managerSelector   string
	metricsPort       int
	every             time.Duration

	mu      sync.Mutex
	samples []sample
	jobs    map[string]*jobRecord
}

func (s *sampler) run(ctx context.Context) {
	ticker := time.NewTicker(s.every)
	defer ticker.Stop()
	for {
		s.take(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// take preserves failed reads as missing evidence. A partly collected reading
// cannot establish a zero count or a low maximum for the failed source.
func (s *sampler) take(ctx context.Context) {
	now := time.Now().UTC()
	reading := sample{At: now, Managers: map[string]managerReading{}}
	for _, source := range []struct {
		name string
		read func() error
	}{
		{sourcePods, func() error { return s.readPods(ctx, &reading) }},
		{sourceResources, func() error { return s.readResources(ctx, now, &reading) }},
		{sourceRetained, func() error { return s.readRetained(ctx, &reading) }},
		{sourceManagers, func() error { return s.readManagers(ctx, &reading) }},
		{sourceAPI, func() error {
			var err error
			reading.APIServer, err = s.readAPIServer(ctx)
			return err
		}},
		{sourceJobs, func() error { return s.readJobs(ctx) }},
	} {
		if err := source.read(); err != nil {
			reading.Incomplete = append(reading.Incomplete, source.name)
			slog.Warn("incomplete sample", "source", source.name, "error", err)
		}
	}
	// Stopping the sampler can cancel an in-flight collection. It is outside
	// the completed measurement, so discard that partial final reading.
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	s.samples = append(s.samples, reading)
	s.mu.Unlock()
}

func (s *sampler) readPods(ctx context.Context, into *sample) error {
	pods, err := s.clientset.CoreV1().Pods(s.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=ptah-operator",
	})
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		switch pod.Status.Phase {
		case corev1.PodPending:
			into.PodsPending++
		case corev1.PodRunning:
			into.PodsRunning++
		}
	}
	return nil
}

// readResources measures how stale each resource's last reading is, against
// its own timestamps rather than against when this loop looked.
func (s *sampler) readResources(ctx context.Context, now time.Time, into *sample) error {
	for _, family := range []struct {
		resource  schema.GroupVersionResource
		observed  []string
		converged string
	}{
		{schemaResource, []string{"status", "target", "lastObservedAt"}, "InSync"},
		{migrationResource, []string{"status", "history", "observedAt"}, "InSync"},
	} {
		list, err := s.dynamic.Resource(family.resource).Namespace(s.namespace).List(ctx, metav1.ListOptions{LabelSelector: s.selector})
		if err != nil {
			return err
		}
		for _, item := range list.Items {
			into.Resources++
			if phase, _, _ := unstructured.NestedString(item.Object, "status", "phase"); phase == family.converged {
				into.Converged++
			}
			if observed, ok := timestampAt(item.Object, family.observed...); ok {
				into.ObservationAgeMax = max(into.ObservationAgeMax, now.Sub(observed))
			}
			if next, ok := timestampAt(item.Object, "status", "nextReconciliationTime"); ok && now.After(next) {
				into.OverdueMax = max(into.OverdueMax, now.Sub(next))
			}
		}
	}
	return nil
}

func timestampAt(object map[string]any, path ...string) (time.Time, bool) {
	raw, found, err := unstructured.NestedString(object, path...)
	if err != nil || !found || raw == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	return parsed, err == nil
}

func (s *sampler) readRetained(ctx context.Context, into *sample) error {
	for _, resource := range []schema.GroupVersionResource{schemaPlanResource, migrationPlanResource} {
		plans, err := s.dynamic.Resource(resource).Namespace(s.namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		into.Plans += len(plans.Items)
	}
	chunks, err := s.dynamic.Resource(planChunkResource).Namespace(s.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	into.Chunks = len(chunks.Items)
	for _, chunk := range chunks.Items {
		// The API carries the bytes base64-encoded, which is what the store
		// costs in etcd; the plan bytes are three quarters of it.
		encoded, _, err := unstructured.NestedString(chunk.Object, "spec", "data")
		if err != nil {
			return fmt.Errorf("read plan chunk %s: %w", chunk.GetName(), err)
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return fmt.Errorf("decode plan chunk %s: %w", chunk.GetName(), err)
		}
		into.ChunkBytes += int64(len(decoded))
	}
	return nil
}

func (s *sampler) readManagers(ctx context.Context, into *sample) error {
	pods, err := s.clientset.CoreV1().Pods(s.operatorNamespace).List(ctx, metav1.ListOptions{LabelSelector: s.managerSelector})
	if err != nil {
		return err
	}
	var problems []error
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		reading, err := scrapePod(ctx, s.clientset, s.operatorNamespace, pod.Name, s.metricsPort)
		if err != nil {
			problems = append(problems, fmt.Errorf("manager %s: %w", pod.Name, err))
			continue
		}
		rss, hasRSS := reading.value("process_resident_memory_bytes", nil)
		cpu, hasCPU := reading.value("process_cpu_seconds_total", nil)
		if !hasRSS || !hasCPU || rss <= 0 || cpu < 0 || math.IsNaN(rss) || math.IsNaN(cpu) || math.IsInf(rss, 0) || math.IsInf(cpu, 0) {
			problems = append(problems, fmt.Errorf("manager %s has no valid process memory or CPU reading", pod.Name))
			continue
		}
		throttle, _ := reading.value("rest_client_rate_limiter_duration_seconds_sum", nil)
		if throttle == 0 {
			throttle = reading.histogram("rest_client_rate_limiter_duration_seconds", nil).sum
		}
		too, _ := reading.value("rest_client_requests_total", map[string]string{"code": "429"})
		into.Managers[pod.Name] = managerReading{
			RSSBytes:        rss,
			CPUSeconds:      cpu,
			WorkqueueDepth:  reading.maxBy("workqueue_depth", "name"),
			ThrottleSeconds: throttle,
			Requests429:     too,
			QueueWait:       reading.histogram("workqueue_queue_duration_seconds", nil),
		}
	}
	if len(into.Managers) == 0 {
		problems = append(problems, errors.New("no running manager produced process metrics"))
	}
	return errors.Join(problems...)
}

func (s *sampler) readAPIServer(ctx context.Context) (*apiReading, error) {
	reading, err := scrapeAPIServer(ctx, s.clientset)
	if err != nil {
		return nil, err
	}
	rejected, _ := reading.value("apiserver_flowcontrol_rejected_requests_total", nil)
	return &apiReading{
		Rejected:  rejected,
		Admission: reading.histogram("apiserver_admission_webhook_admission_duration_seconds", map[string]string{"name": "*"}).onlyPtah(reading),
	}, nil
}

// onlyPtah keeps the admission latency of this operator's webhooks, which are
// the ones named under operator.ptah.run.
func (h histogram) onlyPtah(reading scrape) histogram {
	out := histogram{buckets: map[float64]float64{}}
	family, ok := reading["apiserver_admission_webhook_admission_duration_seconds"]
	if !ok {
		return out
	}
	for _, metric := range family.GetMetric() {
		name := ""
		for _, pair := range metric.GetLabel() {
			if pair.GetName() == "name" {
				name = pair.GetValue()
			}
		}
		if !hasSuffixDomain(name, "operator.ptah.run") || metric.GetHistogram() == nil {
			continue
		}
		reading := metric.GetHistogram()
		out.count += float64(reading.GetSampleCount())
		out.sum += reading.GetSampleSum()
		for _, bucket := range reading.GetBucket() {
			out.buckets[bucket.GetUpperBound()] += float64(bucket.GetCumulativeCount())
		}
	}
	return out
}

func hasSuffixDomain(name, domain string) bool {
	return name == domain || len(name) > len(domain) && name[len(name)-len(domain)-1:] == "."+domain
}

func (s *sampler) readJobs(ctx context.Context) error {
	jobs, err := s.clientset.BatchV1().Jobs(s.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=ptah-operator",
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range jobs.Items {
		job := &jobs.Items[index]
		record, ok := s.jobs[string(job.UID)]
		if !ok {
			record = &jobRecord{
				Name:      job.Name,
				Operation: job.Labels["operator.ptah.run/operation"],
				Created:   job.CreationTimestamp.UTC(),
			}
			switch {
			case job.Labels["operator.ptah.run/migration"] != "":
				record.Family, record.Resource = "migration", job.Labels["operator.ptah.run/migration"]
			default:
				record.Family, record.Resource = "schema", job.Labels["operator.ptah.run/schema"]
			}
			s.jobs[string(job.UID)] = record
		}
		if job.Status.StartTime != nil && record.Started == nil {
			started := job.Status.StartTime.UTC()
			record.Started = &started
		}
		if finished, failed, done := jobEnd(job); done && record.Finished == nil {
			record.Finished, record.Failed = &finished, failed
		}
	}
	return nil
}

// jobEnd is when a Job reached a terminal condition, and which one.
func jobEnd(job *batchv1.Job) (time.Time, bool, bool) {
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobComplete:
			return condition.LastTransitionTime.UTC(), false, true
		case batchv1.JobFailed:
			return condition.LastTransitionTime.UTC(), true, true
		}
	}
	return time.Time{}, false, false
}

func (s *sampler) snapshot() ([]sample, []jobRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	samples := append([]sample(nil), s.samples...)
	jobs := make([]jobRecord, 0, len(s.jobs))
	for _, record := range s.jobs {
		jobs = append(jobs, *record)
	}
	return samples, jobs
}

func (s *sampler) String() string {
	return fmt.Sprintf("sampler(%s every %s)", s.namespace, s.every)
}
