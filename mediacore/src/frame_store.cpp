#include <algorithm>
#include <cstdlib>
#include <cstring>
#include <limits>

#include "internal.hpp"

namespace mediacore {
namespace {
int64_t sat_sub(int64_t a, int64_t b) {
  if (b > 0 && a < std::numeric_limits<int64_t>::min() + b) return std::numeric_limits<int64_t>::min();
  return a - b;
}

bool scene_cut(const mc_frame_store_config& cfg, const StoredFrame* prev, const StoredFrame& f) {
  if (cfg.scene_cut_threshold <= 0.0 || cfg.max_cuts_per_stream == 0 || prev == nullptr)
    return false;
  // A geometry change (e.g. the source renegotiated its resolution) is always a cut.
  if (prev->frame.width != f.frame.width || prev->frame.height != f.frame.height) return true;
  return motion_score(&prev->frame, &f.frame) >= cfg.scene_cut_threshold;
}
}  // namespace

FrameStore::FrameStore(const mc_frame_store_config& cfg) : cfg_(cfg) {
  if (cfg.retention_ns <= 0 || cfg.thin_after_ns < 0 || cfg.thinned_interval_ns <= 0 ||
      cfg.max_bytes == 0 || !(cfg.scene_cut_threshold >= 0.0))
    fail(MC_ERR_INVALID_ARG, "invalid frame store config");
}

void FrameStore::push(const mc_frame& f) {
  validate_frame(&f, "push");
  const size_t bytes = checked_frame_bytes(f.width, f.height, f.fmt);
  if (bytes > cfg_.max_bytes) fail(MC_ERR_FULL, "frame larger than store max_bytes");

  auto sf = std::make_shared<StoredFrame>();
  sf->buf.reset(new uint8_t[bytes]);
  sf->bytes = bytes;
  const size_t row = static_cast<size_t>(f.width) * static_cast<size_t>(pixfmt_bpp(f.fmt));
  for (int32_t y = 0; y < f.height; ++y)
    std::memcpy(sf->buf.get() + static_cast<size_t>(y) * row,
                f.data + static_cast<size_t>(y) * static_cast<size_t>(f.stride), row);
  sf->frame = f;
  sf->frame.data = sf->buf.get();
  sf->frame.stride = static_cast<int32_t>(row);

  std::lock_guard<std::mutex> lk(mu_);
  Stream& s = streams_[f.stream_id];
  if (s.has_last && f.ts_ns <= s.last_ts) {
    fail(MC_ERR_INVALID_ARG, "out-of-order frame (ts_ns not strictly increasing)");
  }
  s.last_ts = f.ts_ns;
  s.has_last = true;
  if (scene_cut(cfg_, s.frames.empty() ? nullptr : s.frames.back().get(), *sf)) {
    if (s.cuts.size() >= cfg_.max_cuts_per_stream) s.cuts.pop_front();
    s.cuts.push_back(sf);
  }
  s.frames.push_back(std::move(sf));
  bytes_ += bytes;
  evict_to_cap_locked();
}

void FrameStore::evict_to_cap_locked() {
  while (bytes_ > cfg_.max_bytes) {
    Stream* best = nullptr;
    size_t best_idx = 0;
    int64_t best_ts = 0;
    for (auto& [id, s] : streams_) {
      for (size_t i = 0; i < s.frames.size(); ++i) {
        if (leased(s.frames[i])) continue;
        const int64_t ts = s.frames[i]->frame.ts_ns;
        if (best == nullptr || ts < best_ts) {
          best = &s;
          best_idx = i;
          best_ts = ts;
        }
        break;  // first unleased is the oldest unleased of this stream
      }
    }
    if (best == nullptr) return;  // everything remaining is leased
    bytes_ -= best->frames[best_idx]->bytes;
    best->frames.erase(best->frames.begin() + static_cast<std::ptrdiff_t>(best_idx));
    ++evicted_;
  }
}

FramePtr FrameStore::latest(uint32_t id) const {
  std::lock_guard<std::mutex> lk(mu_);
  auto it = streams_.find(id);
  if (it == streams_.end() || it->second.frames.empty()) return nullptr;
  return it->second.frames.back();
}

FramePtr FrameStore::nearest(uint32_t id, int64_t ts, int64_t max_dist) const {
  std::lock_guard<std::mutex> lk(mu_);
  auto it = streams_.find(id);
  if (it == streams_.end() || it->second.frames.empty()) return nullptr;
  const auto& fr = it->second.frames;
  auto ub = std::lower_bound(fr.begin(), fr.end(), ts,
                             [](const std::shared_ptr<StoredFrame>& p, int64_t t) {
                               return p->frame.ts_ns < t;
                             });
  // candidates: ub (first >= ts) and its predecessor (< ts)
  auto dist = [ts](int64_t t) {  // |t - ts| without overflow
    return t >= ts ? static_cast<uint64_t>(t) - static_cast<uint64_t>(ts)
                   : static_cast<uint64_t>(ts) - static_cast<uint64_t>(t);
  };
  std::shared_ptr<StoredFrame> best;
  uint64_t bd = 0;
  if (ub != fr.begin()) {
    best = *(ub - 1);
    bd = dist(best->frame.ts_ns);
  }
  if (ub != fr.end()) {
    const uint64_t d = dist((*ub)->frame.ts_ns);
    if (!best || d < bd) {  // ties -> earlier
      best = *ub;
      bd = d;
    }
  }
  if (max_dist > 0 && bd > static_cast<uint64_t>(max_dist)) return nullptr;
  return best;
}

std::vector<FramePtr> FrameStore::range(uint32_t id, int64_t from, int64_t to, size_t cap,
                                        size_t* total) const {
  std::vector<FramePtr> out;
  size_t matched = 0;
  {
    std::lock_guard<std::mutex> lk(mu_);
    auto it = streams_.find(id);
    if (it != streams_.end() && from < to) {
      const auto& fr = it->second.frames;
      auto lo = std::lower_bound(fr.begin(), fr.end(), from,
                                 [](const std::shared_ptr<StoredFrame>& p, int64_t t) {
                                   return p->frame.ts_ns < t;
                                 });
      auto hi = std::lower_bound(lo, fr.end(), to,
                                 [](const std::shared_ptr<StoredFrame>& p, int64_t t) {
                                   return p->frame.ts_ns < t;
                                 });
      matched = static_cast<size_t>(hi - lo);
      const size_t take = std::min(matched, cap);
      out.reserve(take);
      for (size_t i = 0; i < take; ++i) out.push_back(*(lo + static_cast<std::ptrdiff_t>(i)));
    }
  }
  if (total != nullptr) *total = matched;
  return out;
}

std::vector<FramePtr> FrameStore::cuts(uint32_t id, int64_t from, int64_t to, size_t cap) const {
  std::vector<FramePtr> out;
  std::lock_guard<std::mutex> lk(mu_);
  auto it = streams_.find(id);
  if (it == streams_.end() || from >= to) return out;
  const auto& c = it->second.cuts;
  auto lo = std::lower_bound(c.begin(), c.end(), from,
                             [](const std::shared_ptr<StoredFrame>& p, int64_t t) {
                               return p->frame.ts_ns < t;
                             });
  for (; lo != c.end() && (*lo)->frame.ts_ns < to && out.size() < cap; ++lo) out.push_back(*lo);
  return out;
}

void FrameStore::prune(int64_t now_ns) {
  std::lock_guard<std::mutex> lk(mu_);
  const int64_t retention_cut = sat_sub(now_ns, cfg_.retention_ns);
  const int64_t thin_cut = sat_sub(now_ns, cfg_.thin_after_ns);
  const int64_t iv = cfg_.thinned_interval_ns;
  for (auto& [id, s] : streams_) {
    auto& fr = s.frames;
    // 1. retention
    std::deque<std::shared_ptr<StoredFrame>> kept;
    for (auto& p : fr) {
      if (p->frame.ts_ns < retention_cut && !leased(p)) {
        bytes_ -= p->bytes;
        ++evicted_;
      } else {
        kept.push_back(std::move(p));
      }
    }
    fr.swap(kept);
    // 2. thinning: among frames older than thin_cut, keep one per boundary bucket, the
    // frame closest to the boundary k*iv (ties -> earlier). Leased frames are never removed.
    std::deque<std::shared_ptr<StoredFrame>> out;
    size_t i = 0;
    auto bucket_of = [iv](int64_t ts) {
      // nearest boundary index: floor((ts + iv/2) / iv), overflow-safe via __int128
      const __int128 v = static_cast<__int128>(ts) + iv / 2;
      __int128 q = v / iv;
      if (v % iv != 0 && v < 0) --q;
      return q;
    };
    auto boundary_dist = [iv, &bucket_of](int64_t ts) {
      const __int128 d = static_cast<__int128>(ts) - bucket_of(ts) * iv;
      return d < 0 ? -d : d;
    };
    while (i < fr.size()) {
      if (fr[i]->frame.ts_ns >= thin_cut) {
        out.push_back(std::move(fr[i]));
        ++i;
        continue;
      }
      const __int128 b = bucket_of(fr[i]->frame.ts_ns);
      size_t j = i;
      size_t win = i;
      while (j < fr.size() && fr[j]->frame.ts_ns < thin_cut && bucket_of(fr[j]->frame.ts_ns) == b) {
        if (boundary_dist(fr[j]->frame.ts_ns) < boundary_dist(fr[win]->frame.ts_ns)) win = j;
        ++j;
      }
      for (size_t k = i; k < j; ++k) {
        if (k == win || leased(fr[k])) {
          out.push_back(std::move(fr[k]));
        } else {
          bytes_ -= fr[k]->bytes;
          ++thinned_;
        }
      }
      i = j;
    }
    fr.swap(out);
  }
}

void FrameStore::remove_stream(uint32_t id) {
  std::lock_guard<std::mutex> lk(mu_);
  auto it = streams_.find(id);
  if (it == streams_.end()) return;
  for (auto& p : it->second.frames) bytes_ -= p->bytes;
  streams_.erase(it);
}

mc_frame_store_stats FrameStore::stats() const {
  std::lock_guard<std::mutex> lk(mu_);
  mc_frame_store_stats st{};
  bool any = false;
  for (const auto& [id, s] : streams_) {
    if (s.frames.empty()) continue;
    ++st.streams;
    st.frames += s.frames.size();
    const int64_t o = s.frames.front()->frame.ts_ns;
    const int64_t n = s.frames.back()->frame.ts_ns;
    if (!any || o < st.oldest_ts_ns) st.oldest_ts_ns = o;
    if (!any || n > st.newest_ts_ns) st.newest_ts_ns = n;
    any = true;
  }
  st.bytes = bytes_;
  st.evicted_total = evicted_;
  st.thinned_total = thinned_;
  return st;
}

}  // namespace mediacore
