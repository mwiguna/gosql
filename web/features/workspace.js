import { getActiveTab, getConnectionById, state, engines } from "../state.js";
import {
  bindConnectionSearch, connectToDatabase, getConnectionSearch, renderTree, resetConnectionTree,
  revealTreeDestination, syncTreeSelection, loadDatabaseCatalog
} from "./connections.js";
import {
  findElement, findElements, brand, button, closeDialog, engineIcon, escapeHtml, icon, iconButton,
  toast, createId
} from "../ui.js";
import { apiRequest } from "../api.js";
import { destroyEditor, mountEditor } from "./editor.js";
import { destroyGrid, renderData } from "./grid.js";

// -----------------------------------------------------------------------------
// Navigation state
// -----------------------------------------------------------------------------
const MAX_WORKSPACE_TABS = 30;
let restoringLocation = false;
let locationRequest = 0;
let navigationHash = "";
let onRunQuery;
let onCloseTab;
const tableRequests = new Map();
const countRequests = new Map();
let closeAllTooltip;

// -----------------------------------------------------------------------------
// Workspace lifecycle
// -----------------------------------------------------------------------------
export function startWorkspace() {
  resetConnectionTree(true);
  restoringLocation = true;
  renderAppShell();
  restoreTableLocation();
}

export function resetWorkspace() {
  for (const request of tableRequests.values()) request.abort();
  tableRequests.clear();
  for (const request of countRequests.values()) request.abort();
  countRequests.clear();
  locationRequest++;
  restoringLocation = false;
  destroyWorkspace();
}

// -----------------------------------------------------------------------------
// Application shell and active workspace
// -----------------------------------------------------------------------------
export function renderAppShell() {
  const sidebarScrollTop = findElement("#tree")?.scrollTop ?? 0;
  for (const [id, request] of tableRequests) {
    if (!state.tabs.some(tab => tab.id === id)) { request.abort(); tableRequests.delete(id); }
  }
  for (const [id, request] of countRequests) {
    if (!state.tabs.some(tab => tab.id === id)) { request.abort(); countRequests.delete(id); }
  }
  destroyWorkspace();
  const userButton = state.currentUser.role === "Super Admin"
    ? button("users", "User Management", "users", "ghost") : "";
  const engine = getActiveTab() ? escapeHtml(getConnectionById(getActiveTab().connectionId)?.engine) : "No active connection";
  findElement("#app").innerHTML = `<div class="app-shell">
    <header class="topbar">
      ${iconButton("sidebar", "Toggle connections", "menu", 'class="mobile-menu icon ghost"')}
      ${brand()}<span class="top-divider"></span><span class="top-title">Database Workspace</span>
      <span class="spacer"></span><span class="pill">DATABASE WORKSPACE</span>
      ${iconButton("help", "Help and shortcuts", "help")}
      ${iconButton("settings", "Workspace settings", "settings")}
      <button class="ghost icon" data-action="account" aria-label="Account menu">
        <span class="avatar">${escapeHtml(state.currentUser.username.slice(0, 2).toUpperCase())}</span>
      </button>
    </header>
    <div class="shell-body">
      <aside class="sidebar" aria-label="Connections">
        <div class="sidebar-title">Connections${iconButton("add-connection", "New connection", "plus")}</div>
        <div class="sidebar-search">${icon("search")}
          <input aria-label="Find connection" placeholder="Find a connection…"
            value="${escapeHtml(getConnectionSearch())}" id="connection-search">
        </div>
        <nav class="tree" id="tree"></nav>
        <div class="sidebar-bottom">
          ${button("history", "SQL History", "clock", "ghost")}${userButton}
        </div>
      </aside>
      <div class="scrim" data-action="sidebar"></div>
      <main class="workspace" id="workspace"></main>
    </div>
    <footer class="statusbar">
      <span class="dot"></span><span>${engine}</span><span>Database Workspace</span>
      <span class="spacer"></span>
      <span class="status-shortcut">Database table browser</span><span>GoSQL 0.1</span>
    </footer>
  </div>`;
  const mobile = findElement(".topbar button");
  mobile.classList.add("mobile-menu");
  renderTree();
  findElement("#tree").scrollTop = sidebarScrollTop;
  renderWorkspace();
  bindConnectionSearch();
}

export function renderWorkspace() {
  syncTableLocation();
  destroyWorkspace();

  const tab = getActiveTab();
  const workspace = findElement("#workspace");
  if (!tab) {
    const cards = engines.map((engine) => {
      const connection = state.connections.find((candidate) => candidate.engine === engine);
      const action = connection ? "connect" : "add-connection";
      const label = connection ? "Connect" : "Add connection";
      const attributes = connection ? `data-id="${connection.id}"` : `data-engine="${engine}"`;
      return `<article class="connection-card">
        <span class="engine-symbol">${engineIcon(engine)}</span>
        <div><h3>${engine}</h3><small>${connection ? escapeHtml(connection.name) : "No connections yet"}</small></div>
        ${button(action, label, "chevron", "", attributes)}
      </article>`;
    }).join("");
    workspace.innerHTML = `<div class="welcome">
      <div class="empty-icon">${icon("database")}</div>
      <h1>A place for your databases.</h1>
      <p>Connect to PostgreSQL, MySQL, MariaDB, or SQLite<br>to browse your tables, views, and columns.</p>
      <div class="connection-cards">${cards}</div>
      <div class="welcome-actions">
        ${button("add-connection", "New Connection", "plus", "primary")}
        ${button("help", "Help", "", "link")}
      </div>
      <div class="welcome-foot">
        <span>${icon("lock")}Your connections are private.</span>
        <span>PostgreSQL · MySQL · MariaDB · SQLite</span>
      </div>
    </div>`;
    return;
  }

  const connection = getConnectionById(tab.connectionId);
  const tabButtons = state.tabs.map((item) => `<div class="workspace-tab ${item.id === tab.id ? "active" : ""}"
      role="tab" tabindex="0" aria-selected="${item.id === tab.id}"
      data-action="switch-tab" data-id="${item.id}">
      ${icon(item.queryTab ? "terminal" : "table")}
      <span>${escapeHtml(item.title)}</span>
      ${iconButton("close-tab", "Close " + item.title, "x", `data-id="${item.id}"`)}
    </div>`).join("");
  const breadcrumb = `<div class="breadcrumb">
    ${engineIcon(connection.engine)}<strong>${escapeHtml(connection.name)}</strong>
    ${icon("chevron")}<span>${escapeHtml(tab.db)}</span>
    ${connection.engine === "PostgreSQL" ? icon("chevron") + "<span>" + escapeHtml(tab.schemaName) + "</span>" : ""}
    ${icon("chevron")}<strong>${escapeHtml(tab.title)}</strong>
    <span class="spacer"></span>
    <span class="connection-meta">${connection.engine === "SQLite" ? escapeHtml(connection.file) : escapeHtml(connection.host) + ":" + escapeHtml(connection.port)}</span>
  </div>`;
  const runAction = state.runningQuery?.tabId === tab.id ? "cancel-query" : "run-query";
  const editorMarkup = tab.consoleOpen ? `<div id="query-search"></div>
    <div class="editor-wrap" id="editor"></div>
    <div class="console-caption">${icon("info")}Tab to indent · Shift+Tab to unindent · Esc then Tab to leave editor.
      <span class="spacer"></span><span>${connection.engine === "SQLite" ? "One SELECT statement" : "SQL"}</span>
    </div>` : "";
  const consoleMarkup = `<section class="console">
    <div class="console-head">
      ${button("toggle-console", "Query console", tab.consoleOpen ? "down" : "chevron", "ghost")}
      <span class="pill">${connection.engine}</span><span class="spacer"></span>
      <div class="console-actions">
        ${button("editor-search", "", "search", "icon ghost", `aria-label="Find and replace" title="Find and Replace" aria-expanded="${Boolean(tab.consoleOpen && tab.findOpen)}" aria-controls="query-search"`)}
        ${button("save-query", '<span class="save-query-label">Save</span>', "bookmark", "ghost", 'aria-label="Save query"')}
        ${button("query-new-tab", '<span class="new-tab-label">Open in New Tab</span>', "external", "ghost", 'aria-label="Open in New Tab"')}
        ${button(runAction, runAction === "cancel-query" ? "Cancel" : "Run", runAction === "cancel-query" ? "x" : "play", "primary console-run")}
        <span class="hint">⌘ / Ctrl ↵</span>
      </div>
    </div>
    ${editorMarkup}
  </section>`;
  const views = ["Data", "Structure", "Indexes", "Constraints"];
  const viewTabs = tab.queryTab ? "" : `<nav class="subtabs">${views.map((view) =>
    button("view", view + (view === "Structure" ? ` <span>${tab.schema.length}</span>` : ""),
      "", tab.view === view ? "active" : "", `data-view="${view}"`)
  ).join("")}</nav>`;
  const note = "Edits and deletions check the row version · Views are read-only · Concurrent changes can affect pagination.";

  workspace.innerHTML = `<div class="workspace-tabs" role="tablist">
    ${tabButtons}${iconButton("new-query", "New query tab", "plus")}${button("close-all-tabs", "", "x", "icon ghost", 'aria-label="Close all tabs"')}
  </div>${breadcrumb}${consoleMarkup}${viewTabs}
  <section class="data-area" id="data-area"></section>
  <div class="table-note">${icon("info")}<span>${note}</span></div>`;

  if (tab.consoleOpen) mountEditor(tab, connection, onRunQuery);
  renderData();
  syncTreeSelection();
  const closeAll = findElement('[data-action="close-all-tabs"]');
  closeAllTooltip = document.createElement("div");
  closeAllTooltip.className = "close-all-tooltip";
  closeAllTooltip.textContent = "Close all tabs";
  closeAllTooltip.hidden = true;
  document.body.append(closeAllTooltip);
  const showTooltip = () => {
    const bounds = closeAll.getBoundingClientRect();
    closeAllTooltip.hidden = false;
    closeAllTooltip.style.left = Math.max(8, Math.min(bounds.left + bounds.width / 2 - closeAllTooltip.offsetWidth / 2, window.innerWidth - closeAllTooltip.offsetWidth - 8)) + "px";
    closeAllTooltip.style.top = bounds.bottom + 7 + "px";
  };
  const hideTooltip = () => { closeAllTooltip.hidden = true; };
  closeAll.addEventListener("pointerenter", showTooltip);
  closeAll.addEventListener("pointerleave", hideTooltip);
  closeAll.addEventListener("focus", showTooltip);
  closeAll.addEventListener("blur", hideTooltip);
  findElement(".workspace-tabs").addEventListener("scroll", hideTooltip);
}

// -----------------------------------------------------------------------------
// Workspace cleanup
// -----------------------------------------------------------------------------
export function destroyWorkspace() {
  closeAllTooltip?.remove();
  closeAllTooltip = null;
  destroyEditor();
  destroyGrid();
}

// -----------------------------------------------------------------------------
// Opening tables and query tabs
// -----------------------------------------------------------------------------
export function openTable(connectionId, db, table, isView = false, schemaName = getConnectionById(connectionId)?.engine === "PostgreSQL" ? "public" : "") {
  const existing = state.tabs.find((tab) => !tab.queryTab && tab.connectionId === connectionId && tab.db === db && tab.schemaName === schemaName && tab.table === table);
  if (existing) {
    state.activeTabId = existing.id;
    renderWorkspace();
    return;
  }
  if (state.tabs.length >= MAX_WORKSPACE_TABS) {
    toast("Close a tab before opening another. Limit: 30 tabs.");
    return;
  }
  if (!table) return;
  if (!["PostgreSQL", "MySQL", "MariaDB", "SQLite"].includes(getConnectionById(connectionId)?.engine)) return toast("Database access is not available for this engine.");
  const tab = createWorkspaceTab(connectionId, db, table, isView, schemaName);
  state.tabs.push(tab);
  state.activeTabId = tab.id;
  renderAppShell();
  if (!tab.queryTab) loadTablePage(tab);
  findElement(".sidebar")?.classList.remove("open");
}

export async function loadTablePage(tab, page = tab.page, recount = false) {
  if (tab.cursorPaging && page > 1 && !tab.pageCursors?.[page]) {
    tab.loadError = "Page cursor is unavailable. Return to the first page.";
    if (state.activeTabId === tab.id) renderData();
    return;
  }
  tableRequests.get(tab.id)?.abort();
  const controller = new AbortController();
  tableRequests.set(tab.id, controller);
  const userId = state.currentUser?.id;
  tab.loading = true;
  tab.loadError = "";
  tab.connectionRequired = false;
  if (recount) {
    countRequests.get(tab.id)?.abort();
    countRequests.delete(tab.id);
    tab.countLoaded = false;
    tab.totalRows = null;
    tab.countError = "";
  }
  if (state.activeTabId === tab.id) renderData();
  try {
    const query = new URLSearchParams({ database: tab.db, schema: tab.schemaName, table: tab.table, page, pageSize: tab.pageSize });
    if (tab.cursorPaging && page > 1) query.set("cursor", tab.pageCursors[page]);
    const data = await apiRequest("/connections/" + encodeURIComponent(tab.connectionId) + "/rows?" + query, { signal: controller.signal });
    if (controller.signal.aborted || state.currentUser?.id !== userId || !state.tabs.includes(tab)) return;
    const pages = tab.totalRows === null ? null : Math.max(1, Math.ceil(tab.totalRows / tab.pageSize));
    if (!data.cursorPaging && pages !== null && page > pages) return loadTablePage(tab, pages);
    if (data.cursorPaging && page > 1 && !data.rows.length) return loadTablePage(tab, page - 1);
    tab.schema = data.columns;
    tab.primaryKey = data.primaryKey;
    tab.versions = data.versions;
    tab.keyValues = data.keyValues || null;
    tab.keyTypes = data.keyTypes || null;
    tab.sqliteStrict = Boolean(data.sqliteStrict);
    tab.sqliteWithoutRowid = Boolean(data.sqliteWithoutRowid);
    tab.sqliteDDL = data.sqliteDDL || "";
    tab.sqliteVirtual = Boolean(data.sqliteVirtual);
    tab.truncated = data.truncated;
    tab.editable = data.editable;
    tab.readOnlyReason = data.readOnlyReason || "";
    tab.indexes = data.indexes;
    tab.constraints = data.constraints;
    if (state.activeTabId === tab.id) {
      const count = findElement('[data-view="Structure"] span');
      if (count) count.textContent = String(tab.schema.length);
    }
    tab.rows = data.rows;
    tab.hasMore = data.hasMore;
    tab.cursorPaging = Boolean(data.cursorPaging);
    if (tab.cursorPaging) {
      if (page === 1) tab.pageCursors = { 1: "" };
      else tab.pageCursors ||= { 1: "" };
      if (data.hasMore && data.nextCursor) tab.pageCursors[page + 1] = data.nextCursor;
      else delete tab.pageCursors[page + 1];
    } else tab.pageCursors = { 1: "" };
    tab.duration = data.duration;
    tab.page = page;
    if (!tab.countLoaded) loadTableCount(tab, userId);
  } catch (error) {
    if (controller.signal.aborted || state.currentUser?.id !== userId || !state.tabs.includes(tab)) return;
    tab.loadError = error.message;
    tab.connectionRequired = error.code === "connection_required";
    if (tab.connectionRequired) { state.connectedConnectionIds.delete(tab.connectionId); renderTree(); }
  } finally {
    if (tableRequests.get(tab.id) === controller) {
      tableRequests.delete(tab.id);
      tab.loading = false;
      if (state.currentUser?.id === userId && state.activeTabId === tab.id && state.tabs.includes(tab)) renderData();
    }
  }
}

async function loadTableCount(tab, userId) {
  if (countRequests.has(tab.id)) return;
  const controller = new AbortController();
  countRequests.set(tab.id, controller);
  tab.countError = "";
  tab.countLoading = true;
  if (state.activeTabId === tab.id) renderData();
  const query = new URLSearchParams({ database: tab.db, schema: tab.schemaName, table: tab.table });
  try {
    const data = await apiRequest("/connections/" + encodeURIComponent(tab.connectionId) + "/rows/count?" + query, { signal: controller.signal });
    if (controller.signal.aborted || state.currentUser?.id !== userId || !state.tabs.includes(tab)) return;
    tab.totalRows = Number.isSafeInteger(data.totalRows) && data.totalRows >= 0 ? data.totalRows : null;
    tab.countLoaded = true;
    const pages = tab.totalRows === null ? null : Math.max(1, Math.ceil(tab.totalRows / tab.pageSize));
    if (!tab.cursorPaging && pages !== null && tab.page > pages) loadTablePage(tab, pages);
    else if (state.activeTabId === tab.id) renderData();
  } catch (error) {
    if (!controller.signal.aborted && state.currentUser?.id === userId && state.tabs.includes(tab)) {
      tab.countLoaded = true;
      tab.countError = error.message;
    }
  } finally {
    if (countRequests.get(tab.id) === controller) {
      countRequests.delete(tab.id);
      tab.countLoading = false;
      if (state.currentUser?.id === userId && state.activeTabId === tab.id && state.tabs.includes(tab)) renderData();
    }
  }
}

export function retryTableCount(tab) {
  if (countRequests.has(tab.id)) return;
  tab.countLoaded = false;
  loadTableCount(tab, state.currentUser?.id);
}

export function openQueryTab(text, destination) {
  const parent = destination || getActiveTab();
  if (!parent) return toast("Open a database first.");
  if (state.tabs.length >= MAX_WORKSPACE_TABS) {
    toast("Close a tab before opening another. Limit: 30 tabs.");
    return;
  }
  const count = state.tabs.filter((tab) => tab.queryTab).length;
  const tab = createWorkspaceTab(parent.connectionId, parent.db, parent.table, false, parent.schemaName);
  tab.schema = structuredClone(parent.schema);
  tab.title = "Query " + (count + 1);
  tab.queryTab = true;
  tab.consoleOpen = true;
  tab.sql = text ?? parent.sql;
  tab.result = { empty: true };
  state.tabs.push(tab);
  state.activeTabId = tab.id;
  renderAppShell();
  findElement(".sidebar")?.classList.remove("open");
}

function createWorkspaceTab(connectionId, db, table, isView, schemaName) {
  const mysql = ["MySQL", "MariaDB"].includes(getConnectionById(connectionId)?.engine);
  const quote = mysql
    ? name => "`" + name.replaceAll("`", "``") + "`"
    : name => '"' + name.replaceAll('"', '""') + '"';
  const qualifiedName = (mysql ? [db, table] : getConnectionById(connectionId)?.engine === "SQLite" ? ["main", table] : [schemaName, table]).filter(Boolean).map(quote).join(".");
  return {
    id: createId(), connectionId, db, schemaName, table,
    title: table, queryTab: false, isView, consoleOpen: false,
    sql: table ? `SELECT *\nFROM ${qualifiedName}\nLIMIT 100;` : "",
    view: "Data", remote: true, loading: true, rows: [], schema: [],
    indexes: [], constraints: [],
    page: 1, pageSize: 20, pageCursors: { 1: "" }, cursorPaging: false,
    search: "", sorts: [], filter: null, result: null
  };
}

// -----------------------------------------------------------------------------
// URL navigation
// -----------------------------------------------------------------------------
function syncTableLocation(replace = false) {
  const tab = getActiveTab();
  if (!state.currentUser || restoringLocation || tab?.queryTab) return;
  const hash = tableLocation(tab);
  navigationHash = hash;
  if (location.hash !== hash) window.history[replace ? "replaceState" : "pushState"](null, "", location.pathname + location.search + hash);
}

export function restoreTableLocation() {
  if (!state.currentUser) return;
  const request = ++locationRequest;
  const userId = state.currentUser.id;
  closeDialog();
  restoringLocation = true;
  let destination;
  let currentConnection;
  let isView;
  try {
    destination = readTableLocation(location.hash);
    if (!destination) {
      state.activeTabId = null;
      restoringLocation = false;
      renderAppShell();
      return;
    }
    currentConnection = getConnectionById(destination.connectionId);
    if (!currentConnection) throw new Error("This connection is not available in your account.");
    if (!["PostgreSQL", "MySQL", "MariaDB", "SQLite"].includes(currentConnection.engine)) throw new Error("Database access is not available for this engine.");
    if (currentConnection.engine === "PostgreSQL") destination.schemaName ||= "public";
    else destination.schemaName = "";
  } catch (error) {
    state.activeTabId = null;
    restoringLocation = false;
    syncTableLocation(true);
    renderAppShell();
    toast(error.message);
    return;
  }
  const existing = state.tabs.find((activeTab) => !activeTab.queryTab && activeTab.connectionId === currentConnection.id && activeTab.db === destination.db && activeTab.schemaName === destination.schemaName && activeTab.table === destination.table);
  if (!existing && state.tabs.length >= MAX_WORKSPACE_TABS) {
    restoringLocation = false;
    syncTableLocation(true);
    toast("Close a tab before opening this workspace link.");
    return;
  }
  let completed = false;
  connectToDatabase(currentConnection.id, async () => {
    if (request !== locationRequest || state.currentUser?.id !== userId) return;
    completed = true;
    let catalog;
    try {
      catalog = await loadDatabaseCatalog(currentConnection.id, destination.db);
    } catch (error) {
      if (request !== locationRequest || state.currentUser?.id !== userId) return;
      state.activeTabId = null;
      restoringLocation = false;
      syncTableLocation(true);
      renderAppShell();
      toast(error.message);
      return;
    }
    if (!catalog || request !== locationRequest || state.currentUser?.id !== userId) return;
    const schema = ["MySQL", "MariaDB", "SQLite"].includes(currentConnection.engine)
      ? { tables: catalog.tables || [], views: catalog.views || [] }
      : catalog.schemas.find(item => item.name === destination.schemaName);
    isView = schema?.views.includes(destination.table) || false;
    if (destination.table && !schema?.tables.includes(destination.table) && !isView) {
      restoringLocation = false;
      toast("The table or view in this link is not available.");
      syncTableLocation(true);
      return;
    }
    revealTreeDestination(currentConnection, destination, isView);
    if (!destination.table) {
      state.activeTabId = null;
      renderAppShell();
      restoringLocation = false;
      navigationHash = location.hash;
      return;
    }
    openTable(currentConnection.id, destination.db, destination.table, isView, destination.schemaName);
    if (!getActiveTab()) { restoringLocation = false; return; }
    getActiveTab().view = destination.view;
    getActiveTab().result = null;
    renderWorkspace();
    restoringLocation = false;
    syncTableLocation(true);
  });
  if (!completed) {
    const onClose = () => {
      if (findElement("#dialog").open) return;
      findElement("#dialog").removeEventListener("close", onClose);
      if (!completed && request === locationRequest) {
        restoringLocation = false;
        syncTableLocation(true);
      }
    };
    findElement("#dialog").addEventListener("close", onClose);
  }
}

function navigateTableLocation() {
  if (navigationHash === location.hash) return;
  navigationHash = location.hash;
  restoreTableLocation();
}

// -----------------------------------------------------------------------------
// Navigation serialization
// -----------------------------------------------------------------------------
export function tableLocation(tab) {
  if (!tab) return "";
  const params = new URLSearchParams({ connection: tab.connectionId, database: tab.db });
  if (tab.schemaName) params.set("schema", tab.schemaName);
  if (tab.table) params.set("table", tab.table);
  if (tab.view && tab.view !== "Data") params.set("view", tab.view);
  return "#" + params.toString();
}

export function readTableLocation(hash) {
  if (!hash || hash === "#") return null;
  const params = new URLSearchParams(hash.replace(/^#/, ""));
  const destination = {
    connectionId: params.get("connection"),
    db: params.get("database"),
    schemaName: params.get("schema") || "",
    table: params.get("table"),
    view: params.get("view") || "Data"
  };
  if (!destination.connectionId || !destination.db || !["Data", "Structure", "Indexes", "Constraints"].includes(destination.view)) {
    throw new Error("This workspace link is incomplete or invalid.");
  }
  return destination;
}

// -----------------------------------------------------------------------------
// User actions
// -----------------------------------------------------------------------------
export function handleWorkspaceAction(action, element) {
  const id = element.dataset.id;
  const activeTab = getActiveTab();
  switch (action) {
    case "open-table":
      openTable(id, element.dataset.db, element.dataset.table, false, element.dataset.schema);
      break;
    case "open-view":
      openTable(id, element.dataset.db, element.dataset.table, true, element.dataset.schema);
      break;
    case "switch-tab":
      state.activeTabId = id;
      renderWorkspace();
      break;
    case "close-tab": {
      tableRequests.get(id)?.abort();
      tableRequests.delete(id);
      onCloseTab(id);
      state.tabs = state.tabs.filter((tab) => tab.id !== id);
      if (state.activeTabId === id) state.activeTabId = state.tabs.at(-1)?.id;
      renderAppShell();
      break;
    }
    case "close-all-tabs":
      for (const request of tableRequests.values()) request.abort();
      tableRequests.clear();
      for (const tab of state.tabs) onCloseTab(tab.id);
      state.tabs = [];
      state.activeTabId = null;
      renderAppShell();
      break;
    case "new-query":
      openQueryTab();
      break;
    case "query-new-tab":
      openQueryTab(activeTab.sql);
      break;
    case "view":
      activeTab.view = element.dataset.view;
      syncTableLocation();
      findElements(".subtabs [data-view]").forEach((buttonElement) => buttonElement.classList.toggle("active", buttonElement.dataset.view === activeTab.view));
      renderData();
      break;
    default:
      return false;
  }
  return true;
}

// -----------------------------------------------------------------------------
// Initialization
// -----------------------------------------------------------------------------
export function initializeWorkspace(callbacks) {
  onRunQuery = callbacks.onRunQuery;
  onCloseTab = callbacks.onCloseTab;
  navigationHash = location.hash;
  window.addEventListener("popstate", navigateTableLocation);
  window.addEventListener("hashchange", navigateTableLocation);
  document.addEventListener("keydown", event => {
    const tabElement = event.target.closest(".workspace-tab");
    if (tabElement && event.target === tabElement && ["Enter", " "].includes(event.key)) {
      event.preventDefault();
      state.activeTabId = tabElement.dataset.id;
      renderWorkspace();
    }
  });
}
