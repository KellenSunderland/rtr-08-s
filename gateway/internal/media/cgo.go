//go:build cgo && mediacore

package media

/*
#cgo CFLAGS: -I${SRCDIR}/../../../mediacore/include
#cgo LDFLAGS: -L${SRCDIR}/../../../mediacore/build -L${SRCDIR}/../../../mediacore/build/lib -lmediacore -Wl,-rpath,${SRCDIR}/../../../mediacore/build -Wl,-rpath,${SRCDIR}/../../../mediacore/build/lib
#include <stdlib.h>
#include <string.h>
#include "mediacore_c.h"

// Helper: build a store config without Go having to know struct layout details.
static void gw_store_cfg(mc_frame_store_config* c, int64_t ret, int64_t thin, int64_t interval, size_t maxb) {
	mc_frame_store_config_default(c);
	if (ret > 0) c->retention_ns = ret;
	if (thin > 0) c->thin_after_ns = thin;
	if (interval > 0) c->thinned_interval_ns = interval;
	if (maxb > 0) c->max_bytes = maxb;
}
*/
import "C"

import (
	"fmt"
	"io"
	"sync"
	"unsafe"
)

// Real reports whether this build links mediacore.
const Real = true

// CGo is the mediacore-backed Media.
type CGo struct {
	store *C.mc_frame_store
}

// New creates a mediacore-backed Media.
func New(cfg StoreConfig) (Media, error) {
	var c C.mc_frame_store_config
	C.gw_store_cfg(&c, C.int64_t(cfg.RetentionNs), C.int64_t(cfg.ThinAfterNs), C.int64_t(cfg.ThinnedIntervalNs), C.size_t(cfg.MaxBytes))
	var s *C.mc_frame_store
	if st := C.mc_frame_store_create(&c, &s); st != C.MC_OK {
		return nil, statusErr(st)
	}
	return &CGo{store: s}, nil
}

func statusErr(st C.mc_status) error {
	switch st {
	case C.MC_OK:
		return nil
	case C.MC_ERR_NOT_FOUND:
		return ErrNotFound
	case C.MC_ERR_EOF:
		return io.EOF
	case C.MC_ERR_UNSUPPORTED:
		return fmt.Errorf("%w: %s", ErrUnsupported, C.GoString(C.mc_last_error()))
	case C.MC_ERR_INVALID_ARG:
		return fmt.Errorf("%w: %s", ErrInvalid, C.GoString(C.mc_last_error()))
	}
	return fmt.Errorf("mediacore status %d: %s", int(st), C.GoString(C.mc_last_error()))
}

// toC copies f into C-allocated memory (Go pointers must not be stored in C structs
// passed to C). The returned func frees the copy.
func toC(f Frame) (C.mc_frame, func(), error) {
	if err := validFrameCgo(f); err != nil {
		return C.mc_frame{}, nil, err
	}
	n := f.Stride * f.H
	p := C.malloc(C.size_t(n))
	if p == nil {
		return C.mc_frame{}, nil, fmt.Errorf("malloc failed")
	}
	copy(unsafe.Slice((*byte)(p), n), f.Data[:n])
	cf := C.mc_frame{
		data:      (*C.uint8_t)(p),
		width:     C.int32_t(f.W),
		height:    C.int32_t(f.H),
		stride:    C.int32_t(f.Stride),
		fmt:       C.mc_pixfmt(f.Fmt),
		ts_ns:     C.int64_t(f.TsNs),
		wall_ns:   C.int64_t(f.WallNs),
		stream_id: C.uint32_t(f.StreamID),
		seq:       C.uint64_t(f.Seq),
	}
	return cf, func() { C.free(p) }, nil
}

func validFrameCgo(f Frame) error {
	if f.W <= 0 || f.H <= 0 || f.Fmt < RGB8 || f.Fmt > GRAY8 || f.Stride < f.W*f.Fmt.BPP() || len(f.Data) < f.Stride*f.H {
		return ErrInvalid
	}
	return nil
}

// fromC deep-copies a C frame into Go memory.
func fromC(cf *C.mc_frame) Frame {
	n := int(cf.stride) * int(cf.height)
	return Frame{
		Data:     C.GoBytes(unsafe.Pointer(cf.data), C.int(n)),
		W:        int(cf.width),
		H:        int(cf.height),
		Stride:   int(cf.stride),
		Fmt:      PixFmt(cf.fmt),
		TsNs:     int64(cf.ts_ns),
		WallNs:   int64(cf.wall_ns),
		StreamID: uint32(cf.stream_id),
		Seq:      uint64(cf.seq),
	}
}

// fromView copies the leased frame into Go memory and releases the lease.
func fromView(v *C.mc_frame_view) Frame {
	f := fromC(v.frame)
	C.mc_frame_view_release(v)
	return f
}

func (m *CGo) Push(f Frame) error {
	cf, free, err := toC(f)
	if err != nil {
		return err
	}
	defer free()
	st := C.mc_frame_store_push(m.store, &cf)
	if st == C.MC_ERR_INVALID_ARG {
		return ErrOutOfOrder
	}
	return statusErr(st)
}

func (m *CGo) Latest(id uint32) (Frame, error) {
	var v C.mc_frame_view
	if st := C.mc_frame_store_latest(m.store, C.uint32_t(id), &v); st != C.MC_OK {
		return Frame{}, statusErr(st)
	}
	return fromView(&v), nil
}

func (m *CGo) Nearest(id uint32, ts, maxDist int64) (Frame, error) {
	var v C.mc_frame_view
	if st := C.mc_frame_store_nearest(m.store, C.uint32_t(id), C.int64_t(ts), C.int64_t(maxDist), &v); st != C.MC_OK {
		return Frame{}, statusErr(st)
	}
	return fromView(&v), nil
}

func (m *CGo) Range(id uint32, from, to int64, capN int) ([]Frame, int, error) {
	if capN <= 0 {
		return nil, 0, ErrInvalid
	}
	// Views live in C memory so the array holds no Go pointers.
	views := (*C.mc_frame_view)(C.calloc(C.size_t(capN), C.size_t(unsafe.Sizeof(C.mc_frame_view{}))))
	if views == nil {
		return nil, 0, fmt.Errorf("calloc failed")
	}
	defer C.free(unsafe.Pointer(views))
	var n, total C.size_t
	st := C.mc_frame_store_range(m.store, C.uint32_t(id), C.int64_t(from), C.int64_t(to), views, C.size_t(capN), &n, &total)
	if st != C.MC_OK {
		return nil, 0, statusErr(st)
	}
	vs := unsafe.Slice(views, int(n))
	out := make([]Frame, 0, int(n))
	for i := range vs {
		out = append(out, fromView(&vs[i]))
	}
	return out, int(total), nil
}

func (m *CGo) Prune(now int64) error {
	return statusErr(C.mc_frame_store_prune(m.store, C.int64_t(now)))
}
func (m *CGo) RemoveStream(id uint32) error {
	return statusErr(C.mc_frame_store_remove_stream(m.store, C.uint32_t(id)))
}

func (m *CGo) Stats() (Stats, error) {
	var s C.mc_frame_store_stats
	if st := C.mc_frame_store_stats_get(m.store, &s); st != C.MC_OK {
		return Stats{}, statusErr(st)
	}
	return Stats{
		Frames: int(s.frames), Bytes: int64(s.bytes), Streams: int(s.streams),
		OldestTsNs: int64(s.oldest_ts_ns), NewestTsNs: int64(s.newest_ts_ns),
		EvictedTotal: uint64(s.evicted_total), ThinnedTotal: uint64(s.thinned_total),
	}, nil
}

func (m *CGo) RangeMeta(id uint32, from, to int64, capN int) ([]FrameMeta, int, error) {
	if capN <= 0 {
		return nil, 0, ErrInvalid
	}
	views := (*C.mc_frame_view)(C.calloc(C.size_t(capN), C.size_t(unsafe.Sizeof(C.mc_frame_view{}))))
	if views == nil {
		return nil, 0, fmt.Errorf("calloc failed")
	}
	defer C.free(unsafe.Pointer(views))
	var n, total C.size_t
	st := C.mc_frame_store_range(m.store, C.uint32_t(id), C.int64_t(from), C.int64_t(to), views, C.size_t(capN), &n, &total)
	if st != C.MC_OK {
		return nil, 0, statusErr(st)
	}
	out := make([]FrameMeta, 0, int(n))
	for _, v := range unsafe.Slice(views, int(n)) {
		f := v.frame
		out = append(out, FrameMeta{
			TsNs: int64(f.ts_ns), WallNs: int64(f.wall_ns),
			W: int(f.width), H: int(f.height), Seq: uint64(f.seq),
		})
	}
	return out, int(total), nil
}

func (m *CGo) MotionScore(a, b Frame) float64 {
	ca, fa, err := toC(a)
	if err != nil {
		return -1
	}
	defer fa()
	cb, fb, err := toC(b)
	if err != nil {
		return -1
	}
	defer fb()
	var score C.double
	if st := C.mc_motion_score(&ca, &cb, &score); st != C.MC_OK {
		return -1
	}
	return float64(score)
}

func (m *CGo) EncodeJPEG(f Frame, quality int) ([]byte, error) {
	cf, free, err := toC(f)
	if err != nil {
		return nil, err
	}
	defer free()
	var buf *C.uint8_t
	var l C.size_t
	if st := C.mc_encode_jpeg(&cf, C.int32_t(quality), &buf, &l); st != C.MC_OK {
		return nil, statusErr(st)
	}
	defer C.mc_buffer_free(buf)
	return C.GoBytes(unsafe.Pointer(buf), C.int(l)), nil
}

func (m *CGo) Resize(f Frame, w, h int) (Frame, error) {
	cf, free, err := toC(f)
	if err != nil {
		return Frame{}, err
	}
	defer free()
	var out C.mc_frame
	if st := C.mc_resize(&cf, C.int32_t(w), C.int32_t(h), C.MC_INTERP_AREA, &out); st != C.MC_OK {
		return Frame{}, statusErr(st)
	}
	defer C.mc_frame_free(&out)
	r := fromC(&out)
	r.TsNs, r.WallNs, r.StreamID, r.Seq = f.TsNs, f.WallNs, f.StreamID, f.Seq
	return r, nil
}

func (m *CGo) NowMonotonicNs() int64 { return int64(C.mc_now_monotonic_ns()) }
func (m *CGo) NowWallNs() int64      { return int64(C.mc_now_wall_ns()) }

func (m *CGo) Close() {
	if m.store != nil {
		C.mc_frame_store_destroy(m.store)
		m.store = nil
	}
}

type cgoCapture struct {
	mu     sync.Mutex // capture handles are single-threaded
	h      *C.mc_capture
	closed bool
}

func (m *CGo) OpenCapture(uri string) (Capture, error) {
	cs := C.CString(uri)
	defer C.free(unsafe.Pointer(cs))
	var h *C.mc_capture
	if st := C.mc_capture_open(cs, &h); st != C.MC_OK {
		return nil, statusErr(st)
	}
	return &cgoCapture{h: h}, nil
}

func (c *cgoCapture) Read() (Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Frame{}, io.EOF
	}
	var out C.mc_frame
	if st := C.mc_capture_read(c.h, &out); st != C.MC_OK {
		return Frame{}, statusErr(st)
	}
	defer C.mc_frame_free(&out)
	return fromC(&out), nil
}

func (c *cgoCapture) FPS() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0
	}
	return float64(C.mc_capture_fps(c.h))
}

// Close waits for an in-flight Read (the handle is single-threaded) and frees the handle.
func (c *cgoCapture) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		C.mc_capture_close(c.h)
	}
	return nil
}
