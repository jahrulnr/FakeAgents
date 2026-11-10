// Package sse implements a small Server-Sent Events writer with explicit
// flushing and optional keep-alive comments.
package sse

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// Writer writes SSE frames. It is safe for use from a single response
// goroutine; the mutex protects against accidental concurrent writes from
// callback-heavy handlers.
type Writer struct {
	mu        sync.Mutex
	w         http.ResponseWriter
	flusher   http.Flusher
	started   bool
	eventsOut int
}

// New wraps w. The caller is responsible for writing status codes before the
// first frame; New attempts to assert http.Flusher.
func New(w http.ResponseWriter) *Writer {
	flusher, _ := w.(http.Flusher)
	return &Writer{w: w, flusher: flusher}
}

// Start writes the SSE response headers and a 200 status if they have not been
// written yet.
func (s *Writer) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startLocked()
}

func (s *Writer) startLocked() {
	if s.started {
		return
	}
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.started = true
	s.flushLocked()
}

// Event marshals payload and writes a named SSE event.
func (s *Writer) Event(name string, payload any) error {
	data, err := marshal(payload)
	if err != nil {
		return err
	}
	return s.EventRaw(name, data)
}

// EventRaw writes a named SSE event with pre-encoded JSON data.
func (s *Writer) EventRaw(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startLocked()
	if name != "" {
		if _, err := fmt.Fprintf(s.w, "event: %s\n", name); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", data); err != nil {
		return err
	}
	s.eventsOut++
	s.flushLocked()
	return nil
}

// Data writes an unnamed data frame (OpenAI Chat Completions style).
func (s *Writer) Data(payload any) error {
	data, err := marshal(payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startLocked()
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", data); err != nil {
		return err
	}
	s.eventsOut++
	s.flushLocked()
	return nil
}

// RawData writes raw payload bytes as one data frame.
func (s *Writer) RawData(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startLocked()
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", data); err != nil {
		return err
	}
	s.eventsOut++
	s.flushLocked()
	return nil
}

// Comment writes an SSE comment, typically used as a heartbeat.
func (s *Writer) Comment(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startLocked()
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return err
	}
	s.eventsOut++
	s.flushLocked()
	return nil
}

// Flush pushes buffered bytes to the client.
func (s *Writer) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
}

func (s *Writer) flushLocked() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// Count returns the number of frames written.
func (s *Writer) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eventsOut
}

func marshal(v any) ([]byte, error) {
	var b []byte
	var err error
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	if raw, ok := v.([]byte); ok {
		return raw, nil
	}
	b, err = json.Marshal(v)
	return b, err
}
