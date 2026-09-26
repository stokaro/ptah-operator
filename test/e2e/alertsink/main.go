// Command alertsink is the receiver the alerting phase points Alertmanager at.
// It accepts the webhook payload Alertmanager sends and writes one JSON line
// per alert to standard output, so the phase reads what was delivered from the
// Pod's log rather than from Alertmanager's own view of what it meant to send.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

// maxPayloadBytes bounds one delivery. Alertmanager groups alerts into one
// request, and the phase raises a handful at a time.
const maxPayloadBytes = 1 << 20

// payload is the part of Alertmanager's webhook body the phase asserts on.
// Version 4 is the only one Alertmanager has sent since it introduced the
// receiver, and anything else is refused rather than guessed at.
type payload struct {
	Version  string  `json:"version"`
	Receiver string  `json:"receiver"`
	Status   string  `json:"status"`
	Alerts   []alert `json:"alerts"`
}

type alert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt"`
}

// delivery is one line of the log: an alert as the receiver got it.
type delivery struct {
	Receiver    string            `json:"receiver"`
	Status      string            `json:"status"`
	AlertName   string            `json:"alertname"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt"`
	ReceivedAt  time.Time         `json:"receivedAt"`
}

func main() {
	address := flag.String("listen", ":8080", "address to accept deliveries on")
	flag.Parse()
	log.SetOutput(os.Stderr)
	server := &http.Server{
		Addr:              *address,
		Handler:           handler(os.Stdout, time.Now),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("alertsink: listening on %s", *address)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("alertsink: %v", err)
	}
}

// handler accepts POST /alerts and answers GET /healthz. A delivery it cannot
// read is answered with 400, which Alertmanager logs and retries, and nothing
// is written for it: a line in the log is a delivery that was understood.
func handler(out io.Writer, now func() time.Time) http.Handler {
	encoder := json.NewEncoder(out)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /alerts", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPayloadBytes))
		if err != nil {
			http.Error(w, "read the delivery: "+err.Error(), http.StatusBadRequest)
			return
		}
		received, err := decode(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		at := now().UTC()
		for _, one := range received.Alerts {
			if err := encoder.Encode(delivery{
				Receiver:    received.Receiver,
				Status:      one.Status,
				AlertName:   one.Labels["alertname"],
				Labels:      one.Labels,
				Annotations: one.Annotations,
				StartsAt:    one.StartsAt,
				EndsAt:      one.EndsAt,
				ReceivedAt:  at,
			}); err != nil {
				http.Error(w, "record the delivery: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func decode(body []byte) (payload, error) {
	var received payload
	if err := json.Unmarshal(body, &received); err != nil {
		return payload{}, fmt.Errorf("the delivery is not JSON: %w", err)
	}
	if received.Version != "4" {
		return payload{}, fmt.Errorf("the delivery is webhook version %q, and this receiver reads version 4", received.Version)
	}
	if len(received.Alerts) == 0 {
		return payload{}, errors.New("the delivery carries no alert")
	}
	for index, one := range received.Alerts {
		if one.Status != "firing" && one.Status != "resolved" {
			return payload{}, fmt.Errorf("alert %d has status %q", index, one.Status)
		}
		if one.Labels["alertname"] == "" {
			return payload{}, fmt.Errorf("alert %d names no alertname", index)
		}
	}
	return received, nil
}
