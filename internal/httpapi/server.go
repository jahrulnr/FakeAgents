// Package httpapi wires provider handlers into a production HTTP server with
// authentication, CORS, required headers, rate limiting, and observability.
package httpapi

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/clock"
	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/engine"
	"github.com/jahrulnr/fakeagents/internal/httpx"
	"github.com/jahrulnr/fakeagents/internal/id"
	"github.com/jahrulnr/fakeagents/internal/observe"
	"github.com/jahrulnr/fakeagents/internal/provider/anthropic"
	"github.com/jahrulnr/fakeagents/internal/provider/openai"
	"github.com/jahrulnr/fakeagents/internal/ratelimit"
	"github.com/jahrulnr/fakeagents/internal/scenario"
	"github.com/jahrulnr/fakeagents/internal/session"
	"github.com/jahrulnr/fakeagents/internal/store"
	"github.com/jahrulnr/fakeagents/internal/wire"
)

// BuildInfo is printed by the version endpoint.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
}

// Deps are the server dependencies.
type Deps struct {
	Config    *config.Config
	Engine    *engine.Engine
	OpenAI    *openai.Handler
	Anthropic *anthropic.Handler
	Sessions  session.Tracker
	Limiter   *ratelimit.Limiter
	Responses store.Store
	Recorder  *observe.Recorder
	Scenarios *scenario.Set
	Logger    *slog.Logger
	Clock     clock.Clock
	IDs       id.Generator
	Version   BuildInfo
}

// Server owns the HTTP listener and middleware chain.
type Server struct {
	cfg       *config.Config
	logger    *slog.Logger
	engine    *engine.Engine
	openai    *openai.Handler
	anthropic *anthropic.Handler
	sessions  session.Tracker
	limiter   *ratelimit.Limiter
	responses store.Store
	recorder  *observe.Recorder
	scenarios *scenario.Set
	clock     clock.Clock
	ids       id.Generator
	version   BuildInfo
	handler   http.Handler
	http      *http.Server
	started   time.Time

	activeMu sync.Mutex
	active   map[string]*observe.Trace
}

// New builds a server and its route table.
func New(deps Deps) *Server {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clk := deps.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	ids := deps.IDs
	if ids == nil {
		ids = id.New()
	}
	server := &Server{
		cfg:       deps.Config,
		logger:    logger,
		engine:    deps.Engine,
		openai:    deps.OpenAI,
		anthropic: deps.Anthropic,
		sessions:  deps.Sessions,
		limiter:   deps.Limiter,
		responses: deps.Responses,
		recorder:  deps.Recorder,
		scenarios: deps.Scenarios,
		clock:     clk,
		ids:       ids,
		version:   deps.Version,
		started:   clk.Now(),
		active:    make(map[string]*observe.Trace),
	}
	server.handler = server.buildHandler()
	server.http = &http.Server{
		Addr:              server.addr(),
		Handler:           server.handler,
		ReadHeaderTimeout: deps.Config.Server.ReadHeaderTimeout.D(),
		ReadTimeout:       deps.Config.Server.ReadTimeout.D(),
		WriteTimeout:      deps.Config.Server.WriteTimeout.D(),
		IdleTimeout:       deps.Config.Server.IdleTimeout.D(),
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}
	return server
}

// Handler exposes the middleware chain for tests.
func (s *Server) Handler() http.Handler { return s.handler }

// Addr returns the configured listen address.
func (s *Server) Addr() string { return s.addr() }

func (s *Server) addr() string {
	host := s.cfg.Server.Host
	port := s.cfg.Server.Port
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// Run listens and serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.addr())
	if err != nil {
		return err
	}
	scheme := "http"
	if s.cfg.Server.TLS.Enabled() {
		scheme = "https"
	}
	base := s.cfg.Server.BasePath
	if base == "/" {
		base = ""
	}
	s.logger.Info("fakeagents listening",
		"url", scheme+"://"+listener.Addr().String()+base,
		"openai_base_url", scheme+"://"+listener.Addr().String()+base+"/v1",
		"anthropic_base_url", scheme+"://"+listener.Addr().String()+base,
		"tls", s.cfg.Server.TLS.Enabled(),
	)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		timeout := s.cfg.Server.ShutdownTimeout.D()
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		s.logger.Info("shutting down")
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			s.logger.Warn("graceful shutdown failed", "error", err)
			_ = s.http.Close()
		}
	}()

	if s.cfg.Server.TLS.Enabled() {
		s.http.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		err = s.http.ServeTLS(listener, s.cfg.Server.TLS.CertFile, s.cfg.Server.TLS.KeyFile)
	} else {
		err = s.http.Serve(listener)
	}
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-shutdownDone
	return nil
}

func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /_fakeagents/health", s.health)
	mux.HandleFunc("GET /_fakeagents/version", s.versionHandler)
	mux.HandleFunc("GET /_fakeagents/scenarios", s.scenariosHandler)
	mux.HandleFunc("GET /_fakeagents/config", s.configHandler)
	mux.HandleFunc("GET /_fakeagents/requests", s.requestsHandler)
	mux.HandleFunc("GET /_fakeagents/sessions", s.sessionsHandler)
	mux.HandleFunc("GET /_fakeagents/active", s.activeHandler)
	mux.HandleFunc("POST /_fakeagents/reset", s.resetHandler)

	mux.HandleFunc("POST /v1/audio/speech", s.openai.Speech)
	mux.HandleFunc("POST /v1/audio/transcriptions", s.openai.Transcription)
	mux.HandleFunc("POST /v1/images/generations", s.openai.ImageGeneration)
	mux.HandleFunc("POST /v1/images/edits", s.openai.ImageEdit)
	mux.HandleFunc("POST /v1/chat/completions", s.openai.ChatCompletions)
	mux.HandleFunc("POST /v1/responses", s.openai.Responses)
	mux.HandleFunc("GET /v1/responses/{id}", s.openai.Retrieve)
	mux.HandleFunc("DELETE /v1/responses/{id}", s.openai.Delete)
	mux.HandleFunc("POST /v1/messages", s.anthropic.Messages)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.anthropic.CountTokens)
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("GET /v1/models/{id}", s.model)
	mux.HandleFunc("GET /api.json", s.modelsDev)
	mux.HandleFunc("GET /api/v1/models", s.openRouterModels)
	mux.HandleFunc("GET /api/v1/model/{id...}", s.openRouterModel)
	mux.HandleFunc("POST /api/v1/chat/completions", s.openai.ChatCompletions)
	mux.HandleFunc("POST /api/v1/responses", s.openai.Responses)
	mux.HandleFunc("/", s.notFound)

	var handler http.Handler = mux
	handler = s.rateLimitMiddleware(handler)
	handler = s.requiredHeadersMiddleware(handler)
	handler = s.authMiddleware(handler)
	handler = s.recoveryMiddleware(handler)
	handler = s.corsMiddleware(handler)
	handler = s.accessLogMiddleware(handler)
	handler = s.basePathMiddleware(handler)
	handler = s.requestIDMiddleware(handler)
	return handler
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if wire.DetectProvider(r) == apierr.ProviderAnthropic {
		s.anthropic.Models(w, r)
		return
	}
	s.openai.Models(w, r)
}

func (s *Server) model(w http.ResponseWriter, r *http.Request) {
	if wire.DetectProvider(r) == apierr.ProviderAnthropic {
		s.anthropic.Model(w, r)
		return
	}
	s.openai.Model(w, r)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	provider := wire.DetectProvider(r)
	apiErr := apierr.New(http.StatusNotFound, "invalid_request_error", "not_found",
		"Unknown request URL: "+r.Method+" "+r.URL.Path)
	wire.WriteError(w, s.wireOptionsFor(r, provider), apiErr)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        s.version.Version,
		"uptime_seconds": int64(time.Since(s.started).Seconds()),
	})
}

func (s *Server) versionHandler(w http.ResponseWriter, r *http.Request) {
	if !s.debugEnabled(w, r) {
		return
	}
	_ = httpx.WriteJSON(w, http.StatusOK, s.version)
}

func (s *Server) scenariosHandler(w http.ResponseWriter, r *http.Request) {
	if !s.debugEnabled(w, r) {
		return
	}
	type entry struct {
		Name        string   `json:"name"`
		Description string   `json:"description,omitempty"`
		Type        string   `json:"type"`
		Priority    int      `json:"priority"`
		Enabled     bool     `json:"enabled"`
		Triggers    []string `json:"triggers,omitempty"`
	}
	var data []entry
	if s.scenarios == nil {
		_ = httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
		return
	}
	for _, compiled := range s.scenarios.All() {
		data = append(data, entry{
			Name:        compiled.Name(),
			Description: compiled.Spec.Description,
			Type:        compiled.Type,
			Priority:    compiled.Spec.Priority,
			Enabled:     compiled.Enabled(),
			Triggers:    compiled.Spec.Match.Triggers,
		})
	}
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (s *Server) configHandler(w http.ResponseWriter, r *http.Request) {
	if !s.debugEnabled(w, r) {
		return
	}
	_ = httpx.WriteJSON(w, http.StatusOK, s.cfg.Redacted())
}

func (s *Server) requestsHandler(w http.ResponseWriter, r *http.Request) {
	if !s.debugEnabled(w, r) {
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 500 {
		limit = 500
	}
	var data []observe.Snapshot
	if s.recorder != nil {
		data = s.recorder.List(limit)
	}
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (s *Server) sessionsHandler(w http.ResponseWriter, r *http.Request) {
	if !s.debugEnabled(w, r) {
		return
	}
	var sessions []session.Info
	if s.sessions != nil {
		sessions = s.sessions.Snapshot()
	}
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": sessions, "count": len(sessions)})
}

func (s *Server) activeHandler(w http.ResponseWriter, r *http.Request) {
	if !s.debugEnabled(w, r) {
		return
	}
	s.activeMu.Lock()
	snapshots := make([]observe.Snapshot, 0, len(s.active))
	for _, trace := range s.active {
		snapshots = append(snapshots, trace.Snapshot())
	}
	s.activeMu.Unlock()
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Time.Before(snapshots[j].Time) })
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": snapshots, "count": len(snapshots)})
}

func (s *Server) trackActive(trace *observe.Trace) {
	if trace == nil || trace.RequestID == "" {
		return
	}
	if trace.Path == "/healthz" || strings.HasPrefix(trace.Path, "/_fakeagents") {
		return
	}
	s.activeMu.Lock()
	s.active[trace.RequestID] = trace
	s.activeMu.Unlock()
}

func (s *Server) untrackActive(trace *observe.Trace) {
	if trace == nil || trace.RequestID == "" {
		return
	}
	s.activeMu.Lock()
	delete(s.active, trace.RequestID)
	s.activeMu.Unlock()
}

func (s *Server) resetHandler(w http.ResponseWriter, r *http.Request) {
	if !s.debugEnabled(w, r) {
		return
	}
	if s.sessions != nil {
		s.sessions.Reset()
	}
	if s.limiter != nil {
		s.limiter.Reset()
	}
	if s.responses != nil {
		s.responses.Reset()
	}
	if s.recorder != nil {
		s.recorder.Reset()
	}
	if s.engine != nil {
		s.engine.ResetScripts()
	}
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) debugEnabled(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.Debug.IsEnabled() {
		return true
	}
	s.notFound(w, r)
	return false
}

// BaseURL returns the externally reachable base URL (best effort).
func (s *Server) BaseURL() string {
	scheme := "http"
	if s.cfg.Server.TLS.Enabled() {
		scheme = "https"
	}
	base := strings.TrimSuffix(s.cfg.Server.BasePath, "/")
	return scheme + "://" + s.Addr() + base
}
