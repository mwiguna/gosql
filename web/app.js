import "./style.css";
import { getActiveTab, getConnectionById, state } from "./state.js";
import { findElement, button, closeDialog, escapeHtml, icon, initializeDialogs, showDialog } from "./ui.js";
import { initializeSession, logout, handleSessionExpired } from "./features/session.js";
import { initializeApi } from "./api.js";
import {
  handleWorkspaceAction, initializeWorkspace, openTable, renderAppShell,
  resetWorkspace, restoreTableLocation, startWorkspace, tableLocation, loadTablePage, retryTableCount
} from "./features/workspace.js";
import {
  handleConnectionsAction, initializeConnections, connectToDatabase, getConnectionCatalog
} from "./features/connections.js";
import { handleEditorAction, loadEditorLibrary } from "./features/editor.js";
import { handleQueriesAction, runQuery, stopQueryForTab } from "./features/queries.js";
import { handleSchemaAction } from "./features/schema.js";
import { handleUsersAction } from "./features/users.js";
import { handleGridAction, loadGridLibrary, initializeGrid } from "./features/grid.js";

// -----------------------------------------------------------------------------
// User actions
// -----------------------------------------------------------------------------
function handleAppAction(action) {
  switch (action) {
    case "close-dialog":
      closeDialog();
      break;
    case "sidebar":
      findElement(".sidebar").classList.toggle("open");
      break;
    case "logout":
      logout();
      break;
    case "settings":
      showDialog("Workspace settings", `<div class="stack">
        <div class="row between"><span>Appearance</span><span class="pill">LIGHT</span></div>
        <div class="row between"><span>Language</span><span class="muted">English</span></div>
        <div class="row between"><span>Row density</span><span class="muted">Compact</span></div>
        <div class="rule"></div>
        <p class="hint">Profiles, saved queries, and history are private to your account. The current table is stored in the URL and reopened after refresh or login. Query results and editor text are not restored. Remember me keeps only your account signed in for up to 30 days.</p>
        <div class="notice">${icon("info")}
          <span>PostgreSQL, MySQL, and MariaDB tables with a supported primary key can be edited or deleted when the database account has permission. Imports and exports are not available yet.</span>
        </div>
      </div>`, button("close-dialog", "Done"));
      break;
    case "help":
      showDialog("Explore GoSQL", `<div class="stack">
        <p>Create a PostgreSQL, MySQL, or MariaDB connection profile to browse your database.</p>
        <div class="notice">${icon("info")}
          <span>Open a table or view to read its data and columns. Use Previous and Next to load another page.</span>
        </div>
        <div class="row between"><span>Find in SQL editor</span><span class="key">Ctrl / ⌘ + F</span></div>
        <div class="row between"><span>Autocomplete</span><span class="key">Ctrl + Space</span></div>
        <div class="row between"><span>Cell value / copy</span><span class="muted">Right-click</span></div>
        <div class="row between"><span>Edit cell</span><span class="muted">Double-click · Enter to save</span></div>
        <div class="row between"><span>New line in text cell</span><span class="muted">Alt+Enter</span></div>
        <p class="hint">Double-click a writable table cell to edit it. Views stay read-only. Query execution is not available yet.</p>
      </div>`, button("close-dialog", "Got it", "", "primary"));
      break;
    default:
      return false;
  }
  return true;
}

// -----------------------------------------------------------------------------
// Navigation context menu
// -----------------------------------------------------------------------------
function closeNavigationMenu() {
  findElement("#navigation-menu")?.remove();
}

function openNavigationMenu(element, x, y) {
  const action = element.dataset.action;
  const id = element.dataset.id;
  const database = element.dataset.db;
  const schema = element.dataset.schema;
  const table = element.dataset.table;
  const menu = document.createElement("div");
  menu.id = "navigation-menu";
  menu.className = "context-menu";
  const add = (name, label, attributes = "") => menu.insertAdjacentHTML("beforeend", button(name, label, "", "", attributes));
  const connection = getConnectionById(id);
  let destination;
  if (["open-table", "open-view"].includes(action)) destination = { connectionId: id, db: database, schemaName: schema, table, view: "Data" };
  else if (action === "view" && getActiveTab()) destination = { ...getActiveTab(), view: element.dataset.view };
  else if (action === "switch-tab") destination = state.tabs.find(tab => tab.id === id && !tab.queryTab);
  if (destination) {
    const link = document.createElement("a");
    link.href = location.pathname + location.search + tableLocation(destination);
    link.target = "_blank";
    link.rel = "noopener";
    link.textContent = "Open in New Tab";
    menu.append(link);
  }
  if (action === "open-table") {
    const attributes = `data-id="${escapeHtml(id)}" data-db="${escapeHtml(database)}" data-schema="${escapeHtml(schema)}" data-table="${escapeHtml(table)}"`;
    const protectedMySQL = ["MySQL", "MariaDB"].includes(connection?.engine) && ["mysql", "information_schema", "performance_schema", "sys"].includes(database.toLowerCase());
    const virtualSQLite = connection?.engine === "SQLite" && getConnectionCatalog(id, database)?.virtualTables?.includes(table);
    if (!protectedMySQL && !virtualSQLite) add("rename-table", ["MySQL", "MariaDB"].includes(connection?.engine) ? "Edit Table" : "Rename Table", attributes);
    if (connection?.engine !== "SQLite" || connection.location === "Server upload") add("database-export", connection?.engine === "SQLite" ? "Download SQLite File" : "Export Table", attributes);
    if (connection?.engine !== "SQLite") add("database-import", "Import File", attributes);
    if (!protectedMySQL && !virtualSQLite) add("delete-table", "Delete Table", attributes);
  } else if (action === "database") {
    const attributes = `data-id="${escapeHtml(id)}" data-db="${escapeHtml(database)}"`;
    add("saved", "Saved Queries", attributes);
    if (connection?.engine !== "SQLite" || connection.location === "Server upload") add("database-export", connection?.engine === "SQLite" ? "Download SQLite File" : "Export Database", attributes);
    if (connection?.engine !== "SQLite") add("database-import", "Import File", attributes);
    if (["PostgreSQL", "MySQL", "MariaDB", "SQLite"].includes(connection?.engine)) {
      if (connection.engine === "SQLite") add("create-table", "Create Table", attributes);
      if (["MySQL", "MariaDB"].includes(connection.engine) && !["mysql", "information_schema", "performance_schema", "sys"].includes(database.toLowerCase())) {
        add("create-table", "Create Table", attributes);
        add("delete-database", "Delete Database", attributes);
      }
    }
    if (connection?.engine === "PostgreSQL") {
      add("rename-database", "Rename Database", attributes);
      add("delete-database", "Delete Database", attributes);
    }
  } else if (action === "schemas" && connection?.engine === "PostgreSQL") {
    add("create-schema", "Create Schema", `data-id="${escapeHtml(id)}" data-db="${escapeHtml(database)}"`);
  } else if (action === "database-schema" && connection?.engine === "PostgreSQL") {
    const attributes = `data-id="${escapeHtml(id)}" data-db="${escapeHtml(database)}" data-schema="${escapeHtml(schema)}"`;
    add("create-table", "Create Table", attributes);
    add("rename-schema", "Rename Schema", attributes);
    add("delete-schema", "Delete Schema", attributes);
  } else if (action === "toggle-connection") {
    if (!connection) return false;
    const attributes = `data-id="${escapeHtml(id)}"`;
    add("edit-connection", "Edit Connection", attributes);
    if (["PostgreSQL", "MySQL", "MariaDB"].includes(connection.engine)) add("create-database", "Create Database", attributes);
    add("disconnect", "Disconnect", `${attributes} ${state.connectedConnectionIds.has(id) ? "" : "disabled"}`);
    add("delete-connection", "Delete Connection", attributes);
  }
  if (!menu.hasChildNodes()) return false;
  closeNavigationMenu();
  document.body.append(menu);
  menu.style.left = Math.max(0, Math.min(x, innerWidth - menu.offsetWidth)) + "px";
  menu.style.top = Math.max(0, Math.min(y, innerHeight - menu.offsetHeight)) + "px";
  menu.querySelector("a, button")?.focus();
  return true;
}

// -----------------------------------------------------------------------------
// Application events
// -----------------------------------------------------------------------------
function initializeAppEvents() {
  document.addEventListener("contextmenu", (domEvent) => {
    closeNavigationMenu();
    const element = domEvent.target.closest("[data-action]");
    if (!state.currentUser || !element) return;
    if (openNavigationMenu(element, domEvent.clientX, domEvent.clientY)) domEvent.preventDefault();
  });
  document.addEventListener("click", closeNavigationMenu);
  document.addEventListener("keydown", (domEvent) => {
    if (domEvent.key === "Escape") closeNavigationMenu();
  });
  window.addEventListener("blur", closeNavigationMenu);
  window.addEventListener("resize", closeNavigationMenu);

  document.addEventListener("click", async event => {
    const element = event.target.closest("[data-action]");
    if (!element || element.disabled) return;
    const action = element.dataset.action;
    if (handleAppAction(action, element)) return;
    if (handleConnectionsAction(action, element)) return;
    if (handleWorkspaceAction(action, element)) return;
    if (handleEditorAction(action, element, runQuery)) return;
    if (handleSchemaAction(action, element, loadTablePage)) return;
    if (handleQueriesAction(action, element)) return;
    if (handleUsersAction(action, element)) return;
    await handleGridAction(action, element);
  });
}

// -----------------------------------------------------------------------------
// Startup
// -----------------------------------------------------------------------------
async function initializeApp() {
  initializeDialogs();
  initializeApi({ onUnauthorized: handleSessionExpired });
  initializeConnections({ onWorkspaceChange: renderAppShell, onOpenTable: openTable, onRefreshTable: loadTablePage });
  initializeWorkspace({ onRunQuery: runQuery, onCloseTab: stopQueryForTab });
  initializeGrid({ onLoadTablePage: loadTablePage, onRetryTableCount: retryTableCount, onReconnectTable: tab => connectToDatabase(tab.connectionId, () => loadTablePage(tab, tab.page, true)) });
  initializeAppEvents();
  await initializeSession({
    onWorkspaceReady: () => Promise.all([loadEditorLibrary(), loadGridLibrary()]),
    onLogin: startWorkspace,
    onRestoreWorkspace: restoreTableLocation,
    onLogout: () => {
      stopQueryForTab();
      resetWorkspace();
    }
  });
}

initializeApp();
