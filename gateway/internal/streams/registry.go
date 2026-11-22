// Package streams owns stream lifecycle: capture goroutines, state, fps and pruning.
package streams

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/argus-video/argus/gateway/internal/auth"
	"github.com/argus-video/argus/gateway/internal/media"
	"github.com/argus-video/argus/gateway/internal/protocol"
)

// DefaultStallAfter is how long without a frame before a live stream is "stalled".
const DefaultStallAfter = 5 * time.Second

var (
	ErrExists   = errors.New("stream id already exists")
	ErrNotFound = errors.New("stream not found")
	ErrQuota    = errors.New("stream quota exceeded")
)

// Stream is one registered capture.
type Stream struct {
	ID, Name, URI string
	Owner         *auth.Principal
	Idx           uint32 // numeric id used in the frame store and frame protocol

	r      *Registry
	cancel context.CancelFunc
	done   chan struct{}

	mu        sync.Mutex
	state     protocol.StreamState
	lastMono  time.Time // process-local time of last frame (for stall detection)
	lastWall  int64
	fps       float64
	lastTsNs  int64
	seq       uint64
	frames    uint64
	dropped   uint64
	errMsg    string
	subs      map[int]chan media.Frame
	nextSubID int
}

// Registry holds all streams.
type Registry struct {
	// StallAfter may be lowered (before streams start) in tests.
	StallAfter time.Duration

	m      media.Media
	policy URIPolicy
	log    *slog.Logger
	origin int64 // gateway monotonic epoch (ns)

	mu      sync.Mutex
	streams map[string]*Stream
	nextIdx uint32
	closed  bool

	stalled atomic.Int32 // number of streams currently stalled (gauge)

	wmu      sync.Mutex
	watchers map[int]chan protocol.StreamInfo
	nextW    int

	wg sync.WaitGroup
}

// NewRegistry creates a registry. Call Start to run the prune/stall ticker.
func NewRegistry(m media.Media, policy URIPolicy, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{StallAfter: DefaultStallAfter, m: m, policy: policy, log: log, origin: m.NowMonotonicNs(), streams: map[string]*Stream{}, nextIdx: 1, watchers: map[int]chan protocol.StreamInfo{}}
}

// OriginNs is the gateway-level clock origin; ts_ms in REST/frames is relative to it
// unless a session is specified.
func (r *Registry) OriginNs() int64 { return r.origin }

// Media returns the underlying media implementation.
func (r *Registry) Media() media.Media { return r.m }

// Start runs the 1s prune + stall check loop until ctx is done.
func (r *Registry) Start(ctx context.Context) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := r.m.Prune(r.m.NowMonotonicNs()); err != nil {
					r.log.Warn("prune failed", "err", err)
				}
				r.CheckStalls()
			}
		}
	}()
}

// Stalled returns the number of streams currently in the stalled state.
func (r *Registry) Stalled() int { return int(r.stalled.Load()) }

// refreshStalled recounts stalled streams for the stalled-streams gauge.
func (r *Registry) refreshStalled() {
	r.mu.Lock()
	all := make([]*Stream, 0, len(r.streams))
	for _, s := range r.streams {
		all = append(all, s)
	}
	r.mu.Unlock()
	var n int32
	for _, s := range all {
		if s.State() == protocol.StateStalled {
			n++
		}
	}
	r.stalled.Store(n)
}

// stallThreshold is how long without a frame counts as a stall. Slow sources
// (e.g. 1 fps) legitimately go several seconds between frames, so the base
// threshold is stretched to four frame intervals when that is longer.
// Caller holds s.mu.
func (s *Stream) stallThreshold(base time.Duration) time.Duration {
	if s.fps > 0 {
		if iv := time.Duration(4 * float64(time.Second) / s.fps); iv > base {
			return iv
		}
	}
	return base
}

// CheckStalls marks live streams with no recent frame as stalled.
func (r *Registry) CheckStalls() {
	r.mu.Lock()
	all := make([]*Stream, 0, len(r.streams))
	for _, s := range r.streams {
		all = append(all, s)
	}
	r.mu.Unlock()
	for _, s := range all {
		s.mu.Lock()
		stalled := s.state == protocol.StateLive && time.Since(s.lastMono) > s.stallThreshold(r.StallAfter)
		if stalled {
			s.state = protocol.StateStalled
		}
		s.mu.Unlock()
		if stalled {
			r.notify(s)
		}
	}
	r.refreshStalled()
}

// Create validates the URI, reserves quota and starts the capture goroutine.
func (r *Registry) Create(owner *auth.Principal, id, name, uri string) (*Stream, error) {
	if !protocol.ValidStreamID(id) {
		return nil, &URIError{"invalid stream id"}
	}
	if len(name) > 100 {
		return nil, &URIError{"name too long"}
	}
	capURI, err := r.policy.Validate(uri)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("shutting down")
	}
	if _, ok := r.streams[id]; ok {
		r.mu.Unlock()
		return nil, ErrExists
	}
	if owner != nil && !owner.Acquire(auth.Streams) {
		r.mu.Unlock()
		return nil, ErrQuota
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Stream{ID: id, Name: name, URI: uri, Owner: owner, Idx: r.nextIdx, r: r, cancel: cancel, done: make(chan struct{}),
		state: protocol.StateStarting, subs: map[int]chan media.Frame{}}
	r.nextIdx++
	r.streams[id] = s
	r.mu.Unlock()
	go s.run(ctx, capURI)
	return s, nil
}

// Get returns a stream by id.
func (r *Registry) Get(id string) (*Stream, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.streams[id]
	return s, ok
}

// List returns streams owned by owner (all if owner is nil), sorted by id.
func (r *Registry) List(owner *auth.Principal) []*Stream {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Stream
	for _, s := range r.streams {
		if owner == nil || s.Owner == owner {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Delete stops a stream, drops its frames and releases quota.
func (r *Registry) Delete(id string) error {
	r.mu.Lock()
	s, ok := r.streams[id]
	if ok {
		delete(r.streams, id)
	}
	r.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	s.stop()
	_ = r.m.RemoveStream(s.Idx)
	if s.Owner != nil {
		s.Owner.Release(auth.Streams)
	}
	s.setState(protocol.StateStopped, "")
	r.notify(s)
	return nil
}

// Close stops every stream and waits for goroutines.
func (r *Registry) Close() {
	r.mu.Lock()
	r.closed = true
	all := make([]*Stream, 0, len(r.streams))
	for _, s := range r.streams {
		all = append(all, s)
	}
	r.mu.Unlock()
	for _, s := range all {
		s.stop()
	}
	r.wg.Wait()
}

// Watch subscribes to stream state changes (non-blocking fanout; slow watchers miss updates).
func (r *Registry) Watch() (<-chan protocol.StreamInfo, func()) {
	ch := make(chan protocol.StreamInfo, 32)
	r.wmu.Lock()
	id := r.nextW
	r.nextW++
	r.watchers[id] = ch
	r.wmu.Unlock()
	return ch, func() {
		r.wmu.Lock()
		delete(r.watchers, id)
		r.wmu.Unlock()
	}
}

func (r *Registry) notify(s *Stream) {
	info := s.Info()
	r.wmu.Lock()
	defer r.wmu.Unlock()
	for _, ch := range r.watchers {
		select {
		case ch <- info:
		default:
		}
	}
}

// ---- Stream

func (s *Stream) stop() {
	s.cancel()
	<-s.done
}

func (s *Stream) setState(st protocol.StreamState, msg string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == st {
		return false
	}
	s.state, s.errMsg = st, msg
	return true
}

// State returns the current state.
func (s *Stream) State() protocol.StreamState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Dropped returns the number of frames rejected by the store or dropped for slow subscribers.
func (s *Stream) Dropped() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Frames returns total frames captured.
func (s *Stream) Frames() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frames
}

// Info renders the wire StreamInfo.
func (s *Stream) Info() protocol.StreamInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := protocol.StreamInfo{ID: s.ID, Name: s.Name, URI: s.URI, State: s.state}
	if s.fps > 0 {
		f := float64(int(s.fps*100+0.5)) / 100
		info.FPS = &f
	}
	if s.lastWall > 0 {
		w := s.lastWall / 1e6
		info.LastFrameWallMs = &w
		age := time.Since(time.Unix(0, s.lastWall)).Milliseconds()
		info.LastFrameAgeMs = &age
	}
	info.FramesTotal, info.DroppedFrames = s.frames, s.dropped
	st, _ := s.r.m.Stats()
	stored := st.Frames
	info.StoredFrames = &stored
	return info
}

// Subscribe returns a channel of captured frames. Sends never block the capture
// goroutine: when the buffer is full the frame is dropped for this subscriber.
func (s *Stream) Subscribe() (<-chan media.Frame, func()) {
	ch := make(chan media.Frame, 2)
	s.mu.Lock()
	id := s.nextSubID
	s.nextSubID++
	s.subs[id] = ch
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, id)
		s.mu.Unlock()
	}
}

func (s *Stream) run(ctx context.Context, capURI string) {
	defer close(s.done)
	log := s.r.log.With("stream", s.ID)
	cp, err := s.r.m.OpenCapture(capURI)
	if err != nil {
		log.Warn("capture open failed", "err", err)
		s.setState(protocol.StateError, err.Error())
		s.r.notify(s)
		return
	}
	// Unblock a blocked Read on stop.
	go func() { <-ctx.Done(); _ = cp.Close() }()
	defer cp.Close()
	for ctx.Err() == nil {
		f, err := cp.Read()
		if err != nil {
			switch {
			case ctx.Err() != nil:
			case errors.Is(err, io.EOF):
				log.Info("capture ended")
				if s.setState(protocol.StateStopped, "") {
					s.r.notify(s)
				}
			default:
				log.Warn("capture read failed", "err", err)
				if s.setState(protocol.StateError, err.Error()) {
					s.r.notify(s)
				}
			}
			return
		}
		s.onFrame(f)
	}
}

func (s *Stream) onFrame(f media.Frame) {
	s.mu.Lock()
	s.seq++
	f.StreamID, f.Seq = s.Idx, s.seq
	s.frames++
	if s.lastTsNs > 0 && f.TsNs > s.lastTsNs {
		inst := 1e9 / float64(f.TsNs-s.lastTsNs)
		if s.fps == 0 {
			s.fps = inst
		} else {
			s.fps = 0.9*s.fps + 0.1*inst
		}
	}
	s.lastTsNs = f.TsNs
	s.lastMono = time.Now()
	s.lastWall = f.WallNs
	changed := s.state != protocol.StateLive
	s.state = protocol.StateLive
	s.mu.Unlock()

	if err := s.r.m.Push(f); err != nil {
		s.mu.Lock()
		s.dropped++
		s.mu.Unlock()
	}
	s.mu.Lock()
	for _, ch := range s.subs {
		select {
		case ch <- f:
		default:
			s.dropped++
		}
	}
	s.mu.Unlock()
	if changed {
		s.r.notify(s)
	}
	s.r.refreshStalled()
}

// Done is closed when the capture goroutine has exited.
func (s *Stream) Done() <-chan struct{} { return s.done }
