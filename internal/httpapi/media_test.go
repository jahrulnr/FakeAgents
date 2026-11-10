package httpapi_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jahrulnr/fakeagents/internal/config"
)

func postMultipart(t *testing.T, s *testServer, endpoint string, fields map[string]string, fileField, fileName string, data []byte, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if fileField != "" {
		part, err := writer.CreateFormFile(fileField, fileName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest("POST", s.http.URL+endpoint, &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := s.http.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, raw
}

func TestImageMedia(t *testing.T) {
	s := newServer(t, nil)
	response, body := s.post(t, "/v1/images/generations", map[string]any{"model": "gpt-image-1", "prompt": "blue sky", "size": "32x48", "n": 2}, nil)
	if response.StatusCode != 200 {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	result := decodeMap(t, body)
	data := result["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("data = %v", data)
	}
	pngData, err := base64.StdEncoding.DecodeString(data[0].(map[string]any)["b64_json"].(string))
	if err != nil {
		t.Fatal(err)
	}
	img, format, err := image.Decode(bytes.NewReader(pngData))
	if err != nil || format != "png" || img.Bounds().Dx() != 32 || img.Bounds().Dy() != 48 {
		t.Fatalf("image = %v %s %v", img, format, err)
	}
	var input bytes.Buffer
	if err := png.Encode(&input, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	response, body = postMultipart(t, s, "/v1/images/edits", map[string]string{"model": "gpt-image-1", "prompt": "red", "size": "32x32"}, "image", "source.png", input.Bytes(), nil)
	if response.StatusCode != 200 {
		t.Fatalf("edit %d %s", response.StatusCode, body)
	}
	if len(decodeMap(t, body)["data"].([]any)) != 1 {
		t.Fatalf("edit %s", body)
	}
	response, body = postMultipart(t, s, "/v1/images/edits", map[string]string{"model": "gpt-image-1", "prompt": "red"}, "image", "bad.png", []byte("broken"), nil)
	if response.StatusCode != 400 {
		t.Fatalf("bad image %d %s", response.StatusCode, body)
	}
	response = s.postStream(t, "/v1/images/generations", map[string]any{"model": "gpt-image-1", "prompt": "sky", "size": "8x8", "stream": true, "partial_images": 1}, nil)
	events := readSSE(t, response)
	if len(events) != 2 || events[0].Name != "image_generation.partial_image" || events[1].Name != "image_generation.completed" {
		t.Fatalf("events = %v", events)
	}
	response, body = s.post(t, "/v1/images/generations", map[string]any{"model": "gpt-image-1", "prompt": "sky", "n": 0}, nil)
	if response.StatusCode != 400 {
		t.Fatalf("n=0: %d %s", response.StatusCode, body)
	}
}

func TestSpeechAndTranscription(t *testing.T) {
	s := newServer(t, func(cfg *config.Config) { cfg.Media.Transcription = "Halo, dunia." })
	for _, format := range []string{"mp3", "wav", "pcm", "aac", "opus", "flac"} {
		t.Run(format, func(t *testing.T) {
			response, body := s.post(t, "/v1/audio/speech", map[string]any{"model": "gpt-4o-mini-tts", "input": "hello", "voice": "alloy", "response_format": format}, nil)
			if response.StatusCode != 200 || len(body) < 100 {
				t.Fatalf("%s: %d length %d", format, response.StatusCode, len(body))
			}
			if format == "wav" && !bytes.HasPrefix(body, []byte("RIFF")) {
				t.Fatal("invalid wav fixture")
			}
		})
	}
	wav, err := os.ReadFile("../provider/openai/fixtures/speech.wav")
	if err != nil {
		t.Fatal(err)
	}
	response, body := postMultipart(t, s, "/v1/audio/transcriptions", map[string]string{"model": "gpt-transcribe"}, "file", "sample.wav", wav, nil)
	if response.StatusCode != 200 || decodeMap(t, body)["text"] != "Halo, dunia." {
		t.Fatalf("transcript %d %s", response.StatusCode, body)
	}
	response, body = postMultipart(t, s, "/v1/audio/transcriptions", map[string]string{"model": "gpt-transcribe", "stream": "true"}, "file", "sample.wav", wav, nil)
	if response.StatusCode != 200 || !strings.Contains(string(body), "transcript.text.done") {
		t.Fatalf("stream %d %s", response.StatusCode, body)
	}
	response, body = postMultipart(t, s, "/v1/audio/transcriptions", map[string]string{"model": "gpt-transcribe"}, "", "", nil, nil)
	if response.StatusCode != 400 {
		t.Fatalf("missing file %d %s", response.StatusCode, body)
	}
	response, body = postMultipart(t, s, "/v1/audio/transcriptions", map[string]string{"model": "gpt-transcribe"}, "file", "bad.wav", []byte("RIFF\x04\x00\x00\x00WAVE"), nil)
	if response.StatusCode != 400 {
		t.Fatalf("truncated WAV %d %s", response.StatusCode, body)
	}
	response, body = s.post(t, "/v1/audio/speech", map[string]any{"model": "tts-1", "input": "hi", "voice": "alloy", "response_format": "invalid"}, nil)
	if response.StatusCode != 400 {
		t.Fatalf("invalid format %d %s", response.StatusCode, body)
	}
}

func TestMediaScenarioErrors(t *testing.T) {
	s := newServer(t, nil)
	headers := map[string]string{"X-FakeAgents-Scenario": "billing"}
	response, body := s.post(t, "/v1/images/generations", map[string]any{"model": "gpt-image-1", "prompt": "sky"}, headers)
	if response.StatusCode != 429 {
		t.Fatalf("image billing %d %s", response.StatusCode, body)
	}
	response, body = s.post(t, "/v1/audio/speech", map[string]any{"model": "tts-1", "input": "hello", "voice": "alloy"}, headers)
	if response.StatusCode != 429 {
		t.Fatalf("speech billing %d %s", response.StatusCode, body)
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	wav, err := os.ReadFile("../provider/openai/fixtures/speech.wav")
	if err != nil {
		t.Fatal(err)
	}
	response, body = postMultipart(t, s, "/v1/audio/transcriptions", map[string]string{"model": "gpt-transcribe"}, "file", "sample.wav", wav, headers)
	if response.StatusCode != 429 {
		t.Fatalf("transcription billing %d %s", response.StatusCode, body)
	}
}

func TestMediaMultipartBodyLimit(t *testing.T) {
	s := newServer(t, func(cfg *config.Config) { cfg.Server.MaxBodyBytes = 100 })
	response, body := postMultipart(t, s, "/v1/audio/transcriptions", map[string]string{"model": "gpt-transcribe"}, "file", "sample.wav", bytes.Repeat([]byte{1}, 1024), nil)
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized multipart = %d: %s", response.StatusCode, body)
	}
}

func TestConfigurableMediaFixtures(t *testing.T) {
	dir := t.TempDir()
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.png"), picture.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.mp3"), []byte("fake-mp3-fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newServer(t, func(cfg *config.Config) {
		cfg.Dir = dir
		cfg.Media.ImageFile = "fixture.png"
		cfg.Media.SpeechFiles = map[string]string{"mp3": "fixture.mp3"}
	})
	response, body := s.post(t, "/v1/images/generations", map[string]any{"model": "gpt-image-1", "prompt": "hello", "size": "4x5"}, nil)
	if response.StatusCode != 200 {
		t.Fatalf("image %d %s", response.StatusCode, body)
	}
	encoded := decodeMap(t, body)["data"].([]any)[0].(map[string]any)["b64_json"].(string)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil || img.Bounds().Dx() != 4 || img.Bounds().Dy() != 5 {
		t.Fatalf("fixture image = %v, error = %v", img, err)
	}
	response, body = s.post(t, "/v1/audio/speech", map[string]any{"model": "tts-1", "input": "hello", "voice": "alloy"}, nil)
	if response.StatusCode != 200 || string(body) != "fake-mp3-fixture" {
		t.Fatalf("speech %d %q", response.StatusCode, body)
	}
}

func TestMediaStreamingFaults(t *testing.T) {
	s := newServer(t, nil)
	response := s.postStream(t, "/v1/images/generations", map[string]any{
		"model": "gpt-image-1", "prompt": "hello", "size": "8x8", "stream": true, "partial_images": 3,
	}, map[string]string{"X-FakeAgents-Scenario": "mid-stream-error"})
	events := readSSE(t, response)
	if len(events) != 4 || events[3].Name != "error" {
		t.Fatalf("stream events = %v", events)
	}
	for _, event := range events {
		if event.Name == "image_generation.completed" {
			t.Fatalf("unexpected completion after error: %v", events)
		}
	}
}

func TestMultimodalInputs(t *testing.T) {
	s := newServer(t, nil)
	const imageData = "data:image/png;base64,AA=="

	response, body := s.post(t, "/v1/chat/completions", map[string]any{
		"model": "fake-gpt-4o",
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": "what is this?"},
				{"type": "image_url", "image_url": map[string]any{"url": imageData}},
			},
		}},
	}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d: %s", response.StatusCode, body)
	}
	chat := decodeMap(t, body)
	if !strings.Contains(chat["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string), "what is this?") {
		t.Fatalf("chat body = %s", body)
	}

	response, body = s.post(t, "/v1/responses", map[string]any{
		"model": "fake-gpt-5",
		"input": []map[string]any{{
			"type": "message",
			"role": "user",
			"content": []map[string]any{
				{"type": "input_text", "text": "describe this"},
				{"type": "input_image", "image_url": imageData},
			},
		}},
	}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("responses status = %d: %s", response.StatusCode, body)
	}
	if text := responsesText(decodeMap(t, body)); !strings.Contains(text, "describe this") {
		t.Fatalf("responses text = %q", text)
	}

	response, body = s.post(t, "/v1/messages", map[string]any{
		"model":      "fake-claude-sonnet",
		"max_tokens": 256,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": "read this"},
				{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AA=="}},
			},
		}},
	}, anthropicHeaders())
	if response.StatusCode != http.StatusOK {
		t.Fatalf("messages status = %d: %s", response.StatusCode, body)
	}
	messages := decodeMap(t, body)
	var text strings.Builder
	for _, raw := range messages["content"].([]any) {
		block := raw.(map[string]any)
		if block["type"] == "text" {
			text.WriteString(block["text"].(string))
		}
	}
	if !strings.Contains(text.String(), "read this") {
		t.Fatalf("messages text = %q", text.String())
	}
}
