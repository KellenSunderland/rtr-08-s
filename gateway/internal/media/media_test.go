//go:build !(cgo && mediacore)

package media

import (
	"testing"
	"time"
)

func mk(id uint32, ts int64) Frame {
	return Frame{Data: make([]byte, 4*4*3), W: 4, H: 4, Stride: 12, Fmt: BGR8, TsNs: ts, StreamID: id}
}

func TestStoreNearestRangeOrder(t *testing.T) {
	m := NewFake(StoreConfig{})
	for _, ts := range []int64{10, 20, 30} {
		if err := m.Push(mk(1, ts)); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Push(mk(1, 30)); err != ErrOutOfOrder {
		t.Fatalf("want out of order, got %v", err)
	}
	f, _ := m.Nearest(1, 25, 0)
	if f.TsNs != 20 { // tie -> earlier
		t.Fatalf("got %d", f.TsNs)
	}
	if _, err := m.Nearest(1, 1000, 5); err != ErrNotFound {
		t.Fatal("expected not found")
	}
	fs, total, _ := m.Range(1, 10, 30, 1)
	if len(fs) != 1 || total != 2 {
		t.Fatalf("%d %d", len(fs), total)
	}
}

func TestRangeMetaMatchesRange(t *testing.T) {
	m := NewFake(StoreConfig{})
	for i, ts := range []int64{100, 200, 300, 400, 500} {
		f := mk(1, ts)
		f.WallNs, f.Seq = 1_700_000_000_000_000_000+ts, uint64(i+1)
		if err := m.Push(f); err != nil {
			t.Fatal(err)
		}
	}
	fs, total, _ := m.Range(1, 150, 450, 10)
	ms, mtotal, err := m.RangeMeta(1, 150, 450, 10)
	if err != nil || len(ms) != len(fs) || mtotal != total {
		t.Fatalf("meta %d/%d vs range %d/%d (%v)", len(ms), mtotal, len(fs), total, err)
	}
	for i := range ms {
		if ms[i].TsNs != fs[i].TsNs || ms[i].WallNs != fs[i].WallNs || ms[i].Seq != fs[i].Seq || ms[i].W != 4 || ms[i].H != 4 {
			t.Fatalf("meta %+v vs frame ts %d", ms[i], fs[i].TsNs)
		}
	}
	ms, mtotal, _ = m.RangeMeta(1, 0, 1000, 2)
	if len(ms) != 2 || mtotal != 5 || ms[0].TsNs != 100 {
		t.Fatalf("capped: %+v total %d", ms, mtotal)
	}
	if _, _, err := m.RangeMeta(1, 0, 1000, 0); err != ErrInvalid {
		t.Fatal("cap 0 must be invalid")
	}
	if ms, _, _ := m.RangeMeta(9, 0, 1000, 5); len(ms) != 0 {
		t.Fatal("unknown stream must be empty")
	}
}

func TestPruneThinning(t *testing.T) {
	m := NewFake(StoreConfig{RetentionNs: 100e9, ThinAfterNs: 10e9, ThinnedIntervalNs: 1e9})
	for i := int64(0); i < 40; i++ { // 10 fps over 4s
		_ = m.Push(mk(1, i*100e6+1))
	}
	_ = m.Prune(20e9)
	s, _ := m.Stats()
	if s.Frames != 4 || s.ThinnedTotal != 36 {
		t.Fatalf("%+v", s)
	}
	_ = m.Prune(200e9)
	s, _ = m.Stats()
	if s.Frames != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestFakeCaptureAndMotion(t *testing.T) {
	m := NewFake(StoreConfig{})
	c, err := m.OpenCapture("fake://16x16@50")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	a, _ := c.Read()
	b, _ := c.Read()
	if s := m.MotionScore(a, b); s < 0.5 {
		t.Fatalf("score %v", s)
	}
	if s := m.MotionScore(a, a); s != 0 {
		t.Fatal(s)
	}
	if _, err := m.OpenCapture("file:///x.mp4"); err == nil {
		t.Fatal("file should be unsupported")
	}
	done := make(chan struct{})
	go func() { _, _ = c.Read(); _, _ = c.Read(); close(done) }()
	_ = c.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Read")
	}
	if _, err := m.EncodeJPEG(a, 80); err != nil {
		t.Fatal(err)
	}
}
