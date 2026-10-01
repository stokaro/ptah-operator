package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type apiIdentity struct {
	Node    string `json:"node"`
	NodeUID string `json:"nodeUID"`
	Pod     string `json:"pod"`
	processIdentity
}

type apiReading struct {
	apiIdentity
	ScrapeStartedAt   time.Time `json:"scrapeStartedAt"`
	ScrapeCompletedAt time.Time `json:"scrapeCompletedAt"`
	Rejected          float64   `json:"rejected"`
	Admission         histogram `json:"admission"`
}

// apiPopulation proves a one-to-one mapping from the declared control-plane
// nodes to running kube-apiserver containers. An absent member is not zero load.
func apiPopulation(nodes []corev1.Node, pods []corev1.Pod, expected int) (map[string]apiIdentity, error) {
	if expected < 1 || len(nodes) != expected {
		return nil, fmt.Errorf("found %d control-plane nodes, require %d", len(nodes), expected)
	}
	byNode := map[string]string{}
	nodeUIDs := map[string]bool{}
	for _, node := range nodes {
		_, control := node.Labels["node-role.kubernetes.io/control-plane"]
		if !control || node.Name == "" || node.UID == "" || byNode[node.Name] != "" || nodeUIDs[string(node.UID)] {
			return nil, errors.New("control-plane nodes need distinct names and UIDs")
		}
		byNode[node.Name] = string(node.UID)
		nodeUIDs[string(node.UID)] = true
	}
	targets := map[string]apiIdentity{}
	seenUID := map[string]bool{}
	for _, pod := range pods {
		nodeUID := byNode[pod.Spec.NodeName]
		if pod.Namespace != "kube-system" || pod.Labels["component"] != "kube-apiserver" || nodeUID == "" || pod.Name == "" || pod.DeletionTimestamp != nil {
			return nil, errors.New("API Pod does not identify a live member on a declared control-plane node")
		}
		id, err := containerIdentity(&pod, "kube-apiserver")
		if err != nil {
			return nil, err
		}
		if _, exists := targets[pod.Spec.NodeName]; exists || seenUID[id.PodUID] {
			return nil, errors.New("duplicate API server node or Pod UID")
		}
		seenUID[id.PodUID] = true
		targets[pod.Spec.NodeName] = apiIdentity{Node: pod.Spec.NodeName, NodeUID: nodeUID, Pod: pod.Name, processIdentity: id}
	}
	if len(targets) != expected {
		return nil, fmt.Errorf("found %d API servers, require %d", len(targets), expected)
	}
	return targets, nil
}

func (s *sampler) readAPIServers(ctx context.Context, into *sample) error {
	nodes, err := s.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/control-plane"})
	if err != nil {
		return err
	}
	pods, err := s.clientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{LabelSelector: "component=kube-apiserver"})
	if err != nil {
		return err
	}
	targets, err := apiPopulation(nodes.Items, pods.Items, s.expectedAPIServers)
	if err != nil {
		return err
	}
	for _, id := range targets {
		into.APITargets = append(into.APITargets, id)
	}
	sort.Slice(into.APITargets, func(i, j int) bool { return into.APITargets[i].Node < into.APITargets[j].Node })
	if s.scrapeAPI == nil {
		return errors.New("no direct API server scraper configured")
	}
	into.APIServers = map[string]apiReading{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var problems []error
	for _, pod := range pods.Items {
		wg.Go(func() {
			id := targets[pod.Spec.NodeName]
			reading := apiReading{apiIdentity: id, ScrapeStartedAt: time.Now().UTC()}
			metrics, scrapeErr := s.scrapeAPI(ctx, pod)
			reading.ScrapeCompletedAt = time.Now().UTC()
			if scrapeErr == nil {
				current, getErr := s.clientset.CoreV1().Pods("kube-system").Get(ctx, pod.Name, metav1.GetOptions{})
				if getErr != nil {
					scrapeErr = getErr
				} else {
					confirmed, identityErr := containerIdentity(current, "kube-apiserver")
					if identityErr != nil || confirmed != id.processIdentity || current.Spec.NodeName != id.Node || current.DeletionTimestamp != nil {
						scrapeErr = errors.New("API server changed identity during scrape")
					}
				}
			}
			if scrapeErr == nil {
				started, found := metrics.value("process_start_time_seconds", nil)
				reading.ProcessStartedAt = started
				reading.Rejected, _ = metrics.value("apiserver_flowcontrol_rejected_requests_total", nil)
				reading.Admission = (histogram{}).onlyPtah(metrics)
				if !found || !validProcessStart(started) || !counterContinues(0, reading.Rejected) || !validHistogram(reading.Admission) {
					scrapeErr = errors.New("API server has invalid process or admission metrics")
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if scrapeErr != nil {
				problems = append(problems, fmt.Errorf("API server %s: %w", id.Node, scrapeErr))
				return
			}
			into.APIServers[id.Node] = reading
		})
	}
	wg.Wait()
	// Population changes during the collection also invalidate an otherwise
	// successful set of scrapes, including a newly added or relabeled member.
	currentNodes, listErr := s.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/control-plane"})
	if listErr != nil {
		problems = append(problems, listErr)
	} else {
		currentPods, podErr := s.clientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{LabelSelector: "component=kube-apiserver"})
		if podErr != nil {
			problems = append(problems, podErr)
		} else {
			current, popErr := apiPopulation(currentNodes.Items, currentPods.Items, s.expectedAPIServers)
			if popErr != nil || !maps.Equal(current, targets) {
				problems = append(problems, errors.New("API population changed during collection"))
			}
		}
	}
	return errors.Join(problems...)
}
