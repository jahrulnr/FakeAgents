package openai

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jahrulnr/fakeagents/internal/httpx"
	"github.com/jahrulnr/fakeagents/internal/scenario"
	"github.com/jahrulnr/fakeagents/internal/sse"
)

type imageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              *int   `json:"n"`
	Size           string `json:"size"`
	Quality        string `json:"quality"`
	Background     string `json:"background"`
	OutputFormat   string `json:"output_format"`
	ResponseFormat string `json:"response_format"`
	Stream         bool   `json:"stream"`
	PartialImages  int    `json:"partial_images"`
}

func (h *Handler) ImageGeneration(w http.ResponseWriter, r *http.Request) {
	body, apiErr := httpx.ReadBody(w, r, h.cfg.Server.MaxBodyBytes)
	if apiErr != nil {
		h.fail(w, r, h.options(r), apiErr)
		return
	}
	var req imageRequest
	if apiErr := httpx.DecodeBytes(body, &req); apiErr != nil {
		h.fail(w, r, h.options(r), apiErr)
		return
	}
	h.renderImage(w, r, req, httpx.DecodeMap(body), scenario.EndpointImageGeneration)
}

func (h *Handler) ImageEdit(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		h.mediaError(w, r, "image", "Expected multipart/form-data with an image.")
		return
	}
	if h.cfg.Server.MaxBodyBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, h.cfg.Server.MaxBodyBytes)
	}
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		h.multipartError(w, r, "image", err)
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := append(r.MultipartForm.File["image"], r.MultipartForm.File["image[]"]...)
	if len(files) == 0 || len(files) > 16 {
		h.mediaError(w, r, "image", "Provide between 1 and 16 images.")
		return
	}
	var source image.Config
	for _, file := range files {
		reader, err := file.Open()
		if err != nil {
			h.mediaError(w, r, "image", "Unable to read image.")
			return
		}
		info, _, err := image.DecodeConfig(reader)
		_ = reader.Close()
		if err != nil {
			h.mediaError(w, r, "image", "Invalid or unsupported image file.")
			return
		}
		if source.Width == 0 {
			source = info
		}
	}
	masks := r.MultipartForm.File["mask"]
	if len(masks) > 1 {
		h.mediaError(w, r, "mask", "Provide at most one mask.")
		return
	}
	if len(masks) == 1 {
		reader, err := masks[0].Open()
		if err != nil {
			h.mediaError(w, r, "mask", "Unable to read mask.")
			return
		}
		info, format, err := image.DecodeConfig(reader)
		_ = reader.Close()
		if err != nil || format != "png" || info.Width != source.Width || info.Height != source.Height {
			h.mediaError(w, r, "mask", "Mask must be a PNG with the source image dimensions.")
			return
		}
	}
	values := r.MultipartForm.Value
	raw := make(map[string]any, len(values))
	for key := range values {
		raw[key] = r.FormValue(key)
	}
	req := imageRequest{
		Model: r.FormValue("model"), Prompt: r.FormValue("prompt"), Size: r.FormValue("size"),
		Quality: r.FormValue("quality"), Background: r.FormValue("background"),
		OutputFormat: r.FormValue("output_format"), ResponseFormat: r.FormValue("response_format"),
	}
	if n := r.FormValue("n"); n != "" {
		value, err := strconv.Atoi(n)
		if err != nil {
			h.mediaError(w, r, "n", "n must be an integer.")
			return
		}
		req.N = &value
	}
	if value := r.FormValue("stream"); value != "" {
		req.Stream, err = strconv.ParseBool(value)
		if err != nil {
			h.mediaError(w, r, "stream", "stream must be a boolean.")
			return
		}
	}
	if value := r.FormValue("partial_images"); value != "" {
		req.PartialImages, err = strconv.Atoi(value)
		if err != nil {
			h.mediaError(w, r, "partial_images", "partial_images must be an integer.")
			return
		}
	}
	h.renderImage(w, r, req, raw, scenario.EndpointImageEdit)
}

func (h *Handler) renderImage(w http.ResponseWriter, r *http.Request, req imageRequest, raw map[string]any, endpoint string) {
	if strings.TrimSpace(req.Prompt) == "" {
		h.mediaError(w, r, "prompt", "prompt is required.")
		return
	}
	if req.Model == "" {
		req.Model = "gpt-image-1"
	}
	count := 1
	if req.N != nil {
		count = *req.N
	}
	if count < 1 || count > 10 {
		h.mediaError(w, r, "n", "n must be between 1 and 10.")
		return
	}
	if req.PartialImages < 0 || req.PartialImages > 3 {
		h.mediaError(w, r, "partial_images", "partial_images must be between 0 and 3.")
		return
	}
	if req.Stream && count != 1 {
		h.mediaError(w, r, "n", "Streaming supports n=1.")
		return
	}
	if req.Size == "" || req.Size == "auto" {
		req.Size = "1024x1024"
	}
	var width, height int
	if _, err := fmt.Sscanf(req.Size, "%dx%d", &width, &height); err != nil ||
		fmt.Sprintf("%dx%d", width, height) != req.Size || width < 1 || height < 1 || width > 2048 || height > 2048 {
		h.mediaError(w, r, "size", "size must be WIDTHxHEIGHT within 1..2048.")
		return
	}
	if req.OutputFormat == "" {
		req.OutputFormat = "png"
	}
	if req.OutputFormat != "png" && req.OutputFormat != "jpeg" {
		h.mediaError(w, r, "output_format", "Supported output formats are png and jpeg.")
		return
	}
	if req.ResponseFormat != "" && req.ResponseFormat != "b64_json" {
		h.mediaError(w, r, "response_format", "Only b64_json is supported.")
		return
	}
	if req.Background == "" || req.Background == "auto" {
		req.Background = "opaque"
	}
	if req.Background != "opaque" && req.Background != "transparent" {
		h.mediaError(w, r, "background", "Invalid background.")
		return
	}
	if req.Background == "transparent" && req.OutputFormat == "jpeg" {
		h.mediaError(w, r, "background", "Transparent JPEG is not supported.")
		return
	}
	if req.Quality == "" || req.Quality == "auto" {
		req.Quality = "medium"
	}
	if req.Quality != "low" && req.Quality != "medium" && req.Quality != "high" {
		h.mediaError(w, r, "quality", "Invalid quality.")
		return
	}

	plan := h.mediaPlan(w, r, endpoint, req.Model, req.Prompt, req.Stream, raw)
	if plan == nil {
		return
	}
	if plan.RawJSON != nil {
		h.writeRaw(w, plan)
		return
	}
	pixels := image.NewNRGBA(image.Rect(0, 0, width, height))
	if path := h.cfg.Media.ImageFile; path != "" {
		if !filepath.IsAbs(path) {
			path = filepath.Join(h.cfg.Dir, path)
		}
		file, err := os.Open(path)
		if err != nil {
			h.fixtureError(w, r, "Unable to read image fixture.")
			return
		}
		fixture, _, err := image.Decode(file)
		_ = file.Close()
		if err != nil || fixture.Bounds().Empty() {
			h.fixtureError(w, r, "Unable to decode image fixture.")
			return
		}
		bounds := fixture.Bounds()
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				pixels.Set(x, y, fixture.At(bounds.Min.X+x*bounds.Dx()/width, bounds.Min.Y+y*bounds.Dy()/height))
			}
		}
	} else {
		hash := sha256.Sum256([]byte(req.Prompt))
		fill := color.NRGBA{R: hash[0], G: hash[1], B: hash[2], A: 255}
		if req.Background == "transparent" {
			fill.A = 0
		}
		for i := 0; i < len(pixels.Pix); i += 4 {
			pixels.Pix[i] = fill.R
			pixels.Pix[i+1] = fill.G
			pixels.Pix[i+2] = fill.B
			pixels.Pix[i+3] = fill.A
		}
	}
	var buffer bytes.Buffer
	var err error
	if req.OutputFormat == "png" {
		err = png.Encode(&buffer, pixels)
	} else {
		err = jpeg.Encode(&buffer, pixels, nil)
	}
	if err != nil {
		h.mediaError(w, r, "output_format", "Image encoding failed.")
		return
	}
	encoded := base64.StdEncoding.EncodeToString(buffer.Bytes())
	data := make([]map[string]any, count)
	for i := range data {
		data[i] = map[string]any{"b64_json": encoded}
	}
	usage := map[string]any{"input_tokens": plan.Usage.InputTokens, "output_tokens": plan.Usage.OutputTokens, "total_tokens": plan.Usage.Total(), "input_tokens_details": map[string]int{"text_tokens": plan.Usage.InputTokens, "image_tokens": 0}}
	if !req.Stream {
		if h.clock.Sleep(r.Context(), plan.Latency.Total) != nil {
			return
		}
		_ = httpx.WriteJSON(w, http.StatusOK, map[string]any{"created": plan.CreatedAt.Unix(), "data": data, "size": req.Size, "quality": req.Quality, "background": req.Background, "output_format": req.OutputFormat, "usage": usage})
		return
	}
	if h.clock.Sleep(r.Context(), plan.Latency.FirstToken) != nil {
		return
	}
	name := "image_generation"
	if endpoint == scenario.EndpointImageEdit {
		name = "image_edit"
	}
	writer := sse.New(w)
	writer.Start()
	for i := 0; i < req.PartialImages; i++ {
		event := map[string]any{"type": name + ".partial_image", "b64_json": encoded, "partial_image_index": i, "created_at": plan.CreatedAt.Unix(), "size": req.Size, "quality": req.Quality, "background": req.Background, "output_format": req.OutputFormat}
		if !h.mediaEvent(r, writer, plan, name+".partial_image", event) {
			return
		}
	}
	if plan.Stream.OmitDone || !h.cfg.Defaults.Stream.SendDoneEnabled() {
		return
	}
	event := map[string]any{"type": name + ".completed", "b64_json": encoded, "created_at": plan.CreatedAt.Unix(), "size": req.Size, "quality": req.Quality, "background": req.Background, "output_format": req.OutputFormat, "usage": usage}
	h.mediaEvent(r, writer, plan, name+".completed", event)
}
