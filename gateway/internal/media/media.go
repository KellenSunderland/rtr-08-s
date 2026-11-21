// Package media abstracts the mediacore C library behind an interface so the
// rest of the gateway (and all tests) can run against a pure-Go fake.
package media

import (
	"errors"
)

// PixFmt mirrors mc_pixfmt.
type PixFmt int

const (
	RGB8  PixFmt = 1
	BGR8  PixFmt = 2
	GRAY8 PixFmt = 3
)

// BPP returns bytes per pixel.
func (f PixFmt) BPP() int {
	if f == GRAY8 {
		return 1
	}
	return 3
}

// Frame is a Go-owned copy of a frame. Frames returned from a Media are
// immutable by convention; do not modify Data.
type Frame struct {
	Data     []byte
	W, H     int
	Stride   int
	Fmt      PixFmt
	TsNs     int64 // monotonic
	WallNs   int64 // unix epoch
	StreamID uint32
	Seq      uint64
}

// Errors.
var (
	ErrNotFound    = errors.New("media: not found")
	ErrOutOfOrder  = errors.New("media: frame ts not strictly increasing")
	ErrInvalid     = errors.New("media: invalid argument")
	ErrUnsupported = errors.New("media: unsupported")
)

// StoreConfig mirrors mc_frame_store_config (nanoseconds / bytes).
type StoreConfig struct {
	RetentionNs       int64
	ThinAfterNs       int64
	ThinnedIntervalNs int64
	MaxBytes          int64
}

// DefaultStoreConfig matches mc_frame_store_config_default.
func DefaultStoreConfig() StoreConfig {
	return StoreConfig{
		RetentionNs:       3600e9,
		ThinAfterNs:       300e9,
		ThinnedIntervalNs: 1e9,
		MaxBytes:          2 << 30,
	}
}

// Stats mirrors mc_frame_store_stats.
type Stats struct {
	Frames       int
	Bytes        int64
	Streams      int
	OldestTsNs   int64
	NewestTsNs   int64
	EvictedTotal uint64
	ThinnedTotal uint64
}

// FrameMeta is a stored frame's metadata, without pixel data.
type FrameMeta struct {
	TsNs   int64 // monotonic
	WallNs int64 // unix epoch
	W, H   int
	Seq    uint64
}

// Capture is a single-threaded frame source. Read blocks until the next frame
// and returns io.EOF at end of stream. Close may be called concurrently with
// Read to unblock it.
type Capture interface {
	Read() (Frame, error)
	FPS() float64
	Close() error
}

// Media is everything the gateway needs from mediacore.
type Media interface {
	// Frame store.
	Push(f Frame) error
	Latest(streamID uint32) (Frame, error)
	// Nearest returns the frame nearest tsNs (ties -> earlier); maxDistNs 0 = unlimited.
	Nearest(streamID uint32, tsNs, maxDistNs int64) (Frame, error)
	// Range returns frames with from <= ts < to, oldest first, at most cap, and the total matched.
	Range(streamID uint32, fromNs, toNs int64, cap int) ([]Frame, int, error)
	Prune(nowNs int64) error
	RemoveStream(streamID uint32) error
	Stats() (Stats, error)
	// RangeMeta is Range without the pixel copies: metadata only, for indexing.
	RangeMeta(streamID uint32, fromNs, toNs int64, cap int) ([]FrameMeta, int, error)

	// Image ops.
	MotionScore(a, b Frame) float64 // negative on error
	EncodeJPEG(f Frame, quality int) ([]byte, error)
	Resize(f Frame, w, h int) (Frame, error)

	// Capture.
	OpenCapture(uri string) (Capture, error)

	// Clocks.
	NowMonotonicNs() int64
	NowWallNs() int64

	Close()
}
