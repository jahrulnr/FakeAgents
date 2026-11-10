package sse_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jahrulnr/fakeagents/internal/sse"
)

func TestWriterFrames(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := sse.New(recorder)
	if err := writer.Event("response.created", map[string]any{"type": "response.created"}); err != nil {
		t.Fatalf("event: %v", err)
	}
	if err := writer.Data(map[string]any{"ok": true}); err != nil {
		t.Fatalf("data: %v", err)
	}
	if err := writer.Comment("ping"); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if err := writer.RawData([]byte("[DONE]")); err != nil {
		t.Fatalf("raw: %v", err)
	}

	body := recorder.Body.String()
	if !strings.Contains(body, "event: response.created\n") {
		t.Fatalf("body = %q", body)
	}
	if !strings.Contains(body, `data: {"type":"response.created"}`) {
		t.Fatalf("body = %q", body)
	}
	if !strings.Contains(body, `data: {"ok":true}`) {
		t.Fatalf("body = %q", body)
	}
	if !strings.Contains(body, ": ping\n\n") {
		t.Fatalf("body = %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("body = %q", body)
	}
	if !strings.Contains(recorder.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type = %q", recorder.Header().Get("Content-Type"))
	}
	if writer.Count() != 4 {
		t.Fatalf("count = %d", writer.Count())
	}
}

func TestWriterStartOnlyOnce(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := sse.New(recorder)
	writer.Start()
	writer.Start()
	if recorder.Code != 200 {
		t.Fatalf("status = %d", recorder.Code)
	}
}
