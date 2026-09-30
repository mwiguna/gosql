import { state } from "./state.js";
import { apiRequest } from "./api.js";
import { toast } from "./ui.js";

export const MAX_HISTORY_ENTRIES = 100;
export const MAX_SAVED_QUERIES = 100;
export const MAX_SQL_LENGTH = 50000;

// -----------------------------------------------------------------------------
// Server workspace storage
// -----------------------------------------------------------------------------
export async function loadWorkspaceData() {
  const userId = state.currentUser?.id;
  const data = await apiRequest("/workspace");
  if (state.currentUser?.id !== userId) return;
  state.savedQueries = data.saved;
  state.queryHistory = data.history;
}

export async function appendQueryHistory(entry, { dedupeLatest = false } = {}) {
  const userId = state.currentUser?.id;
  if (!userId) return;
  const saved = { ...entry, sql: entry.sql.slice(0, MAX_SQL_LENGTH), rowCount: entry.rowCount ?? 0 };
  const result = await apiRequest("/history" + (dedupeLatest ? "?dedupeLatest=1" : ""), { method: "POST", body: saved });
  if (result?.stored === false) return false;
  if (state.currentUser?.id !== userId) return;
  const history = [saved, ...state.queryHistory.filter(item => item.id !== saved.id)];
  const counts = new Map();
  state.queryHistory = history.filter(item => {
    const key = item.connectionId + "\0" + item.db;
    const count = counts.get(key) || 0;
    counts.set(key, count + 1);
    return count < MAX_HISTORY_ENTRIES;
  });
  return true;
}

export async function runRecordedSQL(path, options, location, sql) {
  const started = performance.now();
  let result, failure;
  try { result = await apiRequest(path, options); }
  catch (error) { failure = error; }
  try {
    await appendQueryHistory({
      id: crypto.randomUUID(), sql: result?.sql || sql, connectionId: location.connectionId,
      db: location.db, schemaName: location.schemaName || "",
      time: new Date().toISOString(), duration: Math.round(performance.now() - started),
      status: failure ? "Error" : "Success", rowCount: result?.affectedRows ?? 0
    });
  } catch (error) { toast("SQL history could not be saved: " + error.message); }
  if (failure) throw failure;
  return result;
}

export async function saveQuery(entry) {
  const userId = state.currentUser?.id;
  await apiRequest("/saved-queries", { method: "POST", body: entry });
  if (state.currentUser?.id !== userId) return;
  state.savedQueries = [entry, ...state.savedQueries.filter(item => item.id !== entry.id)];
}

export async function deleteSavedQuery(id) {
  const userId = state.currentUser?.id;
  await apiRequest("/saved-queries/" + encodeURIComponent(id), { method: "DELETE" });
  if (state.currentUser?.id === userId) state.savedQueries = state.savedQueries.filter(item => item.id !== id);
}

export async function deleteHistoryEntry(id) {
  const userId = state.currentUser?.id;
  await apiRequest("/history/" + encodeURIComponent(id), { method: "DELETE" });
  if (state.currentUser?.id === userId) state.queryHistory = state.queryHistory.filter(item => item.id !== id);
}

export async function clearQueryHistory(connectionId, db) {
  const userId = state.currentUser?.id;
  const query = connectionId && db ? "?" + new URLSearchParams({ connectionId, db }) : "";
  await apiRequest("/history" + query, { method: "DELETE" });
  if (state.currentUser?.id === userId) state.queryHistory = query
    ? state.queryHistory.filter(item => item.connectionId !== connectionId || item.db !== db) : [];
}

// -----------------------------------------------------------------------------
// One-time browser migration
// -----------------------------------------------------------------------------
// Data lama hanya dibaca. Hapus salinan browser setelah transaksi server berhasil;
// digest membuat retry aman jika respons hilang setelah commit.
export async function migrateLegacyWorkspace(userId) {
  const key = "gosql-data-" + userId;
  const raw = localStorage.getItem(key);
  if (!raw) return;
  const data = JSON.parse(raw);
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(raw));
  const source = [...new Uint8Array(digest)].map(byte => byte.toString(16).padStart(2, "0")).join("");
  const location = item => ({
    id: String(item.id), sql: item.sql, connectionId: item.connectionId,
    db: item.db, schemaName: item.schemaName ?? ""
  });
  const saved = (data.saved ?? []).map(item => ({ ...location(item), name: item.name }));
  const history = (data.history ?? []).map(item => ({
    ...location(item), time: item.time, duration: item.duration ?? 0,
    status: item.status, rowCount: item.rowCount ?? 0
  }));
  await apiRequest("/workspace/import", { method: "POST", body: { source, saved, history } });
  if (localStorage.getItem(key) === raw) localStorage.removeItem(key);
}
