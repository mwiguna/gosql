import { migrateLegacyWorkspace } from "../storage.js";
import { apiRequest, connectionFromProfile } from "../api.js";
import { state } from "../state.js";
import { findElement, brand, closeDialog, toast } from "../ui.js";

// -----------------------------------------------------------------------------
// Session lifecycle
// -----------------------------------------------------------------------------
let channel;
let sessionRequest = 0;
let heartbeatPending = false;
let onWorkspaceReady;
let onLogin;
let onLogout;
let onRestoreWorkspace;
let connectionRevision = 0;

export async function setConnected(id, enabled) {
  const userId = state.currentUser?.id;
  connectionRevision++;
  try {
    await apiRequest("/session/connections", { method: "POST", body: { id, enabled } });
    if (state.currentUser?.id !== userId) return false;
    if (enabled) state.connectedConnectionIds.add(id);
    else state.connectedConnectionIds.delete(id);
    connectionRevision++;
    return true;
  } catch (error) { toast(error.message); return false; }
}

// -----------------------------------------------------------------------------
// Login page
// -----------------------------------------------------------------------------
function renderLoginPage(setup = false) {
  onLogout();
  state.currentUser = null;
  state.nativeFilePickerAllowed = false;
  state.users = [];
  state.connections = [];
  state.savedQueries = [];
  state.queryHistory = [];
  state.tabs = [];
  state.activeTabId = null;
  state.connectedConnectionIds.clear();
  const confirmPassword = setup
    ? '<label>Confirm password<input type="password" name="confirm" required minlength="8" autocomplete="new-password" placeholder="Repeat your password"></label>'
    : "";
  const rememberMe = setup ? ""
    : '<label class="check-label"><input name="remember" type="checkbox">Remember me for 30 days</label>';
  findElement("#app").innerHTML = `<div class="auth">
    <header class="auth-header">${brand()}<span class="pill">DATABASE WORKSPACE</span></header>
    <main class="auth-main"><div>
      <section class="auth-card">
        <h1>${setup ? "Your workspace starts here." : "Welcome back."}</h1>
        <p>${setup ? "Create the administrator account for this GoSQL installation." : "Sign in to your database workspace."}</p>
        <form id="auth-form">
          <label>Username<input name="username" required autocomplete="username" maxlength="64" placeholder="Username"></label>
          <label>Password<input type="password" name="password" required minlength="8" maxlength="1024"
            autocomplete="${setup ? "new-password" : "current-password"}" placeholder="At least 8 characters"></label>
          ${confirmPassword}${rememberMe}
          <div id="auth-error" class="error-text" role="alert"></div>
          <button class="primary" type="submit">${setup ? "Create Admin Account" : "Sign In"}</button>
        </form>
      </section>
      <p class="demo-caption">Accounts and connection profiles are saved by GoSQL.<br>
        Browse PostgreSQL, MySQL, and MariaDB tables and views; edit tables with write access.</p>
    </div></main>
    <footer class="auth-footer">One workspace. All your databases.</footer>
  </div>`;
  findElement("#auth-form").onsubmit = async event => {
    event.preventDefault();
    const form = event.target;
    const data = new FormData(form);
    const errorNode = findElement("#auth-error");
    if (setup && data.get("password") !== data.get("confirm")) {
      errorNode.textContent = "Passwords do not match.";
      return;
    }
    const submit = form.querySelector('[type="submit"]');
    submit.disabled = true;
    try {
      const body = { username: data.get("username").trim(), password: data.get("password") };
      if (!setup) body.remember = data.has("remember");
      const response = await apiRequest(setup ? "/setup" : "/login", { method: "POST", body });
      form.reset();
      await login(response);
      channel.postMessage({ type: "login" });
    } catch (error) {
      errorNode.textContent = error.message;
      if (error.code === "already_initialized") await refreshSession();
    } finally { submit.disabled = false; }
  };
}

// -----------------------------------------------------------------------------
// Sign in, restore, and sign out
// -----------------------------------------------------------------------------
async function login(data) {
  const request = ++sessionRequest;
  try { await migrateLegacyWorkspace(data.user.id); }
  catch (error) { toast("Previous workspace could not be migrated: " + error.message); }
  if (request !== sessionRequest) return;
  const [profiles, workspace] = await Promise.all([apiRequest("/connections"), apiRequest("/workspace"), onWorkspaceReady()]);
  if (request !== sessionRequest) return;
  state.currentUser = data.user;
  state.nativeFilePickerAllowed = Boolean(data.nativeFilePickerAllowed);
  state.connections = profiles.map(connectionFromProfile);
  state.savedQueries = workspace.saved;
  state.queryHistory = workspace.history;
  state.tabs = [];
  state.activeTabId = null;
  state.connectedConnectionIds = new Set((data.connections || []).filter(id => profiles.some(profile => profile.id === id)));
  onLogin();
}

export async function refreshSession() {
  const request = ++sessionRequest;
  const revision = connectionRevision;
  const data = await apiRequest("/session");
  if (request !== sessionRequest) return;
  if (!data.user) {
    closeDialog();
    renderLoginPage(data.setupRequired);
    return;
  }
  if (state.currentUser?.id !== data.user.id) {
    closeDialog();
    await login(data);
    return;
  }
  state.currentUser = data.user;
  state.nativeFilePickerAllowed = Boolean(data.nativeFilePickerAllowed);
  if (revision !== connectionRevision) return;
  const connections = new Set(data.connections || []);
  const changed = state.connectedConnectionIds.size !== connections.size || [...connections].some(id => !state.connectedConnectionIds.has(id));
  state.connectedConnectionIds = connections;
  if (changed) {
    onRestoreWorkspace();
  }
}

export function handleSessionExpired() {
  if (!state.currentUser) return;
  sessionRequest++;
  closeDialog();
  renderLoginPage();
}

export async function logout() {
  try {
    await apiRequest("/logout", { method: "POST" });
    sessionRequest++;
    closeDialog();
    renderLoginPage();
    channel.postMessage({ type: "logout" });
  } catch (error) { toast(error.message); }
}

async function heartbeat() {
  if (!state.currentUser || heartbeatPending) return;
  heartbeatPending = true;
  try { await refreshSession(); }
  catch { /* Gangguan jaringan sementara tidak menghapus workspace yang sedang dibuka. */ }
  finally { heartbeatPending = false; }
}

// -----------------------------------------------------------------------------
// Initialization
// -----------------------------------------------------------------------------
export async function initializeSession(callbacks) {
  onWorkspaceReady = callbacks.onWorkspaceReady;
  onLogin = callbacks.onLogin;
  onLogout = callbacks.onLogout;
  onRestoreWorkspace = callbacks.onRestoreWorkspace;
  channel = new BroadcastChannel("gosql-session");
  channel.onmessage = () => refreshSession().catch(error => toast(error.message));
  window.addEventListener("pageshow", event => {
    if (event.persisted) return refreshSession().catch(error => toast(error.message));
  });
  setInterval(heartbeat, 30000);
  try { await refreshSession(); }
  catch {
    findElement("#app").innerHTML = '<div class="auth"><main class="auth-main"><div><h1>GoSQL is unavailable</h1><p>Check that GoSQL is running, then reload this page.</p><button id="retry-session" class="primary">Reload</button></div></main></div>';
    findElement("#retry-session").onclick = () => location.reload();
  }
}
