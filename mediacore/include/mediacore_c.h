/*
 * mediacore C ABI — stable boundary used by the Go gateway (cgo) and the
 * Python agent (ctypes). Everything here is plain C; the implementation is C++20.
 *
 * Conventions
 * -----------
 * - All functions return mc_status unless documented otherwise. On error, a
 *   thread-local message is available via mc_last_error().
 * - Timestamps: `ts_ns` is a MONOTONIC clock in nanoseconds (never wall time);
 *   `wall_ns` is Unix epoch nanoseconds captured at the same instant. Callers
 *   must never mix the two domains when computing durations.
 * - Memory: objects created with *_create are freed with the matching *_destroy.
 *   Frames allocated with mc_frame_alloc are freed with mc_frame_free. Buffers
 *   returned via uint8_t** are freed with mc_buffer_free.
 * - Thread safety: a frame store, audio ring, VAD, and jitter buffer may each be
 *   used from multiple threads concurrently (internally locked). Capture handles
 *   are single-threaded.
 * - All sizes/dimensions are validated; overflow in width*height*channels must
 *   be rejected with MC_ERR_INVALID_ARG rather than wrapping.
 */
#ifndef MEDIACORE_C_H
#define MEDIACORE_C_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#if defined(_WIN32)
#define MC_API __declspec(dllexport)
#else
#define MC_API __attribute__((visibility("default")))
#endif

/* ---------------------------------------------------------------- status */

typedef enum mc_status {
    MC_OK = 0,
    MC_ERR_INVALID_ARG = 1,
    MC_ERR_NOT_FOUND = 2,
    MC_ERR_IO = 3,
    MC_ERR_FULL = 4,
    MC_ERR_EOF = 5,
    MC_ERR_UNSUPPORTED = 6,
    MC_ERR_INTERNAL = 7
} mc_status;

/* Library version string, e.g. "0.1.0". Never NULL. */
MC_API const char* mc_version(void);
/* Thread-local human-readable message for the last error on this thread.
 * Returns "" if none. Pointer valid until the next mediacore call on this thread. */
MC_API const char* mc_last_error(void);

/* ---------------------------------------------------------------- frames */

typedef enum mc_pixfmt {
    MC_PIXFMT_RGB8 = 1,  /* 3 bytes/pixel, R,G,B */
    MC_PIXFMT_BGR8 = 2,  /* 3 bytes/pixel, B,G,R (OpenCV native) */
    MC_PIXFMT_GRAY8 = 3  /* 1 byte/pixel */
} mc_pixfmt;

/* A frame owned by the caller (data allocated with mc_frame_alloc or by caller). */
typedef struct mc_frame {
    uint8_t* data;      /* row-major, `stride` bytes per row */
    int32_t width;
    int32_t height;
    int32_t stride;     /* bytes per row, >= width * bytes_per_pixel(fmt) */
    mc_pixfmt fmt;
    int64_t ts_ns;      /* monotonic */
    int64_t wall_ns;    /* unix epoch */
    uint32_t stream_id;
    uint64_t seq;       /* per-stream increasing sequence number */
} mc_frame;

/* Allocates `data` for the given geometry (stride = width * bpp) and fills the
 * geometry fields; other fields are zeroed. */
MC_API mc_status mc_frame_alloc(int32_t width, int32_t height, mc_pixfmt fmt, mc_frame* out);
/* Frees frame->data and zeroes the struct. Safe on a zeroed struct. */
MC_API void mc_frame_free(mc_frame* frame);
/* Deep copy; `out` is allocated by this call. */
MC_API mc_status mc_frame_copy(const mc_frame* in, mc_frame* out);
MC_API int32_t mc_pixfmt_bpp(mc_pixfmt fmt);
MC_API void mc_buffer_free(uint8_t* buf);

/* --------------------------------------------------------- image ops */

typedef enum mc_interp { MC_INTERP_NEAREST = 0, MC_INTERP_BILINEAR = 1, MC_INTERP_AREA = 2 } mc_interp;

/* out is allocated by the call. */
MC_API mc_status mc_resize(const mc_frame* in, int32_t width, int32_t height, mc_interp interp, mc_frame* out);
/* Scales to fit inside width x height preserving aspect, pads with `pad_value`. out allocated. */
MC_API mc_status mc_letterbox(const mc_frame* in, int32_t width, int32_t height, uint8_t pad_value, mc_frame* out);
MC_API mc_status mc_to_gray(const mc_frame* in, mc_frame* out);
MC_API mc_status mc_convert(const mc_frame* in, mc_pixfmt fmt, mc_frame* out);
/* Crop region [x, x+w) x [y, y+h); must be fully inside the frame. out allocated. */
MC_API mc_status mc_crop(const mc_frame* in, int32_t x, int32_t y, int32_t w, int32_t h, mc_frame* out);
/* Mean absolute per-pixel difference of the grayscale images, normalized to [0,1].
 * Frames must have the same geometry. */
MC_API mc_status mc_motion_score(const mc_frame* a, const mc_frame* b, double* score);
/* Fraction of pixels whose gray difference exceeds `threshold` (0..255), in [0,1]. */
MC_API mc_status mc_changed_fraction(const mc_frame* a, const mc_frame* b, uint8_t threshold, double* fraction);
/* JPEG encode; *buf allocated (free with mc_buffer_free). quality 1..100. */
MC_API mc_status mc_encode_jpeg(const mc_frame* in, int32_t quality, uint8_t** buf, size_t* len);
/* Decode JPEG/PNG/BMP/etc. (whatever OpenCV supports) into BGR8. out allocated. */
MC_API mc_status mc_decode_image(const uint8_t* buf, size_t len, mc_frame* out);

/* -------------------------------------------------------- frame store */
/*
 * Time-indexed store of frames for N streams with rolling retention
 * (default 1 hour). Frames older than `thin_after_ns` are thinned so that at
 * most one frame per `thinned_interval_ns` is kept; frames older than
 * `retention_ns` are dropped. Memory is capped at `max_bytes` (oldest frames
 * evicted first, across all streams).
 */
typedef struct mc_frame_store mc_frame_store;

typedef struct mc_frame_store_config {
    int64_t retention_ns;        /* default 3600e9 */
    int64_t thin_after_ns;       /* default 300e9 (5 min): full-rate before, thinned after */
    int64_t thinned_interval_ns; /* default 1e9 (1 fps) */
    size_t max_bytes;            /* default 2 GiB */
} mc_frame_store_config;

/* Fills `cfg` with defaults. */
MC_API void mc_frame_store_config_default(mc_frame_store_config* cfg);
MC_API mc_status mc_frame_store_create(const mc_frame_store_config* cfg, mc_frame_store** out);
MC_API void mc_frame_store_destroy(mc_frame_store* store);

/* Copies the frame into the store. Returns MC_ERR_INVALID_ARG if ts_ns is not
 * strictly greater than the stream's last ts_ns (out-of-order frames are rejected). */
MC_API mc_status mc_frame_store_push(mc_frame_store* store, const mc_frame* frame);

/* A read-only lease on a stored frame. Must be released with mc_frame_view_release
 * (the store will not evict a leased frame). */
typedef struct mc_frame_view {
    const mc_frame* frame;
    void* lease;  /* opaque */
} mc_frame_view;

MC_API void mc_frame_view_release(mc_frame_view* view);

/* Latest frame for a stream. MC_ERR_NOT_FOUND if none. */
MC_API mc_status mc_frame_store_latest(mc_frame_store* store, uint32_t stream_id, mc_frame_view* out);
/* Frame with ts_ns nearest to `ts_ns` (ties → earlier). MC_ERR_NOT_FOUND if the
 * stream has no frames within +/- `max_distance_ns` (0 → unlimited). */
MC_API mc_status mc_frame_store_nearest(mc_frame_store* store, uint32_t stream_id, int64_t ts_ns,
                                        int64_t max_distance_ns, mc_frame_view* out);
/* All frames with from_ns <= ts_ns < to_ns, oldest first, at most `cap`. `*n`
 * receives the count written; `*total` (optional) the total that matched. Views
 * must each be released. */
MC_API mc_status mc_frame_store_range(mc_frame_store* store, uint32_t stream_id, int64_t from_ns,
                                      int64_t to_ns, mc_frame_view* out, size_t cap, size_t* n,
                                      size_t* total);
/* Apply retention/thinning as of `now_ns`. Normally called by the owner once a second. */
MC_API mc_status mc_frame_store_prune(mc_frame_store* store, int64_t now_ns);
/* Drop all frames for a stream. */
MC_API mc_status mc_frame_store_remove_stream(mc_frame_store* store, uint32_t stream_id);

typedef struct mc_frame_store_stats {
    size_t frames;
    size_t bytes;
    size_t streams;
    int64_t oldest_ts_ns;  /* 0 if empty */
    int64_t newest_ts_ns;  /* 0 if empty */
    uint64_t evicted_total;
    uint64_t thinned_total;
} mc_frame_store_stats;

MC_API mc_status mc_frame_store_stats_get(mc_frame_store* store, mc_frame_store_stats* out);

typedef struct mc_stream_stats {
    size_t frames;
    size_t bytes;
    size_t leased_frames;     /* frames currently held by an outstanding lease */
    int64_t oldest_ts_ns;     /* 0 if empty */
    int64_t newest_ts_ns;     /* 0 if empty */
    size_t window_frames;     /* frames with now_ns - window_ns <= ts_ns < now_ns */
    size_t expiring_frames;   /* frames a prune at now_ns would drop by retention, i.e.
                                 ts_ns < now_ns - retention_ns (leases ignored) */
    uint64_t evicted_total;   /* retention + size evictions for this stream */
    uint64_t thinned_total;
    uint64_t store_queries;   /* stream_stats calls served by the store so far */
} mc_stream_stats;

/* Per-stream statistics as of `now_ns`. MC_ERR_NOT_FOUND if the stream is unknown. */
MC_API mc_status mc_frame_store_stream_stats(mc_frame_store* store, uint32_t stream_id,
                                             int64_t now_ns, int64_t window_ns,
                                             mc_stream_stats* out);

/* --------------------------------------------------------- capture */

typedef struct mc_capture mc_capture;
/* uri: file path, RTSP/HTTP URL, or "device:N" for a local camera. */
MC_API mc_status mc_capture_open(const char* uri, mc_capture** out);
/* Reads the next frame as BGR8 into `out` (allocated by this call). Fills ts_ns
 * (monotonic now) and wall_ns; stream_id/seq are left to the caller. MC_ERR_EOF at end. */
MC_API mc_status mc_capture_read(mc_capture* cap, mc_frame* out);
MC_API double mc_capture_fps(mc_capture* cap);  /* 0 if unknown */
MC_API void mc_capture_close(mc_capture* cap);

/* ----------------------------------------------------------- audio */

/* Lock-free-ish ring of interleaved int16 PCM samples (capacity in frames,
 * i.e. samples per channel). Overwrites oldest data when full and counts drops. */
typedef struct mc_audio_ring mc_audio_ring;
MC_API mc_status mc_audio_ring_create(int32_t sample_rate, int32_t channels, size_t capacity_frames,
                                      mc_audio_ring** out);
MC_API void mc_audio_ring_destroy(mc_audio_ring* ring);
MC_API mc_status mc_audio_ring_push(mc_audio_ring* ring, const int16_t* samples, size_t frames);
/* Pops up to `cap` frames; `*got` receives the count. */
MC_API mc_status mc_audio_ring_pop(mc_audio_ring* ring, int16_t* out, size_t cap, size_t* got);
MC_API size_t mc_audio_ring_available(mc_audio_ring* ring);
MC_API uint64_t mc_audio_ring_dropped(mc_audio_ring* ring);
MC_API void mc_audio_ring_clear(mc_audio_ring* ring);

/* Linear-interpolation resampler for mono int16 PCM. `*n_out` receives frames
 * written; MC_ERR_FULL if `cap` is too small (required size = ceil(n_in * out_rate / in_rate)). */
MC_API mc_status mc_resample_pcm16(const int16_t* in, size_t n_in, int32_t in_rate, int32_t out_rate,
                                   int16_t* out, size_t cap, size_t* n_out);
/* Saturating mix of two mono int16 buffers of length n. out may alias a or b. */
MC_API mc_status mc_mix_pcm16(const int16_t* a, const int16_t* b, size_t n, int16_t* out);
/* Apply gain (linear) with saturation, in place. */
MC_API mc_status mc_gain_pcm16(int16_t* samples, size_t n, float gain);
/* RMS energy in dBFS of a mono buffer (−inf → -120.0 for silence). */
MC_API mc_status mc_rms_dbfs(const int16_t* samples, size_t n, double* dbfs);

/* Energy-based voice activity detector. */
typedef struct mc_vad mc_vad;
typedef struct mc_vad_config {
    int32_t sample_rate;        /* e.g. 16000 */
    int32_t frame_ms;           /* analysis window, e.g. 20 */
    double threshold_dbfs;      /* speech if rms > threshold, e.g. -40.0 */
    int32_t min_speech_ms;      /* debounce before SPEECH_START, e.g. 100 */
    int32_t hangover_ms;        /* silence before SPEECH_END, e.g. 300 */
} mc_vad_config;
typedef enum mc_vad_event { MC_VAD_NONE = 0, MC_VAD_SPEECH_START = 1, MC_VAD_SPEECH_END = 2 } mc_vad_event;

MC_API void mc_vad_config_default(mc_vad_config* cfg);
MC_API mc_status mc_vad_create(const mc_vad_config* cfg, mc_vad** out);
MC_API void mc_vad_destroy(mc_vad* vad);
/* Feed mono samples (any length; internally framed). `*event` receives the
 * first state transition that occurred within this call (NONE if none).
 * `*in_speech` (optional) receives the state after processing. */
MC_API mc_status mc_vad_process(mc_vad* vad, const int16_t* samples, size_t n, mc_vad_event* event,
                                int32_t* in_speech);
MC_API void mc_vad_reset(mc_vad* vad);

/* Jitter buffer for sequenced audio packets. Reorders by seq, conceals gaps with
 * silence, and holds `target_delay_ms` of audio before playout starts. */
typedef struct mc_jitter_buffer mc_jitter_buffer;
MC_API mc_status mc_jitter_buffer_create(int32_t sample_rate, int32_t target_delay_ms, int32_t max_delay_ms,
                                         mc_jitter_buffer** out);
MC_API void mc_jitter_buffer_destroy(mc_jitter_buffer* jb);
/* Push a packet with sequence number `seq` and capture timestamp `ts_ns`. Packets
 * older than the playout head are dropped (MC_OK, counted as late). */
MC_API mc_status mc_jitter_buffer_push(mc_jitter_buffer* jb, uint32_t seq, int64_t ts_ns,
                                       const int16_t* samples, size_t n);
/* Pop up to `cap` frames for playout. Returns silence for gaps. `*n` frames written;
 * `*ts_ns` (optional) timestamp of first sample. MC_ERR_EOF if not yet primed. */
MC_API mc_status mc_jitter_buffer_pop(mc_jitter_buffer* jb, int16_t* out, size_t cap, size_t* n,
                                      int64_t* ts_ns);
typedef struct mc_jitter_stats {
    uint64_t received, late, lost, concealed_frames;
    int32_t current_delay_ms;
} mc_jitter_stats;
MC_API mc_status mc_jitter_buffer_stats(mc_jitter_buffer* jb, mc_jitter_stats* out);
MC_API void mc_jitter_buffer_reset(mc_jitter_buffer* jb);

/* ------------------------------------------------------------ capabilities */
/* Extra (not in the original contract): 1 if built with OpenCV (video files, RTSP, devices,
 * OpenCV codecs), 0 for the dependency-free build (stb codecs, "dir:<path>[@fps]" only). */
MC_API int32_t mc_has_opencv(void);

/* ------------------------------------------------------------ clock */
MC_API int64_t mc_now_monotonic_ns(void);
MC_API int64_t mc_now_wall_ns(void);

#ifdef __cplusplus
}
#endif
#endif /* MEDIACORE_C_H */
