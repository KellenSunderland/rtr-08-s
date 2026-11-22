// Dependency-free image operations (identical in OpenCV and non-OpenCV builds).
#include <algorithm>
#include <cmath>
#include <cstdlib>
#include <cstring>

#include "internal.hpp"

namespace mediacore {
namespace {

inline uint8_t gray_px(const uint8_t* p, mc_pixfmt fmt) {
  switch (fmt) {
    case MC_PIXFMT_RGB8:
      return static_cast<uint8_t>((77u * p[0] + 150u * p[1] + 29u * p[2] + 128u) >> 8);
    case MC_PIXFMT_BGR8:
      return static_cast<uint8_t>((77u * p[2] + 150u * p[1] + 29u * p[0] + 128u) >> 8);
    default:
      return p[0];
  }
}

const uint8_t* row_ptr(const mc_frame* f, int32_t y) {
  return f->data + static_cast<size_t>(y) * static_cast<size_t>(f->stride);
}

void gray_row(const mc_frame* f, int32_t y, uint8_t* dst) {
  const uint8_t* src = row_ptr(f, y);
  const size_t bpp = static_cast<size_t>(pixfmt_bpp(f->fmt));
  for (int32_t x = 0; x < f->width; ++x) dst[x] = gray_px(src + static_cast<size_t>(x) * bpp, f->fmt);
}

void init_out(const mc_frame* meta_src, int32_t w, int32_t h, mc_pixfmt fmt, mc_frame* out) {
  const mc_frame meta = *meta_src;
  mc_frame tmp;
  frame_alloc(w, h, fmt, &tmp);
  tmp.ts_ns = meta.ts_ns;
  tmp.wall_ns = meta.wall_ns;
  tmp.stream_id = meta.stream_id;
  tmp.seq = meta.seq;
  *out = tmp;
}

struct Tap {
  int idx;
  float w;
};

// Per-destination-coordinate source taps for one axis.
std::vector<std::vector<Tap>> build_taps(int src_n, int dst_n, mc_interp interp) {
  std::vector<std::vector<Tap>> taps(static_cast<size_t>(dst_n));
  const double scale = static_cast<double>(src_n) / dst_n;
  for (int d = 0; d < dst_n; ++d) {
    auto& t = taps[static_cast<size_t>(d)];
    if (interp == MC_INTERP_NEAREST) {
      const int s = std::clamp(static_cast<int>(std::floor((d + 0.5) * scale)), 0, src_n - 1);
      t.push_back({s, 1.0f});
    } else if (interp == MC_INTERP_BILINEAR) {
      const double c = (d + 0.5) * scale - 0.5;
      const int s0 = static_cast<int>(std::floor(c));
      const float f = static_cast<float>(c - s0);
      const int a = std::clamp(s0, 0, src_n - 1);
      const int b = std::clamp(s0 + 1, 0, src_n - 1);
      t.push_back({a, 1.0f - f});
      if (b != a) t.push_back({b, f});
      else t.back().w = 1.0f;
    } else {  // area: fractional box coverage
      const double lo = d * scale;
      const double hi = (d + 1) * scale;
      const int s0 = std::clamp(static_cast<int>(std::floor(lo)), 0, src_n - 1);
      const int s1 = std::clamp(static_cast<int>(std::ceil(hi)) - 1, s0, src_n - 1);
      double total = 0.0;
      for (int s = s0; s <= s1; ++s) total += std::min<double>(hi, s + 1) - std::max<double>(lo, s);
      for (int s = s0; s <= s1; ++s) {
        const double cov = std::min<double>(hi, s + 1) - std::max<double>(lo, s);
        if (cov > 0) t.push_back({s, static_cast<float>(cov / total)});
      }
      if (t.empty()) t.push_back({s0, 1.0f});
    }
  }
  return taps;
}

inline uint8_t to_u8(float v) {
  return static_cast<uint8_t>(std::clamp(static_cast<int>(std::lround(v)), 0, 255));
}

}  // namespace

void resize(const mc_frame* in, int32_t w, int32_t h, mc_interp interp, mc_frame* out) {
  validate_frame(in, "resize");
  if (out == nullptr) fail(MC_ERR_INVALID_ARG, "null out");
  if (interp != MC_INTERP_NEAREST && interp != MC_INTERP_BILINEAR && interp != MC_INTERP_AREA)
    fail(MC_ERR_INVALID_ARG, "invalid interpolation");
  (void)checked_frame_bytes(w, h, in->fmt);
  const size_t ch = static_cast<size_t>(pixfmt_bpp(in->fmt));
  const auto xt = build_taps(in->width, w, interp);
  const auto yt = build_taps(in->height, h, interp);
  const size_t dw = static_cast<size_t>(w);
  // Horizontal pass into float rows (src height x dst width).
  std::vector<float> mid(static_cast<size_t>(in->height) * dw * ch);
  for (int32_t y = 0; y < in->height; ++y) {
    const uint8_t* src = row_ptr(in, y);
    float* dst = mid.data() + static_cast<size_t>(y) * dw * ch;
    for (size_t x = 0; x < dw; ++x)
      for (size_t c = 0; c < ch; ++c) {
        float acc = 0.0f;
        for (const Tap& t : xt[x]) acc += t.w * src[static_cast<size_t>(t.idx) * ch + c];
        dst[x * ch + c] = acc;
      }
  }
  mc_frame tmp;
  init_out(in, w, h, in->fmt, &tmp);
  for (size_t y = 0; y < static_cast<size_t>(h); ++y) {
    uint8_t* dst = tmp.data + y * static_cast<size_t>(tmp.stride);
    for (size_t i = 0; i < dw * ch; ++i) {
      float acc = 0.0f;
      for (const Tap& t : yt[y]) acc += t.w * mid[static_cast<size_t>(t.idx) * dw * ch + i];
      dst[i] = to_u8(acc);
    }
  }
  *out = tmp;
}

void letterbox(const mc_frame* in, int32_t w, int32_t h, uint8_t pad, mc_frame* out) {
  validate_frame(in, "letterbox");
  if (out == nullptr) fail(MC_ERR_INVALID_ARG, "null out");
  (void)checked_frame_bytes(w, h, in->fmt);
  const double scale =
      std::min(static_cast<double>(w) / in->width, static_cast<double>(h) / in->height);
  const int32_t nw = std::clamp(static_cast<int32_t>(std::lround(in->width * scale)), 1, w);
  const int32_t nh = std::clamp(static_cast<int32_t>(std::lround(in->height * scale)), 1, h);
  const int32_t ox = (w - nw) / 2;
  const int32_t oy = (h - nh) / 2;
  mc_frame scaled;
  resize(in, nw, nh, scale < 1.0 ? MC_INTERP_AREA : MC_INTERP_BILINEAR, &scaled);
  mc_frame tmp;
  try {
    init_out(in, w, h, in->fmt, &tmp);
  } catch (...) {
    frame_free(&scaled);
    throw;
  }
  std::memset(tmp.data, pad, static_cast<size_t>(tmp.stride) * static_cast<size_t>(h));
  const size_t bpp = static_cast<size_t>(pixfmt_bpp(in->fmt));
  for (int32_t y = 0; y < nh; ++y)
    std::memcpy(tmp.data + static_cast<size_t>(y + oy) * static_cast<size_t>(tmp.stride) +
                    static_cast<size_t>(ox) * bpp,
                scaled.data + static_cast<size_t>(y) * static_cast<size_t>(scaled.stride),
                static_cast<size_t>(nw) * bpp);
  frame_free(&scaled);
  *out = tmp;
}

void to_gray(const mc_frame* in, mc_frame* out) {
  validate_frame(in, "to_gray");
  if (out == nullptr) fail(MC_ERR_INVALID_ARG, "null out");
  mc_frame tmp;
  init_out(in, in->width, in->height, MC_PIXFMT_GRAY8, &tmp);
  for (int32_t y = 0; y < in->height; ++y)
    gray_row(in, y, tmp.data + static_cast<size_t>(y) * static_cast<size_t>(tmp.stride));
  *out = tmp;
}

void convert(const mc_frame* in, mc_pixfmt fmt, mc_frame* out) {
  validate_frame(in, "convert");
  if (out == nullptr) fail(MC_ERR_INVALID_ARG, "null out");
  if (pixfmt_bpp(fmt) == 0) fail(MC_ERR_INVALID_ARG, "invalid target format");
  if (fmt == MC_PIXFMT_GRAY8) {
    to_gray(in, out);
    return;
  }
  mc_frame tmp;
  init_out(in, in->width, in->height, fmt, &tmp);
  const size_t w = static_cast<size_t>(in->width);
  for (int32_t y = 0; y < in->height; ++y) {
    const uint8_t* s = row_ptr(in, y);
    uint8_t* d = tmp.data + static_cast<size_t>(y) * static_cast<size_t>(tmp.stride);
    for (size_t x = 0; x < w; ++x) {
      if (in->fmt == MC_PIXFMT_GRAY8) {
        d[3 * x] = d[3 * x + 1] = d[3 * x + 2] = s[x];
      } else if (in->fmt == fmt) {
        std::memcpy(d + 3 * x, s + 3 * x, 3);
      } else {  // RGB <-> BGR
        d[3 * x] = s[3 * x + 2];
        d[3 * x + 1] = s[3 * x + 1];
        d[3 * x + 2] = s[3 * x];
      }
    }
  }
  *out = tmp;
}

void crop(const mc_frame* in, int32_t x, int32_t y, int32_t w, int32_t h, mc_frame* out) {
  validate_frame(in, "crop");
  if (out == nullptr) fail(MC_ERR_INVALID_ARG, "null out");
  if (x < 0 || y < 0 || w <= 0 || h <= 0 || static_cast<int64_t>(x) + w > in->width ||
      static_cast<int64_t>(y) + h > in->height)
    fail(MC_ERR_INVALID_ARG, "crop region outside frame");
  mc_frame tmp;
  init_out(in, w, h, in->fmt, &tmp);
  const size_t bpp = static_cast<size_t>(pixfmt_bpp(in->fmt));
  for (int32_t r = 0; r < h; ++r)
    std::memcpy(tmp.data + static_cast<size_t>(r) * static_cast<size_t>(tmp.stride),
                row_ptr(in, y + r) + static_cast<size_t>(x) * bpp, static_cast<size_t>(w) * bpp);
  *out = tmp;
}

void crop_letterbox(const mc_frame* in, int32_t cx, int32_t cy, int32_t cw, int32_t ch, int32_t w,
                    int32_t h, uint8_t pad, mc_frame* out) {
  validate_frame(in, "crop_letterbox");
  if (out == nullptr) fail(MC_ERR_INVALID_ARG, "null out");
  if (cx < 0 || cy < 0 || cw <= 0 || ch <= 0 || static_cast<int64_t>(cx) + cw > in->width ||
      static_cast<int64_t>(cy) + ch > in->height)
    fail(MC_ERR_INVALID_ARG, "crop region outside frame");
  if (w <= 0 || h <= 0) fail(MC_ERR_INVALID_ARG, "invalid target size");

  const int32_t bpp = pixfmt_bpp(in->fmt);
  const size_t pb = static_cast<size_t>(bpp);
  const size_t bytes = static_cast<size_t>(w * h * bpp);
  if (bytes > kMaxFrameBytes) fail(MC_ERR_INVALID_ARG, "target frame too large");

  const double scale = std::min(static_cast<double>(w) / cw, static_cast<double>(h) / ch);
  const int32_t nw = std::clamp(static_cast<int32_t>(std::lround(cw * scale)), 1, w);
  const int32_t nh = std::clamp(static_cast<int32_t>(std::lround(ch * scale)), 1, h);
  const int32_t ox = (w - nw) / 2;
  const int32_t oy = (h - nh) / 2;

  // Source column (relative to the crop) for each destination column.
  std::vector<int32_t> xs(static_cast<size_t>(nw));
  for (int32_t dx = 0; dx < nw; ++dx)
    xs[static_cast<size_t>(dx)] =
        static_cast<int32_t>((static_cast<int64_t>(dx) * cw + nw / 2) / nw);

  auto* data = static_cast<uint8_t*>(std::malloc(bytes));
  if (data == nullptr) fail(MC_ERR_INTERNAL, "out of memory");
  std::memset(data, pad, bytes);
  const size_t stride = static_cast<size_t>(w) * pb;
  for (int32_t dy = 0; dy < nh; ++dy) {
    const int32_t sy = cy + static_cast<int32_t>((static_cast<int64_t>(dy) * ch + nh / 2) / nh);
    const uint8_t* src = row_ptr(in, sy);
    uint8_t* dst = data + static_cast<size_t>(dy + oy) * stride + static_cast<size_t>(ox) * pb;
    for (int32_t dx = 0; dx < nw; ++dx)
      std::memcpy(dst + static_cast<size_t>(dx) * pb,
                  src + static_cast<size_t>(cx + xs[static_cast<size_t>(dx)]) * pb, pb);
  }
  mc_frame tmp{};
  tmp.data = data;
  tmp.width = w;
  tmp.height = h;
  tmp.stride = static_cast<int32_t>(stride);
  tmp.fmt = in->fmt;
  tmp.ts_ns = in->ts_ns;
  tmp.wall_ns = in->wall_ns;
  tmp.stream_id = in->stream_id;
  tmp.seq = in->seq;
  *out = tmp;
}

namespace {
template <class F>
void for_each_gray_pair(const mc_frame* a, const mc_frame* b, F&& f) {
  validate_frame(a, "a");
  validate_frame(b, "b");
  if (a->width != b->width || a->height != b->height)
    fail(MC_ERR_INVALID_ARG, "frame geometry mismatch");
  std::vector<uint8_t> ra(static_cast<size_t>(a->width)), rb(static_cast<size_t>(a->width));
  for (int32_t y = 0; y < a->height; ++y) {
    gray_row(a, y, ra.data());
    gray_row(b, y, rb.data());
    for (size_t x = 0; x < ra.size(); ++x) f(ra[x], rb[x]);
  }
}
}  // namespace

double motion_score(const mc_frame* a, const mc_frame* b) {
  uint64_t sum = 0;
  for_each_gray_pair(a, b, [&](uint8_t p, uint8_t q) { sum += static_cast<uint64_t>(p > q ? p - q : q - p); });
  const double n = static_cast<double>(a->width) * static_cast<double>(a->height);
  return std::clamp(static_cast<double>(sum) / n / 255.0, 0.0, 1.0);
}

double changed_fraction(const mc_frame* a, const mc_frame* b, uint8_t threshold) {
  uint64_t cnt = 0;
  for_each_gray_pair(a, b, [&](uint8_t p, uint8_t q) {
    const int d = p > q ? p - q : q - p;
    if (d > threshold) ++cnt;
  });
  return static_cast<double>(cnt) / (static_cast<double>(a->width) * static_cast<double>(a->height));
}

}  // namespace mediacore
