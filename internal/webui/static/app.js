const $ = (id) => document.getElementById(id);

const state = {
  apiKey: sessionStorage.getItem("claudeRelayAdminKey") || "",
  accounts: [],
  autoRefresh: false,
  pendingOAuth: readPendingOAuth(),
};

const loginView = $("loginView");
const appView = $("appView");
const loginForm = $("loginForm");
const apiKeyInput = $("apiKeyInput");
const loginError = $("loginError");
const accountsList = $("accountsList");
const emptyAccounts = $("emptyAccounts");
const oauthDialog = $("oauthDialog");
let toastTimer;

loginForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  loginError.textContent = "";
  const key = apiKeyInput.value.trim();
  if (!key) return;
  state.apiKey = key;
  setBusy(loginForm.querySelector("button[type=submit]"), true, "正在验证…");
  try {
    await loadAccounts();
    sessionStorage.setItem("claudeRelayAdminKey", key);
    showApp();
  } catch (error) {
    state.apiKey = "";
    loginError.textContent = error.message;
  } finally {
    setBusy(loginForm.querySelector("button[type=submit]"), false);
  }
});

$("toggleSecret").addEventListener("click", () => {
  const visible = apiKeyInput.type === "text";
  apiKeyInput.type = visible ? "password" : "text";
  $("toggleSecret").textContent = visible ? "显示" : "隐藏";
});

$("logoutButton").addEventListener("click", () => logout());
$("refreshButton").addEventListener("click", () => refreshDashboard(true));
$("copyEndpoint").addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText(location.origin);
    showToast("API 地址已复制");
  } catch {
    showToast("浏览器未允许复制，请手动复制地址", true);
  }
});

$("openOAuthButton").addEventListener("click", openOAuthDialog);
document.querySelectorAll(".oauth-trigger").forEach((button) => button.addEventListener("click", openOAuthDialog));
$("startOAuthButton").addEventListener("click", startOAuth);
$("exchangeOAuthButton").addEventListener("click", exchangeOAuth);
$("restartOAuthButton").addEventListener("click", resetOAuth);
$("finishOAuthButton").addEventListener("click", () => {
  oauthDialog.close();
  resetOAuth();
  refreshDashboard(false);
});
oauthDialog.addEventListener("close", () => $("oauthError").textContent = "");

async function api(path, options = {}) {
  const headers = new Headers(options.headers || {});
  headers.set("x-api-key", state.apiKey);
  if (options.body && !headers.has("content-type")) headers.set("content-type", "application/json");
  const response = await fetch(path, {...options, headers});
  let payload = null;
  try { payload = await response.json(); } catch { /* empty or non-JSON response */ }
  if (!response.ok) {
    if (response.status === 401) logout("管理密钥无效或已经变更");
    const message = payload?.error?.message || payload?.message || `请求失败（${response.status}）`;
    throw new Error(message);
  }
  return payload;
}

async function loadAccounts() {
  const payload = await api("/admin/v1/accounts");
  state.accounts = Array.isArray(payload.accounts) ? payload.accounts : [];
  state.autoRefresh = Boolean(payload.auto_refresh_enabled);
  renderDashboard();
}

async function refreshDashboard(notify) {
  const button = $("refreshButton");
  setBusy(button, true, "读取中…");
  try {
    await loadAccounts();
    if (notify) showToast("账号状态已刷新");
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
  }
}

function renderDashboard() {
  $("totalMetric").textContent = String(state.accounts.length);
  const enabled = state.accounts.filter((account) => account.enabled).length;
  $("enabledMetric").textContent = String(enabled);
  $("refreshMetric").textContent = state.autoRefresh ? "开启" : "停止";
  $("refreshMetric").className = state.autoRefresh ? "" : "metric-warning";
  $("endpointMetric").textContent = location.origin;
  $("accountSummary").textContent = `${enabled} 个启用 · ${state.accounts.length - enabled} 个关闭`;
  accountsList.replaceChildren();
  emptyAccounts.classList.toggle("hidden", state.accounts.length !== 0);

  for (const account of state.accounts) {
    accountsList.appendChild(accountRow(account));
  }
}

function accountRow(account) {
  const row = document.createElement("article");
  row.className = "account-row";

  const avatar = document.createElement("div");
  avatar.className = "account-avatar";
  avatar.textContent = (account.alias || "?").slice(0, 2);

  const main = document.createElement("div");
  main.className = "account-main";
  const alias = document.createElement("strong");
  alias.textContent = account.alias;
  const email = document.createElement("small");
  email.textContent = account.email || "未记录邮箱";
  main.append(alias, email);

  const identity = document.createElement("div");
  identity.className = "account-meta";
  const identityTitle = document.createElement("small");
  identityTitle.textContent = "账号身份";
  const uuid = document.createElement("strong");
  uuid.textContent = shortenUUID(account.account_uuid);
  uuid.title = account.account_uuid || "";
  identity.append(identityTitle, uuid);

  const expiry = document.createElement("div");
  expiry.className = "account-meta";
  const expiryTitle = document.createElement("small");
  expiryTitle.textContent = account.has_refresh_token ? "可自动续期" : "无刷新凭据";
  const expiryValue = document.createElement("strong");
  expiryValue.textContent = formatExpiry(account.expires_at);
  expiry.append(expiryTitle, expiryValue);

  const controls = document.createElement("div");
  controls.className = "account-controls";
  const pill = document.createElement("span");
  pill.className = `pill ${account.enabled ? "enabled" : "disabled"}`;
  pill.textContent = account.enabled ? "已启用" : "已关闭";
  const button = document.createElement("button");
  button.type = "button";
  button.className = `account-action ${account.enabled ? "danger" : ""}`;
  button.textContent = account.enabled ? "关闭" : "启用";
  button.addEventListener("click", () => toggleAccount(account, button));
  controls.append(pill, button);

  row.append(avatar, main, identity, expiry, controls);
  return row;
}

async function toggleAccount(account, button) {
  if (!account.enabled) {
    const confirmed = window.confirm(`启用 ${account.alias} 表示本项目将接管它的 OAuth 刷新。\n\n请确认已经在 CLIProxy 等旧服务中停用该账号。`);
    if (!confirmed) return;
  }
  const action = account.enabled ? "disable" : "enable";
  setBusy(button, true, account.enabled ? "关闭中…" : "启用中…");
  try {
    await api(`/admin/v1/accounts/${encodeURIComponent(account.alias)}/${action}`, {method: "POST"});
    await loadAccounts();
    showToast(`${account.alias} 已${account.enabled ? "关闭" : "启用"}`);
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
  }
}

function showApp() {
  loginView.classList.add("hidden");
  appView.classList.remove("hidden");
  restoreOAuthUI();
}

function logout(message = "") {
  state.apiKey = "";
  state.accounts = [];
  sessionStorage.removeItem("claudeRelayAdminKey");
  appView.classList.add("hidden");
  loginView.classList.remove("hidden");
  apiKeyInput.value = "";
  loginError.textContent = message;
}

function openOAuthDialog() {
  $("oauthError").textContent = "";
  restoreOAuthUI();
  oauthDialog.showModal();
  if (!state.pendingOAuth) setTimeout(() => $("oauthAlias").focus(), 50);
}

async function startOAuth() {
  const alias = $("oauthAlias").value.trim();
  const error = $("oauthError");
  error.textContent = "";
  if (!/^[A-Za-z0-9._-]{1,64}$/.test(alias)) {
    error.textContent = "别名只能包含英文字母、数字、点、下划线或连字符。";
    return;
  }
  const button = $("startOAuthButton");
  setBusy(button, true, "正在创建…");
  try {
    const result = await api("/admin/v1/oauth/claude/start", {
      method: "POST",
      body: JSON.stringify({alias}),
    });
    state.pendingOAuth = {
      alias,
      sessionId: result.session_id,
      authorizationURL: result.authorization_url,
      createdAt: Date.now(),
    };
    sessionStorage.setItem("claudeRelayPendingOAuth", JSON.stringify(state.pendingOAuth));
    setOAuthStep(2);
    $("authorizationLink").href = result.authorization_url;
    window.open(result.authorization_url, "_blank", "noopener,noreferrer");
  } catch (requestError) {
    error.textContent = requestError.message;
  } finally {
    setBusy(button, false);
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
  const button = $("exchangeOAuthButton");
  setBusy(button, true, "正在交换凭据…");
  try {
    await api("/admin/v1/oauth/claude/exchange", {
      method: "POST",
      body: JSON.stringify({session_id: state.pendingOAuth.sessionId, code}),
    });
    state.pendingOAuth = null;
    sessionStorage.removeItem("claudeRelayPendingOAuth");
    setOAuthStep(3);
    await loadAccounts();
  } catch (requestError) {
    error.textContent = requestError.message;
  } finally {
    setBusy(button, false);
  }
}

function restoreOAuthUI() {
  if (state.pendingOAuth && Date.now() - state.pendingOAuth.createdAt < 30 * 60 * 1000) {
    $("authorizationLink").href = state.pendingOAuth.authorizationURL;
    setOAuthStep(2);
    return;
  }
  if (state.pendingOAuth) resetOAuth();
  else setOAuthStep(1);
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
    $("stepDot" + number).classList.toggle("active", number <= step);
  }
}

function readPendingOAuth() {
  try { return JSON.parse(sessionStorage.getItem("claudeRelayPendingOAuth") || "null"); }
  catch { return null; }
}

function formatExpiry(value) {
  if (!value) return "未知有效期";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "有效期异常";
  const diff = date.getTime() - Date.now();
  if (diff <= 0) return "已过期";
  const hours = Math.floor(diff / 3_600_000);
  if (hours < 24) return `${Math.max(1, hours)} 小时后`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days} 天后`;
  return new Intl.DateTimeFormat("zh-CN", {year: "numeric", month: "short", day: "numeric"}).format(date);
}

function shortenUUID(value) {
  if (!value || value.length < 16) return value || "未知";
  return `${value.slice(0, 8)}…${value.slice(-4)}`;
}

function setBusy(button, busy, label = "") {
  if (!button) return;
  if (busy) {
    button.dataset.originalText = button.textContent;
    button.textContent = label;
    button.disabled = true;
  } else {
    button.textContent = button.dataset.originalText || button.textContent;
    button.disabled = false;
  }
}

function showToast(message, isError = false) {
  const toast = $("toast");
  toast.textContent = message;
  toast.classList.toggle("error", isError);
  toast.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toast.classList.remove("show"), 2800);
}

if (state.apiKey) {
  loadAccounts().then(showApp).catch(() => logout("管理密钥已失效，请重新输入。"));
}
