const test = require("node:test");
const assert = require("node:assert/strict");
const metrics = require("./static/usage-metrics.js");

test("derives cache coverage and reuse from mutually exclusive input categories", () => {
  const value = metrics.cacheStats({
    input_tokens: 100,
    cache_creation_5m_tokens: 20,
    cache_creation_1h_tokens: 30,
    cache_read_tokens: 150,
  });

  assert.deepEqual(value, {
    write5m: 20,
    write1h: 30,
    write: 50,
    read: 150,
    coverage: 0.5,
    reuse: 3,
  });
  assert.equal(metrics.formatRatio(value.coverage, "%"), "50.0%");
  assert.equal(metrics.formatRatio(value.reuse, "×"), "3.0×");
});

test("reports unavailable ratios when the denominator is absent", () => {
  assert.deepEqual(metrics.cacheStats({ cache_read_tokens: 25 }), {
    write5m: 0,
    write1h: 0,
    write: 0,
    read: 25,
    coverage: 1,
    reuse: null,
  });
  assert.equal(metrics.formatRatio(null, "×"), "—");
});

test("invalid or negative counters cannot produce misleading ratios", () => {
  assert.deepEqual(metrics.cacheStats({
    input_tokens: -10,
    cache_creation_5m_tokens: "invalid",
    cache_creation_1h_tokens: -20,
    cache_read_tokens: Number.POSITIVE_INFINITY,
  }), {
    write5m: 0,
    write1h: 0,
    write: 0,
    read: 0,
    coverage: null,
    reuse: null,
  });
});
