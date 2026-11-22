#include <algorithm>
#include <cstring>
#include <limits>

#include "internal.hpp"

namespace mediacore {

AudioRing::AudioRing(int32_t sample_rate, int32_t channels, size_t capacity_frames)
    : channels_(0), capacity_(capacity_frames) {
  if (sample_rate <= 0 || channels <= 0 || channels > 64 || capacity_frames == 0)
    fail(MC_ERR_INVALID_ARG, "invalid audio ring parameters");
  channels_ = static_cast<size_t>(channels);
  if (capacity_frames > (static_cast<size_t>(1) << 30) / channels_)
    fail(MC_ERR_INVALID_ARG, "audio ring too large");
  buf_.assign(capacity_ * channels_, 0);
}

void AudioRing::push(const int16_t* samples, size_t frames) {
  if (frames == 0) return;
  if (samples == nullptr) fail(MC_ERR_INVALID_ARG, "null samples");
  std::lock_guard<std::mutex> lk(mu_);
  if (frames >= capacity_) {
    on_drop(size_ + (frames - capacity_));
    const int16_t* src = samples + (frames - capacity_) * channels_;
    std::memcpy(buf_.data(), src, capacity_ * channels_ * sizeof(int16_t));
    head_ = 0;
    size_ = capacity_;
    return;
  }
  if (size_ + frames > capacity_) {
    const size_t over = size_ + frames - capacity_;
    on_drop(over);
    head_ = (head_ + over) % capacity_;
    size_ -= over;
  }
  size_t tail = (head_ + size_) % capacity_;
  size_t first = std::min(frames, capacity_ - tail);
  std::memcpy(buf_.data() + tail * channels_, samples, first * channels_ * sizeof(int16_t));
  if (frames > first)
    std::memcpy(buf_.data(), samples + first * channels_,
                (frames - first) * channels_ * sizeof(int16_t));
  size_ += frames;
}

size_t AudioRing::pop(int16_t* out, size_t cap) {
  std::lock_guard<std::mutex> lk(mu_);
  const size_t n = std::min(cap, size_);
  if (n == 0) return 0;
  if (out == nullptr) fail(MC_ERR_INVALID_ARG, "null out");
  const size_t first = std::min(n, capacity_ - head_);
  std::memcpy(out, buf_.data() + head_ * channels_, first * channels_ * sizeof(int16_t));
  if (n > first)
    std::memcpy(out + first * channels_, buf_.data(), (n - first) * channels_ * sizeof(int16_t));
  head_ = (head_ + n) % capacity_;
  size_ -= n;
  return n;
}

size_t AudioRing::available() const {
  std::lock_guard<std::mutex> lk(mu_);
  return size_;
}
uint64_t AudioRing::dropped() const {
  std::lock_guard<std::mutex> lk(mu_);
  return dropped_total();
}
void AudioRing::clear() {
  std::lock_guard<std::mutex> lk(mu_);
  head_ = 0;
  size_ = 0;
}

}  // namespace mediacore
