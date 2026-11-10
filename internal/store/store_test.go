package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jahrulnr/fakeagents/internal/clock"
	"github.com/jahrulnr/fakeagents/internal/store"
)

func TestPutGetDeleteAndEviction(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	memory := store.NewMemory(clk, time.Hour, 2)
	defer memory.Close()

	for _, id := range []string{"a", "b", "c"} {
		memory.Put(&store.Record{ID: id, Response: json.RawMessage(`{"id":"` + id + `"}`)})
	}
	if memory.Len() != 2 {
		t.Fatalf("len = %d", memory.Len())
	}
	if _, ok := memory.Get("a"); ok {
		t.Fatal("oldest record should be evicted")
	}
	if record, ok := memory.Get("c"); !ok || record.ID != "c" {
		t.Fatalf("record c missing: %v", record)
	}
	if !memory.Delete("c") || memory.Delete("c") {
		t.Fatal("delete semantics are wrong")
	}
}

func TestTTLExpiry(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	memory := store.NewMemory(clk, time.Hour, 0)
	defer memory.Close()
	memory.Put(&store.Record{ID: "x"})
	if _, ok := memory.Get("x"); !ok {
		t.Fatal("record missing before expiry")
	}
	clk.Advance(2 * time.Hour)
	if _, ok := memory.Get("x"); ok {
		t.Fatal("record should have expired")
	}
}

func TestPurgeExpired(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	memory := store.NewMemory(clk, time.Hour, 0)
	defer memory.Close()
	memory.Put(&store.Record{ID: "x"})
	memory.Put(&store.Record{ID: "y"})
	clk.Advance(2 * time.Hour)
	if removed := memory.PurgeExpired(clk.Now()); removed != 2 {
		t.Fatalf("removed = %d", removed)
	}
	if memory.Len() != 0 {
		t.Fatal("records remain")
	}
}
