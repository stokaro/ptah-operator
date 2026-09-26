package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// workload is what one measurement runs, read from a file so the report can
// carry exactly what was run beside what it cost. Every dimension the capacity
// page names is either a field here or held fixed by the lab, and the report
// says which.
type workload struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Schemas and Migrations are how many resources of each family run at
	// once, each against a database of its own, so each is its own realm.
	Schemas    int `json:"schemas"`
	Migrations int `json:"migrations"`
	// Interval is every resource's spec.interval.
	Interval duration `json:"interval"`
	// Settle bounds each wait for the workload to converge.
	Settle duration `json:"settle"`
	// SteadyState is how long the converged workload is watched unchanged.
	SteadyState duration `json:"steadyState"`
	// SampleEvery is the sampling period for every series in the report.
	SampleEvery duration `json:"sampleEvery"`
	// ChangeBatch is how many resources of each family move to a second
	// artifact at once.
	ChangeBatch int `json:"changeBatch"`
	// Outage is how long operation Pods cannot reach the registry.
	Outage duration `json:"outage"`
}

// duration reads a Go duration string from JSON.
type duration struct{ time.Duration }

func (d *duration) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return fmt.Errorf("a duration is a string such as \"1m\": %w", err)
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

func (d duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func loadWorkload(path string) (workload, error) {
	content, err := os.ReadFile(path) //nolint:gosec // The path is the caller's own argument.
	if err != nil {
		return workload{}, err
	}
	var w workload
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&w); err != nil {
		return workload{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return w, w.validate()
}

func (w workload) validate() error {
	var problems []error
	if w.Name == "" {
		problems = append(problems, errors.New("name is empty"))
	}
	if w.Schemas < 0 || w.Migrations < 0 || w.Schemas+w.Migrations == 0 {
		problems = append(problems, errors.New("the workload runs no resources"))
	}
	if w.ChangeBatch < 0 || w.ChangeBatch > w.Schemas && w.ChangeBatch > w.Migrations {
		problems = append(problems, fmt.Errorf("changeBatch %d is more than either family runs", w.ChangeBatch))
	}
	for name, value := range map[string]time.Duration{
		"interval": w.Interval.Duration, "settle": w.Settle.Duration,
		"steadyState": w.SteadyState.Duration, "sampleEvery": w.SampleEvery.Duration,
	} {
		if value <= 0 {
			problems = append(problems, fmt.Errorf("%s has to be positive", name))
		}
	}
	if w.Outage.Duration < 0 {
		problems = append(problems, errors.New("outage cannot be negative"))
	}
	return errors.Join(problems...)
}
