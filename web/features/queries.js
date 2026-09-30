import { getActiveTab, getConnectionById, state } from "../state.js";
import {
  findElement, button, closeDialog, confirmAction, escapeHtml, icon, iconButton, showDialog, toast,
  createId
} from "../ui.js";
import { openQueryTab, renderWorkspace } from "./workspace.js";
import {
  MAX_SAVED_QUERIES, MAX_SQL_LENGTH, saveQuery, appendQueryHistory,
  loadWorkspaceData, clearQueryHistory, deleteHistoryEntry, deleteSavedQuery
} from "../storage.js";
import { connectToDatabase } from "./connections.js";
import { apiRequest } from "../api.js";
import { getQueryText } from "./editor.js";
import { renderData } from "./grid.js";

// -----------------------------------------------------------------------------
// Query execution and cancellation
// -----------------------------------------------------------------------------
export function stopQueryForTab(tabId) {
  if (!state.runningQuery || (tabId && state.runningQuery.tabId !== tabId)) return;
  const runningTabId = state.runningQuery.tabId;
  state.runningQuery.controller.abort();
  state.runningQuery = null;
  if (state.activeTabId === runningTabId) {
    const control = findElement('[data-action="cancel-query"]');
    if (control) { control.dataset.action = "run-query"; control.innerHTML = icon("play") + "Run"; }
  }
}

export async function runQuery() {
  const tab = getActiveTab();
  if (!tab) return;
  if (state.runningQuery) return toast("Another query is still running.");
  const sql = getQueryText(tab).trim();
  if (!sql) return toast("Enter SQL before running a query.");
  if (sql.length > MAX_SQL_LENGTH) return toast("SQL cannot exceed 50,000 characters.");
  const connection = getConnectionById(tab.connectionId);
  if (!["PostgreSQL", "MySQL", "MariaDB", "SQLite"].includes(connection?.engine)) return toast("SQL execution is not available for this engine.");
  const controller = new AbortController();
  const running = { tabId: tab.id, controller };
  state.runningQuery = running;
  const control = findElement('[data-action="run-query"]');
  if (control) { control.dataset.action = "cancel-query"; control.innerHTML = icon("stop") + "Cancel"; }
  const started = performance.now();
  let status = "Success", rowCount = 0;
  try {
    const result = await apiRequest(`/connections/${encodeURIComponent(tab.connectionId)}/query`, {
      method: "POST", body: { database: tab.db, sql }, signal: controller.signal
    });
    rowCount = result.columns.length ? result.rows.length : result.affectedRows;
    tab.result = { rows: result.rows, columns: result.columns, duration: result.duration };
    if (!result.columns.length) tab.result.message = `${result.command || "Statement completed"} · ${result.duration} ms`;
    tab.page = 1;
    if (state.activeTabId === tab.id) {
      if (!tab.queryTab && tab.view !== "Data") { tab.view = "Data"; renderWorkspace(); }
      else renderData();
    }
  } catch (error) {
    status = controller.signal.aborted ? "Cancelled" : "Error";
    if (status === "Error") {
      tab.result = { error: error.message };
      if (state.activeTabId === tab.id) {
        if (!tab.queryTab && tab.view !== "Data") { tab.view = "Data"; renderWorkspace(); }
        else renderData();
      }
    }
  } finally {
    try {
      const entry = {
        id: createId(), sql, connectionId: tab.connectionId, db: tab.db,
        schemaName: tab.schemaName || "", time: new Date().toISOString(),
        duration: Math.round(performance.now() - started), status, rowCount
      };
      await appendQueryHistory(entry, { dedupeLatest: true });
    } catch (error) { toast("Query finished, but history could not be saved: " + error.message); }
    if (state.runningQuery === running) state.runningQuery = null;
    if (state.activeTabId === tab.id && !state.runningQuery) {
      const control = findElement('[data-action="cancel-query"]');
      if (control) { control.dataset.action = "run-query"; control.innerHTML = icon("play") + "Run"; }
    }
  }
}

function cancelQuery() { stopQueryForTab(); }

// -----------------------------------------------------------------------------
// Saved queries and history
// -----------------------------------------------------------------------------
function openSaveQueryDialog() {
  const activeTab = getActiveTab();
  if (!activeTab) return toast("Open a query first.");
  if (state.savedQueries.filter(item => item.connectionId === activeTab.connectionId && item.db === activeTab.db).length >= MAX_SAVED_QUERIES) return toast("Saved query limit reached for this database. Delete one before saving another.");
  if (activeTab.sql.length > MAX_SQL_LENGTH) return toast("Queries longer than 50,000 characters cannot be saved.");
  const connection = getConnectionById(activeTab.connectionId);
  showDialog("Save query", `<form id="save-query-form" class="stack">
    <label>Query name<input name="name" required placeholder="Query name" maxlength="100"></label>
    <p class="hint">Saved to ${escapeHtml(connection.name)} / ${escapeHtml(activeTab.db)}. This query cannot be used across connections.</p>
    <pre class="sql-preview">${escapeHtml(activeTab.sql)}</pre>
    <button type="submit" class="primary">Save Query</button>
  </form>`);
  findElement("#save-query-form").onsubmit = async (event) => {
    event.preventDefault();
    const submit = event.target.querySelector('[type="submit"]');
    submit.disabled = true;
    try {
      await saveQuery({ id: createId(), name: new FormData(event.target).get("name"), sql: activeTab.sql, connectionId: activeTab.connectionId, db: activeTab.db, schemaName: activeTab.schemaName });
      closeDialog();
      toast("Query saved.");
    } catch (error) { toast(error.message); }
    finally { submit.disabled = false; }
  };
}

async function openQueryCollection(kind, connectionId, db, visible = 20) {
  const userId = state.currentUser?.id;
  await loadWorkspaceData();
  if (!userId || state.currentUser?.id !== userId) return;
  const isHistory = kind === "history";
  const items = (isHistory ? state.queryHistory : state.savedQueries)
    .filter(item => item.connectionId === connectionId && item.db === db);
  const cards = items.slice(0, visible).map((item) => {
    const connection = getConnectionById(item.connectionId);
    const stats = isHistory
      ? ` · ${item.duration} ms · ${item.rowCount ?? 0} rows` : "";
    return `<article class="collection-item">
      <div class="row">
        <strong class="grow">${escapeHtml(isHistory ? item.status : item.name)}</strong>
        <small>${isHistory ? new Date(item.time).toLocaleString() : ""}</small>
        ${iconButton(isHistory ? "delete-history" : "delete-saved", "Delete query", "trash", `data-id="${escapeHtml(item.id)}"`)}
      </div>
      <code>${escapeHtml(item.sql)}</code>
      <div class="row">
        <span class="muted grow">${escapeHtml(connection?.name || "Deleted connection")} / ${escapeHtml(item.db)}${stats}</span>
        ${button(isHistory ? "open-history" : "open-saved", "Open Query", "external", "",
          `data-id="${escapeHtml(item.id)}" ${connection ? "" : "disabled"}`)}
      </div>
    </article>`;
  }).join("");
  const body = items.length ? `<div class="collection-list">${cards}</div>`
    : `<div class="result-empty" style="padding:35px 0">
      ${icon(isHistory ? "clock" : "bookmark")}
      <h3>${isHistory ? "No queries yet" : "No saved queries"}</h3>
      <p class="hint">${isHistory ? "Run a query to see its history here." : "Save a query from the SQL console to find it here."}</p>
    </div>`;
  const loadMore = items.length > visible
    ? button("load-query-collection", `Load More (${items.length - visible} remaining)`, "down", "", `data-kind="${kind}" data-id="${escapeHtml(connectionId)}" data-db="${escapeHtml(db)}" data-visible="${visible + 20}"`)
    : "";
  const footer = loadMore + (isHistory && items.length
    ? button("clear-history", "Clear History", "trash", "danger", `data-id="${escapeHtml(connectionId)}" data-db="${escapeHtml(db)}"`) : "")
    + button("close-dialog", "Close");
  showDialog((isHistory ? "SQL History · " : "Saved Queries · ") + escapeHtml(db), body, footer, true);
}

function openStoredQuery(id, fromHistory = false) {
  const item = (fromHistory ? state.queryHistory : state.savedQueries).find((query) => query.id === id);
  if (!item) return;
  closeDialog();
  connectToDatabase(item.connectionId, () => {
    openQueryTab(item.sql, { ...item, table: "", schema: [] });
  });
}

// -----------------------------------------------------------------------------
// User actions
// -----------------------------------------------------------------------------
export function handleQueriesAction(action, element) {
  const id = element.dataset.id;
  switch (action) {
    case "run-query":
      runQuery();
      break;
    case "cancel-query":
      cancelQuery();
      break;
    case "save-query":
      openSaveQueryDialog();
      break;
    case "saved":
      openQueryCollection("saved", id, element.dataset.db).catch(error => toast(error.message));
      break;
    case "history":
      if (!getActiveTab()) return toast("Open a database first."), true;
      openQueryCollection("history", getActiveTab().connectionId, getActiveTab().db).catch(error => toast(error.message));
      break;
    case "load-query-collection":
      openQueryCollection(element.dataset.kind, id, element.dataset.db, Number(element.dataset.visible)).catch(error => toast(error.message));
      break;
    case "clear-history":
      confirmAction("Clear SQL history?", `All query history for ${escapeHtml(element.dataset.db)} will be removed. Saved queries will remain.`, "Clear History", () => {
        clearQueryHistory(id, element.dataset.db).then(() => openQueryCollection("history", id, element.dataset.db)).catch(error => toast(error.message));
      });
      break;
    case "delete-history":
      {
        const item = state.queryHistory.find(query => query.id === id);
        if (item) deleteHistoryEntry(id).then(() => openQueryCollection("history", item.connectionId, item.db)).catch(error => toast(error.message));
      }
      break;
    case "delete-saved": {
      const item = state.savedQueries.find((query) => query.id === id);
      if (!item) break;
      deleteSavedQuery(id).then(() => openQueryCollection("saved", item.connectionId, item.db)).catch(error => toast(error.message));
      break;
    }
    case "open-saved":
      openStoredQuery(id);
      break;
    case "open-history":
      openStoredQuery(id, true);
      break;
    default:
      return false;
  }
  return true;
}
