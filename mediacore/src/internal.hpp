// Internal C++ interfaces for mediacore. Not installed.
#pragma once

#include <cstddef>
#include <cstdint>
#include <deque>
#include <functional>
#include <map>
#include <memory>
#include <mutex>
#include <stdexcept>
#include <string>
#include <unordered_map>
#include <vector>

#include "mediacore_c.h"

namespace cv {
class Mat;
}

namespace mediacore {

// ---------------------------------------------------------------- errors
class Error : public std::runtime_error {
 public:
  Error(mc_status st, const std::string& msg) : std::runtime_error(msg), status_(st) {}
  mc_status status() const noexcept { return status_; }

 private:
  mc_status status_;
};

[[noreturn]] void fail(mc_status st, const std::string& msg);
void set_last_error(const std::string& msg);
const char* last_error() noexcept;

// ---------------------------------------------------------------- clock
int64_t now_monotonic_ns() noexcept;
int64_t now_wall_ns() noexcept;

// ---------------------------------------------------------------- frames
constexpr uint64_t kMaxFrameBytes = 1ULL << 30;

int32_t pixfmt_bpp(mc_pixfmt fmt) noexcept;  // 0 if invalid
// Returns tightly packed byte size; throws MC_ERR_INVALID_ARG on bad geometry/overflow.
size_t checked_frame_bytes(int32_t w, int32_t h, mc_pixfmt fmt);
// Validates a caller-supplied frame (non-null data, stride, geometry). Throws on failure.
void validate_frame(const mc_frame* f, const char* what);
void frame_alloc(int32_t w, int32_t h, mc_pixfmt fmt, mc_frame* out);
void frame_free(mc_frame* f) noexcept;
void frame_copy(const mc_frame* in, mc_frame* out);

bool has_opencv() noexcept;

// ---------------------------------------------------------------- image ops
void resize(const mc_frame* in, int32_t w, int32_t h, mc_interp interp, mc_frame* out);
void letterbox(const mc_frame* in, int32_t w, int32_t h, uint8_t pad, mc_frame* out);
void to_gray(const mc_frame* in, mc_frame* out);
void convert(const mc_frame* in, mc_pixfmt fmt, mc_frame* out);
void crop(const mc_frame* in, int32_t x, int32_t y, int32_t w, int32_t h, mc_frame* out);
double motion_score(const mc_frame* a, const mc_frame* b);
double changed_fraction(const mc_frame* a, const mc_frame* b, uint8_t threshold);
uint8_t* encode_jpeg(const mc_frame* in, int32_t quality, size_t* len);
void decode_image(const uint8_t* buf, size_t len, mc_frame* out);

// ---------------------------------------------------------------- frame store
struct StoredFrame {
  mc_frame frame{};  // frame.data points into `buf`; stride == width * bpp
  std::unique_ptr<uint8_t[]> buf;
  size_t bytes = 0;
};
using FramePtr = std::shared_ptr<const StoredFrame>;

class FrameStore {
 public:
  explicit FrameStore(const mc_frame_store_config& cfg);
  void push(const mc_frame& f);
  FramePtr latest(uint32_t stream_id) const;
  FramePtr nearest(uint32_t stream_id, int64_t ts_ns, int64_t max_distance_ns) const;
  // Collects up to `cap` frames in [from, to); *total receives the number matching.
  std::vector<FramePtr> range(uint32_t stream_id, int64_t from_ns, int64_t to_ns, size_t cap,
                              size_t* total) const;
  void prune(int64_t now_ns);
  void remove_stream(uint32_t stream_id);
  mc_frame_store_stats stats() const;

 private:
  struct Stream {
    std::deque<std::shared_ptr<StoredFrame>> frames;
    int64_t last_ts = 0;
    bool has_last = false;
  };
  static bool leased(const std::shared_ptr<StoredFrame>& p) { return p.use_count() > 1; }
  void evict_to_cap_locked();

  mc_frame_store_config cfg_;
  mutable std::mutex mu_;
  std::unordered_map<uint32_t, Stream> streams_;
  size_t bytes_ = 0;
  uint64_t evicted_ = 0;
  uint64_t thinned_ = 0;
};

// ---------------------------------------------------------------- capture
class Capture {
 public:
  explicit Capture(const std::string& uri);
  ~Capture();
  Capture(const Capture&) = delete;
  Capture& operator=(const Capture&) = delete;
  void read(mc_frame* out);
  double fps() const;

 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};

// ---------------------------------------------------------------- audio
// Shared drop accounting for the audio components. Callers hold their own lock.
class DropAccounting {
 public:
  virtual ~DropAccounting() = default;
  uint64_t dropped_total() const { return dropped_; }

 protected:
  virtual void on_drop(uint64_t n) { dropped_ += n; }
  uint64_t dropped_ = 0;
};

class AudioRing : public DropAccounting {
 public:
  AudioRing(int32_t sample_rate, int32_t channels, size_t capacity_frames);
  void push(const int16_t* samples, size_t frames);
  size_t pop(int16_t* out, size_t cap);
  size_t available() const;
  uint64_t dropped() const;
  void clear();

 private:
  mutable std::mutex mu_;
  size_t channels_;
  size_t capacity_;
  std::vector<int16_t> buf_;
  size_t head_ = 0;  // in frames
  size_t size_ = 0;  // in frames
};

size_t resample_required(size_t n_in, int32_t in_rate, int32_t out_rate);
size_t resample_pcm16(const int16_t* in, size_t n_in, int32_t in_rate, int32_t out_rate,
                      int16_t* out, size_t cap);
void mix_pcm16(const int16_t* a, const int16_t* b, size_t n, int16_t* out);
void gain_pcm16(int16_t* s, size_t n, float gain);
double rms_dbfs(const int16_t* s, size_t n);

class Vad {
 public:
  explicit Vad(const mc_vad_config& cfg);
  mc_vad_event process(const int16_t* s, size_t n, int32_t* in_speech);
  void reset();

 private:
  std::mutex mu_;
  mc_vad_config cfg_;
  size_t frame_samples_;
  int32_t start_frames_;
  int32_t end_frames_;
  std::vector<int16_t> pending_;
  bool in_speech_ = false;
  int32_t loud_run_ = 0;
  int32_t quiet_run_ = 0;
};

class JitterBuffer : public DropAccounting {
 public:
  using ClockFn = std::function<int64_t()>;
  JitterBuffer(int32_t sample_rate, int32_t target_delay_ms, int32_t max_delay_ms);
  void push(uint32_t seq, int64_t ts_ns, const int16_t* samples, size_t n);
  // Returns false when not primed.
  bool pop(int16_t* out, size_t cap, size_t* n, int64_t* ts_ns);
  mc_jitter_stats stats() const;
  void reset();

  /**
   * Enables adaptive target delay.
   * @param min_target_ms     lower bound for the adaptive target
   * @param max_target_ms     upper bound for the adaptive target
   * @param adapt_interval_ms minimum time between re-evaluations
   */
  void set_adaptive(int32_t min_target_ms, int32_t adapt_interval_ms);
  void set_clock(ClockFn fn);
  mc_jitter_adaptive_stats adaptive_stats() const;

 protected:
  void on_drop(uint64_t n) override;

 private:
  struct Packet {
    int64_t ts_ns;
    std::vector<int16_t> samples;
  };
  void reset_locked();
  size_t buffered_locked() const;
  void note_arrival_locked(int64_t ts_ns);
  void maybe_adapt_locked();

  mutable std::mutex mu_;
  int32_t rate_;
  size_t target_samples_;
  size_t max_samples_;
  std::map<int64_t, Packet> packets_;  // key: extended (unwrapped) sequence number
  bool have_anchor_ = false;
  uint32_t anchor_seq_ = 0;
  int64_t anchor_ext_ = 0;
  bool primed_ = false;
  int64_t next_ext_ = 0;
  size_t offset_ = 0;      // samples already played from packet next_ext_
  size_t gap_offset_ = 0;  // silence samples already emitted for missing packet next_ext_
  size_t last_len_;
  int64_t play_ts_ = 0;
  bool have_ts_ = false;
  mc_jitter_stats st_{};

  ClockFn clock_;
  size_t configured_target_samples_ = 0;
  bool adaptive_ = false;
  size_t min_target_samples_ = 0;
  int64_t adapt_interval_ns_ = 0;
  int64_t last_adapt_ns_ = 0;
  bool have_arrival_ = false;
  int64_t last_arrival_ns_ = 0;
  int64_t last_pkt_ts_ns_ = 0;
  double jitter_ns_ = 0.0;
  uint64_t drops_since_adapt_ = 0;
  uint64_t adjustments_ = 0;
  uint64_t rebuffers_ = 0;
};

}  // namespace mediacore
