package main

import (
	"encoding/json"
	"fmt"
	"slices"
)

const (
	sourcePods              = "pods"
	sourceResources         = "resources"
	sourceRetained          = "retained-objects"
	sourceManagers          = "manager-metrics"
	sourceManagerContinuity = "manager-counter-continuity"
	sourceAPI               = "api-server-metrics"
	sourceAPIContinuity     = "api-server-counter-continuity"
	sourceJobs              = "jobs"
	sourceCycles            = "refresh-cycles"
)

var sampleFields = map[string][]string{
	sourcePods:      {"podsPending", "podsRunning"},
	sourceResources: {"resources", "converged", "observationAgeMax", "overdueMax", "resourceFreshness"},
	sourceRetained:  {"plans", "chunks", "chunkBytes"},
	sourceManagers:  {"managers"},
	sourceAPI:       {"apiServers"},
	sourceJobs:      {}, // Job readings are retained separately; preserve the failed source.
}

var scenarioFields = map[string][]string{
	sourceCycles:            {"refreshCycles"},
	sourceAPIContinuity:     {"apiServers", "apiRejected"},
	sourceManagerContinuity: {"managerCPUCoresAverage", "queueWaitSeconds", "clientThrottleSeconds", "requests429"},
	sourcePods:              {"podsPendingMax", "podsRunningMax"},
	sourceResources:         {"observationAgeMaxSeconds", "overdueMaxSeconds", "eligibleFreshness"},
	sourceRetained:          {"plansAtEnd", "chunkBytesAtEnd"},
	sourceManagers:          {"managerRSSMaxBytes", "managerCPUCoresAverage", "workqueueDepthMax", "queueWaitSeconds", "clientThrottleSeconds", "requests429"},
	sourceAPI:               {"apiServers", "apiRejected"},
	sourceJobs:              {"jobsCreated", "jobsFailed", "jobsPerMinuteAverage", "jobsPerMinutePeak", "jobStartSeconds", "jobCompletionSeconds"},
}

// JSON null makes missing evidence distinct from a measured zero, including
// partially collected lists and maxima over windows with a failed reading.
func marshalIncomplete(value any, missing []string, fields map[string][]string) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		return nil, err
	}
	for _, source := range missing {
		names, ok := fields[source]
		if !ok {
			return nil, fmt.Errorf("unknown incomplete measurement source %q", source)
		}
		for _, name := range names {
			object[name] = json.RawMessage("null")
		}
	}
	return json.Marshal(object)
}

func (s sample) MarshalJSON() ([]byte, error) {
	type plain sample
	return marshalIncomplete(plain(s), s.Incomplete, sampleFields)
}

func (s scenarioCost) missing(source string) bool {
	return s.Samples == 0 || s.Incomplete[source] > 0 || source == sourceManagerContinuity && s.Incomplete[sourceManagers] > 0 || source == sourceAPI && s.Incomplete[sourceAPIContinuity] > 0
}

func (s scenarioCost) MarshalJSON() ([]byte, error) {
	type plain scenarioCost
	var missing []string
	for source := range scenarioFields {
		if s.missing(source) {
			missing = append(missing, source)
		}
	}
	// Refuse a new failure category until its affected figures are declared.
	for source := range s.Incomplete {
		if !slices.Contains(missing, source) {
			missing = append(missing, source)
		}
	}
	return marshalIncomplete(plain(s), missing, scenarioFields)
}
