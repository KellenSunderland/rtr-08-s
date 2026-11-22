package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/argus-video/argus/gateway/internal/auth"
	"github.com/argus-video/argus/gateway/internal/frames"
	"github.com/argus-video/argus/gateway/internal/media"
	"github.com/argus-video/argus/gateway/internal/protocol"
	"github.com/argus-video/argus/gateway/internal/session"
	"github.com/argus-video/argus/gateway/internal/streams"
	"github.com/coder/websocket"
)

const (
	tokA     = "token-aaaaaaaa"
	tokB     = "token-bbbbbbbb"
	internal = "internal-secret"
)

type env struct {
	t     *testing.T
	ts    *httptest.Server
	reg   *streams.Registry
	agent *fakeAgent
	exp   string // clip export root
}

func newEnv(t *testing.T) *env {
	t.Helper()
	agent := newFakeAgent(t)
	m := media.NewFake(media.StoreConfig{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := streams.NewRegistry(m, streams.URIPolicy{AllowedSchemes: []string{"fake"}}, log)
	a := auth.New([]auth.TokenSpec{{Name: "a", Token: tokA, MaxStreams: 3, MaxSessions: 1}, {Name: "b", Token: tokB}}, internal)
	exp := t.TempDir()
	srv := New(Deps{Auth: a, Streams: reg, Sessions: session.NewManager(), AgentURL: agent.srv.URL, InternalToken: internal, ExportRoot: exp, Log: log})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); reg.Close(); agent.srv.Close() })
	return &env{t, ts, reg, agent, exp}
}

func (e *env) do(method, path, tok string, body any) *http.Response {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rd)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func (e *env) mustStatus(resp *http.Response, want int) []byte {
	e.t.Helper()
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != want {
		e.t.Fatalf("status %d, want %d: %s", resp.StatusCode, want, b)
	}
	return b
}

func (e *env) addStream(id, uri, tok string) {
	e.t.Helper()
	e.mustStatus(e.do("POST", "/v1/streams", tok, protocol.CreateStreamRequest{ID: id, Name: id, URI: uri}), 201)
}

func (e *env) waitFrames(id string, n uint64) *streams.Stream {
	e.t.Helper()
	st, _ := e.reg.Get(id)
	for i := 0; i < 400 && st.Frames() < n; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if st.Frames() < n {
		e.t.Fatal("not enough frames")
	}
	return st
}

func TestAuthAndErrorBodies(t *testing.T) {
	e := newEnv(t)
	e.mustStatus(e.do("GET", "/healthz", "", nil), 200)
	resp := e.do("GET", "/v1/streams", "", nil)
	b := e.mustStatus(resp, 401)
	var eb map[string]string
	if err := json.Unmarshal(b, &eb); err != nil || eb["code"] != "unauthorized" || eb["message"] == "" {
		t.Fatalf("error body %s", b)
	}
	e.mustStatus(e.do("GET", "/v1/streams", "wrong", nil), 401)
	e.mustStatus(e.do("GET", "/v1/streams", tokA, nil), 200)
	// internal endpoints need the internal token even from loopback
	e.mustStatus(e.do("GET", "/internal/frames/x", tokA, nil), 401)
	m := e.mustStatus(e.do("GET", "/metrics", "", nil), 200)
	if !strings.Contains(string(m), "argus_gateway_streams") {
		t.Fatalf("metrics: %s", m)
	}
}

func TestStreamCRUDQuotaAndIsolation(t *testing.T) {
	e := newEnv(t)
	e.addStream("cam1", "fake://16x16@20", tokA)
	e.mustStatus(e.do("POST", "/v1/streams", tokA, protocol.CreateStreamRequest{ID: "cam1", URI: "fake://16x16@20"}), 409)
	e.mustStatus(e.do("POST", "/v1/streams", tokA, protocol.CreateStreamRequest{ID: "cam2", URI: "file:///etc/passwd"}), 400)
	e.mustStatus(e.do("POST", "/v1/streams", tokA, protocol.CreateStreamRequest{ID: "BAD", URI: "fake://16x16@20"}), 400)
	e.addStream("cam2", "fake://16x16@20", tokA)
	e.addStream("cam3", "fake://16x16@20", tokA)
	b := e.mustStatus(e.do("POST", "/v1/streams", tokA, protocol.CreateStreamRequest{ID: "cam4", URI: "fake://16x16@20"}), 429)
	if !strings.Contains(string(b), `"quota"`) {
		t.Fatal(string(b))
	}
	var list []protocol.StreamInfo
	_ = json.Unmarshal(e.mustStatus(e.do("GET", "/v1/streams", tokA, nil), 200), &list)
	if len(list) != 3 {
		t.Fatalf("%d", len(list))
	}
	_ = json.Unmarshal(e.mustStatus(e.do("GET", "/v1/streams", tokB, nil), 200), &list)
	if len(list) != 0 {
		t.Fatal("token b must not see a's streams")
	}
	e.mustStatus(e.do("GET", "/v1/streams/cam1", tokB, nil), 404)
	e.mustStatus(e.do("DELETE", "/v1/streams/cam1", tokB, nil), 404)
	e.mustStatus(e.do("DELETE", "/v1/streams/cam1", tokA, nil), 204)
	e.mustStatus(e.do("GET", "/v1/streams/cam1", tokA, nil), 404)
	e.addStream("cam4", "fake://16x16@20", tokA) // quota released
}

func TestFrameNearestTimestamp(t *testing.T) {
	e := newEnv(t)
	e.addStream("cam1", "fake://64x48@20", tokA)
	e.waitFrames("cam1", 15)

	resp := e.do("GET", "/v1/streams/cam1/frame", tokA, nil)
	body := e.mustStatus(resp, 200)
	if resp.Header.Get("Content-Type") != "image/jpeg" || !bytes.HasPrefix(body, []byte{0xFF, 0xD8}) {
		t.Fatal("not a jpeg")
	}
	latest, err := strconv.ParseInt(resp.Header.Get("X-Frame-Ts-Ms"), 10, 64)
	if err != nil || resp.Header.Get("X-Frame-Wall-Ms") == "" {
		t.Fatalf("headers %v", resp.Header)
	}
	want := latest - 400
	resp = e.do("GET", "/v1/streams/cam1/frame?max_w=16&ts_ms="+strconv.FormatInt(want, 10), tokA, nil)
	body = e.mustStatus(resp, 200)
	got, _ := strconv.ParseInt(resp.Header.Get("X-Frame-Ts-Ms"), 10, 64)
	if d := got - want; d < -40 || d > 40 {
		t.Fatalf("nearest frame off by %dms (want %d got %d)", d, want, got)
	}
	img, err := jpeg.Decode(bytes.NewReader(body))
	if err != nil || img.Bounds().Dx() != 16 || img.Bounds().Dy() != 12 {
		t.Fatalf("max_w downscale: %v %v", err, img.Bounds())
	}
	e.mustStatus(e.do("GET", "/v1/streams/cam1/frame?ts_ms=abc", tokA, nil), 400)
	e.mustStatus(e.do("GET", "/v1/streams/cam1/frame?ts_ms=-5", tokA, nil), 400)
	e.mustStatus(e.do("GET", "/v1/streams/cam1/frame?max_w=5000", tokA, nil), 400)
	e.mustStatus(e.do("GET", "/v1/streams/cam1/frame?ts_ms=99999999", tokA, nil), 404) // beyond max distance
	e.mustStatus(e.do("GET", "/v1/streams/cam1/frame", tokB, nil), 404)
	e.mustStatus(e.do("GET", "/v1/streams/nope/frame", tokA, nil), 404)
}

func TestClipExportBounds(t *testing.T) {
	e := newEnv(t)
	e.addStream("cam1", "fake://32x24@20", tokA)
	e.waitFrames("cam1", 30)
	resp := e.do("GET", "/v1/streams/cam1/frame", tokA, nil)
	e.mustStatus(resp, 200)
	latest, _ := strconv.ParseInt(resp.Header.Get("X-Frame-Ts-Ms"), 10, 64)

	base := "/v1/streams/cam1/clip?"
	for q, want := range map[string]int{
		"": 400, "from_ms=0": 400, "from_ms=10&to_ms=5": 400, "from_ms=-1&to_ms=5": 400, "from_ms=x&to_ms=5": 400,
		"from_ms=0&to_ms=600001":        400, // > 10 minutes
		"from_ms=0&to_ms=1000&fps=10.5": 400,
		"from_ms=0&to_ms=1000&fps=0":    400,
		"from_ms=0&to_ms=1000&fps=-2":   400,
		"from_ms=0&to_ms=1000&fps=nan":  400,
		"from_ms=1000000&to_ms=1005000": 404, // empty range
		"from_ms=0&to_ms=1000&max_w=3":  400,
	} {
		if resp := e.do("GET", base+q, tokA, nil); resp.StatusCode != want {
			t.Errorf("%q: got %d want %d", q, resp.StatusCode, want)
		}
	}
	e.mustStatus(e.do("GET", base+"from_ms=0&to_ms=1000", tokB, nil), 404)

	from := latest - 1000
	if from < 0 {
		from = 0
	}
	q := "from_ms=" + strconv.FormatInt(from, 10) + "&to_ms=" + strconv.FormatInt(latest+1, 10) + "&fps=4"
	resp = e.do("GET", base+q, tokA, nil)
	zb := e.mustStatus(resp, 200)
	if resp.Header.Get("Content-Type") != "application/zip" {
		t.Fatal(resp.Header)
	}
	zr, err := zip.NewReader(bytes.NewReader(zb), int64(len(zb)))
	if err != nil {
		t.Fatal(err)
	}
	jpgs := 0
	var index struct {
		Frames []struct {
			File string `json:"file"`
			TsMs int64  `json:"ts_ms"`
		} `json:"frames"`
		FPS float64 `json:"fps"`
	}
	for _, f := range zr.File {
		if strings.Contains(f.Name, "..") || strings.HasPrefix(f.Name, "/") {
			t.Fatalf("unsafe entry %q", f.Name)
		}
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		switch {
		case f.Name == "index.json":
			if err := json.Unmarshal(data, &index); err != nil {
				t.Fatal(err)
			}
		case strings.HasSuffix(f.Name, ".jpg"):
			jpgs++
			if !bytes.HasPrefix(data, []byte{0xFF, 0xD8}) {
				t.Fatal("bad jpeg in zip")
			}
		}
	}
	// ~1s at 4 fps => about 4-5 frames; never more than the sampled slots.
	if jpgs < 3 || jpgs > 6 || jpgs != len(index.Frames) || index.FPS != 4 {
		t.Fatalf("jpgs=%d index=%+v", jpgs, index)
	}
	for _, f := range index.Frames {
		if f.TsMs < from || f.TsMs > latest {
			t.Fatalf("frame ts %d outside [%d,%d]", f.TsMs, from, latest)
		}
	}
}

func TestClipExportToNamedDirectory(t *testing.T) {
	e := newEnv(t)
	e.addStream("cam1", "fake://32x24@20", tokA)
	e.waitFrames("cam1", 30)
	resp := e.do("GET", "/v1/streams/cam1/frame", tokA, nil)
	e.mustStatus(resp, 200)
	latest, _ := strconv.ParseInt(resp.Header.Get("X-Frame-Ts-Ms"), 10, 64)
	from := max(latest-1000, 0)
	q := "?from_ms=" + strconv.FormatInt(from, 10) + "&to_ms=" + strconv.FormatInt(latest+1, 10) + "&fps=4"

	body := e.mustStatus(e.do("POST", "/v1/streams/cam1/clip/export"+q+"&name=weekend", tokA, nil), 201)
	var out struct {
		Path   string `json:"path"`
		Frames int    `json:"frames"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Frames < 3 || !strings.HasPrefix(out.Path, "weekend") {
		t.Fatalf("unexpected response %+v", out)
	}
	zb, err := os.ReadFile(filepath.Join(e.exp, out.Path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zip.NewReader(bytes.NewReader(zb), int64(len(zb))); err != nil {
		t.Fatalf("exported file is not a zip: %v", err)
	}

	e.mustStatus(e.do("POST", "/v1/streams/cam1/clip/export"+q, tokA, nil), 400)                                  // name required
	e.mustStatus(e.do("POST", "/v1/streams/cam1/clip/export"+q+"&name="+strings.Repeat("x", 65), tokA, nil), 400) // too long
	e.mustStatus(e.do("POST", "/v1/streams/cam1/clip/export"+q+"&name=weekend", tokB, nil), 404)                  // not the owner
}

func TestSessionCRUDAndQuota(t *testing.T) {
	e := newEnv(t)
	e.addStream("cam1", "fake://16x16@10", tokA)
	e.mustStatus(e.do("POST", "/v1/sessions", tokA, protocol.CreateSessionRequest{Scenario: "pets", StreamIDs: []string{"nope"}}), 404)
	e.mustStatus(e.do("POST", "/v1/sessions", tokB, protocol.CreateSessionRequest{Scenario: "pets", StreamIDs: []string{"cam1"}}), 404)
	e.mustStatus(e.do("POST", "/v1/sessions", tokA, protocol.CreateSessionRequest{Scenario: "", StreamIDs: []string{"cam1"}}), 400)
	var si protocol.SessionInfo
	_ = json.Unmarshal(e.mustStatus(e.do("POST", "/v1/sessions", tokA, protocol.CreateSessionRequest{Scenario: "pets", StreamIDs: []string{"cam1"}}), 201), &si)
	if si.State != "open" || len(si.ID) != 36 {
		t.Fatalf("%+v", si)
	}
	e.mustStatus(e.do("POST", "/v1/sessions", tokA, protocol.CreateSessionRequest{Scenario: "pets", StreamIDs: []string{"cam1"}}), 429)
	e.mustStatus(e.do("GET", "/v1/sessions/"+si.ID, tokB, nil), 404)
	e.mustStatus(e.do("GET", "/v1/sessions/"+si.ID, tokA, nil), 200)
	e.mustStatus(e.do("GET", "/v1/sessions/not-a-uuid", tokA, nil), 404)
	e.mustStatus(e.do("DELETE", "/v1/sessions/"+si.ID, tokA, nil), 204)
	e.mustStatus(e.do("GET", "/v1/sessions/"+si.ID, tokA, nil), 404)
	e.mustStatus(e.do("POST", "/v1/sessions", tokA, protocol.CreateSessionRequest{Scenario: "pets", StreamIDs: []string{"cam1"}}), 201)
}

func TestProxyHistoryAndScenarios(t *testing.T) {
	e := newEnv(t)
	b := e.mustStatus(e.do("GET", "/v1/scenarios", tokA, nil), 200)
	if !strings.Contains(string(b), "pets") {
		t.Fatal(string(b))
	}
	b = e.mustStatus(e.do("GET", "/v1/history?since_ms=0&until_ms=10&q=dog&limit=5&token="+tokA, "", nil), 200)
	if !strings.Contains(string(b), "q=dog") || !strings.Contains(string(b), "tok=ok") {
		t.Fatalf("query/internal token not forwarded: %s", b)
	}
	e.mustStatus(e.do("GET", "/v1/history?since_ms=0", tokA, nil), 400)
	e.mustStatus(e.do("GET", "/v1/history?since_ms=0&until_ms=1&limit=999", tokA, nil), 400)
	e.mustStatus(e.do("GET", "/v1/history?since_ms=0&until_ms=1&stream_id=other", tokA, nil), 404)
	e.mustStatus(e.do("GET", "/v1/scenarios", "", nil), 401)
	e.agent.srv.Close()
	e.mustStatus(e.do("GET", "/v1/scenarios", tokA, nil), 502)
}

// ---------------------------------------------------------------- session WS

type fakeAgent struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []string // raw text messages received over the session ws
	hdr string
}

func (a *fakeAgent) received() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string{}, a.got...)
}

func newFakeAgent(t *testing.T) *fakeAgent {
	a := &fakeAgent{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/scenarios", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"name":"pets","description":"d","event_types":[],"tools":[]}]`)
	})
	mux.HandleFunc("GET /v1/history", func(w http.ResponseWriter, r *http.Request) {
		tok := "bad"
		if r.Header.Get("X-Internal-Token") == internal {
			tok = "ok"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"query":"`+r.URL.RawQuery+`","tok=`+tok+`":1}`)
	})
	mux.HandleFunc("GET /internal/sessions/{id}/ws", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.hdr = r.Header.Get("X-Internal-Token")
		a.mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		send := func(v any) {
			b, _ := json.Marshal(v)
			_ = c.Write(context.Background(), websocket.MessageText, b)
		}
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			a.mu.Lock()
			a.got = append(a.got, string(data))
			a.mu.Unlock()
			m, _ := protocol.Decode(data)
			switch v := m.(type) {
			case protocol.SessionStart:
				send(protocol.SessionStarted{Type: "session.started", SessionID: "agent-made-up"}) // gateway must swallow this
			case protocol.TextMessage:
				switch v.Text {
				case "hello":
					send(protocol.TranscriptFinal{Type: "transcript.final", Text: "hello", UtteranceID: "u-1"})
					for i, d := range []string{"a", "b", "c"} {
						send(protocol.AssistantText{Type: "assistant.text", UtteranceID: "u-1", Delta: d, Final: i == 2})
					}
				case "long":
					send(protocol.AssistantText{Type: "assistant.text", UtteranceID: "u-2", Delta: "x"})
				}
			case protocol.BargeIn:
				send(protocol.AssistantCancelled{Type: "assistant.cancelled", UtteranceID: "u-2", Reason: "barge_in"})
				send(protocol.AssistantText{Type: "assistant.text", UtteranceID: "u-2", Delta: "STALE"}) // must be dropped
				send(protocol.AssistantText{Type: "assistant.text", UtteranceID: "u-3", Delta: "next", Final: true})
			case protocol.Stop:
				_ = c.Close(websocket.StatusNormalClosure, "bye")
				return
			}
		}
	})
	a.srv = httptest.NewServer(mux)
	return a
}

func (e *env) newSession(streamIDs ...string) protocol.SessionInfo {
	var si protocol.SessionInfo
	_ = json.Unmarshal(e.mustStatus(e.do("POST", "/v1/sessions", tokA, protocol.CreateSessionRequest{Scenario: "pets", StreamIDs: streamIDs}), 201), &si)
	return si
}

func (e *env) dialSession(id, tok string) *websocket.Conn {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.ts.URL, "http")+"/v1/sessions/"+id+"/ws",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + tok}}})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { c.CloseNow() })
	return c
}

func send(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func recv(t *testing.T, c *websocket.Conn) protocol.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	m, err := protocol.Decode(data)
	if err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return m
}

func startMsg() protocol.SessionStart {
	return protocol.SessionStart{Type: "session.start", Scenario: "pets", StreamIDs: []string{"cam1"}, Locale: "en-US"}
}

func TestSessionWSRelayOrderingAndBargeIn(t *testing.T) {
	e := newEnv(t)
	e.addStream("cam1", "fake://16x16@10", tokA)
	e.waitFrames("cam1", 1)
	si := e.newSession("cam1")
	c := e.dialSession(si.ID, tokA)

	send(t, c, startMsg())
	ss, ok := recv(t, c).(protocol.SessionStarted)
	if !ok || ss.SessionID != si.ID || len(ss.Streams) != 1 || ss.Streams[0].ID != "cam1" {
		t.Fatalf("session.started: %+v", ss)
	}

	// audio is stamped with the session clock by the gateway
	send(t, c, protocol.ClientAudio{Type: "audio.chunk", AudioChunk: protocol.AudioChunk{Seq: 1, TsMs: 987654321, SampleRate: 16000, PCM16B64: "AAAA"}})

	send(t, c, protocol.TextMessage{Type: "text.message", Text: "hello"})
	if tf, ok := recv(t, c).(protocol.TranscriptFinal); !ok || tf.UtteranceID != "u-1" {
		t.Fatalf("want transcript.final, got %+v", tf)
	}
	for i, want := range []string{"a", "b", "c"} {
		at, ok := recv(t, c).(protocol.AssistantText)
		if !ok || at.Delta != want || at.UtteranceID != "u-1" || at.Final != (i == 2) {
			t.Fatalf("delta %d out of order: %+v", i, at)
		}
	}

	send(t, c, protocol.TextMessage{Type: "text.message", Text: "long"})
	if at, ok := recv(t, c).(protocol.AssistantText); !ok || at.UtteranceID != "u-2" {
		t.Fatalf("%+v", at)
	}
	send(t, c, protocol.BargeIn{Type: "control.barge_in"})
	if ac, ok := recv(t, c).(protocol.AssistantCancelled); !ok || ac.UtteranceID != "u-2" || ac.Reason != "barge_in" {
		t.Fatalf("%+v", ac)
	}
	// cancelled is last for u-2: the stale delta is dropped, next message is u-3
	if at, ok := recv(t, c).(protocol.AssistantText); !ok || at.UtteranceID != "u-3" || at.Delta != "next" {
		t.Fatalf("stale message leaked after cancel: %+v", at)
	}

	// what the agent saw: session.start first, barge-in passed through, audio re-stamped
	got := e.agent.received()
	if len(got) < 5 || !strings.Contains(got[0], `"session.start"`) {
		t.Fatalf("agent got %v", got)
	}
	if !strings.Contains(strings.Join(got, "\n"), `"control.barge_in"`) {
		t.Fatal("barge_in not passed through")
	}
	for _, g := range got {
		if strings.Contains(g, `"audio.chunk"`) {
			var ac protocol.ClientAudio
			_ = json.Unmarshal([]byte(g), &ac)
			if ac.TsMs == 987654321 || ac.TsMs < 0 || ac.TsMs > 60000 {
				t.Fatalf("audio ts not stamped: %d", ac.TsMs)
			}
			if ac.PCM16B64 != "AAAA" || ac.Seq != 1 {
				t.Fatalf("audio payload altered: %+v", ac)
			}
		}
	}
	e.agent.mu.Lock()
	hdr := e.agent.hdr
	e.agent.mu.Unlock()
	if hdr != internal {
		t.Fatalf("internal token not sent to agent: %q", hdr)
	}

	// control.stop is forwarded and both sides close
	send(t, c, protocol.Stop{Type: "control.stop"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("console ws not closed after stop")
			}
			break
		}
	}
}

func TestSessionWSInjectsStreamStatusAndClosesOnDelete(t *testing.T) {
	e := newEnv(t)
	e.addStream("cam1", "fake://16x16@10?frames=5", tokA)
	si := e.newSession("cam1")
	c := e.dialSession(si.ID, tokA)
	send(t, c, startMsg())
	recv(t, c) // session.started
	// the stream hits EOF after 5 frames => stopped; gateway must inject stream.status
	seen := map[protocol.StreamState]bool{}
	for !seen[protocol.StateStopped] {
		st, ok := recv(t, c).(protocol.StreamStatus)
		if !ok || st.Stream.ID != "cam1" {
			t.Fatalf("unexpected %+v", st)
		}
		seen[st.Stream.State] = true
	}
	e.mustStatus(e.do("DELETE", "/v1/sessions/"+si.ID, tokA, nil), 204)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("ws not closed on session delete")
			}
			return
		}
	}
}

func TestSessionWSProtocolViolations(t *testing.T) {
	e := newEnv(t)
	e.addStream("cam1", "fake://16x16@10", tokA)
	si := e.newSession("cam1")

	// other token cannot attach
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.ts.URL, "http")+"/v1/sessions/"+si.ID+"/ws",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + tokB}}})
	if err == nil || resp == nil || resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %v %v", err, resp)
	}

	// first message must be session.start
	c := e.dialSession(si.ID, tokA)
	send(t, c, protocol.TextMessage{Type: "text.message", Text: "hi"})
	if em, ok := recv(t, c).(protocol.ErrorMessage); !ok || em.Code != "bad_request" {
		t.Fatalf("%+v", em)
	}
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("want policy violation close, got %v", err)
	}

	// oversize text frame (>64KiB) closes with 1009 after session.start
	c = e.dialSession(si.ID, tokA)
	send(t, c, startMsg())
	recv(t, c)
	big := `{"type":"text.message","text":"` + strings.Repeat("x", 70<<10) + `"}`
	if err := c.Write(context.Background(), websocket.MessageText, []byte(big)); err != nil {
		t.Fatal(err)
	}
	var last error
	for i := 0; i < 5; i++ {
		_, _, last = c.Read(ctx)
		if last != nil {
			break
		}
	}
	if websocket.CloseStatus(last) != websocket.StatusMessageTooBig {
		t.Fatalf("want 1009, got %v", last)
	}
}

// ---------------------------------------------------------------- frames WS

func dialFrames(t *testing.T, e *env, path string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.ts.URL, "http")+path,
		&websocket.DialOptions{HTTPHeader: http.Header{"X-Internal-Token": {internal}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func countFrames(t *testing.T, c *websocket.Conn, d time.Duration, idx uint32) (n int, lastTs int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		si, ts, jpg, ok := frames.Decode(b)
		if !ok || si != idx || !bytes.HasPrefix(jpg, []byte{0xFF, 0xD8}) {
			t.Fatalf("bad frame message (ok=%v idx=%d)", ok, si)
		}
		if ts < lastTs {
			t.Fatal("ts went backwards")
		}
		n++
		lastTs = ts
	}
}

func TestFramesWSMotionGating(t *testing.T) {
	e := newEnv(t)
	e.addStream("still", "fake://32x24@50?pattern=static", tokA)
	e.addStream("busy", "fake://32x24@50?pattern=moving", tokA)
	stIdx := func(id string) uint32 { s, _ := e.reg.Get(id); return s.Idx }

	// no token => rejected
	req, _ := http.NewRequest("GET", e.ts.URL+"/internal/frames/still", nil)
	if resp, _ := http.DefaultClient.Do(req); resp == nil || resp.StatusCode != 401 {
		t.Fatal("internal token required")
	}

	// static scene: only the max_interval fallback produces frames (~1 + 1.2s/0.4s)
	c := dialFrames(t, e, "/internal/frames/still?min_motion=0.02&max_interval_ms=400&max_w=16")
	n, _ := countFrames(t, c, 1300*time.Millisecond, stIdx("still"))
	if n < 2 || n > 5 {
		t.Fatalf("static stream: got %d frames, want 2..5 (gated)", n)
	}

	// moving scene with a huge max_interval: every frame passes the motion gate (50 fps)
	c = dialFrames(t, e, "/internal/frames/busy?min_motion=0.02&max_interval_ms=60000")
	n, _ = countFrames(t, c, 600*time.Millisecond, stIdx("busy"))
	if n < 10 {
		t.Fatalf("moving stream: got %d frames, want >=10", n)
	}

	// a threshold above any possible score gates everything except the first frame
	c = dialFrames(t, e, "/internal/frames/busy?min_motion=1&max_interval_ms=60000")
	n, _ = countFrames(t, c, 600*time.Millisecond, stIdx("busy"))
	if n != 1 && n != 2 { // score of 0.8 (20 vs 220 swing) < 1
		t.Fatalf("expected only the first frame, got %d", n)
	}
}
