// Package store implements the in-memory Responses API store used by
// previous_response_id chaining.
package store

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/jahrulnr/fakeagents/internal/clock"
)

// Record is one stored response.
type Record struct {
	ID         string            `json:"id"`
	CreatedAt  time.Time         `json:"created_at"`
	Model      string            `json:"model"`
	Provider   string            `json:"provider"`
	SessionKey string            `json:"session_key,omitempty"`
	Input      []json.RawMessage `json:"input,omitempty"`
	Response   json.RawMessage   `json:"response"`
	Output     []json.RawMessage `json:"output,omitempty"`
}

// Store is the response store contract.
type Store interface {
	Put(record *Record)
	Get(id string) (*Record, bool)
	Delete(id string) bool
	Len() int
	Reset()
	PurgeExpired(now time.Time) int
}

// Memory is a bounded, TTL-aware in-memory Store.
type Memory struct {
	mu      sync.RWMutex
	clock   clock.Clock
	ttl     time.Duration
	max     int
	records map[string]*Record
	order   map[string]uint64
	nextSeq uint64
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

// NewMemory builds a store. ttl <= 0 disables expiry. max <= 0 disables the
// size bound.
func NewMemory(clk clock.Clock, ttl time.Duration, max int) *Memory {
	if clk == nil {
		clk = clock.Real{}
	}
	m := &Memory{
		clock:   clk,
		ttl:     ttl,
		max:     max,
		records: make(map[string]*Record),
		order:   make(map[string]uint64),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if ttl > 0 {
		go m.janitor()
	} else {
		close(m.done)
	}
	return m
}

// Put implements Store.
func (m *Memory) Put(record *Record) {
	if record == nil || record.ID == "" {
		return
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = m.clock.Now()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.records[record.ID]; !exists {
		m.nextSeq++
		m.order[record.ID] = m.nextSeq
	}
	m.records[record.ID] = record
	for m.max > 0 && len(m.records) > m.max {
		oldestID := ""
		var oldestSeq uint64
		for id := range m.records {
			seq := m.order[id]
			if oldestID == "" || seq < oldestSeq {
				oldestID = id
				oldestSeq = seq
			}
		}
		if oldestID == "" {
			break
		}
		delete(m.records, oldestID)
		delete(m.order, oldestID)
	}
}

// Get implements Store.
func (m *Memory) Get(id string) (*Record, bool) {
	m.mu.RLock()
	record, ok := m.records[id]
	m.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if m.ttl > 0 && m.clock.Now().Sub(record.CreatedAt) > m.ttl {
		m.Delete(id)
		return nil, false
	}
	return record, true
}

// Delete implements Store.
func (m *Memory) Delete(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.records[id]
	delete(m.records, id)
	delete(m.order, id)
	return ok
}

// Len implements Store.
func (m *Memory) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.records)
}

// Reset implements Store.
func (m *Memory) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = make(map[string]*Record)
	m.order = make(map[string]uint64)
	m.nextSeq = 0
}

// PurgeExpired removes expired records and returns the count removed.
func (m *Memory) PurgeExpired(now time.Time) int {
	if m.ttl <= 0 {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := 0
	for id, record := range m.records {
		if now.Sub(record.CreatedAt) > m.ttl {
			delete(m.records, id)
			delete(m.order, id)
			removed++
		}
	}
	return removed
}

// Close stops the background janitor and waits for it to exit.
func (m *Memory) Close() {
	m.once.Do(func() { close(m.stop) })
	<-m.done
}

func (m *Memory) janitor() {
	defer close(m.done)
	interval := max(m.ttl/2, time.Minute)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.PurgeExpired(m.clock.Now())
		case <-m.stop:
			return
		}
	}
}
