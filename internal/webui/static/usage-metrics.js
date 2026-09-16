(function attachUsageMetrics(root, factory) {
  const metrics = factory();
  if (typeof module === "object" && module.exports) module.exports = metrics;
  else root.ClaudeRelayUsageMetrics = metrics;
})(typeof globalThis === "object" ? globalThis : this, function createUsageMetrics() {
  function nonNegativeNumber(value) {
    const number = Number(value || 0);
    return Number.isFinite(number) ? Math.max(0, number) : 0;
  }

  function cacheStats(usage = {}) {
    const input = nonNegativeNumber(usage.input_tokens);
    const write5m = nonNegativeNumber(usage.cache_creation_5m_tokens);
    const write1h = nonNegativeNumber(usage.cache_creation_1h_tokens);
    const read = nonNegativeNumber(usage.cache_read_tokens);
    const write = write5m + write1h;
    const totalInput = input + write + read;
    return {
      write5m,
      write1h,
      write,
      read,
      coverage: totalInput > 0 ? read / totalInput : null,
      reuse: write > 0 ? read / write : null,
    };
  }

  function formatRatio(value, suffix) {
    if (!Number.isFinite(value)) return "—";
    const scaled = suffix === "%" ? value * 100 : value;
    return `${scaled.toFixed(1)}${suffix}`;
  }

  return { cacheStats, formatRatio };
});

