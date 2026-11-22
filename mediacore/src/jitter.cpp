#include <algorithm>
#include <chrono>
#include <cmath>
#include <cstring>

#include "internal.hpp"

namespace mediacore {

JitterBuffer::JitterBuffer(int32_t sample_rate, int32_t target_delay_ms, int32_t max_delay_ms)
    : rate_(sample_rate),
      target_samples_(0),
      max_samples_(0),
      last_len_(0),
      clock_(now_monotonic_ns) {
  if (sample_rate <= 0 || target_delay_ms < 0 || max_delay_ms < target_delay_ms ||
      max_delay_ms <= 0 || max_delay_ms > 600000)
    fail(MC_ERR_INVALID_ARG, "invalid jitter buffer parameters");
  target_samples_ = static_cast<size_t>(static_cast<int64_t>(sample_rate) * target_delay_ms / 1000);
  max_samples_ = static_cast<size_t>(static_cast<int64_t>(sample_rate) * max_delay_ms / 1000);
  last_len_ = std::max<size_t>(1, static_cast<size_t>(sample_rate) / 50);  // 20 ms default
  configured_target_samples_ = target_samples_;
}

void JitterBuffer::on_drop(uint64_t n) {
  DropAccounting::on_drop(n);
  drops_since_adapt_ += n;
}

void JitterBuffer::set_adaptive(int32_t min_target_ms, int32_t adapt_interval_ms) {
  std::lock_guard<std::mutex> lk(mu_);
  if (min_target_ms < 0) {
    adaptive_ = false;
    return;
  }
  if (adapt_interval_ms < 0) fail(MC_ERR_INVALID_ARG, "negative adapt interval");
  const size_t min_samples = static_cast<size_t>(static_cast<int64_t>(rate_) * min_target_ms / 1000);
  if (min_samples > max_samples_) fail(MC_ERR_INVALID_ARG, "min target above max delay");
  adaptive_ = true;
  min_target_samples_ = min_samples;
  adapt_interval_ns_ = static_cast<int64_t>(adapt_interval_ms) * 1000000LL;
}

void JitterBuffer::set_clock(ClockFn fn) {
  std::lock_guard<std::mutex> lk(mu_);
  clock_ = fn ? std::move(fn) : ClockFn(now_monotonic_ns);
}

// RFC 3550 style smoothed inter-arrival jitter: J += (|D| - J) / 16.
void JitterBuffer::note_arrival_locked(int64_t ts_ns) {
  const int64_t arrival = clock_();
  if (have_arrival_) {
    const double d = static_cast<double>((arrival - last_arrival_ns_) - (ts_ns - last_pkt_ts_ns_));
    jitter_ns_ += (std::fabs(d) - jitter_ns_) / 16.0;
  }
  have_arrival_ = true;
  last_arrival_ns_ = arrival;
  last_pkt_ts_ns_ = ts_ns;
}

void JitterBuffer::maybe_adapt_locked() {
  if (!adaptive_) return;
  const int64_t now = std::chrono::duration_cast<std::chrono::nanoseconds>(
                          std::chrono::system_clock::now().time_since_epoch())
                          .count();
  if (last_adapt_ns_ != 0 && now - last_adapt_ns_ < adapt_interval_ns_) return;
  last_adapt_ns_ = now;
  const double want = static_cast<double>(min_target_samples_) + 4.0 * jitter_ns_ * rate_ / 1e9;
  size_t target = std::min(max_samples_, static_cast<size_t>(want));
  if (drops_since_adapt_ > 0) target -= target / 4;  // we overran: back off
  target = std::max(target, min_target_samples_);
  drops_since_adapt_ = 0;
  if (target != target_samples_) {
    target_samples_ = target;
    ++adjustments_;
  }
}

mc_jitter_adaptive_stats JitterBuffer::adaptive_stats() const {
  std::lock_guard<std::mutex> lk(mu_);
  mc_jitter_adaptive_stats s{};
  s.jitter_ms = jitter_ns_ / 1e6;
  s.target_delay_ms = static_cast<int32_t>(static_cast<int64_t>(target_samples_) * 1000 / rate_);
  s.adjustments = adjustments_;
  s.rebuffers = rebuffers_;
  s.overrun_drops = dropped_total();
  return s;
}

size_t JitterBuffer::buffered_locked() const {
  size_t t = 0;
  for (const auto& [k, p] : packets_) t += p.samples.size();
  return t >= offset_ ? t - offset_ : 0;
}

void JitterBuffer::push(uint32_t seq, int64_t ts_ns, const int16_t* samples, size_t n) {
  if (n > 0 && samples == nullptr) fail(MC_ERR_INVALID_ARG, "null samples");
  if (n > (static_cast<size_t>(1) << 24)) fail(MC_ERR_INVALID_ARG, "packet too large");
  std::lock_guard<std::mutex> lk(mu_);
  ++st_.received;
  // Unwrap the 32-bit sequence relative to the most recent packet (handles 2^32 wrap).
  int64_t ext;
  if (!have_anchor_) {
    ext = static_cast<int64_t>(1) << 40;
  } else {
    ext = anchor_ext_ + static_cast<int64_t>(static_cast<int32_t>(seq - anchor_seq_));
  }
  have_anchor_ = true;
  anchor_seq_ = seq;
  anchor_ext_ = ext;

  if ((primed_ && ext < next_ext_) || packets_.count(ext) != 0) {  // late or duplicate
    ++st_.late;
    return;
  }
  packets_.emplace(ext, Packet{ts_ns, std::vector<int16_t>(samples, samples + n)});
  if (n > 0) last_len_ = n;
  note_arrival_locked(ts_ns);
  maybe_adapt_locked();

  if (!primed_ && buffered_locked() >= target_samples_) {
    primed_ = true;
    next_ext_ = packets_.begin()->first;
    offset_ = 0;
    gap_offset_ = 0;
    have_ts_ = false;
  }
  // Overrun: discard oldest audio beyond max delay (counted as lost).
  while (primed_ && buffered_locked() > max_samples_ && !packets_.empty()) {
    auto it = packets_.begin();
    if (it->first > next_ext_) {
      next_ext_ = it->first;  // skip the missing ones in front
      gap_offset_ = 0;
    }
    packets_.erase(it);
    ++next_ext_;
    offset_ = 0;
    gap_offset_ = 0;
    have_ts_ = false;
    ++st_.lost;
    on_drop(1);
  }
}

bool JitterBuffer::pop(int16_t* out, size_t cap, size_t* n, int64_t* ts_ns) {
  if (n == nullptr || (cap > 0 && out == nullptr)) fail(MC_ERR_INVALID_ARG, "null argument");
  std::lock_guard<std::mutex> lk(mu_);
  *n = 0;
  if (!primed_) return false;
  size_t written = 0;
  bool first = true;
  auto note_ts = [&](int64_t t) {
    if (first && ts_ns != nullptr) *ts_ns = t;
    first = false;
  };
  const int64_t ns_per_sample_num = 1000000000LL;
  while (written < cap) {
    auto it = packets_.find(next_ext_);
    if (it != packets_.end()) {
      const Packet& p = it->second;
      if (offset_ >= p.samples.size()) {  // finished (or empty) packet
        packets_.erase(it);
        ++next_ext_;
        offset_ = 0;
        have_ts_ = false;
        continue;
      }
      const int64_t t = p.ts_ns + static_cast<int64_t>(offset_) * ns_per_sample_num / rate_;
      note_ts(t);
      const size_t take = std::min(cap - written, p.samples.size() - offset_);
      std::memcpy(out + written, p.samples.data() + offset_, take * sizeof(int16_t));
      written += take;
      offset_ += take;
      play_ts_ = p.ts_ns + static_cast<int64_t>(offset_) * ns_per_sample_num / rate_;
      have_ts_ = true;
      if (offset_ >= p.samples.size()) {
        packets_.erase(it);
        ++next_ext_;
        offset_ = 0;
      }
    } else if (!packets_.empty()) {
      // Missing packet with later ones buffered: conceal with silence of one packet length.
      if (gap_offset_ == 0) ++st_.lost;
      if (have_ts_) note_ts(play_ts_);
      else note_ts(packets_.begin()->second.ts_ns);
      const size_t take = std::min(cap - written, last_len_ - gap_offset_);
      std::memset(out + written, 0, take * sizeof(int16_t));
      written += take;
      gap_offset_ += take;
      st_.concealed_frames += take;
      if (have_ts_) play_ts_ += static_cast<int64_t>(take) * ns_per_sample_num / rate_;
      if (gap_offset_ >= last_len_) {
        gap_offset_ = 0;
        ++next_ext_;
      }
    } else {
      break;  // underrun: nothing buffered
    }
  }
  *n = written;
  if (adaptive_ && cap > 0 && written == 0 && packets_.empty()) {
    primed_ = false;  // underrun: re-prime at the current target
    ++rebuffers_;
  }
  return true;
}

mc_jitter_stats JitterBuffer::stats() const {
  std::lock_guard<std::mutex> lk(mu_);
  mc_jitter_stats s = st_;
  s.current_delay_ms =
      static_cast<int32_t>(static_cast<int64_t>(buffered_locked()) * 1000 / rate_);
  return s;
}

void JitterBuffer::reset_locked() {
  packets_.clear();
  have_anchor_ = false;
  primed_ = false;
  next_ext_ = 0;
  offset_ = 0;
  gap_offset_ = 0;
  have_ts_ = false;
  play_ts_ = 0;
  st_ = mc_jitter_stats{};
  target_samples_ = configured_target_samples_;
  last_adapt_ns_ = 0;
  have_arrival_ = false;
  jitter_ns_ = 0.0;
  drops_since_adapt_ = 0;
  adjustments_ = 0;
  rebuffers_ = 0;
  dropped_ = 0;
}

void JitterBuffer::reset() {
  std::lock_guard<std::mutex> lk(mu_);
  reset_locked();
}

}  // namespace mediacore
