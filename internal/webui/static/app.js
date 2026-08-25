// Claude Relay console. All timestamps arriving from /admin/v1 are epoch milliseconds.

const $ = (id) => document.getElementById(id);
const POLL_INTERVAL = 5000;
const REQUEST_LIMIT = 200;

const state = {
  apiKey: sessionStorage.getItem("claudeRelayAdminKey") || "",
  overview: null,
  accounts: [],
  accountUsage: {},
  usageLoading: new Set(),
  requests: [],
  usage: null,
  prices: [],
  expandedFiveHourAccounts: new Set(),
  autoRefreshEnabled: true,
  panel: "accounts",
  pendingOAuth: readJSON(sessionStorage, "claudeRelayPendingOAuth"),
  relayKeyVisible: false,
  filters: { account: "", outcome: "" },
  accountQuery: "",
  accountStatus: "all",
};

let pollTimer = null;
let toastTimer = null;
let inFlight = false;

/* ---------------- transport ---------------- */

async function api(path, options = {}) {
  const headers = new Headers(options.headers || {});
  headers.set("x-api-key", state.apiKey);
  if (options.body && !headers.has("content-type")) headers.set("content-type", "application/json");
  const response = await fetch(path, { ...options, headers });
  let payload = null;
  try { payload = await response.json(); } catch { /* empty or non-JSON body */ }
  if (!response.ok) {
    if (response.status === 401) logout("管理密钥无效或已经变更。");
    throw new Error(payload?.error?.message || payload?.message || `请求失败（${response.status}）`);
  }
  return payload;
}

async function probeHealth() {
  try {
    const response = await fetch("/healthz", { cache: "no-store" });
    return response.ok;
  } catch {
    return false;
  }
}

/* ---------------- loading ---------------- */

async function loadOverview() {
  state.overview = await api("/admin/v1/overview");
}

async function loadAccounts() {
  const payload = await api("/admin/v1/accounts");
  state.accounts = Array.isArray(payload.accounts) ? payload.accounts : [];
}

async function loadRequests() {
  const payload = await api(`/admin/v1/requests?limit=${REQUEST_LIMIT}`);
  state.requests = Array.isArray(payload.requests) ? payload.requests : [];
}

async function loadUsage({ notify = false } = {}) {
  const seconds = Number($("usageRange")?.value || 604800);
  const from = seconds > 0 ? Date.now() - seconds * 1000 : 0;
  const [usage, prices] = await Promise.all([
    api(`/admin/v1/usage?from=${from}`),
    api("/admin/v1/usage/prices"),
  ]);
  state.usage = usage;
  state.prices = Array.isArray(prices.prices) ? prices.prices : [];
  renderUsage();
  if (notify) showToast("用量统计已刷新");
}

async function loadAccountUsage(account, { notify = false } = {}) {
  const alias = account.alias;
  if (!alias || state.usageLoading.has(alias)) return false;
  state.usageLoading.add(alias);
  renderAccounts();
  try {
    const path = `/admin/v1/accounts/${encodeURIComponent(alias)}/usage/refresh`;
    const usage = await api(path, { method: "POST" });
    state.accountUsage[alias] = { status: "success", ...usage, refresh_error: "" };
    if (notify) showToast(`${alias} 的订阅额度已刷新`);
    return true;
  } catch (error) {
    const previous = state.accountUsage[alias];
    state.accountUsage[alias] = previous?.status === "success"
      ? { ...previous, refresh_error: error.message }
      : { status: "error", error: error.message };
    if (notify) showToast(error.message, true);
    return false;
  } finally {
    state.usageLoading.delete(alias);
    renderAccounts();
  }
}

async function refreshAll({ notify = false, includeRequests = null } = {}) {
  if (inFlight) return;
  inFlight = true;
  const wantRequests = includeRequests ?? state.panel === "requests";
  try {
    const tasks = [loadOverview(), loadAccounts()];
    if (wantRequests) tasks.push(loadRequests());
    const [healthy] = await Promise.all([probeHealth(), ...tasks]);
    renderHealth(healthy);
    render();
    if (notify) showToast("已刷新");
  } catch (error) {
    if (state.apiKey) showToast(error.message, true);
  } finally {
    inFlight = false;
  }
}

function startPolling() {
  stopPolling();
  if (!state.autoRefreshEnabled) return;
  pollTimer = setInterval(() => {
    if (document.visibilityState === "visible" && state.apiKey) refreshAll();
  }, POLL_INTERVAL);
}

function stopPolling() {
  if (pollTimer) clearInterval(pollTimer);
  pollTimer = null;
}

/* ---------------- rendering ---------------- */

function render() {
  renderStats();
  renderAccounts();
  renderRequests();
  renderConnect();
}

function renderHealth(healthy) {
  const chip = $("healthChip");
  chip.className = `chip ${healthy ? "chip-ok" : "chip-bad"}`;
  $("healthText").textContent = healthy ? "服务在线" : "服务无响应";
}

function renderStats() {
  const overview = state.overview;
  if (!overview) return;
  const totals = overview.accounts || {};
  const summary = overview.requests || {};

  const available = totals.available || 0;
  const total = totals.total || 0;
  const attention = state.accounts.length
    ? state.accounts.filter((account) => {
      const label = accountStatus(account).label;
      return label === "冷却中" || label === "待刷新" || label === "令牌过期";
    }).length
    : (totals.cooling || 0) + (totals.expired || 0);

  setStat("statAvailable", available, available === 0 && total > 0 ? "is-bad" : "");
  $("statAvailableTotal").textContent = ` / ${total}`;
  $("statAvailableNote").textContent = `共 ${totals.total || 0} 个账号，${totals.enabled || 0} 个已启用`;

  $("statAttention").textContent = String(attention);
  $("statAttention").closest(".stat").classList.toggle("is-warn", attention > 0);
  $("statAttentionIcon").textContent = attention > 0 ? "!" : "✓";
  $("statAttentionIcon").classList.toggle("stat-icon-warn", attention > 0);
  $("statAttentionIcon").classList.toggle("stat-icon-ok", attention === 0);
  $("statAttentionNote").textContent = attention > 0
    ? `${totals.cooling || 0} 个冷却 · ${totals.expired || 0} 个凭据需处理`
    : "所有已启用账号均正常";

  setStat("statRecent", summary.recent_requests || 0, (summary.recent_failures || 0) > 0 ? "is-warn" : "");
  $("statRecentNote").textContent = (summary.recent_failures || 0) > 0
    ? `${summary.recent_failures} 次失败`
    : "无失败";

  const requestTotal = summary.requests || 0;
  const failures = summary.failures || 0;
  $("statSuccess").textContent = requestTotal === 0 ? "—" : `${Math.round(((requestTotal - failures) / requestTotal) * 100)}%`;
  $("statSuccessNote").textContent = requestTotal === 0 ? "本次启动尚无请求" : `${requestTotal} 次累计请求 · ${failures} 次失败`;

  $("versionTag").textContent = overview.version || "dev";
  $("uptimeText").textContent = overview.started_at ? `已运行 ${formatDuration(Date.now() - overview.started_at)}` : "";
}

function setStat(id, value, modifier) {
  const element = $(id);
  element.textContent = String(value);
  const card = element.closest(".stat");
  if (!card) return;
  card.classList.toggle("is-warn", modifier === "is-warn");
  card.classList.toggle("is-bad", modifier === "is-bad");
}

function accountStatus(account) {
  const now = Date.now();
  if (!account.enabled) {
    return { label: "已停用", css: "badge-off", note: "不参与调度，也不刷新令牌" };
  }
  if (account.cooldown && account.cooldown.until_at > now) {
    const scope = account.cooldown.model ? `模型 ${account.cooldown.model}` : "全部模型";
    return {
      label: "冷却中",
      css: "badge-warn",
      note: `${scope} · ${formatDuration(account.cooldown.until_at - now)}后恢复${account.cooldown.reason ? " · " + account.cooldown.reason : ""}`,
    };
  }
  if (isExpired(account)) {
    if (state.overview?.auto_refresh_enabled && account.has_refresh_token) {
      return { label: "待刷新", css: "badge-warn", note: "下一次请求时将自动续期" };
    }
    return { label: "令牌过期", css: "badge-bad", note: "刷新令牌或重新授权后才能使用" };
  }
  return { label: "可用", css: "badge-ok", note: "" };
}

function isExpired(account) {
  if (!account.expires_at) return false;
  const expiry = Date.parse(account.expires_at);
  return Number.isFinite(expiry) && expiry <= Date.now();
}

function renderAccounts() {
  const body = $("accountsBody");
  body.replaceChildren();
  const hasAccounts = state.accounts.length !== 0;
  $("accountsEmpty").classList.toggle("hidden", hasAccounts);

  const enabled = state.accounts.filter((account) => account.enabled).length;
  $("accountSummary").textContent = !hasAccounts
    ? "没有账号"
    : `${state.accounts.length} 个账号 · ${enabled} 个已启用 · ${state.accounts.length - enabled} 个已停用`;
  $("tabAccountCount").textContent = String(state.accounts.length);
  const query = state.accountQuery.trim().toLowerCase();
  const visible = state.accounts.filter((account) => {
    if (query && ![account.alias, account.email, account.account_uuid].some((value) =>
      String(value || "").toLowerCase().includes(query))) return false;
    if (state.accountStatus === "enabled" && !account.enabled) return false;
    if (state.accountStatus === "disabled" && account.enabled) return false;
    if (state.accountStatus === "attention" && !accountNeedsAttention(account)) return false;
    return true;
  });

  $("accountsNoMatch").classList.toggle("hidden", !hasAccounts || visible.length !== 0);
  for (const account of visible) {
    body.appendChild(accountRow(account));
  }
}

function accountRow(account) {
  const row = document.createElement("article");
  const status = accountStatus(account);
  const pool = accountPoolView(account.pool);
  row.className = `account-item ${status.css.replace("badge-", "account-")}`;

  const identity = document.createElement("div");
  identity.className = "account-identity";
  const avatar = document.createElement("span");
  avatar.className = "account-avatar";
  avatar.textContent = account.alias.slice(0, 2).toUpperCase();
  avatar.setAttribute("aria-hidden", "true");
  const identityCopy = document.createElement("div");
  identityCopy.className = "identity-copy";
  const identityLine = document.createElement("div");
  identityLine.className = "identity-line";
  identityLine.append(copyChip(account.alias, {
    label: account.alias,
    className: "alias-chip",
    message: `别名 ${account.alias} 已复制`,
    title: "点击复制别名",
  }));
  const statusBadge = badge(status.label, status.css);
  statusBadge.classList.add("account-status-badge");
  statusBadge.title = status.note || status.label;
  identityLine.append(statusBadge);
  identityCopy.append(identityLine, accountContactLine(account));
  const poolBadge = badge(pool.label, pool.css);
  poolBadge.classList.add("account-pool-badge");
  poolBadge.title = pool.note;
  identityCopy.appendChild(poolBadge);
  identity.append(avatar, identityCopy);

  const stats = account.stats || {};
  const metrics = document.createElement("div");
  metrics.className = "account-metrics";
  metrics.append(
    accountMetric("请求", stats.requests ? `${stats.requests}` : "—", stats.requests ? `${stats.failures || 0} 次失败` : ""),
    accountMetric("活跃会话", `${account.active_sessions || 0}`, "近 5 分钟"),
    accountMetric("粘性绑定", `${account.sticky_sessions || 0}`, "近 1 小时"),
    accountMetric("并发", `${account.in_flight || 0}`, "当前请求"),
  );

  const actions = document.createElement("div");
  actions.className = "account-actions";
  actions.appendChild(button(account.enabled ? "停用" : "启用", "btn btn-inline btn-toggle", () => toggleAccount(account)));
  actions.appendChild(button("详情", "btn btn-inline", () => openActions(account)));

  const main = document.createElement("div");
  main.className = "account-item-main";
  main.append(identity, accountUsageSummary(account), metrics, actions);
  row.appendChild(main);

  return row;
}

function accountContactLine(account) {
  if (account.email) {
    return copyChip(account.email, {
      label: account.email,
      className: "contact-chip",
      message: "邮箱已复制",
      title: "点击复制邮箱",
    });
  }
  if (account.account_uuid) {
    return copyChip(account.account_uuid, {
      label: shortenUUID(account.account_uuid),
      className: "contact-chip",
      message: "账号身份已复制",
      title: `点击复制账号身份 ${account.account_uuid}`,
    });
  }
  return small("未知身份");
}

function accountMetric(label, value, noteText) {
  const metric = document.createElement("div");
  metric.className = "account-metric";
  metric.append(small(label), strong(value));
  if (noteText) metric.appendChild(small(noteText));
  return metric;
}

function accountNeedsAttention(account) {
  const label = accountStatus(account).label;
  return label === "冷却中" || label === "待刷新" || label === "令牌过期";
}

function renderRequests() {
  const select = $("filterAccount");
  const aliases = [...new Set(state.requests.map((record) => record.account).filter(Boolean))].sort();
  const previous = state.filters.account;
  select.replaceChildren(option("全部", ""));
  for (const alias of aliases) select.appendChild(option(alias, alias));
  select.value = aliases.includes(previous) ? previous : "";
  state.filters.account = select.value;

  const visible = state.requests.filter((record) => {
    if (state.filters.account && record.account !== state.filters.account) return false;
    if (state.filters.outcome === "failed" && !isFailure(record)) return false;
    return true;
  });

  const capacity = state.overview?.requests?.capacity ?? 0;
  $("requestSummary").textContent = capacity === 0
    ? "请求记录已关闭（request_log_size = 0）"
    : `显示 ${visible.length} / ${state.requests.length} 条 · 内存容量 ${capacity} 条`;

  const body = $("requestsBody");
  body.replaceChildren();
  $("requestsEmpty").classList.toggle("hidden", visible.length !== 0);
  $("requestsEmptyNote").textContent = capacity === 0
    ? "配置项 request_log_size 为 0，本服务不保留任何请求记录。"
    : "请求记录只保存在内存中，重启进程即清空，且不包含任何提示词或响应内容。";

  for (const record of visible) {
    body.appendChild(requestRow(record));
  }
}

function isFailure(record) {
  return Boolean(record.error) || record.status === 0 || record.status >= 400;
}

function renderUsage() {
  const dashboard = state.usage;
  if (!dashboard) return;
  const totals = dashboard.totals || {};
  const usage = totals.usage || {};
  $("usageCost").textContent = formatUSD(totals.cost_usd || 0);
  $("usageCostNote").textContent = totals.unpriced ? "不含尚未定价模型" : "按发生时价格估算";
  $("usageInput").textContent = formatTokens(usage.input_tokens || 0);
  $("usageCacheRead").textContent = formatTokens(usage.cache_read_tokens || 0);
  $("usageOutput").textContent = formatTokens(usage.output_tokens || 0);

  const unpriced = Array.isArray(dashboard.unpriced_models) ? dashboard.unpriced_models : [];
  const warning = $("usageUnpriced");
  warning.classList.toggle("hidden", unpriced.length === 0);
  warning.textContent = unpriced.length ? `尚未定价：${unpriced.join("、")}。原始 usage 已保存，添加价格后即可估值。` : "";

  renderUsageRows($("usageModelsBody"), dashboard.by_model || [], "model", true);
  renderUsageRows($("usageAccountsBody"), dashboard.by_account || [], "account", false);

  const estimates = Array.isArray(dashboard.five_hour_estimates) ? dashboard.five_hour_estimates : [];
  $("usageEstimatesEmpty").classList.toggle("hidden", estimates.length !== 0);
  $("usageEstimatesTable").classList.toggle("hidden", estimates.length === 0);
  const estimateBody = $("usageEstimatesBody");
  estimateBody.replaceChildren();
  for (const estimate of estimates.slice().reverse()) {
    const account = estimate.account || "";
    const models = Array.isArray(estimate.by_model) ? estimate.by_model : [];
    const row = document.createElement("tr");
    const accountCell = cell(document.createTextNode(account || "—"));
    let detailRow = null;
    if (models.length > 0) {
      const expanded = state.expandedFiveHourAccounts.has(account);
      const toggle = document.createElement("button");
      toggle.type = "button";
      toggle.className = "usage-estimate-toggle";
      toggle.setAttribute("aria-expanded", String(expanded));
      const indicator = document.createElement("span");
      indicator.className = "usage-estimate-indicator";
      indicator.textContent = "›";
      toggle.append(indicator, document.createTextNode(account || "—"));
      accountCell.replaceChildren(toggle);

      detailRow = buildFiveHourModelDetails(estimate, models);
      detailRow.classList.toggle("hidden", !expanded);
      toggle.addEventListener("click", () => {
        const nextExpanded = toggle.getAttribute("aria-expanded") !== "true";
        toggle.setAttribute("aria-expanded", String(nextExpanded));
        detailRow.classList.toggle("hidden", !nextExpanded);
        if (nextExpanded) state.expandedFiveHourAccounts.add(account);
        else state.expandedFiveHourAccounts.delete(account);
      });
    }
    const observedCost = numberCell(formatUSD(estimate.observed_cost_usd || 0));
    const fullWindowCost = numberCell(formatUSD(estimate.full_window_usd || 0));
    if (estimate.unpriced) {
      observedCost.title = "不含未定价模型";
      fullWindowCost.title = "不含未定价模型";
    }
    row.append(
      accountCell,
      cell(document.createTextNode(`${new Date(estimate.from).toLocaleString()} → ${new Date(estimate.to).toLocaleString()}`)),
      numberCell(`${Number(estimate.used_percent_delta || 0).toFixed(1)}%`),
      observedCost,
      fullWindowCost,
    );
    estimateBody.appendChild(row);
    if (detailRow) estimateBody.appendChild(detailRow);
  }

  const pricesBody = $("pricesBody");
  pricesBody.replaceChildren();
  for (const price of state.prices) {
    const row = document.createElement("tr");
    row.title = price.source || "";
    row.append(
      cell(document.createTextNode(price.model_pattern)),
      cell(document.createTextNode(price.effective_from <= 1000 ? "初始价格" : new Date(price.effective_from).toLocaleString())),
      numberCell(formatPrice(price.input_usd_per_mtok)),
      numberCell(formatPrice(price.cache_creation_5m_usd_per_mtok)),
      numberCell(formatPrice(price.cache_creation_1h_usd_per_mtok)),
      numberCell(formatPrice(price.cache_read_usd_per_mtok)),
      numberCell(formatPrice(price.output_usd_per_mtok)),
    );
    pricesBody.appendChild(row);
  }
}

function buildFiveHourModelDetails(estimate, models) {
  const row = document.createElement("tr");
  row.className = "usage-estimate-details hidden";
  const wrapperCell = document.createElement("td");
  wrapperCell.colSpan = 5;
  const wrapper = document.createElement("div");
  wrapper.className = "usage-estimate-models";
  const table = document.createElement("table");
  table.className = "grid usage-table usage-estimate-model-table";
  const head = document.createElement("thead");
  const headRow = document.createElement("tr");
  for (const [label, numeric] of [["模型", false], ["请求", true], ["输入", true], ["缓存写入", true], ["缓存读取", true], ["输出", true], ["API 等价值", true], ["占比", true]]) {
    const th = document.createElement("th");
    th.textContent = label;
    if (numeric) th.className = "col-number";
    headRow.appendChild(th);
  }
  head.appendChild(headRow);
  const body = document.createElement("tbody");
  const total = Number(estimate.observed_cost_usd || 0);
  for (const value of models) {
    const usage = value.usage || {};
    const writes = (usage.cache_creation_5m_tokens || 0) + (usage.cache_creation_1h_tokens || 0);
    const modelRow = document.createElement("tr");
    modelRow.append(
      cell(stack(strong(value.model || "—"), value.unpriced ? small("未定价") : null)),
      numberCell(formatTokens(usage.requests || 0)),
      numberCell(formatTokens(usage.input_tokens || 0)),
      numberCell(formatTokens(writes)),
      numberCell(formatTokens(usage.cache_read_tokens || 0)),
      numberCell(formatTokens(usage.output_tokens || 0)),
      numberCell(value.unpriced ? "—" : formatUSD(value.cost_usd || 0)),
      numberCell(value.unpriced || total <= 0 ? "—" : `${(Number(value.cost_usd || 0) / total * 100).toFixed(1)}%`),
    );
    body.appendChild(modelRow);
  }
  table.append(head, body);
  wrapper.appendChild(table);
  wrapperCell.appendChild(wrapper);
  row.appendChild(wrapperCell);
  return row;
}

function renderUsageRows(body, values, key, includeCacheWrite) {
  body.replaceChildren();
  for (const value of values) {
    const usage = value.usage || {};
    const row = document.createElement("tr");
    row.append(
      cell(stack(strong(value[key] || "—"), value.unpriced ? small("未定价") : null)),
      numberCell(formatTokens(usage.requests || 0)),
      numberCell(formatTokens(usage.input_tokens || 0)),
    );
    if (includeCacheWrite) {
      const writes = (usage.cache_creation_5m_tokens || 0) + (usage.cache_creation_1h_tokens || 0);
      row.appendChild(numberCell(formatTokens(writes)));
    }
    row.append(
      numberCell(formatTokens(usage.cache_read_tokens || 0)),
      numberCell(formatTokens(usage.output_tokens || 0)),
      numberCell(value.unpriced ? "—" : formatUSD(value.cost_usd || 0)),
    );
    body.appendChild(row);
  }
}

function numberCell(value) {
  const td = cell(document.createTextNode(value));
  td.className = "col-number";
  return td;
}

function formatTokens(value) {
  const number = Number(value || 0);
  if (number >= 1_000_000_000) return `${(number / 1_000_000_000).toFixed(2)}B`;
  if (number >= 1_000_000) return `${(number / 1_000_000).toFixed(2)}M`;
  if (number >= 1_000) return `${(number / 1_000).toFixed(1)}K`;
  return number.toLocaleString();
}

function formatUSD(value) {
  return `$${Number(value || 0).toFixed(Number(value || 0) < 1 ? 4 : 2)}`;
}

function formatPrice(value) {
  return `$${Number(value || 0).toFixed(2)}`;
}

function openPriceDialog() {
  $("pricePattern").value = "";
  const local = new Date(Date.now() - new Date().getTimezoneOffset() * 60000).toISOString().slice(0, 16);
  $("priceEffective").value = local;
  for (const id of ["priceInput", "priceOutput", "priceCache5m", "priceCache1h", "priceCacheRead"]) $(id).value = "";
  $("priceSource").value = "";
  $("priceError").textContent = "";
  $("priceDialog").showModal();
  setTimeout(() => $("pricePattern").focus(), 40);
}

async function savePrice(element) {
  const fields = ["priceInput", "priceOutput", "priceCache5m", "priceCache1h", "priceCacheRead"];
  const values = fields.map((id) => Number($(id).value));
  const effective = new Date($("priceEffective").value).getTime();
  if (!$("pricePattern").value.trim() || values.some((value) => !Number.isFinite(value) || value < 0) || !Number.isFinite(effective)) {
    $("priceError").textContent = "请填写模型规则、生效时间和全部非负价格。";
    return;
  }
  setBusy(element, true);
  try {
    await api("/admin/v1/usage/prices", { method: "POST", body: JSON.stringify({
      model_pattern: $("pricePattern").value.trim(), effective_from: effective,
      input_usd_per_mtok: values[0], output_usd_per_mtok: values[1],
      cache_creation_5m_usd_per_mtok: values[2], cache_creation_1h_usd_per_mtok: values[3],
      cache_read_usd_per_mtok: values[4], source: $("priceSource").value.trim(),
    }) });
    $("priceDialog").close();
    await loadUsage();
    showToast("模型价格版本已添加");
  } catch (error) {
    $("priceError").textContent = error.message;
  } finally {
    setBusy(element, false);
  }
}

async function clearUsage() {
  const accepted = await confirmDialog({ title: "清空用量统计", lead: "这会永久删除已采集的 token 聚合和订阅额度快照。模型价格不会删除。", items: ["历史 API 等价值和五小时估值无法恢复"], accept: "永久清空", danger: true });
  if (!accepted) return;
  try {
    await api("/admin/v1/usage", { method: "DELETE" });
    await loadUsage();
    showToast("用量统计已清空");
  } catch (error) {
    showToast(error.message, true);
  }
}

function requestRow(record) {
  const row = document.createElement("tr");

  const time = cell(document.createTextNode(formatRelative(record.at)));
  time.title = new Date(record.at).toLocaleString();
  row.appendChild(time);

  const failed = isFailure(record);
  const label = record.status ? String(record.status) : "无响应";
  const outcome = cell(stack(
    badge(label, failed ? "badge-bad" : "badge-ok"),
    record.error ? small(record.error) : null,
  ));
  row.appendChild(outcome);

  row.appendChild(cell(document.createTextNode(record.model || "—")));

  const path = document.createElement("code");
  path.className = "mono";
  path.textContent = endpointLabel(record.path);
  path.title = record.path || "";
  row.appendChild(cell(path));

  const ingress = ingressView(record.ingress);
  const ingressCell = cell(badge(ingress.label, ingress.css));
  ingressCell.title = ingress.note;
  row.appendChild(ingressCell);

  const client = clientClassView(record.client_class);
  const clientCell = cell(stack(
    badge(client.label, client.css),
    small(`${evidenceSummary(record.client_evidence)} · ${relayActionLabel(record.relay_action)}`),
  ));
  clientCell.title = `分类器 v${record.classification_version || "?"}`;
  row.appendChild(clientCell);

  row.appendChild(cell(stack(
    strong(record.account || "—"),
    record.failover ? small(`已从 ${record.failover.account} 转移（${record.failover.status || record.failover.error || "无响应"}）`) : null,
  )));

  row.appendChild(cell(badge(selectionLabel(record.selection), "badge-off badge-plain")));

  const duration = cell(document.createTextNode(`${record.duration_ms} ms`));
  duration.className = "col-number";
  row.appendChild(duration);

  const id = document.createElement("code");
  id.className = "mono";
  id.textContent = (record.request_id || "").slice(0, 12) || "—";
  id.title = record.request_id || "";
  const idCell = cell(id);
  row.appendChild(idCell);
  return row;
}

const SELECTION_LABELS = {
  header: "指定账号",
  account_uuid: "签名身份",
  sticky: "粘性会话",
  session_pending: "新会话并发请求",
  load_balance: "负载均衡",
  cache_affinity: "缓存亲和",
};

function selectionLabel(selection) {
  if (!selection) return "未选中";
  return SELECTION_LABELS[selection] || selection;
}

function clientClassView(value) {
  if (value === "cc_candidate") return { label: "CC 候选", css: "badge-ok" };
  if (value === "compatible") return { label: "普通兼容", css: "badge-off" };
  if (value === "ambiguous") return { label: "特征不完整", css: "badge-warn" };
  return { label: "未分类", css: "badge-off" };
}

function endpointLabel(path) {
  if (path === "/v1/messages/count_tokens") return "count_tokens";
  if (path === "/v1/messages") return "messages";
  return path || "—";
}

// 账号池是单向可渗透的：official 入口可以使用任意账号，兼容入口只能使用
// compatible 池的账号。
function accountPoolView(pool) {
  if (pool === "official") return { label: "official 专用", css: "badge-ok", note: "只承接 official 入口请求" };
  return { label: "共享", css: "badge-off badge-plain", note: "兼容入口与 official 入口都可使用" };
}

function ingressView(ingress) {
  if (ingress === "official") return { label: "official", css: "badge-ok", note: "official 入口密钥" };
  return { label: "compatible", css: "badge-off badge-plain", note: "兼容入口密钥" };
}

const USAGE_WINDOW_LABELS = {
  five_hour: "5 小时",
  seven_day: "7 天",
  seven_day_oauth_apps: "OAuth 应用 7 天",
  seven_day_opus: "Opus 7 天",
  seven_day_sonnet: "Sonnet 7 天",
  seven_day_cowork: "Cowork 7 天",
  seven_day_fable: "Fable 7 天",
};

function accountUsageDisplay(account, usage) {
  const windows = usage?.status === "success" && Array.isArray(usage.windows)
    ? usage.windows.filter((window) => {
      if (window.id !== "five_hour" || !window.resets_at) return true;
      const reset = Date.parse(window.resets_at);
      return !Number.isFinite(reset) || reset > Date.now();
    })
    : [];
  const sampled = account.five_hour_window;
  const sampledAt = Number(sampled?.observed_at) || 0;
  const fetchedAt = usage?.status === "success" ? Number(usage.fetched_at) || 0 : 0;
  let responseDerived = false;
  if (sampled) {
    const index = windows.findIndex((window) => window.id === "five_hour");
    if (index < 0) {
      windows.unshift(sampled);
      responseDerived = true;
    } else if (sampledAt >= fetchedAt) {
      windows[index] = sampled;
      responseDerived = true;
    }
  }
  return {
    windows,
    updatedAt: responseDerived ? sampledAt : fetchedAt,
    responseDerived,
  };
}

function accountUsageSummary(account) {
  const usage = state.accountUsage[account.alias];
  const display = accountUsageDisplay(account, usage);
  const loading = state.usageLoading.has(account.alias);
  const wrapper = document.createElement("div");
  wrapper.className = "account-usage";

  const heading = document.createElement("div");
  heading.className = "usage-heading";
  const title = document.createElement("span");
  title.className = "usage-title";
  title.textContent = "订阅额度";
  heading.appendChild(title);
  if (usage?.status === "success" && usage.plan_type) {
    const plan = document.createElement("span");
    plan.className = "usage-plan";
    plan.textContent = usage.plan_type.toUpperCase();
    heading.appendChild(plan);
  }
  wrapper.appendChild(heading);

  const refresh = button(loading ? "读取中" : "刷新", "btn btn-inline usage-refresh", () => {
    void loadAccountUsage(account, { notify: true });
  });
  refresh.disabled = loading;

  if (!usage && display.windows.length === 0) {
    const empty = document.createElement("div");
    empty.className = "usage-empty";
    empty.append(small(loading ? "正在读取额度…" : "尚未读取"), refresh);
    wrapper.appendChild(empty);
    return wrapper;
  }

  if (usage?.status === "error" && display.windows.length === 0) {
    const error = document.createElement("div");
    error.className = "usage-error";
    const message = small(usage.error || "额度读取失败");
    message.title = usage.error || "";
    error.append(badge("读取失败", "badge-bad"), message, refresh);
    wrapper.appendChild(error);
    return wrapper;
  }

  const windows = display.windows;
  for (const window of windows.slice(0, 2)) wrapper.appendChild(quotaMeter(window));
  if (windows.length === 0) {
    const empty = small("上游未返回额度窗口");
    empty.className = "usage-empty-note";
    wrapper.appendChild(empty);
  } else if (windows.length > 2) {
    const more = small(`还有 ${windows.length - 2} 个额度窗口`);
    more.className = "usage-more";
    wrapper.appendChild(more);
  }

  const footer = document.createElement("div");
  footer.className = "usage-footer";
  let footerText = display.responseDerived
    ? `请求响应更新于 ${formatRelative(display.updatedAt)}`
    : `更新于 ${formatRelative(display.updatedAt)}`;
  if (usage?.status === "error") footerText = "手动刷新失败，显示请求响应结果";
  if (usage?.refresh_error) footerText = "手动刷新失败，显示最近结果";
  const fetched = small(footerText);
  if (usage?.status === "error") fetched.title = usage.error || "额度读取失败";
  if (usage?.refresh_error) fetched.title = `最近刷新失败：${usage.refresh_error}`;
  footer.append(fetched, refresh);
  wrapper.appendChild(footer);
  return wrapper;
}

function quotaMeter(window) {
  const remaining = Math.max(0, Math.min(100, Number(window.remaining_percent) || 0));
  const wrapper = document.createElement("div");
  wrapper.className = "quota-meter";
  const heading = document.createElement("div");
  heading.className = "quota-meter-head";
  const label = document.createElement("span");
  label.textContent = USAGE_WINDOW_LABELS[window.id] || window.id;
  const percent = document.createElement("strong");
  percent.textContent = `${Math.round(remaining)}%`;
  heading.append(label, percent);
  const track = document.createElement("div");
  track.className = "quota-track";
  const fill = document.createElement("span");
  fill.className = remaining <= 20 ? "quota-low" : remaining <= 50 ? "quota-mid" : "";
  fill.style.width = `${remaining}%`;
  track.appendChild(fill);
  const reset = small(window.resets_at ? `重置 ${formatUsageReset(window.resets_at)}` : "重置时间未知");
  reset.className = "quota-reset";
  wrapper.append(heading, track, reset);
  return wrapper;
}

function evidenceSummary(evidence) {
  if (!evidence) return "无证据";
  const checks = [
    ["billing", evidence.billing_block],
    ["version", evidence.cc_version],
    ["entrypoint", evidence.known_entrypoint],
    ["metadata", evidence.structured_metadata],
    ["ua", evidence.claude_user_agent],
    ["session", evidence.claude_code_session],
    ["x-app", evidence.x_app_cli],
  ];
  return checks.map(([label, present]) => `${label} ${present ? "✓" : "·"}`).join(" ");
}

function relayActionLabel(action) {
  if (action === "passthrough") return "原样透传";
  if (action === "minimal_attribution") return "最小归因";
  if (action === "unchanged") return "无需修改";
  return "未转发";
}

function renderConnect() {
  const overview = state.overview;
  if (!overview) return;
  const endpoint = overview.endpoint || location.origin;
  const compatibleKey = overview.relay_api_key || "";
  const officialKey = overview.official_api_key || "";
  const shownCompatible = state.relayKeyVisible ? compatibleKey : maskKey(compatibleKey);
  const shownOfficial = officialKey ? (state.relayKeyVisible ? officialKey : maskKey(officialKey)) : "未配置";
  const claudeCodeKey = officialKey ? shownOfficial : shownCompatible;

  $("connectEndpoint").textContent = endpoint;
  $("connectKey").textContent = shownCompatible;
  $("connectOfficialKey").textContent = shownOfficial;
  $("toggleRelayKey").textContent = state.relayKeyVisible ? "隐藏" : "显示";

  $("snippetBash").textContent =
    `export ANTHROPIC_BASE_URL="${endpoint}"\nexport ANTHROPIC_AUTH_TOKEN="${claudeCodeKey}"`;
  $("snippetPowershell").textContent =
    `$env:ANTHROPIC_BASE_URL = "${endpoint}"\n$env:ANTHROPIC_AUTH_TOKEN = "${claudeCodeKey}"`;
  $("snippetCurl").textContent =
    `curl ${endpoint}/v1/messages \\\n  -H "x-api-key: ${shownCompatible}" \\\n  -H "anthropic-version: 2023-06-01" \\\n  -H "content-type: application/json" \\\n  -d '{"model":"claude-haiku-4-5-20251001","max_tokens":16,` +
    `"messages":[{"role":"user","content":"ping"}]}'`;

  $("runtimeListen").textContent = overview.listen || "—";
  $("runtimeUpstream").textContent = overview.upstream || "—";
  $("runtimeProxy").textContent = overview.upstream_proxy || "未配置";
  $("runtimeAutoRefresh").textContent = overview.auto_refresh_enabled ? "开启" : "已全局停止";
  $("runtimeMaxBytes").textContent = formatBytes(overview.max_request_bytes || 0);
  $("runtimeLogSize").textContent = `${overview.requests?.capacity ?? 0} 条`;
  $("runtimeStarted").textContent = overview.started_at ? new Date(overview.started_at).toLocaleString() : "—";
}

/* ---------------- account actions ---------------- */

async function toggleAccount(account) {
  if (!account.enabled) {
    const accepted = await confirmDialog({
      title: `启用 ${account.alias}`,
      lead: "启用后，本服务将成为该账号 refresh token 的唯一持有者。请先确认：",
      items: [
        "旧服务（例如 CLIProxyAPI）已停用同一个 Claude 账号",
        "没有其他进程仍在刷新这条令牌链",
      ],
      accept: "确认启用",
    });
    if (!accepted) return;
  }
  await runAction(
    `/admin/v1/accounts/${encodeURIComponent(account.alias)}/${account.enabled ? "disable" : "enable"}`,
    { method: "POST" },
    `${account.alias} 已${account.enabled ? "停用" : "启用"}`,
  );
}

async function checkAccount(account, element) {
  setBusy(element, true, "检测中");
  try {
    const result = await api(`/admin/v1/accounts/${encodeURIComponent(account.alias)}/check`, { method: "POST" });
    if (result.ok) {
      showToast(`${account.alias} 连通正常（${result.duration_ms} ms）`);
    } else {
      showToast(`${account.alias} 检测失败：${result.detail || result.status}`, true);
    }
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(element, false);
    refreshAll();
  }
}

function openActions(account) {
  $("actionsTitle").textContent = account.alias;

  const detail = $("actionsDetail");
  const detailRows = [
    detailRow("账号池", accountPoolView(account.pool).note),
    detailRow("账号身份", account.account_uuid || "—"),
    detailRow("邮箱", account.email || "未记录"),
    detailRow("导入于", account.created_at ? new Date(account.created_at).toLocaleString() : "—"),
    detailRow("令牌到期", account.expires_at ? new Date(account.expires_at).toLocaleString() : "未知"),
    detailRow("上次刷新", account.last_refresh_at ? new Date(account.last_refresh_at).toLocaleString() : "尚未刷新"),
  ];
  const usage = state.accountUsage[account.alias];
  const display = accountUsageDisplay(account, usage);
  if (usage?.status === "success" || display.windows.length > 0) {
    if (usage?.plan_type) detailRows.push(detailRow("订阅套餐", usage.plan_type.toUpperCase()));
    for (const window of display.windows) {
      detailRows.push(detailRow(
        `额度 · ${USAGE_WINDOW_LABELS[window.id] || window.id}`,
        `${Math.round(window.remaining_percent)}% 剩余 · ${formatUsageReset(window.resets_at)}`,
      ));
    }
    if (usage?.extra_usage?.enabled) {
      detailRows.push(detailRow(
        "额外用量",
        `$${(usage.extra_usage.used_credits_cents / 100).toFixed(2)} / $${(usage.extra_usage.monthly_limit_cents / 100).toFixed(2)}`,
      ));
    }
    if (display.updatedAt) {
      detailRows.push(detailRow(display.responseDerived ? "响应采样时间" : "额度获取时间", new Date(display.updatedAt).toLocaleString()));
    }
    if (usage?.status === "error") detailRows.push(detailRow("完整额度刷新", `失败：${usage.error}`));
  } else if (usage?.status === "error") {
    detailRows.push(detailRow("订阅额度", `读取失败：${usage.error}`));
  }
  detail.replaceChildren(...detailRows);

  const list = $("actionsList");
  list.replaceChildren();

  list.appendChild(button("复制账号 UUID", "btn", () => copyText(account.account_uuid, "账号 UUID 已复制")));
  list.appendChild(button("重命名", "btn", () => { $("actionsDialog").close(); openRename(account); }));
  list.appendChild(button("刷新订阅额度", "btn", async () => {
    $("actionsDialog").close();
    await loadAccountUsage(account, { notify: true });
  }));

  const toOfficialOnly = account.pool !== "official";
  const targetPool = toOfficialOnly ? "official" : "compatible";
  list.appendChild(button(toOfficialOnly ? "设为 official 专用" : "设为共享账号", "btn", async () => {
    $("actionsDialog").close();
    const accepted = await confirmDialog({
      title: toOfficialOnly ? `将 ${account.alias} 设为 official 专用` : `将 ${account.alias} 设为共享账号`,
      lead: toOfficialOnly
        ? "账号之后只承接通过 Claude Code 检测的 official 入口请求，兼容流量不会再落到它上面。"
        : "账号之后同时承接兼容入口和 official 入口的请求。",
      items: [
        "现有粘性会话绑定会被清除",
        "账号的启用状态和冷却状态保持不变",
      ],
      accept: "确认",
    });
    if (!accepted) return;
    await runAction(`/admin/v1/accounts/${encodeURIComponent(account.alias)}/pool`, {
      method: "POST",
      body: JSON.stringify({ pool: targetPool }),
    }, toOfficialOnly ? `${account.alias} 已设为 official 专用` : `${account.alias} 已设为共享账号`);
  }));

  const refresh = button("立即刷新令牌", "btn", () => {
    $("actionsDialog").close();
    runAction(`/admin/v1/accounts/${encodeURIComponent(account.alias)}/refresh`, { method: "POST" }, `${account.alias} 令牌已刷新`);
  });
  const refreshBlocked = !account.enabled || !state.overview?.auto_refresh_enabled;
  refresh.disabled = refreshBlocked;
  list.appendChild(refresh);
  if (refreshBlocked) {
    list.appendChild(note(account.enabled ? "自动刷新已被全局停止。" : "账号停用时不会刷新令牌。"));
  }

  const cooling = account.cooldown && account.cooldown.until_at > Date.now();
  const clear = button("解除冷却，立即回到调度", "btn", () => {
    $("actionsDialog").close();
    runAction(`/admin/v1/accounts/${encodeURIComponent(account.alias)}/cooldown/clear`, { method: "POST" }, `${account.alias} 冷却已解除`);
  });
  clear.disabled = !cooling;
  list.appendChild(clear);

  list.appendChild(button("删除账号", "btn btn-danger", async () => {
    $("actionsDialog").close();
    const accepted = await confirmDialog({
      title: `删除 ${account.alias}`,
      lead: "该账号的 OAuth 令牌、冷却状态、粘性会话绑定和长期用量记录会被一并删除，且无法恢复。",
      items: [
        "删除不会吊销 Anthropic 侧的授权，需要时请到 Anthropic 账号页手动撤销",
        "若只是暂时不用，停用比删除更安全",
      ],
      accept: "永久删除",
      danger: true,
    });
    if (!accepted) return;
    await runAction(`/admin/v1/accounts/${encodeURIComponent(account.alias)}`, { method: "DELETE" }, `${account.alias} 已删除`);
  }));

  $("actionsDialog").showModal();
}

function openRename(account) {
  $("renameError").textContent = "";
  $("renameAlias").value = account.alias;
  $("renameDialog").dataset.alias = account.alias;
  $("renameDialog").showModal();
}

async function submitRename() {
  const dialog = $("renameDialog");
  const current = dialog.dataset.alias;
  const alias = $("renameAlias").value.trim();
  const error = $("renameError");
  error.textContent = "";
  if (!/^[A-Za-z0-9._-]{1,64}$/.test(alias)) {
    error.textContent = "别名只能包含字母、数字、点、下划线或连字符。";
    return;
  }
  const element = $("submitRenameButton");
  setBusy(element, true, "保存中");
  try {
    await api(`/admin/v1/accounts/${encodeURIComponent(current)}/rename`, {
      method: "POST",
      body: JSON.stringify({ alias }),
    });
    dialog.close();
    showToast(`已重命名为 ${alias}`);
    await refreshAll();
  } catch (requestError) {
    error.textContent = requestError.message;
  } finally {
    setBusy(element, false);
  }
}

async function runAction(path, options, successMessage) {
  try {
    await api(path, options);
    showToast(successMessage);
    await refreshAll();
  } catch (error) {
    showToast(error.message, true);
  }
}

/* ---------------- import and OAuth ---------------- */

async function submitImport() {
  const alias = $("importAlias").value.trim();
  const credential = $("importCredential").value.trim();
  const replace = $("importReplace").checked;
  const error = $("importError");
  error.textContent = "";
  if (!/^[A-Za-z0-9._-]{1,64}$/.test(alias)) {
    error.textContent = "别名只能包含字母、数字、点、下划线或连字符。";
    return;
  }
  if (!credential) {
    error.textContent = "请粘贴凭据 JSON。";
    return;
  }
  if (replace) {
    $("importDialog").close();
    const accepted = await confirmDialog({
      title: `替换 ${alias} 的现有凭据`,
      lead: "如果该别名或账号身份已经存在，其 OAuth 令牌将被替换并立即停用。",
      items: [
        "确认粘贴的是目标账号的完整凭据",
        "替换后需要重新手动启用账号",
      ],
      accept: "确认替换",
      danger: true,
    });
    if (!accepted) {
      $("importDialog").showModal();
      return;
    }
  }
  const element = $("submitImportButton");
  setBusy(element, true, "导入中");
  try {
    await api("/admin/v1/accounts/import", {
      method: "POST",
      body: JSON.stringify({ alias, credential, replace }),
    });
    if ($("importDialog").open) $("importDialog").close();
    $("importAlias").value = "";
    $("importCredential").value = "";
    $("importReplace").checked = false;
    showToast(`${alias} 已导入，当前为停用状态`);
    await refreshAll();
  } catch (requestError) {
    if (!$("importDialog").open) $("importDialog").showModal();
    error.textContent = requestError.message;
  } finally {
    setBusy(element, false);
  }
}

async function startOAuth() {
  const alias = $("oauthAlias").value.trim();
  const error = $("oauthError");
  error.textContent = "";
  if (!/^[A-Za-z0-9._-]{1,64}$/.test(alias)) {
    error.textContent = "别名只能包含字母、数字、点、下划线或连字符。";
    return;
  }
  const element = $("startOAuthButton");
  setBusy(element, true, "创建中");
  try {
    const result = await api("/admin/v1/oauth/claude/start", {
      method: "POST",
      body: JSON.stringify({ alias }),
    });
    state.pendingOAuth = {
      alias,
      sessionId: result.session_id,
      authorizationURL: result.authorization_url,
      createdAt: Date.now(),
    };
    sessionStorage.setItem("claudeRelayPendingOAuth", JSON.stringify(state.pendingOAuth));
    $("authorizationLink").href = result.authorization_url;
    setOAuthStep(2);
    window.open(result.authorization_url, "_blank", "noopener,noreferrer");
  } catch (requestError) {
    error.textContent = requestError.message;
  } finally {
    setBusy(element, false);
  }
}

async function exchangeOAuth() {
  const code = $("oauthCode").value.trim();
  const error = $("oauthError");
  error.textContent = "";
  if (!state.pendingOAuth) {
    error.textContent = "授权会话已经丢失，请重新开始。";
    return;
  }
  if (!code) {
    error.textContent = "请粘贴授权码或完整回调地址。";
    return;
  }
  const element = $("exchangeOAuthButton");
  setBusy(element, true, "交换凭据中");
  try {
    await api("/admin/v1/oauth/claude/exchange", {
      method: "POST",
      body: JSON.stringify({ session_id: state.pendingOAuth.sessionId, code }),
    });
    state.pendingOAuth = null;
    sessionStorage.removeItem("claudeRelayPendingOAuth");
    setOAuthStep(3);
    await refreshAll();
  } catch (requestError) {
    error.textContent = requestError.message;
  } finally {
    setBusy(element, false);
  }
}

function restoreOAuthUI() {
  if (state.pendingOAuth && Date.now() - state.pendingOAuth.createdAt < 30 * 60 * 1000) {
    $("authorizationLink").href = state.pendingOAuth.authorizationURL;
    setOAuthStep(2);
    return;
  }
  resetOAuth();
}

function resetOAuth() {
  state.pendingOAuth = null;
  sessionStorage.removeItem("claudeRelayPendingOAuth");
  $("oauthAlias").value = "";
  $("oauthCode").value = "";
  $("oauthError").textContent = "";
  $("authorizationLink").href = "#";
  setOAuthStep(1);
}

function setOAuthStep(step) {
  for (let number = 1; number <= 3; number += 1) {
    $("oauthStep" + number).classList.toggle("hidden", number !== step);
    $("stepDot" + number).classList.toggle("is-active", number <= step);
  }
}

/* ---------------- session ---------------- */

function showApp() {
  $("loginView").classList.add("hidden");
  $("appView").classList.remove("hidden");
  startPolling();
}

function logout(message = "") {
  state.apiKey = "";
  state.overview = null;
  state.accounts = [];
  state.requests = [];
  stopPolling();
  sessionStorage.removeItem("claudeRelayAdminKey");
  $("appView").classList.add("hidden");
  $("loginView").classList.remove("hidden");
  $("apiKeyInput").value = "";
  $("loginError").textContent = message;
}

function selectPanel(name) {
  state.panel = name;
  for (const tab of document.querySelectorAll(".tab")) {
    const active = tab.dataset.panel === name;
    tab.classList.toggle("is-active", active);
    tab.setAttribute("aria-selected", String(active));
  }
  for (const panel of ["accounts", "requests", "usage", "connect"]) {
    $("panel-" + panel).classList.toggle("hidden", panel !== name);
  }
  if (name === "requests") refreshAll({ includeRequests: true });
  if (name === "usage") loadUsage().catch((error) => showToast(error.message, true));
}

/* ---------------- theme ---------------- */

const THEMES = ["auto", "light", "dark"];
const THEME_LABELS = { auto: "主题：跟随系统", light: "主题：浅色", dark: "主题：深色" };

function applyTheme(theme) {
  localStorage.setItem("claudeRelayTheme", theme);
  if (theme === "auto") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", theme);
  $("themeButton").textContent = THEME_LABELS[theme];
}

/* ---------------- helpers ---------------- */

function cell(child, className) {
  const td = document.createElement("td");
  if (className) td.dataset.role = className;
  if (child) td.appendChild(child);
  return td;
}

function stack(...children) {
  const div = document.createElement("div");
  div.className = "stack";
  for (const child of children) if (child) div.appendChild(child);
  return div;
}

function strong(text) {
  const element = document.createElement("strong");
  element.textContent = text;
  return element;
}

function small(text) {
  const element = document.createElement("small");
  element.textContent = text;
  return element;
}

function badge(text, css) {
  const span = document.createElement("span");
  span.className = `badge ${css}`;
  span.textContent = text;
  return span;
}

const COPY_GLYPH = '<svg viewBox="0 0 16 16"><rect x="5.75" y="5.75" width="8.5" height="8.5" rx="2"/><path d="M10.75 3.9A1.9 1.9 0 0 0 8.85 2H3.65A1.65 1.65 0 0 0 2 3.65v5.2a1.9 1.9 0 0 0 1.9 1.9"/></svg>';

// A click-to-copy label: the text stays readable and selectable-looking, the
// glyph marks it as copyable without spending a separate button slot.
function copyChip(value, { label, className = "", message, title }) {
  const chip = document.createElement("button");
  chip.type = "button";
  chip.className = `copy-chip ${className}`.trim();
  chip.title = title;
  const text = document.createElement("span");
  text.className = "copy-chip-text";
  text.textContent = label;
  const icon = document.createElement("span");
  icon.className = "copy-chip-icon";
  icon.setAttribute("aria-hidden", "true");
  icon.innerHTML = COPY_GLYPH;
  chip.append(text, icon);
  chip.addEventListener("click", async () => {
    if (!(await copyText(value, message))) return;
    chip.classList.add("is-copied");
    clearTimeout(chip.copiedTimer);
    chip.copiedTimer = setTimeout(() => chip.classList.remove("is-copied"), 1200);
  });
  return chip;
}

function button(label, className, handler) {
  const element = document.createElement("button");
  element.type = "button";
  element.className = className;
  element.textContent = label;
  element.addEventListener("click", () => handler(element));
  return element;
}

function note(text) {
  const element = document.createElement("p");
  element.className = "action-note";
  element.textContent = text;
  return element;
}

function option(label, value) {
  const element = document.createElement("option");
  element.textContent = label;
  element.value = value;
  return element;
}

function detailRow(label, value) {
  const wrapper = document.createElement("div");
  const dt = document.createElement("dt");
  dt.textContent = label;
  const dd = document.createElement("dd");
  dd.className = "mono";
  dd.textContent = value;
  dd.title = value;
  wrapper.append(dt, dd);
  return wrapper;
}

function confirmDialog({ title, lead, items = [], accept = "确认", danger = false }) {
  const dialog = $("confirmDialog");
  $("confirmTitle").textContent = title;
  const body = $("confirmBody");
  body.replaceChildren();
  const paragraph = document.createElement("p");
  paragraph.textContent = lead;
  body.appendChild(paragraph);
  if (items.length) {
    const list = document.createElement("ul");
    for (const item of items) {
      const entry = document.createElement("li");
      entry.textContent = item;
      list.appendChild(entry);
    }
    body.appendChild(list);
  }
  const acceptButton = $("confirmAccept");
  acceptButton.textContent = accept;
  acceptButton.className = danger ? "btn btn-danger" : "btn btn-primary";
  const rejectButtons = [...dialog.querySelectorAll(".confirm-reject")];

  // The answer is driven by explicit clicks rather than the dialog close event,
  // which some browsers skip when a method="dialog" form closes the dialog.
  return new Promise((resolve) => {
    let settled = false;
    const finish = (accepted) => {
      if (settled) return;
      settled = true;
      acceptButton.removeEventListener("click", onAccept);
      for (const element of rejectButtons) element.removeEventListener("click", onReject);
      dialog.removeEventListener("cancel", onReject);
      if (dialog.open) dialog.close();
      resolve(accepted);
    };
    const onAccept = () => finish(true);
    const onReject = () => finish(false);
    acceptButton.addEventListener("click", onAccept);
    for (const element of rejectButtons) element.addEventListener("click", onReject);
    dialog.addEventListener("cancel", onReject);
    dialog.showModal();
  });
}

async function copyText(value, message) {
  if (!value) return false;
  if (await writeClipboard(value)) {
    showToast(message);
    return true;
  }
  showToast("复制失败，请手动选中文本复制", true);
  return false;
}

// navigator.clipboard only exists in a secure context (HTTPS or localhost), so a
// console served over plain HTTP has to fall back to the legacy selection copy.
async function writeClipboard(value) {
  if (window.isSecureContext && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(value);
      return true;
    } catch {
      // Permission denied or blocked: try the legacy path below.
    }
  }
  // A modal dialog makes the rest of the document inert, so the scratch area has
  // to live inside the open dialog for the selection to take.
  const host = document.querySelector("dialog[open]") || document.body;
  const area = document.createElement("textarea");
  area.value = value;
  area.setAttribute("readonly", "");
  area.style.cssText = "position:fixed;top:0;left:0;width:1px;height:1px;padding:0;border:0;opacity:0;";
  host.appendChild(area);
  const selection = document.getSelection();
  const previous = selection?.rangeCount ? selection.getRangeAt(0) : null;
  try {
    area.select();
    area.setSelectionRange(0, value.length);
    return document.execCommand("copy");
  } catch {
    return false;
  } finally {
    area.remove();
    if (previous) {
      selection.removeAllRanges();
      selection.addRange(previous);
    }
  }
}

function maskKey(key) {
  if (!key) return "—";
  if (key.length <= 8) return "•".repeat(key.length);
  return `${key.slice(0, 4)}${"•".repeat(Math.min(16, key.length - 8))}${key.slice(-4)}`;
}

function shortenUUID(value) {
  if (!value) return "未知身份";
  if (value.length < 16) return value;
  return `${value.slice(0, 8)}…${value.slice(-4)}`;
}

function formatUsageReset(value) {
  if (!value) return "未提供重置时间";
  const reset = Date.parse(value);
  if (!Number.isFinite(reset)) return "重置时间未知";
  const diff = reset - Date.now();
  if (diff > 0) return `${formatDuration(diff)}后重置`;
  return new Date(reset).toLocaleString();
}

function formatRelative(millis) {
  if (!millis) return "—";
  const diff = Date.now() - millis;
  if (diff < 0) return "刚刚";
  if (diff < 10_000) return "刚刚";
  return `${formatDuration(diff)}前`;
}

function formatDuration(millis) {
  const seconds = Math.max(0, Math.floor(millis / 1000));
  if (seconds < 60) return `${seconds} 秒`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} 分钟`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 小时${minutes % 60 ? ` ${minutes % 60} 分钟` : ""}`;
  const days = Math.floor(hours / 24);
  return `${days} 天${hours % 24 ? ` ${hours % 24} 小时` : ""}`;
}

function formatBytes(bytes) {
  if (!bytes) return "—";
  const units = ["B", "KiB", "MiB", "GiB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${Number.isInteger(value) ? value : value.toFixed(1)} ${units[unit]}`;
}

function setBusy(element, busy, label = "") {
  if (!element) return;
  if (busy) {
    element.dataset.idleLabel = element.textContent;
    element.textContent = label;
    element.disabled = true;
  } else {
    element.textContent = element.dataset.idleLabel || element.textContent;
    element.disabled = false;
  }
}

function showToast(message, isError = false) {
  const toast = $("toast");
  toast.textContent = message;
  toast.classList.toggle("error", isError);
  toast.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toast.classList.remove("show"), 3200);
}

function readJSON(storage, key) {
  try { return JSON.parse(storage.getItem(key) || "null"); }
  catch { return null; }
}

/* ---------------- wiring ---------------- */

$("loginForm").addEventListener("submit", async (event) => {
  event.preventDefault();
  const error = $("loginError");
  error.textContent = "";
  const key = $("apiKeyInput").value.trim();
  if (!key) return;
  state.apiKey = key;
  const submit = $("loginForm").querySelector("button[type=submit]");
  setBusy(submit, true, "验证中");
  try {
    await loadOverview();
    sessionStorage.setItem("claudeRelayAdminKey", key);
    showApp();
    await refreshAll();
  } catch (requestError) {
    state.apiKey = "";
    error.textContent = requestError.message;
  } finally {
    setBusy(submit, false);
  }
});

$("toggleSecret").addEventListener("click", () => {
  const input = $("apiKeyInput");
  const visible = input.type === "text";
  input.type = visible ? "password" : "text";
  $("toggleSecret").textContent = visible ? "显示" : "隐藏";
});

$("logoutButton").addEventListener("click", () => logout());
$("refreshButton").addEventListener("click", () => refreshAll({ notify: true }));

$("autoRefreshButton").addEventListener("click", () => {
  state.autoRefreshEnabled = !state.autoRefreshEnabled;
  $("autoRefreshButton").setAttribute("aria-pressed", String(state.autoRefreshEnabled));
  $("autoRefreshButton").textContent = state.autoRefreshEnabled ? "自动刷新" : "已暂停";
  startPolling();
});

$("themeButton").addEventListener("click", () => {
  const current = localStorage.getItem("claudeRelayTheme") || "auto";
  applyTheme(THEMES[(THEMES.indexOf(current) + 1) % THEMES.length]);
});

for (const tab of document.querySelectorAll(".tab")) {
  tab.addEventListener("click", () => selectPanel(tab.dataset.panel));
}

$("filterAccount").addEventListener("change", (event) => {
  state.filters.account = event.target.value;
  renderRequests();
});
$("filterOutcome").addEventListener("change", (event) => {
  state.filters.outcome = event.target.value;
  renderRequests();
});

$("accountSearch").addEventListener("input", (event) => {
  state.accountQuery = event.target.value;
  renderAccounts();
});
$("accountStatusFilter").addEventListener("change", (event) => {
  state.accountStatus = event.target.value;
  renderAccounts();
});

$("openOAuthButton").addEventListener("click", openOAuthDialog);
for (const trigger of document.querySelectorAll(".oauth-trigger")) {
  trigger.addEventListener("click", openOAuthDialog);
}
function openOAuthDialog() {
  $("oauthError").textContent = "";
  restoreOAuthUI();
  $("oauthDialog").showModal();
  if (!state.pendingOAuth) setTimeout(() => $("oauthAlias").focus(), 40);
}

$("startOAuthButton").addEventListener("click", startOAuth);
$("exchangeOAuthButton").addEventListener("click", exchangeOAuth);
$("restartOAuthButton").addEventListener("click", resetOAuth);
$("finishOAuthButton").addEventListener("click", () => {
  $("oauthDialog").close();
  resetOAuth();
  refreshAll();
});

$("importButton").addEventListener("click", () => {
  $("importError").textContent = "";
  $("importDialog").showModal();
  setTimeout(() => $("importAlias").focus(), 40);
});
$("submitImportButton").addEventListener("click", submitImport);
$("submitRenameButton").addEventListener("click", submitRename);

$("usageRange").addEventListener("change", () => loadUsage().catch((error) => showToast(error.message, true)));
$("refreshUsageButton").addEventListener("click", () => loadUsage({ notify: true }).catch((error) => showToast(error.message, true)));
$("addPriceButton").addEventListener("click", openPriceDialog);
$("savePriceButton").addEventListener("click", (event) => savePrice(event.currentTarget));
$("clearUsageButton").addEventListener("click", clearUsage);

$("toggleRelayKey").addEventListener("click", () => {
  state.relayKeyVisible = !state.relayKeyVisible;
  renderConnect();
});
$("copyRelayKey").addEventListener("click", () => copyText(state.overview?.relay_api_key, "中转密钥已复制"));
$("copyOfficialKey").addEventListener("click", () => copyText(state.overview?.official_api_key, "Official 入口密钥已复制"));
for (const element of document.querySelectorAll(".copy-endpoint")) {
  element.addEventListener("click", () => copyText(state.overview?.endpoint || location.origin, "请求地址已复制"));
}
for (const element of document.querySelectorAll("[data-copy]")) {
  element.addEventListener("click", () => {
    const source = $(element.dataset.copy).textContent;
    const compatibleKey = state.overview?.relay_api_key;
    const officialKey = state.overview?.official_api_key;
    // Snippets render masked keys while hidden, but copying should stay usable.
    let resolved = source;
    if (!state.relayKeyVisible && compatibleKey) resolved = resolved.split(maskKey(compatibleKey)).join(compatibleKey);
    if (!state.relayKeyVisible && officialKey) resolved = resolved.split(maskKey(officialKey)).join(officialKey);
    copyText(resolved, "已复制到剪贴板");
  });
}

document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible" && state.apiKey) refreshAll();
});

applyTheme(localStorage.getItem("claudeRelayTheme") || "auto");

if (state.apiKey) {
  loadOverview()
    .then(() => { showApp(); return refreshAll(); })
    .catch(() => logout("管理密钥已失效，请重新输入。"));
}
