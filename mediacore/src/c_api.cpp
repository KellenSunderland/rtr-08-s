// C ABI boundary: every entry point catches all exceptions and maps them to mc_status,
// recording a thread-local message retrievable via mc_last_error().
#include <cstdlib>
#include <cstring>
#include <new>
#ifdef MC_WITH_OPENCV
#include <opencv2/core.hpp>
#endif

#include "internal.hpp"
#include "mediacore_c.h"

struct mc_frame_store {
  explicit mc_frame_store(const mc_frame_store_config& c) : impl(c) {}
  mediacore::FrameStore impl;
};
struct mc_capture {
  explicit mc_capture(const std::string& uri) : impl(uri) {}
  mediacore::Capture impl;
};
struct mc_audio_ring {
  mc_audio_ring(int32_t r, int32_t c, size_t n) : impl(r, c, n) {}
  mediacore::AudioRing impl;
};
struct mc_vad {
  explicit mc_vad(const mc_vad_config& c) : impl(c) {}
  mediacore::Vad impl;
};
struct mc_jitter_buffer {
  mc_jitter_buffer(int32_t r, int32_t t, int32_t m) : impl(r, t, m) {}
  mediacore::JitterBuffer impl;
};

namespace {

using mediacore::fail;

template <class F>
mc_status guard(F&& f) noexcept {
  try {
    f();
    return MC_OK;
  } catch (const mediacore::Error& e) {
    mediacore::set_last_error(e.what());
    return e.status();
  } catch (const std::bad_alloc&) {
    mediacore::set_last_error("out of memory");
    return MC_ERR_INTERNAL;
#ifdef MC_WITH_OPENCV
  } catch (const cv::Exception& e) {
    mediacore::set_last_error(std::string("opencv: ") + e.what());
    return MC_ERR_INTERNAL;
#endif
  } catch (const std::exception& e) {
    mediacore::set_last_error(e.what());
    return MC_ERR_INTERNAL;
  } catch (...) {
    mediacore::set_last_error("unknown error");
    return MC_ERR_INTERNAL;
  }
}

template <class T>
T* need(T* p, const char* name) {
  if (p == nullptr) fail(MC_ERR_INVALID_ARG, std::string("null argument: ") + name);
  return p;
}

using LeasePtr = mediacore::FramePtr;

void make_view(const LeasePtr& fp, mc_frame_view* out) {
  auto* lease = new LeasePtr(fp);
  out->frame = &fp->frame;
  out->lease = lease;
}

}  // namespace

extern "C" {

const char* mc_version(void) { return "0.1.0"; }
const char* mc_last_error(void) { return mediacore::last_error(); }

mc_status mc_frame_alloc(int32_t width, int32_t height, mc_pixfmt fmt, mc_frame* out) {
  return guard([&] { mediacore::frame_alloc(width, height, fmt, need(out, "out")); });
}
void mc_frame_free(mc_frame* frame) { mediacore::frame_free(frame); }
mc_status mc_frame_copy(const mc_frame* in, mc_frame* out) {
  return guard([&] { mediacore::frame_copy(in, need(out, "out")); });
}
int32_t mc_pixfmt_bpp(mc_pixfmt fmt) { return mediacore::pixfmt_bpp(fmt); }
void mc_buffer_free(uint8_t* buf) { std::free(buf); }

mc_status mc_resize(const mc_frame* in, int32_t w, int32_t h, mc_interp interp, mc_frame* out) {
  return guard([&] { mediacore::resize(in, w, h, interp, out); });
}
mc_status mc_letterbox(const mc_frame* in, int32_t w, int32_t h, uint8_t pad, mc_frame* out) {
  return guard([&] { mediacore::letterbox(in, w, h, pad, out); });
}
mc_status mc_to_gray(const mc_frame* in, mc_frame* out) {
  return guard([&] { mediacore::to_gray(in, out); });
}
mc_status mc_convert(const mc_frame* in, mc_pixfmt fmt, mc_frame* out) {
  return guard([&] { mediacore::convert(in, fmt, out); });
}
mc_status mc_crop(const mc_frame* in, int32_t x, int32_t y, int32_t w, int32_t h, mc_frame* out) {
  return guard([&] { mediacore::crop(in, x, y, w, h, out); });
}
mc_status mc_crop_letterbox(const mc_frame* in, int32_t x, int32_t y, int32_t w, int32_t h,
                            int32_t width, int32_t height, uint8_t pad_value, mc_frame* out) {
  return guard(
      [&] { mediacore::crop_letterbox(in, x, y, w, h, width, height, pad_value, out); });
}
mc_status mc_motion_score(const mc_frame* a, const mc_frame* b, double* score) {
  return guard([&] { *need(score, "score") = mediacore::motion_score(a, b); });
}
mc_status mc_changed_fraction(const mc_frame* a, const mc_frame* b, uint8_t threshold,
                              double* fraction) {
  return guard(
      [&] { *need(fraction, "fraction") = mediacore::changed_fraction(a, b, threshold); });
}
mc_status mc_encode_jpeg(const mc_frame* in, int32_t quality, uint8_t** buf, size_t* len) {
  return guard([&] {
    need(buf, "buf");
    need(len, "len");
    *buf = mediacore::encode_jpeg(in, quality, len);
  });
}
mc_status mc_decode_image(const uint8_t* buf, size_t len, mc_frame* out) {
  return guard([&] { mediacore::decode_image(buf, len, out); });
}

void mc_frame_store_config_default(mc_frame_store_config* cfg) {
  if (cfg == nullptr) return;
  cfg->retention_ns = 3600LL * 1000000000LL;
  cfg->thin_after_ns = 300LL * 1000000000LL;
  cfg->thinned_interval_ns = 1000000000LL;
  cfg->max_bytes = static_cast<size_t>(2) << 30;
}
mc_status mc_frame_store_create(const mc_frame_store_config* cfg, mc_frame_store** out) {
  return guard([&] {
    need(out, "out");
    *out = nullptr;
    mc_frame_store_config c;
    if (cfg != nullptr) {
      c = *cfg;
    } else {
      mc_frame_store_config_default(&c);
    }
    *out = new mc_frame_store(c);
  });
}
void mc_frame_store_destroy(mc_frame_store* store) { delete store; }

mc_status mc_frame_store_push(mc_frame_store* store, const mc_frame* frame) {
  return guard([&] { need(store, "store")->impl.push(*need(frame, "frame")); });
}
void mc_frame_view_release(mc_frame_view* view) {
  if (view == nullptr) return;
  delete static_cast<LeasePtr*>(view->lease);
  view->lease = nullptr;
  view->frame = nullptr;
}
mc_status mc_frame_store_latest(mc_frame_store* store, uint32_t stream_id, mc_frame_view* out) {
  return guard([&] {
    need(out, "out");
    *out = mc_frame_view{};
    auto fp = need(store, "store")->impl.latest(stream_id);
    if (!fp) fail(MC_ERR_NOT_FOUND, "no frames for stream");
    make_view(fp, out);
  });
}
mc_status mc_frame_store_nearest(mc_frame_store* store, uint32_t stream_id, int64_t ts_ns,
                                 int64_t max_distance_ns, mc_frame_view* out) {
  return guard([&] {
    need(out, "out");
    *out = mc_frame_view{};
    auto fp = need(store, "store")->impl.nearest(stream_id, ts_ns, max_distance_ns);
    if (!fp) fail(MC_ERR_NOT_FOUND, "no frame within distance");
    make_view(fp, out);
  });
}
mc_status mc_frame_store_range(mc_frame_store* store, uint32_t stream_id, int64_t from_ns,
                               int64_t to_ns, mc_frame_view* out, size_t cap, size_t* n,
                               size_t* total) {
  return guard([&] {
    need(n, "n");
    *n = 0;
    if (total != nullptr) *total = 0;
    if (cap > 0) need(out, "out");
    auto v = need(store, "store")->impl.range(stream_id, from_ns, to_ns, cap, total);
    size_t made = 0;
    try {
      for (; made < v.size(); ++made) make_view(v[made], &out[made]);
    } catch (...) {
      for (size_t i = 0; i < made; ++i) mc_frame_view_release(&out[i]);
      throw;
    }
    *n = made;
  });
}
mc_status mc_frame_store_prune(mc_frame_store* store, int64_t now_ns) {
  return guard([&] { need(store, "store")->impl.prune(now_ns); });
}
mc_status mc_frame_store_remove_stream(mc_frame_store* store, uint32_t stream_id) {
  return guard([&] { need(store, "store")->impl.remove_stream(stream_id); });
}
mc_status mc_frame_store_stats_get(mc_frame_store* store, mc_frame_store_stats* out) {
  return guard([&] { *need(out, "out") = need(store, "store")->impl.stats(); });
}

mc_status mc_capture_open(const char* uri, mc_capture** out) {
  return guard([&] {
    need(out, "out");
    *out = nullptr;
    *out = new mc_capture(std::string(need(uri, "uri")));
  });
}
mc_status mc_capture_read(mc_capture* cap, mc_frame* out) {
  return guard([&] { need(cap, "cap")->impl.read(out); });
}
double mc_capture_fps(mc_capture* cap) {
  if (cap == nullptr) return 0.0;
  try {
    return cap->impl.fps();
  } catch (...) {
    return 0.0;
  }
}
void mc_capture_close(mc_capture* cap) { delete cap; }

mc_status mc_audio_ring_create(int32_t sample_rate, int32_t channels, size_t capacity_frames,
                               mc_audio_ring** out) {
  return guard([&] {
    need(out, "out");
    *out = nullptr;
    *out = new mc_audio_ring(sample_rate, channels, capacity_frames);
  });
}
void mc_audio_ring_destroy(mc_audio_ring* ring) { delete ring; }
mc_status mc_audio_ring_push(mc_audio_ring* ring, const int16_t* samples, size_t frames) {
  return guard([&] { need(ring, "ring")->impl.push(samples, frames); });
}
mc_status mc_audio_ring_pop(mc_audio_ring* ring, int16_t* out, size_t cap, size_t* got) {
  return guard([&] {
    need(got, "got");
    *got = 0;
    *got = need(ring, "ring")->impl.pop(out, cap);
  });
}
size_t mc_audio_ring_available(mc_audio_ring* ring) {
  return ring != nullptr ? ring->impl.available() : 0;
}
uint64_t mc_audio_ring_dropped(mc_audio_ring* ring) {
  return ring != nullptr ? ring->impl.dropped() : 0;
}
void mc_audio_ring_clear(mc_audio_ring* ring) {
  if (ring != nullptr) ring->impl.clear();
}

mc_status mc_resample_pcm16(const int16_t* in, size_t n_in, int32_t in_rate, int32_t out_rate,
                            int16_t* out, size_t cap, size_t* n_out) {
  return guard([&] {
    need(n_out, "n_out");
    *n_out = 0;
    *n_out = mediacore::resample_pcm16(in, n_in, in_rate, out_rate, out, cap);
  });
}
mc_status mc_mix_pcm16(const int16_t* a, const int16_t* b, size_t n, int16_t* out) {
  return guard([&] { mediacore::mix_pcm16(a, b, n, out); });
}
mc_status mc_gain_pcm16(int16_t* samples, size_t n, float gain) {
  return guard([&] { mediacore::gain_pcm16(samples, n, gain); });
}
mc_status mc_rms_dbfs(const int16_t* samples, size_t n, double* dbfs) {
  return guard([&] { *need(dbfs, "dbfs") = mediacore::rms_dbfs(samples, n); });
}

void mc_vad_config_default(mc_vad_config* cfg) {
  if (cfg == nullptr) return;
  cfg->sample_rate = 16000;
  cfg->frame_ms = 20;
  cfg->threshold_dbfs = -40.0;
  cfg->min_speech_ms = 100;
  cfg->hangover_ms = 300;
}
mc_status mc_vad_create(const mc_vad_config* cfg, mc_vad** out) {
  return guard([&] {
    need(out, "out");
    *out = nullptr;
    mc_vad_config c;
    if (cfg != nullptr) {
      c = *cfg;
    } else {
      mc_vad_config_default(&c);
    }
    *out = new mc_vad(c);
  });
}
void mc_vad_destroy(mc_vad* vad) { delete vad; }
mc_status mc_vad_process(mc_vad* vad, const int16_t* samples, size_t n, mc_vad_event* event,
                         int32_t* in_speech) {
  return guard([&] {
    if (event != nullptr) *event = MC_VAD_NONE;
    const mc_vad_event e = need(vad, "vad")->impl.process(samples, n, in_speech);
    if (event != nullptr) *event = e;
  });
}
void mc_vad_reset(mc_vad* vad) {
  if (vad != nullptr) vad->impl.reset();
}

mc_status mc_jitter_buffer_create(int32_t sample_rate, int32_t target_delay_ms,
                                  int32_t max_delay_ms, mc_jitter_buffer** out) {
  return guard([&] {
    need(out, "out");
    *out = nullptr;
    *out = new mc_jitter_buffer(sample_rate, target_delay_ms, max_delay_ms);
  });
}
void mc_jitter_buffer_destroy(mc_jitter_buffer* jb) { delete jb; }
mc_status mc_jitter_buffer_push(mc_jitter_buffer* jb, uint32_t seq, int64_t ts_ns,
                                const int16_t* samples, size_t n) {
  return guard([&] { need(jb, "jb")->impl.push(seq, ts_ns, samples, n); });
}
mc_status mc_jitter_buffer_pop(mc_jitter_buffer* jb, int16_t* out, size_t cap, size_t* n,
                               int64_t* ts_ns) {
  bool primed = true;
  const mc_status st = guard([&] { primed = need(jb, "jb")->impl.pop(out, cap, n, ts_ns); });
  if (st == MC_OK && !primed) {
    mediacore::set_last_error("jitter buffer not primed");
    return MC_ERR_EOF;
  }
  return st;
}
mc_status mc_jitter_buffer_stats(mc_jitter_buffer* jb, mc_jitter_stats* out) {
  return guard([&] { *need(out, "out") = need(jb, "jb")->impl.stats(); });
}
void mc_jitter_buffer_reset(mc_jitter_buffer* jb) {
  if (jb != nullptr) jb->impl.reset();
}

int32_t mc_has_opencv(void) { return mediacore::has_opencv() ? 1 : 0; }

int64_t mc_now_monotonic_ns(void) { return mediacore::now_monotonic_ns(); }
int64_t mc_now_wall_ns(void) { return mediacore::now_wall_ns(); }

}  // extern "C"
