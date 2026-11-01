package streams

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/argus-video/argus/gateway/internal/auth"
	"github.com/argus-video/argus/gateway/internal/media"
	"github.com/argus-video/argus/gateway/internal/protocol"
)

func newReg(t *testing.T) *Registry {
	t.Helper()
	m := media.NewFake(media.StoreConfig{})
	r := NewRegistry(m, URIPolicy{AllowedSchemes: []string{"fake"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(r.Close)
	return r
}

func waitState(t *testing.T, s *Stream, want protocol.StreamState, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if s.State() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state %s, want %s", s.State(), want)
}

func TestStreamGoesLiveAndStoresFrames(t *testing.T) {
	r := newReg(t)
	p := auth.New([]auth.TokenSpec{{Name: "a", Token: "token-aaaaaaaa", MaxStreams: 2}}, "x").Lookup("token-aaaaaaaa")
	watch, cancel := r.Watch()
	defer cancel()
	s, err := r.Create(p, "cam1", "Cam", "fake://32x24@50")
	if err != nil {
		t.Fatal(err)
	}
	if s.Info().State != protocol.StateStarting && s.Info().State != protocol.StateLive {
		t.Fatal("unexpected initial state")
	}
	waitState(t, s, protocol.StateLive, 2*time.Second)
	select {
	case info := <-watch:
		if info.ID != "cam1" || info.State != protocol.StateLive {
			t.Fatalf("%+v", info)
		}
	case <-time.After(time.Second):
		t.Fatal("no status notification")
	}
	time.Sleep(300 * time.Millisecond)
	info := s.Info()
	if info.FPS == nil || *info.FPS < 20 || *info.FPS > 80 || info.LastFrameWallMs == nil {
		t.Fatalf("bad info %+v", info)
	}
	if _, err := r.Media().Latest(s.Idx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(p, "cam1", "dup", "fake://8x8@5"); !errors.Is(err, ErrExists) {
		t.Fatalf("want exists, got %v", err)
	}
	if _, err := r.Create(p, "cam2", "", "fake://8x8@5"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(p, "cam3", "", "fake://8x8@5"); !errors.Is(err, ErrQuota) {
		t.Fatalf("want quota, got %v", err)
	}
	if err := r.Delete("cam1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Media().Latest(s.Idx); !errors.Is(err, media.ErrNotFound) {
		t.Fatal("frames should be dropped on delete")
	}
	if _, err := r.Create(p, "cam3", "", "fake://8x8@5"); err != nil {
		t.Fatalf("quota should be released: %v", err)
	}
	if err := r.Delete("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if got := len(r.List(p)); got != 2 {
		t.Fatalf("list %d", got)
	}
}

func TestStalledDetectionAndRecovery(t *testing.T) {
	r := newReg(t)
	r.StallAfter = 150 * time.Millisecond
	s, err := r.Create(nil, "cam1", "", "fake://8x8@100?stall_after=5")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, s, protocol.StateLive, 2*time.Second)
	time.Sleep(300 * time.Millisecond)
	r.CheckStalls()
	if s.State() != protocol.StateStalled {
		t.Fatalf("state %s", s.State())
	}
}

func TestEOFStopsAndOpenFailureErrors(t *testing.T) {
	r := newReg(t)
	s, _ := r.Create(nil, "cam1", "", "fake://8x8@100?frames=3")
	waitState(t, s, protocol.StateStopped, 2*time.Second)
	e, _ := r.Create(nil, "cam2", "", "fake://8x8@100?pattern=error")
	waitState(t, e, protocol.StateError, 2*time.Second)
	if _, err := r.Create(nil, "cam3", "", "file:///etc/passwd"); err == nil {
		t.Fatal("scheme must be rejected")
	}
	var ue *URIError
	if _, err := r.Create(nil, "Bad", "", "fake://8x8@1"); !errors.As(err, &ue) {
		t.Fatal("bad id should be a validation error")
	}
}

func TestSlowSubscriberNeverBlocksCapture(t *testing.T) {
	r := newReg(t)
	s, _ := r.Create(nil, "cam1", "", "fake://8x8@200")
	_, cancel := s.Subscribe() // never drained
	defer cancel()
	time.Sleep(500 * time.Millisecond)
	if s.Frames() < 50 {
		t.Fatalf("capture blocked: %d frames", s.Frames())
	}
	if s.Dropped() == 0 {
		t.Fatal("expected drops for the slow subscriber")
	}
}

func TestSlowStreamNotStalledEarly(t *testing.T) {
	r := newReg(t)
	r.StallAfter = 100 * time.Millisecond
	s, err := r.Create(nil, "slow", "", "fake://8x8@2")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, s, protocol.StateLive, 2*time.Second)
	for deadline := time.Now().Add(2 * time.Second); s.Info().FPS == nil && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond) // fps is only known after the second frame
	}
	time.Sleep(250 * time.Millisecond) // > StallAfter, < 4 frame intervals
	r.CheckStalls()
	if st := s.State(); st != protocol.StateLive {
		t.Fatalf("state %s, want live", st)
	}
	if r.Stalled() != 0 {
		t.Fatalf("stalled gauge %d", r.Stalled())
	}
}

func TestInfoReportsHealth(t *testing.T) {
	r := newReg(t)
	s, err := r.Create(nil, "cam1", "", "fake://16x16@50")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, s, protocol.StateLive, 2*time.Second)
	time.Sleep(100 * time.Millisecond)
	info := s.Info()
	if info.FramesTotal == 0 || info.StoredFrames == nil || *info.StoredFrames == 0 {
		t.Fatalf("missing counters: %+v", info)
	}
	if info.LastFrameAgeMs == nil || *info.LastFrameAgeMs < 0 || *info.LastFrameAgeMs > 1000 {
		t.Fatalf("bad age: %+v", info)
	}
}
