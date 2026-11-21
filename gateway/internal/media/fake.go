//go:build !(cgo && mediacore)

package media

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Fake is an in-memory Media with simplified retention/thinning semantics.
type Fake struct {
	cfg   StoreConfig
	start time.Time

	mu      sync.Mutex
	streams map[uint32][]Frame // sorted by TsNs
	last    map[uint32]int64
	bytes   int64
	evicted uint64
	thinned uint64
}

// New returns the default Media implementation for this build (the fake).
func New(cfg StoreConfig) (Media, error) { return NewFake(cfg), nil }

// Real reports whether this build links mediacore.
const Real = false

// NewFake creates a Fake store.
func NewFake(cfg StoreConfig) *Fake {
	d := DefaultStoreConfig()
	if cfg.RetentionNs <= 0 {
		cfg.RetentionNs = d.RetentionNs
	}
	if cfg.ThinAfterNs <= 0 {
		cfg.ThinAfterNs = d.ThinAfterNs
	}
	if cfg.ThinnedIntervalNs <= 0 {
		cfg.ThinnedIntervalNs = d.ThinnedIntervalNs
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = d.MaxBytes
	}
	return &Fake{cfg: cfg, start: time.Now(), streams: map[uint32][]Frame{}, last: map[uint32]int64{}}
}

func (m *Fake) NowMonotonicNs() int64 { return int64(time.Since(m.start)) + 1 }
func (m *Fake) NowWallNs() int64      { return time.Now().UnixNano() }
func (m *Fake) Close()                {}

func validFrame(f Frame) error {
	if f.W <= 0 || f.H <= 0 || f.Stride < f.W*f.Fmt.BPP() || f.Fmt < RGB8 || f.Fmt > GRAY8 {
		return ErrInvalid
	}
	if len(f.Data) < f.Stride*(f.H-1)+f.W*f.Fmt.BPP() {
		return ErrInvalid
	}
	return nil
}

func (m *Fake) Push(f Frame) error {
	if err := validFrame(f); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.last[f.StreamID]; ok && f.TsNs <= l {
		return ErrOutOfOrder
	}
	f.Data = append([]byte(nil), f.Data...)
	m.streams[f.StreamID] = append(m.streams[f.StreamID], f)
	m.last[f.StreamID] = f.TsNs
	m.bytes += int64(len(f.Data))
	m.evictLocked()
	return nil
}

func (m *Fake) evictLocked() {
	for m.bytes > m.cfg.MaxBytes {
		var oid uint32
		var oldest int64 = -1
		for id, fs := range m.streams {
			if len(fs) > 0 && (oldest < 0 || fs[0].TsNs < oldest) {
				oldest, oid = fs[0].TsNs, id
			}
		}
		if oldest < 0 {
			return
		}
		m.bytes -= int64(len(m.streams[oid][0].Data))
		m.streams[oid] = m.streams[oid][1:]
		m.evicted++
	}
}

func (m *Fake) Latest(id uint32) (Frame, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fs := m.streams[id]
	if len(fs) == 0 {
		return Frame{}, ErrNotFound
	}
	return fs[len(fs)-1], nil
}

func (m *Fake) Nearest(id uint32, ts, maxDist int64) (Frame, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fs := m.streams[id]
	if len(fs) == 0 {
		return Frame{}, ErrNotFound
	}
	i := sort.Search(len(fs), func(i int) bool { return fs[i].TsNs >= ts })
	best := -1
	if i < len(fs) {
		best = i
	}
	if i > 0 {
		if best < 0 || ts-fs[i-1].TsNs <= fs[best].TsNs-ts { // ties -> earlier
			best = i - 1
		}
	}
	d := fs[best].TsNs - ts
	if d < 0 {
		d = -d
	}
	if maxDist > 0 && d > maxDist {
		return Frame{}, ErrNotFound
	}
	return fs[best], nil
}

func (m *Fake) Range(id uint32, from, to int64, cap int) ([]Frame, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fs := m.streams[id]
	i := sort.Search(len(fs), func(i int) bool { return fs[i].TsNs >= from })
	var out []Frame
	total := 0
	for ; i < len(fs) && fs[i].TsNs < to; i++ {
		total++
		if len(out) < cap {
			out = append(out, fs[i])
		}
	}
	return out, total, nil
}

func (m *Fake) Prune(now int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, fs := range m.streams {
		var kept []Frame
		var lastKept int64 = -1 << 62
		for _, f := range fs {
			age := now - f.TsNs
			if age > m.cfg.RetentionNs {
				m.bytes -= int64(len(f.Data))
				m.evicted++
				continue
			}
			if age > m.cfg.ThinAfterNs {
				if f.TsNs-lastKept < m.cfg.ThinnedIntervalNs {
					m.bytes -= int64(len(f.Data))
					m.thinned++
					continue
				}
			}
			lastKept = f.TsNs
			kept = append(kept, f)
		}
		m.streams[id] = kept
	}
	return nil
}

func (m *Fake) RemoveStream(id uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.streams[id] {
		m.bytes -= int64(len(f.Data))
	}
	delete(m.streams, id)
	delete(m.last, id)
	return nil
}

func (m *Fake) Stats() (Stats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Stats{Bytes: m.bytes, EvictedTotal: m.evicted, ThinnedTotal: m.thinned}
	for _, fs := range m.streams {
		if len(fs) == 0 {
			continue
		}
		s.Streams++
		s.Frames += len(fs)
		if s.OldestTsNs == 0 || fs[0].TsNs < s.OldestTsNs {
			s.OldestTsNs = fs[0].TsNs
		}
		if l := fs[len(fs)-1].TsNs; l > s.NewestTsNs {
			s.NewestTsNs = l
		}
	}
	return s, nil
}

// RangeMeta walks the stream's frames without copying pixel data.
func (m *Fake) RangeMeta(id uint32, from, to int64, cap int) ([]FrameMeta, int, error) {
	if cap <= 0 {
		return nil, 0, ErrInvalid
	}
	fs := m.streams[id]
	i := sort.Search(len(fs), func(i int) bool { return fs[i].TsNs >= from })
	var out []FrameMeta
	total := 0
	for ; i < len(fs) && fs[i].TsNs <= to; i++ {
		total++
		if len(out) < cap {
			f := &fs[i]
			out = append(out, FrameMeta{TsNs: f.TsNs, WallNs: f.WallNs, W: f.W, H: f.H, Seq: f.Seq})
		}
	}
	return out, total, nil
}

// gray returns the luma at pixel (x,y).
func gray(f Frame, x, y int) int {
	o := y*f.Stride + x*f.Fmt.BPP()
	if f.Fmt == GRAY8 {
		return int(f.Data[o])
	}
	return (int(f.Data[o])*30 + int(f.Data[o+1])*59 + int(f.Data[o+2])*11) / 100
}

func (m *Fake) MotionScore(a, b Frame) float64 {
	if a.W != b.W || a.H != b.H || validFrame(a) != nil || validFrame(b) != nil {
		return -1
	}
	var sum int64
	for y := 0; y < a.H; y++ {
		for x := 0; x < a.W; x++ {
			d := gray(a, x, y) - gray(b, x, y)
			if d < 0 {
				d = -d
			}
			sum += int64(d)
		}
	}
	return float64(sum) / float64(a.W*a.H) / 255.0
}

func (m *Fake) EncodeJPEG(f Frame, quality int) ([]byte, error) {
	if err := validFrame(f); err != nil {
		return nil, err
	}
	if quality < 1 || quality > 100 {
		return nil, ErrInvalid
	}
	img := image.NewRGBA(image.Rect(0, 0, f.W, f.H))
	for y := 0; y < f.H; y++ {
		for x := 0; x < f.W; x++ {
			o := y*f.Stride + x*f.Fmt.BPP()
			var c color.RGBA
			switch f.Fmt {
			case RGB8:
				c = color.RGBA{f.Data[o], f.Data[o+1], f.Data[o+2], 255}
			case BGR8:
				c = color.RGBA{f.Data[o+2], f.Data[o+1], f.Data[o], 255}
			default:
				c = color.RGBA{f.Data[o], f.Data[o], f.Data[o], 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (m *Fake) Resize(f Frame, w, h int) (Frame, error) {
	if err := validFrame(f); err != nil || w <= 0 || h <= 0 {
		return Frame{}, ErrInvalid
	}
	bpp := f.Fmt.BPP()
	out := f
	out.W, out.H, out.Stride = w, h, w*bpp
	out.Data = make([]byte, out.Stride*h)
	for y := 0; y < h; y++ {
		sy := y * f.H / h
		for x := 0; x < w; x++ {
			sx := x * f.W / w
			copy(out.Data[y*out.Stride+x*bpp:][:bpp], f.Data[sy*f.Stride+sx*bpp:][:bpp])
		}
	}
	return out, nil
}

var fakeURIRe = regexp.MustCompile(`^fake://(\d+)x(\d+)@(\d+(?:\.\d+)?)$`)

// OpenCapture supports fake://<w>x<h>@<fps>[?pattern=moving|static|error|eof&frames=N&stall_after=N].
// "stall_after=N" stops producing frames after N frames (blocks until closed).
func (m *Fake) OpenCapture(uri string) (Capture, error) {
	base, query, _ := cutQuery(uri)
	mt := fakeURIRe.FindStringSubmatch(base)
	if mt == nil {
		return nil, fmt.Errorf("%w: fake media only supports fake://<w>x<h>@<fps> URIs", ErrUnsupported)
	}
	w, _ := strconv.Atoi(mt[1])
	h, _ := strconv.Atoi(mt[2])
	fps, _ := strconv.ParseFloat(mt[3], 64)
	if w <= 0 || h <= 0 || w > 4096 || h > 4096 || fps <= 0 || fps > 240 {
		return nil, ErrInvalid
	}
	q, _ := url.ParseQuery(query)
	c := &fakeCapture{m: m, w: w, h: h, fps: fps, pattern: q.Get("pattern"), done: make(chan struct{})}
	if c.pattern == "" {
		c.pattern = "moving"
	}
	c.maxFrames, _ = strconv.Atoi(q.Get("frames"))
	c.stallAfter, _ = strconv.Atoi(q.Get("stall_after"))
	if c.pattern == "error" {
		return nil, fmt.Errorf("%w: simulated open failure", ErrInvalid)
	}
	c.next = time.Now()
	return c, nil
}

func cutQuery(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '?' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

type fakeCapture struct {
	m                     *Fake
	w, h                  int
	fps                   float64
	pattern               string
	maxFrames, stallAfter int
	n                     int
	next                  time.Time
	once                  sync.Once
	done                  chan struct{}
}

func (c *fakeCapture) FPS() float64 { return c.fps }
func (c *fakeCapture) Close() error { c.once.Do(func() { close(c.done) }); return nil }

func (c *fakeCapture) Read() (Frame, error) {
	if c.maxFrames > 0 && c.n >= c.maxFrames {
		return Frame{}, io.EOF
	}
	if c.stallAfter > 0 && c.n >= c.stallAfter {
		<-c.done
		return Frame{}, io.EOF
	}
	if d := time.Until(c.next); d > 0 {
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-c.done:
			t.Stop()
			return Frame{}, io.EOF
		}
	}
	select {
	case <-c.done:
		return Frame{}, io.EOF
	default:
	}
	c.next = c.next.Add(time.Duration(float64(time.Second) / c.fps))
	c.n++
	f := Frame{W: c.w, H: c.h, Stride: c.w * 3, Fmt: BGR8, TsNs: c.m.NowMonotonicNs(), WallNs: c.m.NowWallNs()}
	f.Data = make([]byte, f.Stride*c.h)
	switch c.pattern {
	case "static":
		for i := range f.Data {
			f.Data[i] = 100
		}
	default: // moving: alternate between dark and bright so consecutive frames differ strongly
		v := byte(20)
		if c.n%2 == 0 {
			v = 220
		}
		for i := range f.Data {
			f.Data[i] = v
		}
	}
	return f, nil
}
