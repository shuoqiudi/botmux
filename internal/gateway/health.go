package gateway

import (
	"context"
	"sync"
	"time"
)

type QueueObservation struct {
	Available   bool
	Persistence bool
	Depth       int64
	Pending     int64
	CheckedAt   time.Time
}

// OperationalQueue is kept separate from the worker queue contracts so tests
// and alternate queue implementations do not gain unrelated obligations.
type OperationalQueue interface {
	InspectQueue(context.Context) (QueueObservation, error)
	ReplayFromDLQ(context.Context, string, int) (string, error)
}

type workerHeartbeat struct {
	mu      sync.RWMutex
	running bool
	last    time.Time
}

func (h *workerHeartbeat) start() {
	h.mu.Lock()
	h.running = true
	h.last = time.Now().UTC()
	h.mu.Unlock()
}

func (h *workerHeartbeat) beat() {
	h.mu.Lock()
	h.last = time.Now().UTC()
	h.mu.Unlock()
}

func (h *workerHeartbeat) stop() {
	h.mu.Lock()
	h.running = false
	h.last = time.Now().UTC()
	h.mu.Unlock()
}

func (h *workerHeartbeat) snapshot() (bool, time.Time) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.running, h.last
}
