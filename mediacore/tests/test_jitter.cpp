#include "helpers.hpp"

namespace {
constexpr int kRate = 8000;
constexpr size_t kPkt = 80;  // 10 ms

struct Jb {
  mc_jitter_buffer* j = nullptr;
  Jb(int target_ms = 30, int max_ms = 500) {
    EXPECT_EQ(mc_jitter_buffer_create(kRate, target_ms, max_ms, &j), MC_OK);
  }
  ~Jb() { mc_jitter_buffer_destroy(j); }
  // Packet whose samples all equal `value`; timestamp derived from `logical` index.
  mc_status push(uint32_t seq, int16_t value, int64_t logical) {
    std::vector<int16_t> p(kPkt, value);
    return mc_jitter_buffer_push(j, seq, logical * 10'000'000, p.data(), p.size());
  }
  std::vector<int16_t> pop(size_t cap, mc_status* st = nullptr, int64_t* ts = nullptr) {
    std::vector<int16_t> out(cap);
    size_t n = 0;
    const mc_status s = mc_jitter_buffer_pop(j, out.data(), cap, &n, ts);
    if (st) *st = s;
    out.resize(n);
    return out;
  }
  mc_jitter_stats stats() {
    mc_jitter_stats s{};
    EXPECT_EQ(mc_jitter_buffer_stats(j, &s), MC_OK);
    return s;
  }
};
// Value of each 80-sample block.
std::vector<int16_t> blocks(const std::vector<int16_t>& v) {
  std::vector<int16_t> b;
  for (size_t i = 0; i < v.size(); i += kPkt) b.push_back(v[i]);
  return b;
}
}  // namespace

TEST(Jitter, NotPrimedUntilTargetDelay) {
  Jb jb;
  mc_status st = MC_OK;
  jb.pop(80, &st);
  EXPECT_EQ(st, MC_ERR_EOF);
  jb.push(0, 1, 0);
  jb.push(1, 2, 1);
  jb.pop(80, &st);
  EXPECT_EQ(st, MC_ERR_EOF);  // 20 ms < 30 ms target
  jb.push(2, 3, 2);
  auto out = jb.pop(80, &st);
  EXPECT_EQ(st, MC_OK);
  EXPECT_EQ(out.size(), 80u);
}

TEST(Jitter, ReordersBySequence) {
  Jb jb;
  jb.push(0, 1, 0);
  jb.push(2, 3, 2);
  jb.push(1, 2, 1);
  jb.push(3, 4, 3);
  int64_t ts = -1;
  auto out = jb.pop(1000, nullptr, &ts);
  ASSERT_EQ(out.size(), 320u);
  EXPECT_EQ(blocks(out), (std::vector<int16_t>{1, 2, 3, 4}));
  EXPECT_EQ(ts, 0);
  auto s = jb.stats();
  EXPECT_EQ(s.received, 4u);
  EXPECT_EQ(s.late, 0u);
  EXPECT_EQ(s.lost, 0u);
  EXPECT_EQ(s.concealed_frames, 0u);
  EXPECT_EQ(s.current_delay_ms, 0);
}

TEST(Jitter, PartialPopsAreContiguous) {
  Jb jb;
  for (uint32_t i = 0; i < 4; ++i) jb.push(i, static_cast<int16_t>(i + 1), i);
  int64_t ts = -1;
  auto a = jb.pop(100, nullptr, &ts);  // 80 + 20 samples
  EXPECT_EQ(ts, 0);
  EXPECT_EQ(a.size(), 100u);
  EXPECT_EQ(a[99], 2);
  auto b = jb.pop(100, nullptr, &ts);
  EXPECT_EQ(ts, 10'000'000 + 20 * 1'000'000'000LL / kRate);
  EXPECT_EQ(b[0], 2);
  EXPECT_EQ(b[59], 2);
  EXPECT_EQ(b[60], 3);
  EXPECT_EQ(jb.stats().current_delay_ms, 15);  // 120 samples left
}

TEST(Jitter, ConcealsGapWithSilence) {
  Jb jb;
  jb.push(0, 1, 0);
  jb.push(2, 3, 2);
  jb.push(3, 4, 3);
  auto out = jb.pop(1000);
  ASSERT_EQ(out.size(), 320u);
  EXPECT_EQ(blocks(out), (std::vector<int16_t>{1, 0, 3, 4}));
  auto s = jb.stats();
  EXPECT_EQ(s.lost, 1u);
  EXPECT_EQ(s.concealed_frames, kPkt);
}

TEST(Jitter, LatePacketsDroppedAndCounted) {
  Jb jb;
  jb.push(0, 1, 0);
  jb.push(2, 3, 2);
  jb.push(3, 4, 3);
  jb.pop(1000);
  EXPECT_EQ(jb.push(1, 2, 1), MC_OK);  // playout already passed seq 1
  EXPECT_EQ(jb.push(0, 1, 0), MC_OK);
  EXPECT_EQ(jb.stats().late, 2u);
  jb.push(4, 5, 4);
  jb.push(5, 6, 5);
  jb.push(6, 7, 6);
  auto out = jb.pop(1000);
  EXPECT_EQ(blocks(out), (std::vector<int16_t>{5, 6, 7}));
  EXPECT_EQ(jb.stats().late, 2u);
  // Duplicate of a buffered packet counts as late too.
  jb.push(7, 8, 7);
  jb.push(7, 8, 7);
  EXPECT_EQ(jb.stats().late, 3u);
}

TEST(Jitter, SequenceWraparound) {
  Jb jb;
  jb.push(0xFFFFFFFFu, 2, 1);
  jb.push(0, 3, 2);
  jb.push(0xFFFFFFFEu, 1, 0);
  jb.push(1, 4, 3);
  auto out = jb.pop(1000);
  EXPECT_EQ(blocks(out), (std::vector<int16_t>{1, 2, 3, 4}));
  EXPECT_EQ(jb.stats().late, 0u);
  // Continue past the wrap with a gap at seq 3.
  jb.push(2, 5, 4);
  jb.push(4, 7, 6);
  jb.push(5, 8, 7);
  auto out2 = jb.pop(1000);
  EXPECT_EQ(blocks(out2), (std::vector<int16_t>{5, 0, 7, 8}));
  EXPECT_EQ(jb.stats().lost, 1u);
}

TEST(Jitter, OverrunDiscardsOldestBeyondMaxDelay) {
  Jb jb(20, 50);  // max 400 samples = 5 packets
  for (uint32_t i = 0; i < 8; ++i) jb.push(i, static_cast<int16_t>(i + 1), i);
  EXPECT_LE(jb.stats().current_delay_ms, 50);
  auto out = jb.pop(10000);
  ASSERT_FALSE(out.empty());
  EXPECT_EQ(blocks(out).back(), 8);  // newest audio retained
  EXPECT_GE(jb.stats().lost, 3u);
}

TEST(Jitter, ResetAndValidation) {
  Jb jb;
  for (uint32_t i = 0; i < 3; ++i) jb.push(i, 1, i);
  mc_jitter_buffer_reset(jb.j);
  EXPECT_EQ(jb.stats().received, 0u);
  mc_status st = MC_OK;
  jb.pop(10, &st);
  EXPECT_EQ(st, MC_ERR_EOF);
  mc_jitter_buffer* x = nullptr;
  EXPECT_EQ(mc_jitter_buffer_create(0, 10, 100, &x), MC_ERR_INVALID_ARG);
  EXPECT_EQ(mc_jitter_buffer_create(8000, 100, 50, &x), MC_ERR_INVALID_ARG);
  EXPECT_EQ(mc_jitter_buffer_push(jb.j, 0, 0, nullptr, 5), MC_ERR_INVALID_ARG);
}

namespace {
int64_t g_now_ns = 0;
int64_t fake_clock(void*) { return g_now_ns; }

mc_jitter_adaptive_stats adaptive(Jb& b) {
  mc_jitter_adaptive_stats s{};
  EXPECT_EQ(mc_jitter_buffer_adaptive_stats(b.j, &s), MC_OK);
  return s;
}
}  // namespace

TEST(JitterAdaptive, TargetFollowsArrivalJitter) {
  Jb b(30, 500);
  ASSERT_EQ(mc_jitter_buffer_set_clock(b.j, fake_clock, nullptr), MC_OK);
  ASSERT_EQ(mc_jitter_buffer_set_adaptive(b.j, 10, 0), MC_OK);
  for (int i = 0; i < 80; ++i) {
    g_now_ns = i * 10'000'000LL + (i % 2) * 5'000'000LL;  // +-5 ms of arrival jitter
    ASSERT_EQ(b.push(static_cast<uint32_t>(i), 1, i), MC_OK);
    if (i % 4 == 3) b.pop(kPkt * 4);
  }
  const auto s = adaptive(b);
  EXPECT_GT(s.jitter_ms, 4.0);
  EXPECT_LT(s.jitter_ms, 5.5);
  EXPECT_GE(s.target_delay_ms, 25);  // 10 ms floor + 4 * jitter
  EXPECT_LE(s.target_delay_ms, 32);
  EXPECT_GT(s.adjustments, 0u);
}

TEST(JitterAdaptive, OffByDefaultAndClampedToMax) {
  Jb b(30, 60);
  for (int i = 0; i < 20; ++i) ASSERT_EQ(b.push(static_cast<uint32_t>(i), 1, i), MC_OK);
  EXPECT_EQ(adaptive(b).target_delay_ms, 30);
  EXPECT_EQ(adaptive(b).adjustments, 0u);
  Jb c(30, 60);
  ASSERT_EQ(mc_jitter_buffer_set_clock(c.j, fake_clock, nullptr), MC_OK);
  ASSERT_EQ(mc_jitter_buffer_set_adaptive(c.j, 10, 0), MC_OK);
  for (int i = 0; i < 200; ++i) {
    g_now_ns = i * 10'000'000LL + (i % 2) * 200'000'000LL;  // wild jitter
    ASSERT_EQ(c.push(static_cast<uint32_t>(i), 1, i), MC_OK);
    if (i % 4 == 3) c.pop(kPkt * 4);
  }
  EXPECT_EQ(adaptive(c).target_delay_ms, 60);
}

TEST(JitterAdaptive, AdaptIntervalLimitsReevaluation) {
  Jb b(30, 500);
  ASSERT_EQ(mc_jitter_buffer_set_clock(b.j, fake_clock, nullptr), MC_OK);
  ASSERT_EQ(mc_jitter_buffer_set_adaptive(b.j, 10, 3600 * 1000), MC_OK);
  for (int i = 0; i < 40; ++i) {
    g_now_ns = i * 10'000'000LL + (i % 2) * 5'000'000LL;
    ASSERT_EQ(b.push(static_cast<uint32_t>(i), 1, i), MC_OK);
  }
  EXPECT_EQ(adaptive(b).adjustments, 1u);  // only the first evaluation ran
  EXPECT_EQ(adaptive(b).target_delay_ms, 10);
}

TEST(JitterAdaptive, UnderrunRebuffers) {
  Jb b(30, 500);
  ASSERT_EQ(mc_jitter_buffer_set_clock(b.j, fake_clock, nullptr), MC_OK);
  ASSERT_EQ(mc_jitter_buffer_set_adaptive(b.j, 30, 0), MC_OK);
  for (int i = 0; i < 3; ++i) {
    g_now_ns = i * 10'000'000LL;
    ASSERT_EQ(b.push(static_cast<uint32_t>(i), 1, i), MC_OK);
  }
  mc_status st = MC_OK;
  EXPECT_EQ(b.pop(kPkt * 3, &st).size(), kPkt * 3);
  EXPECT_EQ(st, MC_OK);
  EXPECT_EQ(b.pop(kPkt, &st).size(), 0u);  // underrun
  EXPECT_EQ(st, MC_OK);
  b.pop(kPkt, &st);
  EXPECT_EQ(st, MC_ERR_EOF);  // re-priming
  EXPECT_EQ(adaptive(b).rebuffers, 1u);
  for (int i = 3; i < 6; ++i) {
    g_now_ns = i * 10'000'000LL;
    ASSERT_EQ(b.push(static_cast<uint32_t>(i), 2, i), MC_OK);
  }
  EXPECT_EQ(blocks(b.pop(kPkt * 3, &st)), (std::vector<int16_t>{2, 2, 2}));
  EXPECT_EQ(st, MC_OK);
}

TEST(JitterAdaptive, OverrunDropsAndValidation) {
  Jb b(20, 40);
  for (int i = 0; i < 10; ++i) ASSERT_EQ(b.push(static_cast<uint32_t>(i), 1, i), MC_OK);
  EXPECT_EQ(adaptive(b).overrun_drops, b.stats().lost);
  EXPECT_GT(adaptive(b).overrun_drops, 0u);
  EXPECT_EQ(mc_jitter_buffer_set_adaptive(b.j, 100, 0), MC_ERR_INVALID_ARG);  // above max delay
  EXPECT_EQ(mc_jitter_buffer_set_adaptive(b.j, 10, -1), MC_ERR_INVALID_ARG);
  EXPECT_EQ(mc_jitter_buffer_set_adaptive(nullptr, 10, 0), MC_ERR_INVALID_ARG);
  EXPECT_EQ(mc_jitter_buffer_adaptive_stats(b.j, nullptr), MC_ERR_INVALID_ARG);
  mc_jitter_buffer_reset(b.j);
  EXPECT_EQ(adaptive(b).overrun_drops, 0u);
}
