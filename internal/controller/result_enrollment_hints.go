package controller

import (
	"sync"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultconsumer"
)

const (
	resultEnrollmentHintLimit = 64
	resultEnrollmentHintTTL   = time.Minute
)

// A hint only avoids enrolling an already enrolled operation again. It never
// authorizes delivery: the receiver and admission read the immutable first-Pod
// pin and current authority on every publication. Losing or expiring a hint
// falls back to the existing direct reads. Failed enrollment is never remembered.
type resultEnrollmentHints struct {
	mu      sync.Mutex
	entries map[resultconsumer.Request]time.Time
	now     func() time.Time
}

// Reconcilers can be copied by restart tests. Keep the mutex behind a pointer,
// using the same lazy-initialization boundary as their result-read bookkeeping.
var resultEnrollmentHintsMade sync.Mutex

func resultEnrollmentHintsOf(field **resultEnrollmentHints) *resultEnrollmentHints {
	resultEnrollmentHintsMade.Lock()
	defer resultEnrollmentHintsMade.Unlock()
	if *field == nil {
		*field = &resultEnrollmentHints{}
	}
	return *field
}

func (h *resultEnrollmentHints) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

func (h *resultEnrollmentHints) known(request resultconsumer.Request) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	deadline, found := h.entries[request]
	return found && h.clock().Before(deadline)
}

func (h *resultEnrollmentHints) remember(request resultconsumer.Request) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.clock()
	if h.entries == nil {
		h.entries = make(map[resultconsumer.Request]time.Time)
	}
	for key, deadline := range h.entries {
		if !now.Before(deadline) {
			delete(h.entries, key)
		}
	}
	if len(h.entries) < resultEnrollmentHintLimit {
		h.entries[request] = now.Add(resultEnrollmentHintTTL)
	}
}
