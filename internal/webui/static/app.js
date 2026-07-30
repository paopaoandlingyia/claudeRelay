// Claude Relay console. All timestamps arriving from /admin/v1 are epoch milliseconds.

const $ = (id) => document.getElementById(id);
const POLL_INTERVAL = 5000;
const REQUEST_LIMIT = 200;

const state = {
  apiKey: sessionStorage.getItem("claudeRelayAdminKey") || "",
  overview: null,
  accounts: [],
  requests: [],
  autoRefreshEnabled: true,
  panel: "accounts",
  pendingOAuth: readJSON(sessionStorage, "claudeRelayPendingOAuth"),
  relayKeyVisible: false,
  filters: { account: "", outcome: "" },
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

  const available = Math.max(0, (totals.enabled || 0) - (totals.cooling || 0));
  setStat("statAvailable", available, available === 0 && (totals.total || 0) > 0 ? "is-bad" : "");
  $("statAvailableNote").textContent = `共 ${totals.total || 0} 个账号，${totals.enabled || 0} 个已启用`;

  setStat("statCooling", totals.cooling || 0, (totals.cooling || 0) > 0 ? "is-warn" : "");
  setStat("statExpired", totals.expired || 0, (totals.expired || 0) > 0 ? "is-warn" : "");

  setStat("statRecent", summary.recent_requests || 0, (summary.recent_failures || 0) > 0 ? "is-warn" : "");
  $("statRecentNote").textContent = (summary.recent_failures || 0) > 0
    ? `${summary.recent_failures} 次失败`
    : "无失败";

  const total = summary.requests || 0;
  const failures = summary.failures || 0;
  $("statSuccess").textContent = total === 0 ? "—" : `${Math.round(((total - failures) / total) * 100)}%`;
  $("statSuccess").parentElement.className = `stat${total > 0 && failures / total > 0.1 ? " is-warn" : ""}`;
  $("statSuccessNote").textContent = total === 0 ? "本次启动尚无请求" : `${total} 次请求，${failures} 次失败`;

  setStat("statSticky", overview.sticky_sessions || 0, "");

  $("versionTag").textContent = overview.version || "dev";
  $("uptimeText").textContent = overview.started_at ? `已运行 ${formatDuration(Date.now() - overview.started_at)}` : "";
}

function setStat(id, value, modifier) {
  const element = $(id);
  element.textContent = String(value);
  element.parentElement.className = `stat${modifier ? " " + modifier : ""}`;
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
    return { label: "令牌过期", css: "badge-bad", note: "刷新令牌或重新授权后才能使用" };
  }
  return { label: "可用", css: "badge-ok", note: "参与缓存亲和调度" };
}

function isExpired(account) {
  if (!account.expires_at) return false;
  const expiry = Date.parse(account.expires_at);
  return Number.isFinite(expiry) && expiry <= Date.now();
}

function renderAccounts() {
  const body = $("accountsBody");
  body.replaceChildren();
  $("accountsEmpty").classList.toggle("hidden", state.accounts.length !== 0);

  const enabled = state.accounts.filter((account) => account.enabled).length;
  $("accountSummary").textContent = state.accounts.length === 0
    ? "没有账号"
    : `${state.accounts.length} 个账号 · ${enabled} 个已启用 · ${state.accounts.length - enabled} 个已停用`;

  for (const account of state.accounts) {
    body.appendChild(accountRow(account));
  }
}

function accountRow(account) {
  const row = document.createElement("tr");
  const status = accountStatus(account);

  row.appendChild(cell(stack(
    strong(account.alias),
    small(account.email || shortenUUID(account.account_uuid)),
  ), "identity-cell"));

  row.appendChild(cell(stack(
    badge(status.label, status.css),
    small(status.note),
  )));

  row.appendChild(cell(stack(
    strong(formatExpiry(account.expires_at)),
    small(account.has_refresh_token
      ? (account.last_refresh_at ? `上次刷新 ${formatRelative(account.last_refresh_at)}` : "可自动续期，尚未刷新过")
      : "无刷新令牌"),
  )));

  const stats = account.stats || {};
  row.appendChild(cell(stack(
    strong(stats.requests ? `${stats.requests} 次` : "—"),
    small(stats.requests
      ? `${stats.failures || 0} 次失败 · 最近 ${formatRelative(stats.last_used_at)}`
      : "本次启动未被使用"),
  )));

  row.appendChild(cell(stack(
    strong(account.sticky_sessions ? `${account.sticky_sessions} 个会话` : "—"),
    small("粘性绑定"),
  )));

  const actions = document.createElement("div");
  actions.className = "row-actions";
  actions.appendChild(button(account.enabled ? "停用" : "启用", "btn btn-inline", () => toggleAccount(account)));
  actions.appendChild(button("检测", "btn btn-inline", (element) => checkAccount(account, element)));
  actions.appendChild(button("⋯", "btn btn-inline", () => openActions(account)));
  const actionCell = cell(actions);
  actionCell.className = "col-actions";
  row.appendChild(actionCell);
  return row;
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
  cache_affinity: "缓存亲和",
};

function selectionLabel(selection) {
  if (!selection) return "未选中";
  return SELECTION_LABELS[selection] || selection;
}

function renderConnect() {
  const overview = state.overview;
  if (!overview) return;
  const endpoint = overview.endpoint || location.origin;
  const key = overview.relay_api_key || "";
  const shown = state.relayKeyVisible ? key : maskKey(key);

  $("connectEndpoint").textContent = endpoint;
  $("connectKey").textContent = shown;
  $("toggleRelayKey").textContent = state.relayKeyVisible ? "隐藏" : "显示";

  $("snippetBash").textContent =
    `export ANTHROPIC_BASE_URL="${endpoint}"\nexport ANTHROPIC_AUTH_TOKEN="${shown}"`;
  $("snippetPowershell").textContent =
    `$env:ANTHROPIC_BASE_URL = "${endpoint}"\n$env:ANTHROPIC_AUTH_TOKEN = "${shown}"`;
  $("snippetCurl").textContent =
    `curl ${endpoint}/v1/messages \\\n  -H "x-api-key: ${shown}" \\\n  -H "anthropic-version: 2023-06-01" \\\n  -H "content-type: application/json" \\\n  -d '{"model":"claude-haiku-4-5-20251001","max_tokens":16,` +
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
  detail.replaceChildren(
    detailRow("账号身份", account.account_uuid || "—"),
    detailRow("邮箱", account.email || "未记录"),
    detailRow("导入于", account.created_at ? new Date(account.created_at).toLocaleString() : "—"),
    detailRow("令牌到期", account.expires_at ? new Date(account.expires_at).toLocaleString() : "未知"),
    detailRow("上次刷新", account.last_refresh_at ? new Date(account.last_refresh_at).toLocaleString() : "尚未刷新"),
  );

  const list = $("actionsList");
  list.replaceChildren();

  list.appendChild(button("复制账号 UUID", "btn", () => copyText(account.account_uuid, "账号 UUID 已复制")));
  list.appendChild(button("重命名", "btn", () => { $("actionsDialog").close(); openRename(account); }));

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
      lead: "该账号的 OAuth 令牌、冷却状态和粘性会话绑定会被一并删除，且无法恢复。",
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
  const element = $("submitImportButton");
  setBusy(element, true, "导入中");
  try {
    await api("/admin/v1/accounts/import", {
      method: "POST",
      body: JSON.stringify({ alias, credential }),
    });
    $("importDialog").close();
    $("importAlias").value = "";
    $("importCredential").value = "";
    showToast(`${alias} 已导入，当前为停用状态`);
    await refreshAll();
  } catch (requestError) {
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
  for (const panel of ["accounts", "requests", "connect"]) {
    $("panel-" + panel).classList.toggle("hidden", panel !== name);
  }
  if (name === "requests") refreshAll({ includeRequests: true });
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
  if (!value) return;
  try {
    await navigator.clipboard.writeText(value);
    showToast(message);
  } catch {
    showToast("浏览器拒绝了剪贴板访问，请手动复制", true);
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

function formatExpiry(value) {
  if (!value) return "未知有效期";
  const expiry = Date.parse(value);
  if (!Number.isFinite(expiry)) return "有效期异常";
  const diff = expiry - Date.now();
  if (diff <= 0) return "已过期";
  return `${formatDuration(diff)}后到期`;
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

$("toggleRelayKey").addEventListener("click", () => {
  state.relayKeyVisible = !state.relayKeyVisible;
  renderConnect();
});
$("copyRelayKey").addEventListener("click", () => copyText(state.overview?.relay_api_key, "中转密钥已复制"));
for (const element of document.querySelectorAll(".copy-endpoint")) {
  element.addEventListener("click", () => copyText(state.overview?.endpoint || location.origin, "请求地址已复制"));
}
for (const element of document.querySelectorAll("[data-copy]")) {
  element.addEventListener("click", () => {
    const source = $(element.dataset.copy).textContent;
    const key = state.overview?.relay_api_key;
    // Snippets render a masked key while hidden, but copying should stay usable.
    const resolved = key && !state.relayKeyVisible ? source.split(maskKey(key)).join(key) : source;
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
