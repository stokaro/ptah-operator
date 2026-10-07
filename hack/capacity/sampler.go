package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	ResourceReadStartedAt  time.Time                 `json:"resourceReadStartedAt"`
	ResourceReadFinishedAt time.Time                 `json:"resourceReadFinishedAt"`
	ResourceFreshness      []resourceFreshness       `json:"resourceFreshness"`
	Incomplete             []string                  `json:"incomplete,omitempty"`
	At                     time.Time                 `json:"at"`
	PodsPending            int                       `json:"podsPending"`
	PodsRunning            int                       `json:"podsRunning"`
	Resources              int                       `json:"resources"`
	Converged              int                       `json:"converged"`
	ObservationAgeMax      time.Duration             `json:"observationAgeMax"`
	OverdueMax             time.Duration             `json:"overdueMax"`
	Plans                  int                       `json:"plans"`
	Chunks                 int                       `json:"chunks"`
	ChunkBytes             int64                     `json:"chunkBytes"`
	Managers               map[string]managerReading `json:"managers"`
	APIServers             map[string]apiReading     `json:"apiServers"`
	APITargets             []apiIdentity             `json:"apiTargets"`
}

// managerReading is one manager Pod's own account of itself.
type managerReading struct {
	APIRequests        *apiRequestReading `json:"apiRequests"`
	PodUID             string             `json:"podUID"`
	ContainerID        string             `json:"containerID"`
	ContainerStartedAt time.Time          `json:"containerStartedAt"`
	ProcessStartedAt   float64            `json:"processStartedAt"`
	RestartCount       int32              `json:"restartCount"`
	RSSBytes           float64            `json:"rssBytes"`
	CPUSeconds         float64            `json:"cpuSeconds"`
	WorkqueueDepth     map[string]float64 `json:"workqueueDepth"`
	ThrottleSeconds    float64            `json:"throttleSeconds"`
	Requests429        float64            `json:"requests429"`
	QueueWait          histogram          `json:"queueWait"`
}

// jobRecord follows one operation Job from creation to its end.
type jobRecord struct {
	Namespace string     `json:"namespace"`
	UID       string     `json:"uid"`
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
	expectedAPIServers int
	expectedManagers   int
	scrapeAPI          func(context.Context, corev1.Pod) (scrape, error)
	clientset          kubernetes.Interface
	dynamic            dynamic.Interface
	namespace          string
	namespaces         []string
	operatorNamespace  string
	selector           string
	managerSelector    string
	metricsPort        int
	every              time.Duration
	collectionTimeout  time.Duration

	mu      sync.Mutex
	samples []sample
	jobs    map[string]*jobRecord
}

// run stops scheduling periodic reads when finish closes, lets the current
// bounded read finish, and collects one final inventory. Scenario completion
// must not cancel the evidence of operations that completed just before it.
func (s *sampler) run(ctx context.Context, finish <-chan struct{}) error {
	ticker := time.NewTicker(s.every)
	defer ticker.Stop()
	budget := s.collectionTimeout
	if budget <= 0 {
		budget = 30 * time.Second
	}
	collect := func() error {
		reading, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		return s.take(reading)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-finish:
			return collect()
		default:
		}
		// Periodic failures remain in the samples. A failed final inventory
		// also fails the run, since no subsequent collection can fill it in.
		_ = collect()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-finish:
			return collect()
		case <-ticker.C:
		}
	}
}

// take preserves failed reads as missing evidence. A partly collected reading
// cannot establish a zero count or a low maximum for the failed source.
func (s *sampler) take(ctx context.Context) error {
	now := time.Now().UTC()
	reading := sample{At: now, Managers: map[string]managerReading{}}
	var problems []error
	for _, source := range []struct {
		name string
		read func() error
	}{
		{sourcePods, func() error { return s.readPods(ctx, &reading) }},
		{sourceResources, func() error { return s.readResources(ctx, now, &reading) }},
		{sourceRetained, func() error { return s.readRetained(ctx, &reading) }},
		{sourceManagers, func() error { return s.readManagers(ctx, &reading) }},
		{sourceAPI, func() error { return s.readAPIServers(ctx, &reading) }},
		{sourceJobs, func() error { return s.readJobs(ctx) }},
	} {
		if err := source.read(); err != nil {
			reading.Incomplete = append(reading.Incomplete, source.name)
			problems = append(problems, fmt.Errorf("%s: %w", source.name, err))
			slog.Warn("incomplete sample", "source", source.name, "error", err)
		}
	}
	// Cancellation is missing evidence too. Do not erase a partially read
	// interval or turn its missing sources into zero-valued measurements.
	s.mu.Lock()
	s.samples = append(s.samples, reading)
	s.mu.Unlock()
	return errors.Join(append(problems, ctx.Err())...)
}

func (s *sampler) readPods(ctx context.Context, into *sample) error {
	for _, namespace := range workloadNamespaces(s.namespace, s.namespaces) {
		if err := s.readPodsNamespace(ctx, into, namespace); err != nil {
			return fmt.Errorf("namespace %s: %w", namespace, err)
		}
	}
	return nil
}

func (s *sampler) readPodsNamespace(ctx context.Context, into *sample, namespace string) error {
	pods, err := s.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
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
	into.ResourceReadStartedAt = time.Now().UTC()
	defer func() { into.ResourceReadFinishedAt = time.Now().UTC() }()
	into.ResourceFreshness = []resourceFreshness{}
	for _, namespace := range workloadNamespaces(s.namespace, s.namespaces) {
		if err := s.readResourcesNamespace(ctx, now, into, namespace); err != nil {
			return fmt.Errorf("namespace %s: %w", namespace, err)
		}
	}
	return nil
}

func (s *sampler) readResourcesNamespace(ctx context.Context, now time.Time, into *sample, namespace string) error {
	for _, family := range []struct {
		resource  schema.GroupVersionResource
		observed  []string
		converged string
	}{
		{schemaResource, []string{"status", "target", "lastObservedAt"}, "InSync"},
		{migrationResource, []string{"status", "history", "observedAt"}, "InSync"},
	} {
		list, err := s.dynamic.Resource(family.resource).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: s.selector})
		if err != nil {
			return err
		}
		readAt := time.Now().UTC()
		for _, item := range list.Items {
			familyName := "schema"
			if family.resource == migrationResource {
				familyName = "migration"
			}
			freshness, err := readResourceFreshness(familyName, &item, readAt, family.observed)
			if err != nil {
				return err
			}
			into.ResourceFreshness = append(into.ResourceFreshness, freshness)
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
	for _, namespace := range workloadNamespaces(s.namespace, s.namespaces) {
		if err := s.readRetainedNamespace(ctx, into, namespace); err != nil {
			return fmt.Errorf("namespace %s: %w", namespace, err)
		}
	}
	return nil
}

func (s *sampler) readRetainedNamespace(ctx context.Context, into *sample, namespace string) error {
	for _, resource := range []schema.GroupVersionResource{schemaPlanResource, migrationPlanResource} {
		plans, err := s.dynamic.Resource(resource).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		into.Plans += len(plans.Items)
	}
	chunks, err := s.dynamic.Resource(planChunkResource).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	into.Chunks += len(chunks.Items)
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
	population, err := managerPopulation(pods.Items, s.expectedManagers)
	if err != nil {
		return err
	}
	var problems []error
	for _, pod := range pods.Items {
		identity, err := managerIdentity(&pod)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		scrapeStarted := time.Now().UTC()
		reading, err := scrapePod(ctx, s.clientset, s.operatorNamespace, pod.Name, s.metricsPort)
		scrapeFinished := time.Now().UTC()
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
		started, hasStarted := reading.value("process_start_time_seconds", nil)
		if !hasStarted || !validProcessStart(started) {
			problems = append(problems, fmt.Errorf("manager %s has no valid process start time", pod.Name))
			continue
		}
		// A scrape by Pod name must still belong to the container listed before it.
		current, err := s.clientset.CoreV1().Pods(s.operatorNamespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			problems = append(problems, fmt.Errorf("confirm manager %s after scrape: %w", pod.Name, err))
			continue
		}
		confirmed, err := managerIdentity(current)
		if err != nil || confirmed != identity {
			problems = append(problems, fmt.Errorf("manager %s changed identity during scrape", pod.Name))
			continue
		}
		identity.ProcessStartedAt = started
		throttle, _ := reading.value("rest_client_rate_limiter_duration_seconds_sum", nil)
		if throttle == 0 {
			throttle = reading.histogram("rest_client_rate_limiter_duration_seconds", nil).sum
		}
		too, _ := reading.value("rest_client_requests_total", map[string]string{"code": "429"})
		var requests *apiRequestReading
		if total, present := reading.value("rest_client_requests_total", nil); present && counterContinues(0, total) {
			requests = &apiRequestReading{Total: total, StartedAt: scrapeStarted, FinishedAt: scrapeFinished}
		}
		queue := reading.histogram("workqueue_queue_duration_seconds", nil)
		depths := reading.maxBy("workqueue_depth", "name")
		validDepths := true
		for _, depth := range depths {
			validDepths = validDepths && counterContinues(0, depth)
		}
		if !validDepths || !counterContinues(0, throttle) || !counterContinues(0, too) || !validHistogram(queue) {
			problems = append(problems, fmt.Errorf("manager %s has invalid client or queue counters", pod.Name))
			continue
		}
		into.Managers[pod.Name] = managerReading{
			APIRequests: requests,
			PodUID:      identity.PodUID, ContainerID: identity.ContainerID,
			ContainerStartedAt: identity.ContainerStartedAt, ProcessStartedAt: identity.ProcessStartedAt,
			RestartCount:    identity.RestartCount,
			RSSBytes:        rss,
			CPUSeconds:      cpu,
			WorkqueueDepth:  depths,
			ThrottleSeconds: throttle,
			Requests429:     too,
			QueueWait:       queue,
		}
	}
	if len(into.Managers) == 0 {
		problems = append(problems, errors.New("no running manager produced process metrics"))
	}
	current, err := s.clientset.CoreV1().Pods(s.operatorNamespace).List(ctx, metav1.ListOptions{LabelSelector: s.managerSelector})
	if err != nil {
		problems = append(problems, err)
	} else {
		confirmed, identityErr := managerPopulation(current.Items, s.expectedManagers)
		if identityErr != nil || !maps.Equal(population, confirmed) {
			problems = append(problems, errors.New("manager population changed during collection"))
		}
	}
	return errors.Join(problems...)
}

func managerPopulation(pods []corev1.Pod, expected int) (map[string]processIdentity, error) {
	if expected < 1 || len(pods) != expected {
		return nil, fmt.Errorf("found %d manager Pods, require %d", len(pods), expected)
	}
	population := map[string]processIdentity{}
	uids := map[string]bool{}
	for _, pod := range pods {
		identity, err := managerIdentity(&pod)
		_, duplicate := population[pod.Name]
		if err != nil || pod.Name == "" || pod.DeletionTimestamp != nil || duplicate || uids[identity.PodUID] {
			return nil, fmt.Errorf("manager Pod %s does not identify a distinct live process", pod.Name)
		}
		population[pod.Name] = identity
		uids[identity.PodUID] = true
	}
	return population, nil
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
	for _, namespace := range workloadNamespaces(s.namespace, s.namespaces) {
		if err := s.readJobsNamespace(ctx, namespace); err != nil {
			return fmt.Errorf("namespace %s: %w", namespace, err)
		}
	}
	return nil
}

func (s *sampler) readJobsNamespace(ctx context.Context, namespace string) error {
	jobs, err := s.clientset.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
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
				Namespace: job.Namespace,
				UID:       string(job.UID),
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
