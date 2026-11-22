// Package api wires the REST surface described in api/openapi.yaml.
package api

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/argus-video/argus/gateway/internal/auth"
	"github.com/argus-video/argus/gateway/internal/frames"
	"github.com/argus-video/argus/gateway/internal/media"
	"github.com/argus-video/argus/gateway/internal/metrics"
	"github.com/argus-video/argus/gateway/internal/protocol"
	"github.com/argus-video/argus/gateway/internal/session"
	"github.com/argus-video/argus/gateway/internal/streams"
	"github.com/google/uuid"
)

const (
	maxClipSpanMs   = 10 * 60 * 1000
	maxClipFPS      = 10.0
	nearestMaxDist  = int64(30 * time.Second)
	defaultMaxW     = 640
	maxMaxW         = 1920
	jpegQuality     = 80
	maxBody         = 16 << 10
	maxProxyRespLen = 8 << 20
)

// Deps are the server's collaborators.
type Deps struct {
	Auth           *auth.Authenticator
	Streams        *streams.Registry
	Sessions       *session.Manager
	AgentURL       string
	InternalToken  string
	OriginPatterns []string
	Log            *slog.Logger
	Metrics        *metrics.Registry
	HTTPClient     *http.Client
}

// Server is the gateway HTTP handler.
type Server struct {
	Deps
	sessWS *session.Handler
	frames *frames.Handler
	mux    *http.ServeMux
}

// New builds the Server and its routes.
func New(d Deps) *Server {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Metrics == nil {
		d.Metrics = metrics.New()
	}
	if d.HTTPClient == nil {
		d.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	s := &Server{Deps: d, mux: http.NewServeMux()}
	s.sessWS = &session.Handler{Sessions: d.Sessions, Streams: d.Streams, AgentURL: d.AgentURL, InternalToken: d.InternalToken,
		OriginPatterns: d.OriginPatterns, Log: d.Log, Metrics: d.Metrics}
	s.frames = &frames.Handler{Streams: d.Streams, Sessions: d.Sessions, Log: d.Log, Metrics: d.Metrics}
	s.registerMetrics()

	v1 := http.NewServeMux()
	v1.HandleFunc("GET /v1/streams", s.listStreams)
	v1.HandleFunc("POST /v1/streams", s.createStream)
	v1.HandleFunc("GET /v1/streams/{id}", s.getStream)
	v1.HandleFunc("DELETE /v1/streams/{id}", s.deleteStream)
	v1.HandleFunc("GET /v1/streams/{id}/frame", s.getFrame)
	v1.HandleFunc("GET /v1/streams/{id}/clip", s.getClip)
	v1.HandleFunc("POST /v1/sessions", s.createSession)
	v1.HandleFunc("GET /v1/sessions", s.listSessions)
	v1.HandleFunc("GET /v1/sessions/{id}", s.getSession)
	v1.HandleFunc("DELETE /v1/sessions/{id}", s.deleteSession)
	v1.Handle("GET /v1/sessions/{id}/ws", http.HandlerFunc(s.sessWS.ServeWS))
	v1.HandleFunc("GET /v1/history", s.proxyHistory)
	v1.HandleFunc("GET /v1/scenarios", s.proxyScenarios)
	v1.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		auth.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "no such route")
	})

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	s.mux.Handle("GET /metrics", d.Metrics.Handler())
	s.mux.Handle("/v1/", d.Auth.Middleware(v1))
	s.mux.Handle("GET /internal/frames/{stream_id}", d.Auth.Internal(s.frames))
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		auth.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "no such route")
	})
	return s
}

// Handler returns the full handler with logging/recovery middleware.
func (s *Server) Handler() http.Handler { return s.logging(s.mux) }

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(c int)           { w.status = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: 200}
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				s.Log.Error("panic in handler", "path", r.URL.Path, "panic", fmt.Sprint(rec))
				auth.WriteError(sw, http.StatusInternalServerError, protocol.CodeInternal, "internal error")
			}
			// Path only: query strings may carry user text (q) or tokens.
			s.Log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "dur_ms", time.Since(start).Milliseconds())
			s.Metrics.Counter("argus_gateway_http_requests_total", "HTTP requests").Add(1)
		}()
		next.ServeHTTP(sw, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		auth.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// ---- metrics

func (s *Server) registerMetrics() {
	s.Metrics.AddCollector(func(w io.Writer) {
		counts := map[protocol.StreamState]int{}
		fmt.Fprintln(w, "# TYPE argus_gateway_stream_fps gauge")
		for _, st := range s.Streams.List(nil) {
			info := st.Info()
			counts[info.State]++
			fps := 0.0
			if info.FPS != nil {
				fps = *info.FPS
			}
			fmt.Fprintf(w, "argus_gateway_stream_fps{stream=%q} %g\n", st.ID, fps)
			fmt.Fprintf(w, "argus_gateway_stream_frames_total{stream=%q} %d\n", st.ID, st.Frames())
			fmt.Fprintf(w, "argus_gateway_stream_dropped_frames_total{stream=%q} %d\n", st.ID, st.Dropped())
		}
		fmt.Fprintf(w, "# TYPE argus_gateway_streams_stalled gauge\nargus_gateway_streams_stalled %d\n", s.Streams.Stalled())
		fmt.Fprintln(w, "# TYPE argus_gateway_streams gauge")
		for _, state := range []protocol.StreamState{"starting", "live", "stalled", "stopped", "error"} {
			fmt.Fprintf(w, "argus_gateway_streams{state=%q} %d\n", state, counts[state])
		}
		fmt.Fprintf(w, "# TYPE argus_gateway_sessions gauge\nargus_gateway_sessions %d\n", s.Sessions.Count())
		if st, err := s.Streams.Media().Stats(); err == nil {
			fmt.Fprintf(w, "# TYPE argus_gateway_store_frames gauge\nargus_gateway_store_frames %d\n", st.Frames)
			fmt.Fprintf(w, "argus_gateway_store_bytes %d\n", st.Bytes)
			fmt.Fprintf(w, "# TYPE argus_gateway_store_evicted_total counter\nargus_gateway_store_evicted_total %d\n", st.EvictedTotal)
			fmt.Fprintf(w, "# TYPE argus_gateway_store_thinned_total counter\nargus_gateway_store_thinned_total %d\n", st.ThinnedTotal)
		}
	})
}

// ---- streams

func (s *Server) ownedStream(w http.ResponseWriter, r *http.Request) (*streams.Stream, bool) {
	st, ok := s.Streams.Get(r.PathValue("id"))
	if !ok || st.Owner != auth.FromContext(r.Context()) {
		auth.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "stream not found")
		return nil, false
	}
	return st, true
}

func (s *Server) listStreams(w http.ResponseWriter, r *http.Request) {
	out := []protocol.StreamInfo{}
	for _, st := range s.Streams.List(auth.FromContext(r.Context())) {
		out = append(out, st.Info())
	}
	writeJSON(w, 200, out)
}

func (s *Server) createStream(w http.ResponseWriter, r *http.Request) {
	var req protocol.CreateStreamRequest
	if !decodeBody(w, r, &req) {
		return
	}
	st, err := s.Streams.Create(auth.FromContext(r.Context()), req.ID, req.Name, req.URI)
	var ue *streams.URIError
	switch {
	case err == nil:
		s.Log.Info("stream created", "stream", st.ID)
		writeJSON(w, http.StatusCreated, st.Info())
	case errors.As(err, &ue):
		auth.WriteError(w, 400, protocol.CodeBadRequest, ue.Msg)
	case errors.Is(err, streams.ErrExists):
		auth.WriteError(w, 409, protocol.CodeBadRequest, "stream id already exists")
	case errors.Is(err, streams.ErrQuota):
		auth.WriteError(w, 429, protocol.CodeQuota, "stream quota exceeded")
	default:
		auth.WriteError(w, 500, protocol.CodeInternal, "internal error")
	}
}

func (s *Server) getStream(w http.ResponseWriter, r *http.Request) {
	if st, ok := s.ownedStream(w, r); ok {
		writeJSON(w, 200, st.Info())
	}
}

func (s *Server) deleteStream(w http.ResponseWriter, r *http.Request) {
	st, ok := s.ownedStream(w, r)
	if !ok {
		return
	}
	if err := s.Streams.Delete(st.ID); err != nil {
		auth.WriteError(w, 404, protocol.CodeNotFound, "stream not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clockOrigin returns the media-clock origin for ts_ms: the session start if
// ?session_id= is given (and owned), otherwise the gateway epoch.
func (s *Server) clockOrigin(w http.ResponseWriter, r *http.Request) (int64, bool) {
	sid := r.URL.Query().Get("session_id")
	if sid == "" {
		return s.Streams.OriginNs(), true
	}
	sess, ok := s.Sessions.Get(sid)
	if !ok || sess.Owner != auth.FromContext(r.Context()) {
		auth.WriteError(w, 404, protocol.CodeNotFound, "session not found")
		return 0, false
	}
	return sess.OriginOn(s.Streams.Media().NowMonotonicNs()), true
}

func intParam(r *http.Request, k string) (int64, bool, bool) { // value, present, ok
	v := r.URL.Query().Get(k)
	if v == "" {
		return 0, false, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, true, err == nil
}

func maxWParam(r *http.Request) (int, bool) {
	n, present, ok := intParam(r, "max_w")
	if !ok || (present && (n < 16 || n > maxMaxW)) {
		return 0, false
	}
	if !present {
		return defaultMaxW, true
	}
	return int(n), true
}

func scaled(m media.Media, f media.Frame, maxW int) (media.Frame, error) {
	if f.W <= maxW {
		return f, nil
	}
	h := f.H * maxW / f.W
	if h < 1 {
		h = 1
	}
	return m.Resize(f, maxW, h)
}

func (s *Server) getFrame(w http.ResponseWriter, r *http.Request) {
	st, ok := s.ownedStream(w, r)
	if !ok {
		return
	}
	maxW, ok := maxWParam(r)
	tsMs, hasTs, ok2 := intParam(r, "ts_ms")
	if !ok || !ok2 || tsMs < 0 {
		auth.WriteError(w, 400, protocol.CodeBadRequest, "invalid ts_ms or max_w")
		return
	}
	origin, ok := s.clockOrigin(w, r)
	if !ok {
		return
	}
	m := s.Streams.Media()
	var f media.Frame
	var err error
	if hasTs {
		f, err = m.Nearest(st.Idx, origin+tsMs*1e6, nearestMaxDist)
	} else {
		f, err = m.Latest(st.Idx)
	}
	if err != nil {
		auth.WriteError(w, 404, protocol.CodeNotFound, "no frame available")
		return
	}
	out, err := scaled(m, f, maxW)
	var jpg []byte
	if err == nil {
		jpg, err = m.EncodeJPEG(out, jpegQuality)
	}
	if err != nil {
		auth.WriteError(w, 500, protocol.CodeInternal, "encode failed")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("Content-Length", strconv.Itoa(len(jpg)))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Ts-Ms", strconv.FormatInt((f.TsNs-origin)/1e6, 10))
	h.Set("X-Frame-Wall-Ms", strconv.FormatInt(f.WallNs/1e6, 10))
	_, _ = w.Write(jpg)
}

type clipEntry struct {
	File   string `json:"file"`
	TsMs   int64  `json:"ts_ms"`
	WallMs int64  `json:"wall_ms"`
}

func (s *Server) getClip(w http.ResponseWriter, r *http.Request) {
	st, ok := s.ownedStream(w, r)
	if !ok {
		return
	}
	from, hf, ok1 := intParam(r, "from_ms")
	to, ht, ok2 := intParam(r, "to_ms")
	maxW, ok3 := maxWParam(r)
	fps := 2.0
	if v := r.URL.Query().Get("fps"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) {
			ok3 = false
		}
		fps = f
	}
	switch {
	case !hf || !ht || !ok1 || !ok2 || !ok3:
		auth.WriteError(w, 400, protocol.CodeBadRequest, "from_ms and to_ms are required integers; fps and max_w must be valid")
		return
	case from < 0 || to <= from:
		auth.WriteError(w, 400, protocol.CodeBadRequest, "require 0 <= from_ms < to_ms")
		return
	case to-from > maxClipSpanMs:
		auth.WriteError(w, 400, protocol.CodeBadRequest, "clip longer than 10 minutes")
		return
	case fps <= 0 || fps > maxClipFPS:
		auth.WriteError(w, 400, protocol.CodeBadRequest, "fps must be in (0, 10]")
		return
	}
	origin, ok := s.clockOrigin(w, r)
	if !ok {
		return
	}
	m := s.Streams.Media()
	fromNs, toNs := origin+from*1e6, origin+to*1e6
	if first, _, err := m.Range(st.Idx, fromNs, toNs, 1); err != nil || len(first) == 0 {
		auth.WriteError(w, 404, protocol.CodeNotFound, "no frames in range")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%d-%d.zip"`, st.ID, from, to))
	zw := zip.NewWriter(w)
	stepNs := int64(1e9 / fps)
	half := stepNs / 2
	if half < 1 {
		half = 1
	}
	var entries []clipEntry
	var lastTs int64 = -1
	for t := fromNs; t < toNs; t += stepNs {
		f, err := m.Nearest(st.Idx, t, half)
		if err != nil || f.TsNs == lastTs || f.TsNs < fromNs || f.TsNs >= toNs {
			continue
		}
		lastTs = f.TsNs
		sf, err := scaled(m, f, maxW)
		if err != nil {
			continue
		}
		jpg, err := m.EncodeJPEG(sf, jpegQuality)
		if err != nil {
			continue
		}
		name := fmt.Sprintf("frames/%06d.jpg", len(entries)+1)
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			return
		}
		if _, err := fw.Write(jpg); err != nil {
			return
		}
		entries = append(entries, clipEntry{name, (f.TsNs - origin) / 1e6, f.WallNs / 1e6})
		if r.Context().Err() != nil {
			return
		}
	}
	if fw, err := zw.Create("index.json"); err == nil {
		_ = json.NewEncoder(fw).Encode(map[string]any{"stream_id": st.ID, "from_ms": from, "to_ms": to, "fps": fps, "frames": entries})
	}
	_ = zw.Close()
}

// ---- sessions

func (s *Server) ownedSession(w http.ResponseWriter, r *http.Request) (*session.Session, bool) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err == nil {
		if sess, ok := s.Sessions.Get(id); ok && sess.Owner == auth.FromContext(r.Context()) {
			return sess, true
		}
	}
	auth.WriteError(w, 404, protocol.CodeNotFound, "session not found")
	return nil, false
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req protocol.CreateSessionRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Scenario == "" || len(req.Scenario) > 100 || len(req.StreamIDs) < 1 || len(req.StreamIDs) > 16 {
		auth.WriteError(w, 400, protocol.CodeBadRequest, "scenario required; stream_ids must have 1..16 entries")
		return
	}
	p := auth.FromContext(r.Context())
	for _, id := range req.StreamIDs {
		st, ok := s.Streams.Get(id)
		if !protocol.ValidStreamID(id) || !ok || st.Owner != p {
			auth.WriteError(w, 404, protocol.CodeNotFound, "unknown stream "+url.QueryEscape(id))
			return
		}
	}
	sess, err := s.Sessions.Create(p, req.Scenario, req.StreamIDs)
	if errors.Is(err, session.ErrQuota) {
		auth.WriteError(w, 429, protocol.CodeQuota, "session quota exceeded")
		return
	} else if err != nil {
		auth.WriteError(w, 500, protocol.CodeInternal, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, sess.Info())
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	out := []protocol.SessionInfo{}
	for _, sess := range s.Sessions.List(auth.FromContext(r.Context())) {
		out = append(out, sess.Info())
	}
	writeJSON(w, 200, out)
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	if sess, ok := s.ownedSession(w, r); ok {
		writeJSON(w, 200, sess.Info())
	}
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.ownedSession(w, r)
	if !ok {
		return
	}
	_ = s.Sessions.Close(sess.ID)
	w.WriteHeader(http.StatusNoContent)
}

// ---- proxies to the agent

func (s *Server) proxyHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	_, hs, ok1 := intParam(r, "since_ms")
	_, hu, ok2 := intParam(r, "until_ms")
	lim, hl, ok3 := intParam(r, "limit")
	if !hs || !hu || !ok1 || !ok2 || !ok3 || (hl && (lim < 1 || lim > 200)) || len([]rune(q.Get("q"))) > 500 {
		auth.WriteError(w, 400, protocol.CodeBadRequest, "since_ms and until_ms are required; limit 1..200; q <= 500 chars")
		return
	}
	if id := q.Get("stream_id"); id != "" {
		if st, ok := s.Streams.Get(id); !ok || st.Owner != auth.FromContext(r.Context()) {
			auth.WriteError(w, 404, protocol.CodeNotFound, "stream not found")
			return
		}
	}
	s.proxy(w, r, "/v1/history")
}

func (s *Server) proxyScenarios(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, "/v1/scenarios")
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, path string) {
	base, err := url.Parse(s.AgentURL)
	if err != nil || base.Host == "" {
		auth.WriteError(w, 500, protocol.CodeInternal, "agent url misconfigured")
		return
	}
	target := *base
	target.Path = path
	qs := r.URL.Query() // the caller's query, minus our own credentials
	qs.Del("token")
	qs.Del("access_token")
	target.RawQuery = qs.Encode()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		auth.WriteError(w, 500, protocol.CodeInternal, "internal error")
		return
	}
	req.Header.Set("X-Internal-Token", s.InternalToken)
	req.Header.Set("Accept", "application/json")
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		s.Log.Warn("agent proxy failed", "path", path, "err", err)
		auth.WriteError(w, http.StatusBadGateway, protocol.CodeUpstream, "agent unavailable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		auth.WriteError(w, http.StatusBadGateway, protocol.CodeUpstream, "agent error")
		return
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, maxProxyRespLen))
}
