package openai

import (
	"context"
	"maps"
	"net/http"
	"time"

	"github.com/jahrulnr/fakeagents/internal/chunk"
	"github.com/jahrulnr/fakeagents/internal/engine"
	"github.com/jahrulnr/fakeagents/internal/sse"
)

func (h *Handler) streamResponses(w http.ResponseWriter, r *http.Request, req *engine.Request, plan *engine.Plan, response map[string]any, items []responseItem) {
	h.prepareHeaders(w, plan)
	if plan.Latency.Total > 0 {
		w.Header().Set("openai-processing-ms", formatMillis(plan.Latency.Total))
	}
	writer := sse.New(w)
	if err := h.clock.Sleep(r.Context(), plan.Latency.FirstToken); err != nil {
		return
	}

	streamer := &responsesStreamer{
		writer:    writer,
		ctx:       r.Context(),
		plan:      plan,
		response:  response,
		delay:     plan.Latency.Chunk,
		chunkSize: plan.Latency.ChunkSize,
		chunkUnit: plan.Latency.ChunkUnit,
	}

	progress := shallowCopy(response)
	progress["status"] = "in_progress"
	progress["output"] = []any{}
	delete(progress, "usage")
	if !streamer.emit("response.created", map[string]any{"response": progress}) {
		return
	}
	if !streamer.emit("response.in_progress", map[string]any{"response": progress}) {
		return
	}

	for index, item := range items {
		switch item.Type {
		case "reasoning":
			if !streamer.reasoning(index, item) {
				return
			}
		case "message":
			if !streamer.message(index, item) {
				return
			}
		case "function_call":
			if !streamer.functionCall(index, item) {
				return
			}
		}
	}

	final := shallowCopy(response)
	if plan.Stream.OmitUsage {
		delete(final, "usage")
	} else if r.URL.Path == "/api/v1/responses" {
		final["usage"] = h.openRouterResponsesUsage(plan.Model, plan.Usage)
	}
	name := "response.completed"
	if plan.Status == engine.StatusIncomplete {
		name = "response.incomplete"
	}
	if h.cfg.Defaults.Stream.SendDoneEnabled() && !plan.Stream.OmitDone {
		if !streamer.writeEvent(name, map[string]any{"response": final}) {
			return
		}
		if r.URL.Path == "/api/v1/responses" {
			_ = writer.RawData([]byte("[DONE]"))
		}
	}
}

type responsesStreamer struct {
	writer    *sse.Writer
	ctx       context.Context
	plan      *engine.Plan
	response  map[string]any
	delay     time.Duration
	chunkSize int
	chunkUnit string
	seq       int
	emitted   int
}

func (s *responsesStreamer) emit(name string, payload map[string]any) bool {
	if err := s.ctx.Err(); err != nil {
		return false
	}
	if limit := s.plan.Stream.AbortAfterChunks; limit > 0 && s.emitted >= limit {
		return false
	}
	if !s.writeEvent(name, payload) {
		return false
	}
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-s.ctx.Done():
			return false
		}
	}
	if s.plan.Stream.Error != nil && s.plan.Stream.ErrorAfterChunks > 0 && s.emitted >= s.plan.Stream.ErrorAfterChunks {
		s.failed()
		return false
	}
	return true
}

func (s *responsesStreamer) writeEvent(name string, payload map[string]any) bool {
	payload["type"] = name
	payload["sequence_number"] = s.seq
	s.seq++
	if err := s.writer.Event(name, payload); err != nil {
		return false
	}
	s.emitted++
	return true
}

func (s *responsesStreamer) failed() {
	apiErr := s.plan.Stream.Error
	if apiErr == nil {
		return
	}
	failed := shallowCopy(s.response)
	failed["status"] = "failed"
	failed["error"] = map[string]any{
		"code":    firstNonEmpty(apiErr.Code, apiErr.Type),
		"message": apiErr.Message,
	}
	s.writeEvent("response.failed", map[string]any{"response": failed})
}

func (s *responsesStreamer) reasoning(index int, item responseItem) bool {
	if !s.emit("response.output_item.added", map[string]any{
		"output_index": index,
		"item":         emptyResponseItem(item),
	}) {
		return false
	}
	part := map[string]any{"type": "summary_text", "text": ""}
	if !s.emit("response.reasoning_summary_part.added", map[string]any{
		"item_id":       item.ID,
		"output_index":  index,
		"summary_index": 0,
		"part":          part,
	}) {
		return false
	}
	for _, delta := range chunk.Split(item.Text, s.chunkSize, s.chunkUnit) {
		if !s.emit("response.reasoning_summary_text.delta", map[string]any{
			"item_id":       item.ID,
			"output_index":  index,
			"summary_index": 0,
			"delta":         delta,
		}) {
			return false
		}
	}
	if !s.emit("response.reasoning_summary_text.done", map[string]any{
		"item_id":       item.ID,
		"output_index":  index,
		"summary_index": 0,
		"text":          item.Text,
	}) {
		return false
	}
	if !s.emit("response.reasoning_summary_part.done", map[string]any{
		"item_id":       item.ID,
		"output_index":  index,
		"summary_index": 0,
		"part":          map[string]any{"type": "summary_text", "text": item.Text},
	}) {
		return false
	}
	return s.emit("response.output_item.done", map[string]any{
		"output_index": index,
		"item":         fullResponseItem(item),
	})
}

func (s *responsesStreamer) message(index int, item responseItem) bool {
	if !s.emit("response.output_item.added", map[string]any{
		"output_index": index,
		"item":         emptyResponseItem(item),
	}) {
		return false
	}
	partType := "output_text"
	if item.Refusal {
		partType = "refusal"
	}
	part := map[string]any{"type": partType}
	if item.Refusal {
		part["refusal"] = ""
	} else {
		part["text"] = ""
		part["annotations"] = []any{}
		part["logprobs"] = []any{}
	}
	if !s.emit("response.content_part.added", map[string]any{
		"item_id":       item.ID,
		"output_index":  index,
		"content_index": 0,
		"part":          part,
	}) {
		return false
	}
	for _, delta := range chunk.Split(item.Text, s.chunkSize, s.chunkUnit) {
		if item.Refusal {
			if !s.emit("response.refusal.delta", map[string]any{
				"item_id":       item.ID,
				"output_index":  index,
				"content_index": 0,
				"delta":         delta,
			}) {
				return false
			}
			continue
		}
		if !s.emit("response.output_text.delta", map[string]any{
			"item_id":       item.ID,
			"output_index":  index,
			"content_index": 0,
			"delta":         delta,
			"logprobs":      []any{},
		}) {
			return false
		}
	}
	if item.Refusal {
		if !s.emit("response.refusal.done", map[string]any{
			"item_id":       item.ID,
			"output_index":  index,
			"content_index": 0,
			"refusal":       item.Text,
		}) {
			return false
		}
	} else {
		if !s.emit("response.output_text.done", map[string]any{
			"item_id":       item.ID,
			"output_index":  index,
			"content_index": 0,
			"text":          item.Text,
			"logprobs":      []any{},
		}) {
			return false
		}
	}
	fullPart := part
	if item.Refusal {
		fullPart = map[string]any{"type": "refusal", "refusal": item.Text}
	} else {
		fullPart = map[string]any{"type": "output_text", "text": item.Text, "annotations": []any{}, "logprobs": []any{}}
	}
	if !s.emit("response.content_part.done", map[string]any{
		"item_id":       item.ID,
		"output_index":  index,
		"content_index": 0,
		"part":          fullPart,
	}) {
		return false
	}
	return s.emit("response.output_item.done", map[string]any{
		"output_index": index,
		"item":         fullResponseItem(item),
	})
}

func (s *responsesStreamer) functionCall(index int, item responseItem) bool {
	if !s.emit("response.output_item.added", map[string]any{
		"output_index": index,
		"item":         emptyResponseItem(item),
	}) {
		return false
	}
	for _, delta := range chunk.Split(item.Arguments, s.chunkSize, s.chunkUnit) {
		if !s.emit("response.function_call_arguments.delta", map[string]any{
			"item_id":      item.ID,
			"output_index": index,
			"delta":        delta,
		}) {
			return false
		}
	}
	if !s.emit("response.function_call_arguments.done", map[string]any{
		"item_id":      item.ID,
		"output_index": index,
		"arguments":    item.Arguments,
	}) {
		return false
	}
	return s.emit("response.output_item.done", map[string]any{
		"output_index": index,
		"item":         fullResponseItem(item),
	})
}

func shallowCopy(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	maps.Copy(out, input)
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
