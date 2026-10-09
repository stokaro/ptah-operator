package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/stokaro/ptah-operator/internal/runner"
	operationworkload "github.com/stokaro/ptah-operator/internal/workload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	databaseDelayLabel  = "operator.ptah.run/capacity-database-delay"
	databaseDelayHealth = 8081
)

// databaseDelayWorkload is the slow-database probe: for Hold, every new
// database session waits Delay before it reaches the database, and then the
// delay is removed. Sessions already open are not touched.
type databaseDelayWorkload struct {
	Delay duration `json:"delay"`
	Hold  duration `json:"hold"`
}

func (d *databaseDelayWorkload) validate(w workload) error {
	var problems []error
	if d.Delay.Duration < time.Second {
		problems = append(problems, errors.New("databaseDelay.delay must be at least one second"))
	}
	// Each resource starts a fresh database operation at least once per
	// interval, so two intervals give every family one inside the fault.
	if d.Hold.Duration < 2*w.Interval.Duration {
		problems = append(problems, errors.New("databaseDelay.hold must span at least two refresh intervals"))
	}
	if w.ApprovalBacklog {
		problems = append(problems, errors.New("the approval backlog runs no database delay"))
	}
	return errors.Join(problems...)
}

// databaseDelayProof is what the report keeps: the fault's own identities, the
// proxy's account of each delayed session, and one fresh database operation per
// family and namespace that the delay reached.
type databaseDelayProof struct {
	Delay               string                      `json:"delay"`
	Hold                string                      `json:"hold"`
	FixtureNamespace    string                      `json:"fixtureNamespace"`
	Service             string                      `json:"service"`
	ProxyPodUID         string                      `json:"proxyPodUID"`
	ProxyImage          string                      `json:"proxyImage"`
	InjectedAt          time.Time                   `json:"injectedAt"`
	RestoredAt          time.Time                   `json:"restoredAt"`
	Operations          map[string]delayedOperation `json:"operations"`
	DelayedSessions     int                         `json:"delayedSessions"`
	ShortestSession     float64                     `json:"shortestDelayedSessionSeconds"`
	ProxyLog            string                      `json:"proxyLog"`
	ProxyLogSHA256      string                      `json:"proxyLogSHA256"`
	RecoveryStartedAt   time.Time                   `json:"recoveryStartedAt"`
	Recovery            string                      `json:"recovery"`
	RemovedProxyObjects []string                    `json:"removedProxyObjects"`
}

// delayedOperation is one operation Pod the delay reached: it either ended in a
// failure that started no mutation, or its Ptah container ran at least the delay.
type delayedOperation struct {
	PodUID     string    `json:"podUID"`
	Operation  string    `json:"operation"`
	Outcome    string    `json:"outcome"`
	CreatedAt  time.Time `json:"createdAt"`
	FinishedAt time.Time `json:"finishedAt"`
	RunSeconds float64   `json:"runSeconds"`
}

// capacityDatabase names the server the bootstrap provisioned for the engine.
type capacityDatabase struct {
	namespace, name, portName string
	port                      int32
}

func (d capacityDatabase) labels() map[string]string {
	return map[string]string{"app.kubernetes.io/name": d.name}
}

func (d capacityDatabase) delayedLabels() map[string]string {
	labels := d.labels()
	labels[databaseDelayLabel] = "true"
	return labels
}

func readCapacityDatabase(statePath string) (capacityDatabase, error) {
	raw, err := os.ReadFile(statePath) //nolint:gosec // The path is the caller's own argument.
	if err != nil {
		return capacityDatabase{}, err
	}
	var state struct {
		Engine           string `json:"engine"`
		FixtureNamespace string `json:"fixtureNamespace"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return capacityDatabase{}, fmt.Errorf("read the capacity bootstrap state: %w", err)
	}
	if state.FixtureNamespace == "" {
		return capacityDatabase{}, errors.New("the capacity bootstrap state names no fixture namespace")
	}
	switch state.Engine {
	case "PostgreSQL":
		return capacityDatabase{namespace: state.FixtureNamespace, name: "capacity-postgres", portName: "postgresql", port: 5432}, nil
	case "MySQL":
		return capacityDatabase{namespace: state.FixtureNamespace, name: "capacity-mysql", portName: "mysql", port: 3306}, nil
	}
	return capacityDatabase{}, fmt.Errorf("the capacity bootstrap state names engine %q", state.Engine)
}

// databaseOperations are the operations that open a database session.
var databaseOperations = map[runner.Operation]bool{
	runner.OperationObserve: true, runner.OperationPlan: true, runner.OperationApply: true,
	runner.OperationMigrationHistory: true, runner.OperationMigrationApply: true,
}

// delayedDatabaseOperation reports whether one operation Pod is evidence the
// delay reached the operator: a database operation created after the fault
// whose Ptah container either ran at least the delay or reported a failure
// that started no mutation and left nothing uncertain. A registry operation, a
// Pod from before the fault, and a failure that may have mutated prove nothing
// about this fault.
func (s *scenarios) delayedDatabaseOperation(pod corev1.Pod, after time.Time, delay time.Duration) (string, delayedOperation, bool) {
	operation := runner.Operation(pod.Labels[operationworkload.LabelOperation])
	if !databaseOperations[operation] || pod.UID == "" || !pod.CreationTimestamp.After(after) ||
		pod.Annotations[operationworkload.AnnotationOperationID] == "" {
		return "", delayedOperation{}, false
	}
	family := ""
	for i := range s.load.Schemas {
		if pod.Namespace == s.in.namespaceFor(i) && pod.Labels[operationworkload.LabelSchema] == s.schemaName(i) {
			family = "schema"
		}
	}
	for i := range s.load.Migrations {
		if pod.Namespace == s.in.namespaceFor(i) && pod.Labels[operationworkload.LabelMigration] == s.migrationName(i) {
			family = "migration"
		}
	}
	if family == "" {
		return "", delayedOperation{}, false
	}
	for _, container := range pod.Status.ContainerStatuses {
		end := container.State.Terminated
		if container.Name != "ptah" || end == nil || end.ExitCode != 0 || end.StartedAt.IsZero() || !end.FinishedAt.After(after) {
			continue
		}
		summary, err := runner.ParseSummaryFor(end.Message, operation, pod.Annotations[operationworkload.AnnotationOperationID])
		if err != nil || summary.MutationStarted || summary.Uncertain {
			continue
		}
		run := end.FinishedAt.Sub(end.StartedAt.Time)
		record := delayedOperation{PodUID: string(pod.UID), Operation: string(operation), CreatedAt: pod.CreationTimestamp.UTC(),
			FinishedAt: end.FinishedAt.UTC(), RunSeconds: run.Seconds()}
		switch {
		case summary.ErrorCode != "":
			record.Outcome = "failed"
		case run >= delay:
			record.Outcome = "delayed"
		default:
			continue
		}
		return family, record, true
	}
	return "", delayedOperation{}, false
}

// databaseDelay points the database Service at a proxy that holds each new
// session, proves operation Pods of both families were reached by it, and
// removes it again before measuring fresh convergence.
func (s *scenarios) databaseDelay(ctx context.Context) (resultErr error) {
	if s.load.DatabaseDelay == nil {
		return nil
	}
	if s.in.fixtureImage == "" || s.in.database.name == "" {
		return errors.New("the database delay needs the fixture image and the bootstrap's database")
	}
	db, delay := s.in.database, s.load.DatabaseDelay.Delay.Duration
	proof := &databaseDelayProof{Delay: s.load.DatabaseDelay.Delay.String(), Hold: s.load.DatabaseDelay.Hold.String(),
		FixtureNamespace: db.namespace, Service: db.name, ProxyImage: s.in.fixtureImage, Operations: map[string]delayedOperation{}}
	s.databaseDelayProof = proof
	services := s.clientset.CoreV1().Services(db.namespace)
	original, err := services.Get(ctx, db.name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read the database Service: %w", err)
	}
	if !reflect.DeepEqual(original.Spec.Selector, db.labels()) || len(original.Spec.Ports) != 1 ||
		original.Spec.Ports[0].Port != db.port || original.Spec.Ports[0].TargetPort.String() != db.portName {
		return errors.New("the database Service is not the one the bootstrap created")
	}

	var created []func(context.Context) error
	flipped := false
	cleanup := func(cleanupCtx context.Context) error {
		var problems []error
		if flipped {
			problems = append(problems, s.setDatabaseSelector(cleanupCtx, db, original.UID, db.delayedLabels(), db.labels()))
			flipped = false
		}
		for i := len(created) - 1; i >= 0; i-- {
			problems = append(problems, created[i](cleanupCtx))
		}
		created = nil
		return errors.Join(problems...)
	}
	defer func() {
		if created != nil || flipped {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()
			resultErr = errors.Join(resultErr, cleanup(cleanupCtx))
		}
	}()

	direct, err := services.Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: db.name + "-direct", Namespace: db.namespace, Labels: map[string]string{databaseDelayLabel: "direct"}},
		Spec: corev1.ServiceSpec{Selector: db.labels(), Ports: []corev1.ServicePort{{
			Name: db.portName, Port: db.port, TargetPort: intstr.FromString(db.portName),
		}}},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create the direct database Service: %w", err)
	}
	created = append(created, s.deleteByUID("Service", direct.Name, direct.UID, func(ctx context.Context, opts metav1.DeleteOptions) error {
		return services.Delete(ctx, direct.Name, opts)
	}, proof))

	policies := s.clientset.NetworkingV1().NetworkPolicies(db.namespace)
	port := intstr.FromInt32(db.port)
	tcp := corev1.ProtocolTCP
	policy, err := policies.Create(ctx, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "capacity-database-delay", Namespace: db.namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: db.labels()},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{databaseDelayLabel: "true"}}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("admit the proxy to the database: %w", err)
	}
	created = append(created, s.deleteByUID("NetworkPolicy", policy.Name, policy.UID, func(ctx context.Context, opts metav1.DeleteOptions) error {
		return policies.Delete(ctx, policy.Name, opts)
	}, proof))

	deployments := s.clientset.AppsV1().Deployments(db.namespace)
	proxy, err := deployments.Create(ctx, s.delayProxyDeployment(db, delay), metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create the database delay proxy: %w", err)
	}
	created = append(created, s.removeDelayProxy(db, proxy.UID, proof))
	pod, err := s.waitDelayProxyReady(ctx, db)
	if err != nil {
		return err
	}
	proof.ProxyPodUID = string(pod.UID)

	if err := s.setDatabaseSelector(ctx, db, original.UID, db.labels(), db.delayedLabels()); err != nil {
		return err
	}
	flipped = true
	proof.InjectedAt = time.Now().UTC()
	if err := s.waitDatabaseEndpoints(ctx, db, pod.Status.PodIP); err != nil {
		return err
	}

	deadline := proof.InjectedAt.Add(s.load.DatabaseDelay.Hold.Duration)
	for {
		for _, namespace := range workloadNamespaces(s.in.namespace, s.in.namespaces) {
			pods, err := s.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/managed-by=ptah-operator"})
			if err != nil {
				return fmt.Errorf("read the database delay evidence: %w", err)
			}
			for _, candidate := range pods.Items {
				if family, record, ok := s.delayedDatabaseOperation(candidate, proof.InjectedAt, delay); ok {
					if _, seen := proof.Operations[namespace+"/"+family]; !seen {
						proof.Operations[namespace+"/"+family] = record
					}
				}
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(pollEvery, time.Until(deadline))):
		}
	}
	if err := s.retainDelayProxyLog(ctx, db, pod, proof, delay); err != nil {
		return err
	}
	s.mark("database delay", proof.InjectedAt, map[string]string{"delayedSessions": fmt.Sprint(proof.DelayedSessions), "proxyPodUID": proof.ProxyPodUID})
	if err := s.requireDelayedFamilies(proof); err != nil {
		return err
	}

	if err := s.setDatabaseSelector(ctx, db, original.UID, db.delayedLabels(), db.labels()); err != nil {
		return err
	}
	flipped = false
	proof.RestoredAt = time.Now().UTC()
	if err := s.waitDatabaseEndpoints(ctx, db, ""); err != nil {
		return err
	}
	if err := cleanup(ctx); err != nil {
		return fmt.Errorf("remove the database delay: %w", err)
	}
	proof.RecoveryStartedAt = proof.RestoredAt
	converged, err := s.waitConverged(ctx, proof.RestoredAt, nil)
	proof.Recovery = converged
	s.mark("database delay recovery", proof.RestoredAt, map[string]string{"converged": converged})
	return err
}

func (s *scenarios) requireDelayedFamilies(proof *databaseDelayProof) error {
	if proof.DelayedSessions == 0 {
		return errors.New("the proxy delayed no database session")
	}
	for _, family := range []struct {
		name  string
		count int
	}{{"schema", s.load.Schemas}, {"migration", s.load.Migrations}} {
		for i := range family.count {
			namespace := s.in.namespaceFor(i)
			if _, ok := proof.Operations[namespace+"/"+family.name]; !ok {
				return fmt.Errorf("the database delay reached no fresh %s operation in %s", family.name, namespace)
			}
		}
	}
	return nil
}

func (s *scenarios) delayProxyDeployment(db capacityDatabase, delay time.Duration) *appsv1.Deployment {
	replicas := int32(1)
	grace := int64(900)
	noEscalation, nonRoot := false, true
	probe := []string{"/e2e-delay-proxy", "-probe", fmt.Sprintf("127.0.0.1:%d", databaseDelayHealth)}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: db.name + "-delay", Namespace: db.namespace, Labels: map[string]string{databaseDelayLabel: "proxy"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: db.delayedLabels()},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: db.delayedLabels()},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken:  &noEscalation,
					TerminationGracePeriodSeconds: &grace,
					ImagePullSecrets:              []corev1.LocalObjectReference{{Name: "demo-registry-pull"}},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &nonRoot,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:            "delay",
						Image:           s.in.fixtureImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command: []string{"/e2e-delay-proxy",
							"-listen", fmt.Sprintf(":%d", db.port),
							"-upstream", fmt.Sprintf("%s-direct.%s.svc.cluster.local:%d", db.name, db.namespace, db.port),
							"-delay", delay.String(),
							"-health", fmt.Sprintf("127.0.0.1:%d", databaseDelayHealth)},
						Ports:          []corev1.ContainerPort{{Name: db.portName, ContainerPort: db.port}},
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: probe}}, PeriodSeconds: 3},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &noEscalation,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
}

func (s *scenarios) waitDelayProxyReady(ctx context.Context, db capacityDatabase) (corev1.Pod, error) {
	deadline := time.Now().Add(restartReadyBudget)
	selector := metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: db.delayedLabels()})
	for {
		pods, err := s.clientset.CoreV1().Pods(db.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return corev1.Pod{}, err
		}
		if len(pods.Items) == 1 && pods.Items[0].Status.PodIP != "" && pods.Items[0].DeletionTimestamp == nil && podReady(pods.Items[0]) {
			return pods.Items[0], nil
		}
		if !time.Now().Before(deadline) {
			return corev1.Pod{}, errors.New("the database delay proxy did not become ready")
		}
		if err := waitCapacityPoll(ctx); err != nil {
			return corev1.Pod{}, err
		}
	}
}

func podReady(pod corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// setDatabaseSelector moves the database Service between the database and the
// proxy. It writes only from the selector it expects, so a Service someone
// else changed is refused rather than overwritten.
func (s *scenarios) setDatabaseSelector(ctx context.Context, db capacityDatabase, uid types.UID, from, to map[string]string) error {
	services := s.clientset.CoreV1().Services(db.namespace)
	for {
		current, err := services.Get(ctx, db.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read the database Service: %w", err)
		}
		if current.UID != uid {
			return errors.New("the database Service was replaced during the delay")
		}
		if reflect.DeepEqual(current.Spec.Selector, to) {
			return nil
		}
		if !reflect.DeepEqual(current.Spec.Selector, from) {
			return fmt.Errorf("the database Service selector is %v, not %v", current.Spec.Selector, from)
		}
		current.Spec.Selector = maps.Clone(to)
		if _, err := services.Update(ctx, current, metav1.UpdateOptions{}); apierrors.IsConflict(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("move the database Service: %w", err)
		}
		return nil
	}
}

// waitDatabaseEndpoints waits until the Service's ready endpoints are exactly
// the proxy, or exactly not the proxy when proxyIP is empty.
func (s *scenarios) waitDatabaseEndpoints(ctx context.Context, db capacityDatabase, proxyIP string) error {
	deadline := time.Now().Add(time.Minute)
	for {
		slices, err := s.clientset.DiscoveryV1().EndpointSlices(db.namespace).List(ctx, metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=" + db.name})
		if err != nil {
			return err
		}
		if delayEndpointsSettled(slices.Items, proxyIP) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the database Service endpoints did not settle (proxy %q)", proxyIP)
		}
		if err := waitCapacityPoll(ctx); err != nil {
			return err
		}
	}
}

func delayEndpointsSettled(slices []discoveryv1.EndpointSlice, proxyIP string) bool {
	ready := 0
	sawProxy := false
	for _, slice := range slices {
		for _, endpoint := range slice.Endpoints {
			if endpoint.Conditions.Ready == nil || !*endpoint.Conditions.Ready {
				continue
			}
			ready++
			for _, address := range endpoint.Addresses {
				if proxyIP != "" && address == proxyIP {
					sawProxy = true
				} else if proxyIP != "" {
					return false
				}
			}
		}
	}
	if proxyIP != "" {
		return sawProxy
	}
	return ready > 0
}

// retainDelayProxyLog keeps the proxy's account of every session and counts the
// sessions it actually held for the delay. A proxy that delayed nothing would
// make every failure above coincidence.
func (s *scenarios) retainDelayProxyLog(ctx context.Context, db capacityDatabase, pod corev1.Pod, proof *databaseDelayProof, delay time.Duration) error {
	raw, err := s.clientset.CoreV1().Pods(db.namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "delay"}).DoRaw(ctx)
	if err != nil {
		return fmt.Errorf("read the database delay proxy log: %w", err)
	}
	sessions, shortest, err := countDelayedSessions(raw, delay)
	if err != nil {
		return err
	}
	proof.DelayedSessions, proof.ShortestSession = sessions, shortest
	path := filepath.Join(s.evidenceDir, "database-delay-proxy.jsonl")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	proof.ProxyLog, proof.ProxyLogSHA256 = filepath.Base(path), hex.EncodeToString(sum[:])
	return nil
}

func countDelayedSessions(raw []byte, delay time.Duration) (int, float64, error) {
	sessions, shortest := 0, 0.0
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var line struct {
			Event          string  `json:"event"`
			DelayedSeconds float64 `json:"delayedSeconds"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			return 0, 0, fmt.Errorf("the database delay proxy wrote an unreadable line: %w", err)
		}
		if line.Event != "connected" {
			continue
		}
		if line.DelayedSeconds < delay.Seconds() {
			return 0, 0, fmt.Errorf("the proxy connected a session after %.3fs, before the %s delay", line.DelayedSeconds, delay)
		}
		if sessions == 0 || line.DelayedSeconds < shortest {
			shortest = line.DelayedSeconds
		}
		sessions++
	}
	return sessions, shortest, scanner.Err()
}

func (s *scenarios) deleteByUID(kind, name string, uid types.UID, remove func(context.Context, metav1.DeleteOptions) error, proof *databaseDelayProof) func(context.Context) error {
	return func(ctx context.Context) error {
		if uid == "" {
			return fmt.Errorf("cannot remove %s %s without its UID", kind, name)
		}
		err := remove(ctx, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("remove %s %s: %w", kind, name, err)
		}
		proof.RemovedProxyObjects = append(proof.RemovedProxyObjects, kind+"/"+name)
		return nil
	}
}

// removeDelayProxy deletes the proxy and waits for its Pod to go. The Pod stops
// accepting at once and drains the sessions it already holds, so a session the
// delay let through ends as the database ends it rather than by a cut socket.
func (s *scenarios) removeDelayProxy(db capacityDatabase, uid types.UID, proof *databaseDelayProof) func(context.Context) error {
	deployments := s.clientset.AppsV1().Deployments(db.namespace)
	name := db.name + "-delay"
	remove := s.deleteByUID("Deployment", name, uid, func(ctx context.Context, opts metav1.DeleteOptions) error {
		foreground := metav1.DeletePropagationForeground
		opts.PropagationPolicy = &foreground
		return deployments.Delete(ctx, name, opts)
	}, proof)
	return func(ctx context.Context) error {
		if err := remove(ctx); err != nil {
			return err
		}
		selector := metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: db.delayedLabels()})
		for {
			pods, err := s.clientset.CoreV1().Pods(db.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				return err
			}
			if len(pods.Items) == 0 {
				return nil
			}
			if err := waitCapacityPoll(ctx); err != nil {
				return fmt.Errorf("the database delay proxy did not drain: %w", err)
			}
		}
	}
}
