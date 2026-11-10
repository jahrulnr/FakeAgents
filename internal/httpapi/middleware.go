package httpapi

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/httpx"
	"github.com/jahrulnr/fakeagents/internal/observe"
	"github.com/jahrulnr/fakeagents/internal/ratelimit"
	"github.com/jahrulnr/fakeagents/internal/session"
	"github.com/jahrulnr/fakeagents/internal/wire"
)

func (s *Server) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if requestID == "" || len(requestID) > 128 {
			requestID = s.ids.New("req_")
		}
		trace := observe.NewTrace(requestID, r.Method, r.URL.Path, s.clock.Now())
		if provider := wire.DetectProvider(r); provider != "" {
			trace.SetProvider(provider, wire.EndpointFromPath(r.URL.Path))
		}
		w.Header().Set("X-Request-Id", requestID)
		ctx := observe.WithTrace(r.Context(), trace)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) basePathMiddleware(next http.Handler) http.Handler {
	base := strings.TrimSuffix(s.cfg.Server.BasePath, "/")
	if base == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != base && !strings.HasPrefix(r.URL.Path, base+"/") {
			s.notFound(w, r)
			return
		}
		clone := r.Clone(r.Context())
		clone.URL.Path = strings.TrimPrefix(r.URL.Path, base)
		if clone.URL.Path == "" {
			clone.URL.Path = "/"
		}
		next.ServeHTTP(w, clone)
	})
}

func (s *Server) accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace := observe.From(r.Context())
		s.trackActive(trace)
		defer s.untrackActive(trace)
		recorder := &responseRecorder{ResponseWriter: w, trace: trace}
		defer func() {
			snapshot := trace.Snapshot()
			if snapshot.Status == 0 {
				snapshot.Status = http.StatusOK
			}
			if s.recorder != nil {
				s.recorder.Add(snapshot)
			}
			if !s.cfg.Logging.AccessLogEnabled() {
				return
			}
			level := slog.LevelInfo
			if snapshot.Status >= 500 {
				level = slog.LevelError
			} else if snapshot.Status >= 400 {
				level = slog.LevelWarn
			}
			s.logger.Log(r.Context(), level, "request",
				"request_id", snapshot.RequestID,
				"method", snapshot.Method,
				"path", snapshot.Path,
				"provider", snapshot.Provider,
				"endpoint", snapshot.Endpoint,
				"model", snapshot.Model,
				"scenario", snapshot.Scenario,
				"session", snapshot.Session,
				"turn", snapshot.Turn,
				"step", snapshot.Step,
				"stream", snapshot.Stream,
				"status", snapshot.Status,
				"duration_ms", snapshot.DurationMS,
				"ttft_ms", snapshot.TTFTMS,
				"tokens_in", snapshot.TokensIn,
				"tokens_out", snapshot.TokensOut,
				"bytes", recorder.bytes,
				"error", snapshot.Error,
			)
		}()
		next.ServeHTTP(recorder, r)
	})
}

type responseRecorder struct {
	http.ResponseWriter
	trace       *observe.Trace
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = status
	if r.trace != nil {
		r.trace.SetStatus(status)
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(data []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if r.trace != nil && r.trace.TTFT == 0 {
		r.trace.MarkTTFT(time.Since(r.trace.Start))
	}
	n, err := r.ResponseWriter.Write(data)
	r.bytes += int64(n)
	return n, err
}

func (r *responseRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("panic recovered", "panic", recovered, "stack", string(debug.Stack()), "path", r.URL.Path)
				if recorder, ok := w.(*responseRecorder); ok && recorder.wroteHeader {
					return
				}
				provider := wire.DetectProvider(r)
				apiErr := apierr.New(http.StatusInternalServerError, "server_error", "server_error", "Internal server error.")
				wire.WriteError(w, s.wireOptionsFor(r, provider), apiErr)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	if !s.cfg.Server.CORS.IsEnabled() {
		return next
	}
	allowedOrigins := s.cfg.Server.CORS.AllowedOrigins
	if len(allowedOrigins) == 0 {
		allowedOrigins = []string{"*"}
	}
	allowedMethods := s.cfg.Server.CORS.AllowedMethods
	if len(allowedMethods) == 0 {
		allowedMethods = []string{"GET", "POST", "DELETE", "OPTIONS"}
	}
	allowedHeaders := s.cfg.Server.CORS.AllowedHeaders
	if len(allowedHeaders) == 0 {
		allowedHeaders = []string{"*"}
	}
	exposedHeaders := s.cfg.Server.CORS.ExposedHeaders
	if len(exposedHeaders) == 0 {
		exposedHeaders = []string{"*"}
	}
	maxAge := int64(s.cfg.Server.CORS.MaxAge.D().Seconds())
	allowCredentials := s.cfg.Server.CORS.AllowCredentials

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !originAllowed(origin, allowedOrigins) {
			next.ServeHTTP(w, r)
			return
		}
		allowAll := contains(allowedOrigins, "*")
		if allowAll && !allowCredentials {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", strings.Join(allowedMethods, ", "))
		if contains(allowedHeaders, "*") {
			if requested := r.Header.Get("Access-Control-Request-Headers"); requested != "" {
				w.Header().Set("Access-Control-Allow-Headers", requested)
			} else {
				w.Header().Set("Access-Control-Allow-Headers", "*")
			}
		} else {
			w.Header().Set("Access-Control-Allow-Headers", strings.Join(allowedHeaders, ", "))
		}
		if contains(exposedHeaders, "*") {
			w.Header().Set("Access-Control-Expose-Headers", "*")
		} else {
			w.Header().Set("Access-Control-Expose-Headers", strings.Join(exposedHeaders, ", "))
		}
		if allowCredentials {
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		if maxAge > 0 {
			w.Header().Set("Access-Control-Max-Age", strconv.FormatInt(maxAge, 10))
		}
		if r.Method == http.MethodOptions {
			w.Header().Add("Vary", "Access-Control-Request-Method")
			w.Header().Add("Vary", "Access-Control-Request-Headers")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	mode := strings.ToLower(strings.TrimSpace(s.cfg.Auth.Mode))
	if mode == "" || mode == "off" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provider := wire.DetectProvider(r)
		endpoint := wire.EndpointFromPath(r.URL.Path)
		if endpoint == "" || exemptPath(r.URL.Path, s.cfg.Auth.ExemptPaths) {
			next.ServeHTTP(w, r)
			return
		}
		presented := presentedKeys(r)
		if len(presented) == 0 {
			if mode == "optional" {
				next.ServeHTTP(w, r)
				return
			}
			s.rejectAuth(w, r, provider)
			return
		}
		if s.cfg.Auth.AcceptAny {
			next.ServeHTTP(w, r)
			return
		}
		for _, candidate := range presented {
			if s.keyAccepted(candidate, provider) {
				next.ServeHTTP(w, r)
				return
			}
		}
		s.rejectAuth(w, r, provider)
	})
}

func presentedKeys(r *http.Request) []string {
	var keys []string
	if value := strings.TrimSpace(r.Header.Get("x-api-key")); value != "" {
		keys = append(keys, value)
	}
	if value := httpx.BearerToken(r); value != "" {
		keys = append(keys, value)
	}
	if value := strings.TrimSpace(r.Header.Get("Authorization")); value != "" && len(keys) == 0 {
		keys = append(keys, value)
	}
	return keys
}

func (s *Server) keyAccepted(candidate, provider string) bool {
	for _, key := range s.cfg.Auth.Keys {
		if key.Key != candidate {
			continue
		}
		if len(key.Providers) == 0 {
			return true
		}
		for _, allowed := range key.Providers {
			if strings.EqualFold(allowed, provider) {
				return true
			}
		}
	}
	return false
}

func (s *Server) rejectAuth(w http.ResponseWriter, r *http.Request, provider string) {
	apiErr := apierr.MustPreset(provider, "authentication")
	if provider == apierr.ProviderAnthropic {
		apiErr.Message = "invalid x-api-key"
	} else {
		apiErr.Message = "Incorrect API key provided. You can find your API key at https://platform.openai.com/account/api-keys."
	}
	wire.WriteError(w, s.wireOptionsFor(r, provider), apiErr)
}

func (s *Server) requiredHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provider := wire.DetectProvider(r)
		endpoint := wire.EndpointFromPath(r.URL.Path)
		if endpoint == "" {
			next.ServeHTTP(w, r)
			return
		}
		for _, rule := range s.cfg.RequiredHeaders {
			if !rule.IsEnabled() {
				continue
			}
			if len(rule.Providers) > 0 && !containsFold(rule.Providers, provider) {
				continue
			}
			if len(rule.Endpoints) > 0 && !contains(rule.Endpoints, endpoint) {
				continue
			}
			if len(rule.Methods) > 0 && !containsFold(rule.Methods, r.Method) {
				continue
			}
			if len(rule.Paths) > 0 {
				matched := false
				for _, pattern := range rule.Paths {
					if matchPath(pattern, r.URL.Path) {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
			if exemptPath(r.URL.Path, rule.ExemptPaths) {
				continue
			}
			value := r.Header.Get(rule.Name)
			if strings.TrimSpace(value) != "" || (rule.AllowEmpty && r.Header.Values(rule.Name) != nil) {
				continue
			}
			status := rule.Status
			if status == 0 {
				status = http.StatusBadRequest
			}
			typ := rule.Type
			if typ == "" {
				typ = "invalid_request_error"
			}
			code := rule.Code
			if code == "" {
				code = "missing_required_header"
			}
			message := rule.Message
			if message == "" {
				message = "Missing required header: " + rule.Name
			}
			apiErr := apierr.New(status, typ, code, message)
			apiErr.Param = rule.Name
			wire.WriteError(w, s.wireOptionsFor(r, provider), apiErr)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	if !s.limiter.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provider := wire.DetectProvider(r)
		endpoint := wire.EndpointFromPath(r.URL.Path)
		if endpoint == "" || exemptPath(r.URL.Path, s.cfg.RateLimit.ExemptPaths) {
			ctx := ratelimit.WithDecision(r.Context(), ratelimit.Decision{Allowed: true})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		key := "global"
		if !strings.EqualFold(s.cfg.RateLimit.Scope, "global") {
			key = session.Resolve(r.Header, s.cfg.Defaults.SessionHeaders, httpx.ClientIP(r))
		}
		decision := s.limiter.AdmitRequest(key)
		ctx := ratelimit.WithDecision(r.Context(), decision)
		if !decision.Allowed {
			wire.SetRateLimitHeaders(w, provider, decision, ratelimit.Decision{})
			apiErr := apierr.MustPreset(provider, "rate_limit")
			apiErr.RetryAfter = decision.RetryAfter
			wire.WriteError(w, s.wireOptionsFor(r, provider), apiErr)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) wireOptionsFor(r *http.Request, provider string) wire.Options {
	requestID := ""
	if trace := observe.From(r.Context()); trace != nil {
		requestID = trace.RequestID
	}
	return wire.Options{
		Provider:   provider,
		RequestID:  requestID,
		APIVersion: s.cfg.Providers.Anthropic.APIVersion,
	}
}

func exemptPath(path string, patterns []string) bool {
	for _, pattern := range patterns {
		if pattern == path {
			return true
		}
		if strings.HasSuffix(pattern, "*") && strings.HasPrefix(path, strings.TrimSuffix(pattern, "*")) {
			return true
		}
	}
	return false
}

func matchPath(pattern, path string) bool {
	if pattern == path {
		return true
	}
	if before, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(path, before)
	}
	return false
}

func contains(values []string, target string) bool {
	return slices.Contains(values, target)
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

func originAllowed(origin string, allowed []string) bool {
	for _, pattern := range allowed {
		if pattern == "*" || pattern == origin {
			return true
		}
		if strings.Contains(pattern, "*") && wildcardMatch(pattern, origin) {
			return true
		}
	}
	return false
}

func wildcardMatch(pattern, value string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == value
	}
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	rest := value[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		index := strings.Index(rest, parts[i])
		if index < 0 {
			return false
		}
		rest = rest[index+len(parts[i]):]
	}
	last := parts[len(parts)-1]
	return strings.HasSuffix(rest, last)
}
