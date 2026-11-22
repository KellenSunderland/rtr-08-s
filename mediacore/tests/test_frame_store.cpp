#include <thread>

#include "helpers.hpp"

using testutil::Frame;

namespace {
constexpr size_t kFrameBytes = 16;  // 4x4 gray

struct Store {
  mc_frame_store* s = nullptr;
  explicit Store(mc_frame_store_config cfg) { EXPECT_EQ(mc_frame_store_create(&cfg, &s), MC_OK); }
  Store() { EXPECT_EQ(mc_frame_store_create(nullptr, &s), MC_OK); }
  ~Store() { mc_frame_store_destroy(s); }
  mc_status push(uint32_t stream, int64_t ts, uint8_t val = 0) {
    Frame f(4, 4, MC_PIXFMT_GRAY8);
    f.fill(val);
    f.f.ts_ns = ts;
    f.f.stream_id = stream;
    f.f.seq = static_cast<uint64_t>(ts);
    return mc_frame_store_push(s, f.get());
  }
  mc_frame_store_stats stats() {
    mc_frame_store_stats st{};
    EXPECT_EQ(mc_frame_store_stats_get(s, &st), MC_OK);
    return st;
  }
  mc_stream_stats stream_stats(uint32_t stream, int64_t now, int64_t window) {
    mc_stream_stats st{};
    EXPECT_EQ(mc_frame_store_stream_stats(s, stream, now, window, &st), MC_OK);
    return st;
  }
  std::vector<int64_t> range(uint32_t stream, int64_t from, int64_t to, size_t* total = nullptr) {
    std::vector<mc_frame_view> v(1000);
    size_t n = 0;
    EXPECT_EQ(mc_frame_store_range(s, stream, from, to, v.data(), v.size(), &n, total), MC_OK);
    std::vector<int64_t> ts;
    for (size_t i = 0; i < n; ++i) {
      ts.push_back(v[i].frame->ts_ns);
      mc_frame_view_release(&v[i]);
    }
    return ts;
  }
};

mc_frame_store_config cfg(int64_t ret, int64_t thin_after, int64_t iv, size_t max_bytes) {
  mc_frame_store_config c;
  mc_frame_store_config_default(&c);
  c.retention_ns = ret;
  c.thin_after_ns = thin_after;
  c.thinned_interval_ns = iv;
  c.max_bytes = max_bytes;
  return c;
}
}  // namespace

TEST(FrameStore, Defaults) {
  mc_frame_store_config c;
  mc_frame_store_config_default(&c);
  EXPECT_EQ(c.retention_ns, 3600LL * 1000000000LL);
  EXPECT_EQ(c.thin_after_ns, 300LL * 1000000000LL);
  EXPECT_EQ(c.thinned_interval_ns, 1000000000LL);
  EXPECT_EQ(c.max_bytes, size_t{2} << 30);
  c.thinned_interval_ns = 0;
  mc_frame_store* s = nullptr;
  EXPECT_EQ(mc_frame_store_create(&c, &s), MC_ERR_INVALID_ARG);
  EXPECT_EQ(s, nullptr);
}

TEST(FrameStore, PushLatestAndCopySemantics) {
  Store st;
  mc_frame_view v{};
  EXPECT_EQ(mc_frame_store_latest(st.s, 1, &v), MC_ERR_NOT_FOUND);
  // Strided input is repacked.
  std::vector<uint8_t> buf(32, 0xCC);
  mc_frame f{};
  f.data = buf.data();
  f.width = 4;
  f.height = 2;
  f.stride = 16;
  f.fmt = MC_PIXFMT_GRAY8;
  f.ts_ns = 10;
  f.stream_id = 1;
  for (size_t i = 0; i < 4; ++i) buf[i] = static_cast<uint8_t>(i + 1);
  for (size_t i = 0; i < 4; ++i) buf[16 + i] = static_cast<uint8_t>(i + 11);
  ASSERT_EQ(mc_frame_store_push(st.s, &f), MC_OK);
  buf.assign(32, 0);  // store must own a copy
  ASSERT_EQ(mc_frame_store_latest(st.s, 1, &v), MC_OK);
  EXPECT_EQ(v.frame->stride, 4);
  EXPECT_EQ(v.frame->data[3], 4);
  EXPECT_EQ(v.frame->data[4 + 3], 14);
  EXPECT_EQ(v.frame->ts_ns, 10);
  mc_frame_view_release(&v);
  EXPECT_EQ(v.lease, nullptr);
  mc_frame_view_release(&v);  // idempotent
  EXPECT_EQ(st.stats().frames, 1u);
  EXPECT_EQ(st.stats().bytes, 8u);
}

TEST(FrameStore, OutOfOrderRejected) {
  Store st;
  ASSERT_EQ(st.push(1, 100), MC_OK);
  EXPECT_EQ(st.push(1, 100), MC_ERR_INVALID_ARG);  // equal
  EXPECT_EQ(st.push(1, 50), MC_ERR_INVALID_ARG);
  EXPECT_EQ(st.push(2, 50), MC_OK);  // independent stream
  EXPECT_EQ(st.push(1, 101), MC_OK);
  EXPECT_EQ(st.stats().frames, 3u);
  EXPECT_EQ(st.stats().streams, 2u);
}

TEST(FrameStore, NearestTiesEarlierAndMaxDistance) {
  Store st;
  for (int64_t t : {100, 200, 400}) ASSERT_EQ(st.push(1, t, static_cast<uint8_t>(t / 100)), MC_OK);
  mc_frame_view v{};
  ASSERT_EQ(mc_frame_store_nearest(st.s, 1, 150, 0, &v), MC_OK);
  EXPECT_EQ(v.frame->ts_ns, 100);  // tie -> earlier
  mc_frame_view_release(&v);
  ASSERT_EQ(mc_frame_store_nearest(st.s, 1, 151, 0, &v), MC_OK);
  EXPECT_EQ(v.frame->ts_ns, 200);
  mc_frame_view_release(&v);
  ASSERT_EQ(mc_frame_store_nearest(st.s, 1, 100000, 0, &v), MC_OK);  // unlimited
  EXPECT_EQ(v.frame->ts_ns, 400);
  mc_frame_view_release(&v);
  ASSERT_EQ(mc_frame_store_nearest(st.s, 1, -50, 0, &v), MC_OK);
  EXPECT_EQ(v.frame->ts_ns, 100);
  mc_frame_view_release(&v);
  EXPECT_EQ(mc_frame_store_nearest(st.s, 1, 150, 49, &v), MC_ERR_NOT_FOUND);
  ASSERT_EQ(mc_frame_store_nearest(st.s, 1, 150, 50, &v), MC_OK);
  mc_frame_view_release(&v);
  EXPECT_EQ(mc_frame_store_nearest(st.s, 9, 150, 0, &v), MC_ERR_NOT_FOUND);
}

TEST(FrameStore, RangeIsHalfOpenWithCap) {
  Store st;
  for (int64_t t = 100; t <= 500; t += 100) ASSERT_EQ(st.push(1, t), MC_OK);
  size_t total = 0;
  EXPECT_EQ(st.range(1, 200, 400, &total), (std::vector<int64_t>{200, 300}));
  EXPECT_EQ(total, 2u);
  EXPECT_EQ(st.range(1, 100, 501), (std::vector<int64_t>{100, 200, 300, 400, 500}));
  EXPECT_TRUE(st.range(1, 400, 400).empty());
  EXPECT_TRUE(st.range(1, 500, 100).empty());
  EXPECT_TRUE(st.range(7, 0, 1000).empty());
  mc_frame_view v[2];
  size_t n = 0;
  ASSERT_EQ(mc_frame_store_range(st.s, 1, 0, 1000, v, 2, &n, &total), MC_OK);
  EXPECT_EQ(n, 2u);
  EXPECT_EQ(total, 5u);
  EXPECT_EQ(v[0].frame->ts_ns, 100);
  EXPECT_EQ(v[1].frame->ts_ns, 200);
  mc_frame_view_release(&v[0]);
  mc_frame_view_release(&v[1]);
}

TEST(FrameStore, RetentionDropsOld) {
  Store st(cfg(1000, 1000000, 10, 1 << 20));
  for (int64_t t = 100; t <= 900; t += 100) ASSERT_EQ(st.push(1, t), MC_OK);
  ASSERT_EQ(mc_frame_store_prune(st.s, 1500), MC_OK);  // cutoff 500: drop ts < 500
  EXPECT_EQ(st.range(1, 0, 10000), (std::vector<int64_t>{500, 600, 700, 800, 900}));
  EXPECT_EQ(st.stats().evicted_total, 4u);
  EXPECT_EQ(st.stats().bytes, 5 * kFrameBytes);
  ASSERT_EQ(mc_frame_store_prune(st.s, 100000), MC_OK);
  EXPECT_EQ(st.stats().frames, 0u);
  EXPECT_EQ(st.stats().oldest_ts_ns, 0);
  EXPECT_EQ(st.push(1, 900), MC_ERR_INVALID_ARG);  // last ts is remembered
}

TEST(FrameStore, ThinningKeepsFrameClosestToBoundary) {
  Store st(cfg(1000000, 1000, 100, 1 << 20));
  for (int64_t t = 0; t < 300; t += 30) ASSERT_EQ(st.push(1, t), MC_OK);
  // Fresh frames (within thin_after) are untouched.
  ASSERT_EQ(mc_frame_store_prune(st.s, 500), MC_OK);
  EXPECT_EQ(st.stats().frames, 10u);
  ASSERT_EQ(mc_frame_store_prune(st.s, 10000), MC_OK);
  // Buckets around boundaries 0,100,200,300: {0,30} {60,90,120} {150,180,210,240} {270}.
  EXPECT_EQ(st.range(1, 0, 1000), (std::vector<int64_t>{0, 90, 210, 270}));
  EXPECT_EQ(st.stats().thinned_total, 6u);
  EXPECT_EQ(st.stats().bytes, 4 * kFrameBytes);
  ASSERT_EQ(mc_frame_store_prune(st.s, 10000), MC_OK);  // idempotent
  EXPECT_EQ(st.stats().frames, 4u);
}

TEST(FrameStore, ThinningOnlyAffectsOldFrames) {
  Store st(cfg(1000000, 1000, 100, 1 << 20));
  for (int64_t t : {0, 10, 20, 5000, 5010, 5020}) ASSERT_EQ(st.push(1, t), MC_OK);
  ASSERT_EQ(mc_frame_store_prune(st.s, 6000), MC_OK);  // old: ts < 5000
  EXPECT_EQ(st.range(1, 0, 10000), (std::vector<int64_t>{0, 5000, 5010, 5020}));
}

TEST(FrameStore, MaxBytesEvictsOldestAcrossStreams) {
  Store st(cfg(1000000, 1000000, 10, 3 * kFrameBytes));
  ASSERT_EQ(st.push(1, 10), MC_OK);
  ASSERT_EQ(st.push(2, 20), MC_OK);
  ASSERT_EQ(st.push(1, 30), MC_OK);
  ASSERT_EQ(st.push(2, 40), MC_OK);  // evicts stream1@10
  EXPECT_EQ(st.range(1, 0, 100), (std::vector<int64_t>{30}));
  EXPECT_EQ(st.range(2, 0, 100), (std::vector<int64_t>{20, 40}));
  ASSERT_EQ(st.push(1, 50), MC_OK);  // evicts stream2@20
  EXPECT_EQ(st.range(2, 0, 100), (std::vector<int64_t>{40}));
  auto s = st.stats();
  EXPECT_EQ(s.frames, 3u);
  EXPECT_EQ(s.bytes, 3 * kFrameBytes);
  EXPECT_EQ(s.evicted_total, 2u);
  // A single frame larger than the cap is refused.
  Frame big(100, 100, MC_PIXFMT_GRAY8);
  big.f.ts_ns = 1000;
  EXPECT_EQ(mc_frame_store_push(st.s, big.get()), MC_ERR_FULL);
}

TEST(FrameStore, LeasedFramesAreNotEvicted) {
  Store st(cfg(1000, 1000000, 10, 2 * kFrameBytes));
  ASSERT_EQ(st.push(1, 100, 11), MC_OK);
  mc_frame_view lease{};
  ASSERT_EQ(mc_frame_store_latest(st.s, 1, &lease), MC_OK);
  ASSERT_EQ(st.push(1, 200, 22), MC_OK);
  ASSERT_EQ(st.push(1, 300, 33), MC_OK);  // over cap: 100 is leased, so 200 goes
  EXPECT_EQ(st.range(1, 0, 1000), (std::vector<int64_t>{100, 300}));
  EXPECT_EQ(lease.frame->data[0], 11);
  // Retention does not drop it either.
  ASSERT_EQ(mc_frame_store_prune(st.s, 5000), MC_OK);
  EXPECT_EQ(st.range(1, 0, 1000), (std::vector<int64_t>{100}));
  mc_frame_view_release(&lease);
  ASSERT_EQ(mc_frame_store_prune(st.s, 5000), MC_OK);
  EXPECT_EQ(st.stats().frames, 0u);
}

TEST(FrameStore, LeasedFrameSurvivesThinningAndRemoveStream) {
  Store st(cfg(1000000, 1000, 100, 1 << 20));
  for (int64_t t : {0, 10, 20}) ASSERT_EQ(st.push(1, t, static_cast<uint8_t>(t)), MC_OK);
  mc_frame_view lease{};
  ASSERT_EQ(mc_frame_store_nearest(st.s, 1, 20, 0, &lease), MC_OK);
  ASSERT_EQ(mc_frame_store_prune(st.s, 10000), MC_OK);
  EXPECT_EQ(st.range(1, 0, 100), (std::vector<int64_t>{0, 20}));
  ASSERT_EQ(mc_frame_store_remove_stream(st.s, 1), MC_OK);
  EXPECT_EQ(st.stats().frames, 0u);
  EXPECT_EQ(lease.frame->data[0], 20);  // memory stays valid until release
  mc_frame_view_release(&lease);
  EXPECT_EQ(st.push(1, 1), MC_OK);  // stream forgotten entirely
}

TEST(FrameStore, ConcurrentPushAndRead) {
  Store st;
  std::vector<std::thread> th;
  for (uint32_t id = 1; id <= 4; ++id)
    th.emplace_back([&, id] {
      for (int64_t t = 1; t <= 300; ++t) {
        ASSERT_EQ(st.push(id, t), MC_OK);
        mc_frame_view v{};
        if (mc_frame_store_latest(st.s, id, &v) == MC_OK) mc_frame_view_release(&v);
        if (t % 50 == 0) mc_frame_store_prune(st.s, t);
      }
    });
  for (auto& t : th) t.join();
  EXPECT_EQ(st.stats().frames, 1200u);
  EXPECT_EQ(st.stats().streams, 4u);
}

TEST(FrameStore, StreamStatsBasics) {
  Store st(cfg(1000, 1000000, 10, 1 << 20));
  mc_stream_stats unused{};
  EXPECT_EQ(mc_frame_store_stream_stats(st.s, 3, 0, 0, &unused), MC_ERR_NOT_FOUND);
  EXPECT_EQ(mc_frame_store_stream_stats(st.s, 3, 0, 0, nullptr), MC_ERR_INVALID_ARG);
  for (int64_t t = 100; t <= 500; t += 100) ASSERT_EQ(st.push(1, t), MC_OK);
  ASSERT_EQ(st.push(2, 50), MC_OK);
  auto a = st.stream_stats(1, 600, 300);
  EXPECT_EQ(a.frames, 5u);
  EXPECT_EQ(a.bytes, 5 * kFrameBytes);
  EXPECT_EQ(a.oldest_ts_ns, 100);
  EXPECT_EQ(a.newest_ts_ns, 500);
  EXPECT_EQ(a.window_frames, 3u);  // [300, 600): 300, 400, 500
  EXPECT_EQ(a.leased_frames, 0u);
  auto b = st.stream_stats(2, 600, 300);
  EXPECT_EQ(b.frames, 1u);
  EXPECT_EQ(b.window_frames, 0u);
  EXPECT_EQ(b.store_queries, a.store_queries + 1);
}

TEST(FrameStore, StreamStatsWindowIsHalfOpen) {
  Store st;
  for (int64_t t = 100; t <= 500; t += 100) ASSERT_EQ(st.push(1, t), MC_OK);
  EXPECT_EQ(st.stream_stats(1, 400, 200).window_frames, 2u);  // 200, 300
  EXPECT_EQ(st.stream_stats(1, 1000, 100).window_frames, 0u);
  EXPECT_EQ(st.stream_stats(1, 501, 1000).window_frames, 5u);
}

TEST(FrameStore, StreamStatsCountersAndLeases) {
  Store st(cfg(1000, 1000000, 10, 3 * kFrameBytes));
  for (int64_t t : {100, 200, 300, 400}) ASSERT_EQ(st.push(1, t), MC_OK);  // 100 evicted by cap
  ASSERT_EQ(st.push(2, 450), MC_OK);                                        // 200 evicted
  auto s1 = st.stream_stats(1, 500, 100);
  EXPECT_EQ(s1.frames, 2u);
  EXPECT_EQ(s1.evicted_total, 2u);
  EXPECT_EQ(st.stream_stats(2, 500, 100).evicted_total, 0u);
  mc_frame_view lease{};
  ASSERT_EQ(mc_frame_store_latest(st.s, 1, &lease), MC_OK);
  EXPECT_EQ(st.stream_stats(1, 500, 100).leased_frames, 1u);
  // Retention cut at 1700 - 1000 = 700 covers every frame; the leased one is only "expiring".
  EXPECT_EQ(st.stream_stats(1, 1700, 100).expiring_frames, 2u);
  ASSERT_EQ(mc_frame_store_prune(st.s, 1700), MC_OK);
  auto s2 = st.stream_stats(1, 1700, 100);
  EXPECT_EQ(s2.frames, 1u);
  EXPECT_EQ(s2.evicted_total, 3u);
  mc_frame_view_release(&lease);
}

TEST(FrameStore, StreamStatsThinnedPerStream) {
  Store st(cfg(1000000, 1000, 100, 1 << 20));
  for (int64_t t = 0; t < 300; t += 30) ASSERT_EQ(st.push(1, t), MC_OK);
  ASSERT_EQ(st.push(2, 5), MC_OK);
  ASSERT_EQ(mc_frame_store_prune(st.s, 10000), MC_OK);
  EXPECT_EQ(st.stream_stats(1, 10000, 100).thinned_total, 6u);
  EXPECT_EQ(st.stream_stats(2, 10000, 100).thinned_total, 0u);
  EXPECT_EQ(st.stats().thinned_total, 6u);
}
