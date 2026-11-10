package openai

import (
	"bytes"
	"embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/httpx"
	"github.com/jahrulnr/fakeagents/internal/scenario"
	"github.com/jahrulnr/fakeagents/internal/sse"
)

//go:embed fixtures/speech.*
var speechFixtures embed.FS

type speechRequest struct {
	Model          string          `json:"model"`
	Input          string          `json:"input"`
	Voice          json.RawMessage `json:"voice"`
	ResponseFormat string          `json:"response_format"`
	StreamFormat   string          `json:"stream_format"`
	Speed          *float64        `json:"speed"`
}

func (h *Handler) Speech(w http.ResponseWriter, r *http.Request) {
	body, apiErr := httpx.ReadBody(w, r, h.cfg.Server.MaxBodyBytes)
	if apiErr != nil {
		h.fail(w, r, h.options(r), apiErr)
		return
	}
	var req speechRequest
	if apiErr := httpx.DecodeBytes(body, &req); apiErr != nil {
		h.fail(w, r, h.options(r), apiErr)
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		h.mediaError(w, r, "model", "model is required.")
		return
	}
	if strings.TrimSpace(req.Input) == "" || utf8.RuneCountInString(req.Input) > 4096 {
		h.mediaError(w, r, "input", "input must contain 1 to 4096 characters.")
		return
	}
	var voiceName string
	var voiceObject struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(req.Voice, &voiceName) != nil {
		_ = json.Unmarshal(req.Voice, &voiceObject)
		voiceName = voiceObject.ID
	}
	if strings.TrimSpace(voiceName) == "" {
		h.mediaError(w, r, "voice", "voice is required.")
		return
	}
	if req.Speed != nil && (*req.Speed < 0.25 || *req.Speed > 4) {
		h.mediaError(w, r, "speed", "speed must be between 0.25 and 4.")
		return
	}
	if req.StreamFormat != "" && req.StreamFormat != "audio" {
		h.mediaError(w, r, "stream_format", "Only audio streaming is supported.")
		return
	}
	if req.ResponseFormat == "" {
		req.ResponseFormat = "mp3"
	}
	contentTypes := map[string]string{"mp3": "audio/mpeg", "opus": "audio/ogg", "aac": "audio/aac", "flac": "audio/flac", "wav": "audio/wav", "pcm": "audio/pcm"}
	contentType, ok := contentTypes[req.ResponseFormat]
	if !ok {
		h.mediaError(w, r, "response_format", "Unsupported audio format.")
		return
	}
	plan := h.mediaPlan(w, r, scenario.EndpointSpeech, req.Model, req.Input, false, httpx.DecodeMap(body))
	if plan == nil {
		return
	}
	if plan.RawJSON != nil {
		h.writeRaw(w, plan)
		return
	}
	var data []byte
	var err error
	if path := h.cfg.Media.SpeechFiles[req.ResponseFormat]; path != "" {
		if !filepath.IsAbs(path) {
			path = filepath.Join(h.cfg.Dir, path)
		}
		data, err = os.ReadFile(path)
	} else {
		data, err = speechFixtures.ReadFile("fixtures/speech." + req.ResponseFormat)
	}
	if err != nil || len(data) == 0 {
		h.fixtureError(w, r, "Unable to read speech fixture.")
		return
	}
	if h.clock.Sleep(r.Context(), plan.Latency.FirstToken) != nil {
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handler) Transcription(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		h.mediaError(w, r, "file", "Expected multipart/form-data.")
		return
	}
	if h.cfg.Server.MaxBodyBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, h.cfg.Server.MaxBodyBytes)
	}
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		h.multipartError(w, r, "file", err)
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, header, err := r.FormFile("file")
	if err != nil {
		h.mediaError(w, r, "file", "file is required.")
		return
	}
	data, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || len(data) == 0 {
		h.mediaError(w, r, "file", "The audio file is empty or unreadable.")
		return
	}
	if len(data) > 25*1024*1024 {
		h.fail(w, r, h.options(r), apierr.MustPreset(apierr.ProviderOpenAI, "request_too_large"))
		return
	}
	allowed := map[string]bool{".wav": true, ".mp3": true, ".mp4": true, ".mpeg": true, ".mpga": true, ".m4a": true, ".ogg": true, ".flac": true, ".webm": true}
	if !allowed[strings.ToLower(filepath.Ext(header.Filename))] {
		h.mediaError(w, r, "file", "Unsupported audio file extension.")
		return
	}
	if !recognizeAudio(data) {
		h.mediaError(w, r, "file", "Invalid or unsupported audio data.")
		return
	}
	model := r.FormValue("model")
	if model == "" {
		h.mediaError(w, r, "model", "model is required.")
		return
	}
	format := r.FormValue("response_format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "text" && format != "srt" && format != "vtt" && format != "verbose_json" {
		h.mediaError(w, r, "response_format", "Unsupported transcription format.")
		return
	}
	stream := false
	if value := r.FormValue("stream"); value != "" {
		switch value {
		case "true":
			stream = true
		case "false":
		default:
			h.mediaError(w, r, "stream", "stream must be true or false.")
			return
		}
	}
	if stream && format != "json" {
		h.mediaError(w, r, "stream", "Streaming requires json format.")
		return
	}
	raw := map[string]any{}
	for key := range r.MultipartForm.Value {
		raw[key] = r.FormValue(key)
	}
	prompt := r.FormValue("prompt")
	plan := h.mediaPlan(w, r, scenario.EndpointTranscription, model, prompt, stream, raw)
	if plan == nil {
		return
	}
	if plan.RawJSON != nil {
		h.writeRaw(w, plan)
		return
	}
	text := h.cfg.Media.Transcription
	if text == "" {
		text = "This is a simulated transcription."
	}
	if plan.Meta.Scenario != "" && plan.Meta.Scenario != "default" {
		text = plan.Text
	}
	if stream {
		if h.clock.Sleep(r.Context(), plan.Latency.FirstToken) != nil {
			return
		}
		writer := sse.New(w)
		writer.Start()
		if !h.mediaEvent(r, writer, plan, "transcript.text.delta", map[string]any{"type": "transcript.text.delta", "delta": text}) {
			return
		}
		if plan.Stream.OmitDone || !h.cfg.Defaults.Stream.SendDoneEnabled() {
			return
		}
		h.mediaEvent(r, writer, plan, "transcript.text.done", map[string]any{"type": "transcript.text.done", "text": text})
		return
	}
	if h.clock.Sleep(r.Context(), plan.Latency.Total) != nil {
		return
	}
	switch format {
	case "json":
		_ = httpx.WriteJSON(w, http.StatusOK, map[string]any{"text": text})
	case "verbose_json":
		_ = httpx.WriteJSON(w, http.StatusOK, map[string]any{"task": "transcribe", "language": r.FormValue("language"), "duration": 1.0, "text": text, "segments": []any{}})
	case "text":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, text)
	case "srt":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(w, "1\n00:00:00,000 --> 00:00:01,000\n%s\n", text)
	case "vtt":
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
		_, _ = fmt.Fprintf(w, "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\n%s\n", text)
	}
}

func recognizeAudio(data []byte) bool {
	if len(data) < 12 {
		return false
	}
	if bytes.HasPrefix(data, []byte("RIFF")) && string(data[8:12]) == "WAVE" {
		return validWAV(data)
	}
	return bytes.HasPrefix(data, []byte("fLaC")) ||
		bytes.HasPrefix(data, []byte("OggS")) ||
		bytes.HasPrefix(data, []byte("ID3")) ||
		bytes.HasPrefix(data, []byte{0x1a, 0x45, 0xdf, 0xa3}) ||
		string(data[4:8]) == "ftyp" ||
		data[0] == 0xff && data[1]&0xe0 == 0xe0
}

func validWAV(data []byte) bool {
	if uint64(binary.LittleEndian.Uint32(data[4:8]))+8 > uint64(len(data)) {
		return false
	}
	var format, samples bool
	for pos := 12; pos+8 <= len(data); {
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		end := pos + 8 + size
		if end > len(data) {
			return false
		}
		switch string(data[pos : pos+4]) {
		case "fmt ":
			format = size >= 16
		case "data":
			samples = size > 0
		}
		pos = end + size%2
	}
	return format && samples
}
