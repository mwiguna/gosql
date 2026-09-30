import { getActiveTab, getConnectionById, state, engines } from "../state.js";
import {
  findElement, findElements, button, closeDialog, confirmAction, engineIcon, escapeHtml, icon,
  showDialog, toast
} from "../ui.js";
import { setConnected } from "./session.js";
import { apiRequest, connectionFromProfile, uploadSQLite, downloadSQLite } from "../api.js";
import { runRecordedSQL } from "../storage.js";
import { columnTypes, columnTypeSQL, lengthTypes, numericTypes, mysqlTypeFields, mysqlTypeSQL, mysqlUnsignedField, updateMySQLTypeFields } from "./column_types.js";

// -----------------------------------------------------------------------------
// Sidebar and connection form state
// -----------------------------------------------------------------------------
let collapsedConnections = new Set();
let expanded = new Set();
let searchConnections = "";
const treeOpen = new Map();
const treeChildren = new Map();
let connectionDraft = null;
let sqliteSelectedFile = null;
let sqlitePickerToken = null;
let connectionPane = "General";
const databaseCatalogs = new Map();
const quoteSQL = name => '"' + name.replaceAll('"', '""') + '"';
const literalSQL = value => "E'" + value.replaceAll("\\", "\\\\").replaceAll("'", "''") + "'";
const mysqlQuote = name => "`" + name.replaceAll("`", "``") + "`";
const mysqlFamily = engine => engine === "MySQL" || engine === "MariaDB";
const mysqlEngineOptions = engine => `<option value="InnoDB">InnoDB</option><option value="MyISAM">MyISAM</option>${engine === "MariaDB" ? '<option value="Aria">Aria</option>' : ""}`;
const databaseQuote = (id, name) => mysqlFamily(getConnectionById(id)?.engine) ? mysqlQuote(name) : quoteSQL(name);
const qualifiedTable = (id, db, schema, table) => mysqlFamily(getConnectionById(id)?.engine)
  ? `${mysqlQuote(db)}.${mysqlQuote(table)}` : getConnectionById(id)?.engine === "SQLite"
    ? `main.${quoteSQL(table)}` : `${quoteSQL(schema)}.${quoteSQL(table)}`;
const historyLocation = (connectionId, db, schemaName = "") => ({ connectionId, db, schemaName });
export function getConnectionCatalog(id, database) { return databaseCatalogs.get(id)?.catalogs.get(database); }

export async function loadDatabaseCatalog(id, database) {
  const connection = getConnectionById(id);
  const entry = databaseCatalogs.get(id);
  if (!connection || !entry || !entry.databases.includes(database)) throw new Error("This database is not available for this connection.");
  if (entry.catalogs.has(database)) return entry.catalogs.get(database);
  if (entry.pending.has(database)) return entry.pending.get(database);
  const userId = state.currentUser?.id;
  const pending = apiRequest("/connections/" + encodeURIComponent(id) + "/catalog?database=" + encodeURIComponent(database))
    .then(catalog => {
      if (state.currentUser?.id !== userId || getConnectionById(id) !== connection || databaseCatalogs.get(id) !== entry || !entry.databases.includes(database)) return null;
      entry.catalogs.set(database, catalog);
      return catalog;
    }).catch(error => {
      if (error.code === "connection_required" && databaseCatalogs.get(id) === entry) {
        databaseCatalogs.delete(id);
        state.connectedConnectionIds.delete(id);
        renderTree();
      }
      throw error;
    }).finally(() => entry.pending.delete(database));
  entry.pending.set(database, pending);
  return pending;
}

export async function refreshDatabaseCatalog(id, database) {
  const entry = databaseCatalogs.get(id);
  if (!entry || !entry.databases.includes(database)) return;
  const catalog = await apiRequest("/connections/" + encodeURIComponent(id) + "/catalog?database=" + encodeURIComponent(database));
  if (databaseCatalogs.get(id) !== entry) return;
  entry.catalogs.set(database, catalog);
  renderTree();
  return catalog;
}

let onWorkspaceChange;
let onOpenTable;
let onRefreshTable;

// -----------------------------------------------------------------------------
// Sidebar controls
// -----------------------------------------------------------------------------
export function resetConnectionTree(clearCatalogs = false) {
  collapsedConnections = new Set(state.connectedConnectionIds);
  expanded.clear();
  treeOpen.clear();
  searchConnections = "";
  if (clearCatalogs) databaseCatalogs.clear();
}

export function revealTreeDestination(connection, destination, isView) {
  collapsedConnections.delete(connection.id);
  treeOpen.set("engine:" + connection.engine, true);
  expanded.add(connection.id + ":" + destination.db);
  if (!destination.table) return;
  if (destination.schemaName) treeOpen.set(connection.id + ":" + destination.db + ":" + destination.schemaName, true);
  const kind = isView
    ? "Views"
    : "Tables";
  treeOpen.set([connection.id, destination.db, destination.schemaName, kind].join(":"), true);
}

export function getConnectionSearch() { return searchConnections; }

export function bindConnectionSearch() {
  const input = findElement("#connection-search");
  input.value = searchConnections;
  // Kolom pencarian tetap readonly saat tidak dipakai agar pengelola sandi browser tidak mengisinya sebagai username.
  input.onfocus = () => { input.readOnly = false; };
  input.onblur = () => { input.readOnly = true; };
  input.oninput = event => {
    searchConnections = event.target.value;
    renderTree();
  };
}

// -----------------------------------------------------------------------------
// Sidebar rendering
// -----------------------------------------------------------------------------
export function renderTree() {
  const target = findElement("#tree");
  if (!target) return;
  const scrollTop = target.scrollTop;
  treeChildren.clear();
  const search = searchConnections.toLowerCase();
  target.innerHTML = engines.filter(engine => state.connections.some(connection => connection.engine === engine)).map((engine) => {
    const matching = state.connections.filter((connection) =>
      connection.engine === engine && connection.name.toLowerCase().includes(search));
    const count = state.connections.filter((connection) => connection.engine === engine).length;
    const opened = treeOpen.get("engine:" + engine) ?? false;
    return `<details class="engine" data-tree-key="engine:${engine}" ${opened ? "open" : ""}>
      <summary class="engine-title">
        ${icon("chevron", "chevron")}
        <span class="engine-symbol">${engineIcon(engine)}</span>${engine}
        <span class="spacer"></span><small>${count}</small>
      </summary>
      ${matching.map(connectionTree).join("")}
    </details>`;
  }).join("");
  target.scrollTop = scrollTop;
  findElements("details[data-tree-key]", target).forEach((detail) => detail.addEventListener("toggle", () => {
    if (!detail.isConnected) return;
    treeOpen.set(detail.dataset.treeKey, detail.open);
    const children = findElement(".tree-indent", detail);
    if (detail.open && children && !children.hasChildNodes() && treeChildren.has(detail.dataset.treeKey)) {
      children.innerHTML = treeChildren.get(detail.dataset.treeKey)();
    }
  }));
}

function connectionTree(connection) {
  const opened = state.connectedConnectionIds.has(connection.id) && !collapsedConnections.has(connection.id)
    && (!["PostgreSQL", "MySQL", "MariaDB"].includes(connection.engine) || databaseCatalogs.has(connection.id));
  const databases = opened
    ? `<div class="tree-indent">${connection.databases.map((db) => databaseTree(connection, db)).join("")}</div>`
    : "";
  return `<div class="tree-row">
    <button data-action="toggle-connection" data-id="${connection.id}" aria-expanded="${opened}">
      ${icon(opened ? "down" : "chevron", "chevron")}
      ${icon(connection.engine === "SQLite" ? "file" : "monitor")}
      <span>${escapeHtml(connection.name)}</span>
      ${state.connectedConnectionIds.has(connection.id) && (!["PostgreSQL", "MySQL", "MariaDB"].includes(connection.engine) || databaseCatalogs.has(connection.id)) ? '<i class="connected-dot"></i>' : ""}
    </button>
  </div>${databases}`;
}

function databaseTree(connection, database) {
  const key = connection.id + ":" + database;
  let content = "";
  if (expanded.has(key)) {
    if (connection.engine === "PostgreSQL") {
      const schemas = (getConnectionCatalog(connection.id, database)?.schemas || []).map(({ name: schemaName, tables, views }) => {
        const schemaKey = key + ":" + schemaName;
        return `<details class="tree-group" data-tree-key="${escapeHtml(schemaKey)}"
            ${treeOpen.get(schemaKey) ?? false ? "open" : ""}>
          <summary data-action="database-schema" data-id="${escapeHtml(connection.id)}" data-db="${escapeHtml(database)}" data-schema="${escapeHtml(schemaName)}">${icon("chevron", "chevron")}${icon("branch")}${escapeHtml(schemaName)}</summary>
          <div class="tree-indent">${objectTree(connection, database, schemaName, { Tables: tables, Views: views })}</div>
        </details>`;
      }).join("");
      content = `<div class="tree-label" data-action="schemas" data-id="${escapeHtml(connection.id)}" data-db="${escapeHtml(database)}">${icon("branch")}Schemas</div>${schemas || '<div class="hint">No accessible schemas.</div>'}`;
    } else {
      const catalog = getConnectionCatalog(connection.id, database);
      content = mysqlFamily(connection.engine) || connection.engine === "SQLite"
        ? objectTree(connection, database, "", { Tables: catalog?.tables || [], Views: catalog?.views || [] })
        : '<div class="hint">Database access is not available for this engine yet.</div>';
    }
  }
  return `<div class="tree-row">
    <button data-action="database" data-id="${connection.id}" data-db="${escapeHtml(database)}">
      ${icon(expanded.has(key) ? "down" : "chevron", "chevron")}
      ${icon("database")}<span>${escapeHtml(database)}</span>
    </button>
  </div>${content ? `<div class="tree-indent">${content}</div>` : ""}`;
}

function objectTree(connection, database, schemaName, groups) {
  return Object.entries(groups).map(([kind, names]) => {
    const key = [connection.id, database, schemaName, kind].join(":");
    const opened = treeOpen.get(key) ?? false;
    const children = () => names.map((name) => {
      const isTable = kind === "Tables";
      const isView = kind === "Views" || kind === "Materialized Views";
      const selected = getActiveTab()?.connectionId === connection.id
        && getActiveTab()?.db === database
        && getActiveTab()?.schemaName === schemaName
        && getActiveTab()?.table === name;
      const action = isTable ? "open-table" : isView ? "open-view" : "inspect-object";
      const virtual = isTable && connection.engine === "SQLite" && getConnectionCatalog(connection.id, database)?.virtualTables?.includes(name);
      const symbol = virtual ? "file" : isTable ? "table" : isView ? "eye"
        : kind === "Functions" ? "terminal" : kind === "Sequences" ? "sort" : "file";
      return `<div class="tree-row ${selected ? "active" : ""}">
        <button data-action="${action}" data-id="${connection.id}" data-db="${escapeHtml(database)}"
          data-schema="${escapeHtml(schemaName)}" data-table="${escapeHtml(name)}" data-kind="${kind}">
          ${icon(symbol)}<span>${escapeHtml(name)}</span>${virtual ? '<small>Virtual</small>' : ""}
        </button>
      </div>`;
    }).join("");
    treeChildren.set(key, children);
    return `<details class="tree-group" data-tree-key="${escapeHtml(key)}" ${opened ? "open" : ""}>
      <summary>${icon("chevron", "chevron")}${icon("folder")}${kind}
        <span class="spacer"></span><small>${names.length}</small>
      </summary>
      <div class="tree-indent">${opened ? children() : ""}</div>
    </details>`;
  }).join("");
}

export function syncTreeSelection() {
  const tab = getActiveTab();
  findElements("#tree .tree-row.active").forEach((row) => row.classList.remove("active"));
  if (!tab) return;
  const buttons = findElements('#tree [data-action="open-table"],#tree [data-action="open-view"]');
  const selected = buttons.find((element) => element.dataset.id === tab.connectionId
    && element.dataset.db === tab.db
    && element.dataset.schema === tab.schemaName
    && element.dataset.table === tab.table);
  selected?.parentElement.classList.add("active");
}

// -----------------------------------------------------------------------------
// Connection profiles
// -----------------------------------------------------------------------------
function openConnectionForm(id, engine = "PostgreSQL") {
  sqliteSelectedFile = null;
  sqlitePickerToken = null;
  connectionDraft = id ? structuredClone(getConnectionById(id)) : { engine, name: "", host: "localhost", port: mysqlFamily(engine) ? "3306" : "5432", username: mysqlFamily(engine) ? "root" : "postgres", database: engine === "PostgreSQL" ? "postgres" : "", ssl: "Prefer", ssh: false, location: state.nativeFilePickerAllowed ? "Native file" : "Local file" };
  if (mysqlFamily(connectionDraft.engine)) connectionDraft.ssh = false;
  connectionPane = "General";
  renderConnectionForm();
}

function readConnectionForm() {
  const form = findElement("#connection-form");
  if (!form) return;
  for (const [key, value] of new FormData(form)) if (!["password", "sshPassword", "passphrase", "privateKey", "filePicker"].includes(key)) connectionDraft[key] = value;
  const check = findElement("#ssh-enabled");
  if (check) connectionDraft.ssh = check.checked;
}

async function chooseSQLiteFile(buttonElement) {
  readConnectionForm();
  const draft = connectionDraft, userId = state.currentUser?.id;
  buttonElement.disabled = true;
  try {
    const selection = await apiRequest("/sqlite/pick-file", { method: "POST", body: {} });
    if (selection.canceled || connectionDraft !== draft || state.currentUser?.id !== userId || !findElement("#dialog")?.open || draft.location === "Server upload") return;
    draft.location = "Native file";
    draft.file = selection.file;
    sqlitePickerToken = selection.token;
    sqliteSelectedFile = null;
    renderConnectionForm();
    toast("Original SQLite file selected. Edits will change this file directly.");
  } catch (error) { toast(error.message); }
  finally { buttonElement.disabled = false; }
}

async function testConnection(buttonElement) {
  readConnectionForm();
  const status = findElement("#connection-test");
  if (connectionDraft.engine === "SQLite") {
    status.textContent = "Testing SQLite file…";
    buttonElement.disabled = true;
    try {
      if (connectionDraft.location === "Server upload") {
        if (sqliteSelectedFile) await uploadSQLite("/sqlite/test", sqliteSelectedFile);
        else if (connectionDraft.id) await apiRequest("/connections/" + encodeURIComponent(connectionDraft.id) + "/catalog", { method: "POST", body: { password: "" } });
        else throw new Error("Choose a SQLite file to upload.");
      } else if (connectionDraft.location === "Native file" && connectionDraft.id && !sqlitePickerToken) {
        await apiRequest("/connections/" + encodeURIComponent(connectionDraft.id) + "/catalog", { method: "POST", body: { password: "" } });
      } else await apiRequest("/sqlite/test", { method: "POST", body: { engine: "SQLite", name: connectionDraft.name || "Connection test", file: connectionDraft.file, location: connectionDraft.location, pickerToken: sqlitePickerToken || "" } });
      if (findElement("#connection-test") === status) status.textContent = "Connection successful.";
    } catch (error) { if (findElement("#connection-test") === status) status.textContent = error.message; }
    finally { buttonElement.disabled = false; }
    return;
  }
  const { id, ownerId, databases, ...fields } = connectionDraft;
  fields.name ||= "Connection test";
  status.textContent = "Testing connection…";
  buttonElement.disabled = true;
  try {
    await apiRequest(connectionDraft.engine === "MariaDB" ? "/mariadb/test" : connectionDraft.engine === "MySQL" ? "/mysql/test" : "/postgres/test", { method: "POST", body: { ...fields, password: findElement("#connection-password")?.value || "" } });
    if (findElement("#connection-test") === status) status.textContent = "Connection successful.";
  } catch (error) {
    if (findElement("#connection-test") === status) status.textContent = error.message;
  } finally { buttonElement.disabled = false; }
}

function renderConnectionForm() {
  const connection = connectionDraft;
  const uploadCopy = connection.location === "Server upload";
  const engineButtons = engines.map((engine) => {
    const symbol = engine === "PostgreSQL" ? "postgres" : mysqlFamily(engine) ? "mysql" : "sqlite";
    return button("pick-engine", engine, symbol,
      connection.engine === engine ? "active" : "", `data-engine="${engine}"`);
  }).join("");
  const panes = connection.engine === "SQLite" ? ["General"]
    : mysqlFamily(connection.engine) ? ["General", "SSL / TLS"]
    : ["General", "SSL / TLS", "SSH Tunnel"];
  const paneButtons = panes.map((pane) => button("connection-pane", pane, "",
    pane === connectionPane ? "active" : "", `data-pane="${pane}"`)).join("");

  let fields;
  if (connectionPane === "General") {
    const nameField = `<label class="full">Connection name
      <input name="name" placeholder="e.g. Local development" required
        value="${escapeHtml(connection.name)}" maxlength="80">
    </label>`;
    if (connection.engine === "SQLite") {
      const fileNotice = uploadCopy
        ? "This file will be copied to GoSQL. Changes affect only the server copy. If the source uses WAL, export a consistent snapshot before uploading its .db file."
        : "Edits to data or structure will change this original SQLite file directly.";
      const displayFile = connection.file?.split(/[\\/]/).at(-1) || "No file selected";
      fields = `${nameField}
        <label class="full">File location<select name="location" id="sqlite-location">
          ${state.nativeFilePickerAllowed || connection.location === "Native file" ? `<option value="Native file" ${connection.location === "Native file" ? "selected" : ""}>Choose File</option>` : ""}
          <option value="Local file" ${connection.location === "Local file" ? "selected" : ""}>Enter File Path</option>
          <option value="Server upload" ${uploadCopy ? "selected" : ""}>Upload Copy</option>
        </select></label>
        ${uploadCopy ? `<label class="full">Upload SQLite database
          <input name="filePicker" id="sqlite-file" type="file" accept=".db,.sqlite,.sqlite3" ${connection.id ? "" : "required"}>
        </label><div class="hint full" id="sqlite-file-name">${escapeHtml(connection.file || "No file selected")}</div>`
          : connection.location === "Local file" ? `<label class="full">Absolute path on GoSQL server
          <input name="file" value="${escapeHtml(connection.file || "")}" placeholder="/path/to/database.sqlite" required>
        </label><p class="hint full">The file must already exist and be readable and writable by the GoSQL server process.</p>`
          : state.nativeFilePickerAllowed ? `<div class="full row">${button("choose-sqlite-file", "Choose SQLite file", "file")}<span id="sqlite-file-name" class="hint">${escapeHtml(displayFile)}</span></div><p class="hint full">The file chooser opens on the computer running GoSQL.</p>`
            : `<p class="hint full">Current server file: ${escapeHtml(connection.file || "No file selected")}. Select “Enter File Path” to change it.</p>`}
        <div class="notice warning full" ${connection.location === "Native file" && !connection.file ? "hidden" : ""}>
          ${icon(uploadCopy ? "upload" : "file")}<span>${fileNotice}</span>
        </div>`;
    } else {
      const databaseField = connection.engine === "PostgreSQL"
        ? `<label>Initial Database<input name="database" value="${escapeHtml(connection.database || "")}" placeholder="postgres" required></label>`
        : "";
      fields = `${nameField}
        <label>Host<input name="host" value="${escapeHtml(connection.host)}" required placeholder="localhost"></label>
        <label>Port<input name="port" type="number" min="1" max="65535" value="${escapeHtml(connection.port)}" required></label>
        <label>Username<input name="username" value="${escapeHtml(connection.username)}" required autocomplete="off"></label>
        ${databaseField}
        <div class="notice full">${icon("lock")}
          <span>You will be asked for your database password when connecting. Passwords are never saved to this profile.</span>
        </div>`;
    }
  } else if (connectionPane === "SSL / TLS") {
    const modes = ["Disable", "Prefer", "Require", "Verify CA", "Verify Full"];
    fields = `<label class="full">SSL mode<select name="ssl">
        ${modes.map((mode) => `<option ${mode === connection.ssl ? "selected" : ""}>${mode}</option>`).join("")}
      </select></label>
      <p class="hint full">Custom certificate upload is not available yet.</p>`;
  } else {
    const credential = connection.sshAuth === "Private key"
      ? "Your private key and passphrase" : "Your SSH password";
    fields = `<label class="check-label full">
        <input id="ssh-enabled" type="checkbox" ${connection.ssh ? "checked" : ""}>Connect through SSH tunnel
      </label>
      <label>SSH host<input name="sshHost" value="${escapeHtml(connection.sshHost || "")}" placeholder="bastion.example.com"></label>
      <label>SSH port<input name="sshPort" type="number" value="${escapeHtml(connection.sshPort || "22")}" min="1" max="65535"></label>
      <label>SSH username<input name="sshUser" value="${escapeHtml(connection.sshUser || "")}" placeholder="ubuntu"></label>
      <label>Authentication<select name="sshAuth" id="ssh-auth">
        <option ${connection.sshAuth === "Private key" ? "" : "selected"}>Password</option>
        <option ${connection.sshAuth === "Private key" ? "selected" : ""}>Private key</option>
      </select></label>
      <div class="notice full">${icon("key")}
        <span>${credential} will be requested when connecting and kept only for the session.</span>
      </div>`;
  }

  const body = `<div class="engine-picker">${engineButtons}</div>
    <div class="form-tabs">${paneButtons}</div>
    <form id="connection-form" autocomplete="off"><div class="form-grid">${fields}</div>
      ${["PostgreSQL", "MySQL", "MariaDB"].includes(connection.engine) ? '<label>Database password for test<input id="connection-password" type="password" autocomplete="new-password"></label>' : ""}
    </form>
    <div id="connection-test" class="hint" style="margin-top:15px"></div>`;
  const footer = button("test-connection", "Test Connection", "refresh")
    + '<span class="spacer"></span>'
    + button("close-dialog", "Cancel")
    + button("save-connection", "Save Connection", "", "primary");
  showDialog(connection.id ? "Edit connection" : "New connection", body, footer);

  findElement("#sqlite-location")?.addEventListener("change", () => {
    readConnectionForm();
    sqliteSelectedFile = null;
    sqlitePickerToken = null;
    connectionDraft.file = "";
    renderConnectionForm();
  });
  findElement("#ssh-auth")?.addEventListener("change", () => {
    readConnectionForm();
    renderConnectionForm();
  });
  findElement("#sqlite-file")?.addEventListener("change", (event) => {
    const file = event.target.files[0];
    if (file) {
      sqliteSelectedFile = file;
      connectionDraft.file = file.name;
      findElement("#sqlite-file-name").textContent = file.name;
    }
  });
}

async function saveConnection() {
  const form = findElement("#connection-form");
  if (!form.reportValidity()) return;
  readConnectionForm();
  const currentConnection = connectionDraft;
  if (!currentConnection.name.trim()) {
    connectionPane = "General";
    renderConnectionForm();
    findElement('#connection-form [name="name"]').reportValidity();
    return;
  }
  if (currentConnection.engine === "PostgreSQL" && !currentConnection.database?.trim()) {
    connectionPane = "General";
    renderConnectionForm();
    findElement('#connection-form [name="database"]').reportValidity();
    return;
  }
  if (mysqlFamily(currentConnection.engine)) delete currentConnection.database;
  if (currentConnection.engine === "SQLite" && !currentConnection.file) {
    toast(currentConnection.location === "Server upload" ? "Choose a SQLite file to upload." : currentConnection.location === "Local file" ? "Enter an absolute SQLite file path on the server." : "Choose a SQLite file on this computer.");
    return;
  }
  if (currentConnection.ssh && (!currentConnection.sshHost || !currentConnection.sshUser)) {
    connectionPane = "SSH Tunnel";
    renderConnectionForm();
    toast("Enter an SSH host and username.");
    return;
  }
  const userId = state.currentUser.id;
  const submit = findElement('[data-action="save-connection"]');
  submit.disabled = true;
  try {
    const { id, ownerId, databases, ...fields } = currentConnection;
    if (fields.engine === "SQLite") {
      for (const key of ["database", "host", "port", "username", "ssl", "sshHost", "sshPort", "sshUser", "sshAuth"]) delete fields[key];
      fields.ssh = false;
      if (fields.location === "Native file") fields.pickerToken = sqlitePickerToken || "";
      else delete fields.pickerToken;
    } else { delete fields.file; delete fields.location; }
    if (fields.ssh) {
      fields.sshPort ||= "22";
      fields.sshAuth ||= "Password";
    }
    const saved = await apiRequest(id ? "/connections/" + encodeURIComponent(id) : "/connections", {
      method: id ? "PATCH" : "POST", body: fields
    });
    if (saved.engine === "SQLite" && saved.location === "Server upload" && sqliteSelectedFile) {
      try { await uploadSQLite("/connections/" + encodeURIComponent(saved.id) + "/sqlite-file", sqliteSelectedFile); }
      catch (error) {
        if (!id) await apiRequest("/connections/" + encodeURIComponent(saved.id), { method: "DELETE" });
        else {
          const { id: oldId, ownerId: oldOwner, databases: oldDatabases, ...oldFields } = getConnectionById(id);
          await apiRequest("/connections/" + encodeURIComponent(id), { method: "PATCH", body: oldFields });
        }
        throw error;
      }
    }
    if (state.currentUser?.id !== userId) return;
    const profile = connectionFromProfile(saved);
    if (!id) {
      state.connections.push(profile);
      treeOpen.set("engine:" + profile.engine, true);
      searchConnections = "";
    } else {
      const index = state.connections.findIndex(item => item.id === id);
      state.connections[index] = profile;
      state.tabs = state.tabs.filter(tab => tab.connectionId !== id);
      if (!getActiveTab()) state.activeTabId = state.tabs[0]?.id;
      setConnected(id, false);
      databaseCatalogs.delete(id);
    }
    closeDialog();
    sqliteSelectedFile = null;
    sqlitePickerToken = null;
    onWorkspaceChange();
    toast("Connection profile saved.");
  } catch (error) { toast(error.message); }
  finally { submit.disabled = false; }
}

function removeConnection(id) {
  const currentConnection = getConnectionById(id);
  let fileNotice = "";
  if (currentConnection.engine === "SQLite") {
    fileNotice = currentConnection.location === "Server upload"
      ? " The uploaded server copy will also be deleted."
      : " The original database file will not be deleted.";
  }
  const message = `${escapeHtml(currentConnection.name)} will be removed.${fileNotice} Any open tabs for this connection will be closed.`;
  confirmAction("Delete connection?", message, "Delete Connection", () => {
    deleteConnectionProfile(id).catch(error => toast(error.message));
  });
}

async function deleteConnectionProfile(id) {
  const userId = state.currentUser.id;
  await apiRequest("/connections/" + encodeURIComponent(id), { method: "DELETE" });
  if (state.currentUser?.id !== userId) return;
  state.tabs = state.tabs.filter((activeTab) => activeTab.connectionId !== id);
  if (!getActiveTab()) state.activeTabId = state.tabs[0]?.id;
  state.connections = state.connections.filter((connection) => connection.id !== id);
  setConnected(id, false);
  databaseCatalogs.delete(id);
  onWorkspaceChange();
  toast("Connection profile deleted.");
}

// -----------------------------------------------------------------------------
// Connect and database actions
// -----------------------------------------------------------------------------
export function connectToDatabase(id, callback) {
  const connection = getConnectionById(id);
  if (!connection) return;
  if (!["PostgreSQL", "MySQL", "MariaDB", "SQLite"].includes(connection.engine)) {
    toast("Database access is not available for this engine.");
    return;
  }
  const reveal = () => {
    collapsedConnections.delete(id);
    treeOpen.set("engine:" + connection.engine, true);
    if (!callback && connection.database) expanded.add(id + ":" + connection.database);
    renderTree();
    callback?.();
  };
  if (state.connectedConnectionIds.has(id) && databaseCatalogs.has(id)) {
    reveal();
    return;
  }
  if (state.connectedConnectionIds.has(id)) {
    const catalogPath = "/connections/" + encodeURIComponent(id) + "/catalog"
      + (connection.engine === "PostgreSQL" ? "?database=" + encodeURIComponent(connection.database) : "");
    return apiRequest(catalogPath)
      .then(catalog => {
        if (getConnectionById(id) !== connection || !state.connectedConnectionIds.has(id)) return;
        connection.databases = catalog.databases;
        databaseCatalogs.set(id, { databases: catalog.databases, catalogs: new Map(connection.engine === "SQLite" ? [[catalog.databases[0], catalog]] : connection.database ? [[connection.database, catalog]] : []), pending: new Map() });
        reveal();
      }).catch(error => {
        if (error.code === "connection_required") {
          state.connectedConnectionIds.delete(id);
          connectToDatabase(id, callback);
        } else toast(error.message);
      });
    return;
  }
  if (connection.ssh) return toast("SSH tunnels are not available yet.");
  if (connection.engine === "SQLite") {
    apiRequest("/connections/" + encodeURIComponent(id) + "/catalog", { method: "POST", body: { password: "" } })
      .then(async catalog => {
        if (getConnectionById(id) !== connection) return;
        if (!await setConnected(id, true)) return;
        connection.databases = catalog.databases;
        databaseCatalogs.set(id, { databases: catalog.databases, catalogs: new Map([[catalog.databases[0], catalog]]), pending: new Map() });
        reveal();
      }).catch(error => toast(error.message));
    return;
  }
  showDialog("Connect to " + escapeHtml(connection.name), `<form id="password-form" class="stack">
    <p>${escapeHtml(connection.username)}@${escapeHtml(connection.host)}:${escapeHtml(connection.port)}${connection.database ? " / " + escapeHtml(connection.database) : ""}</p>
    <label>Database password<input type="password" name="password" autocomplete="off" autofocus></label>
    <p class="hint">Credentials stay in server memory while GoSQL is active and expire after 5 minutes without an active tab. Disconnect or sign out to clear them.</p>
    <div id="connect-error" class="error-text" role="alert"></div>
    <button type="submit" class="primary">Connect</button>
  </form>`);
  const form = findElement("#password-form");
  form.onsubmit = async event => {
    event.preventDefault();
    const userId = state.currentUser.id;
    const submit = form.querySelector('[type="submit"]');
    submit.disabled = true;
    submit.textContent = "Connecting…";
    try {
      const catalog = await apiRequest("/connections/" + encodeURIComponent(id) + "/catalog", {
        method: "POST", body: { password: new FormData(form).get("password") }
      });
      form.reset();
      if (state.currentUser?.id !== userId || getConnectionById(id) !== connection) return;
      if (!await setConnected(id, true)) return;
      connection.databases = catalog.databases;
      databaseCatalogs.set(id, { databases: catalog.databases, catalogs: new Map(connection.database ? [[connection.database, catalog]] : []), pending: new Map() });
      if (findElement("#password-form") === form) closeDialog();
      reveal();
    } catch (error) {
      const node = form.querySelector("#connect-error");
      if (node) node.textContent = error.message;
    } finally { submit.disabled = false; submit.textContent = "Connect"; }
  };
}

export function exportPreview(id) {
  const connection = getConnectionById(id);
  if (connection?.engine === "SQLite" && connection.location === "Server upload") {
    downloadSQLite(id, connection.file).catch(error => toast(error.message));
    return;
  }
  toast("Database export is not available yet.");
}
export function importDialog() { toast("Database import is not available yet."); }

function columnRow(column = { name: "", type: "text", nullable: true, primary: false }, customTypes = []) {
  return `<div class="table-column-row">
    <div class="table-column-fields">
      <label>Name<input data-field="name" required maxlength="63" value="${escapeHtml(column.name)}"></label>
      <label>Type<select data-field="type">${[...columnTypes, ...customTypes].map(type => `<option value="${escapeHtml(type)}" ${type === column.type ? "selected" : ""}>${escapeHtml(type)}</option>`).join("")}</select></label>
      <label data-length-field hidden>Length<input data-field="length" type="number" min="1" max="10485760" value="255" disabled></label>
      <label data-precision-field hidden>Precision (digits)<input data-field="precision" type="number" min="1" max="1000" value="18" disabled></label>
      <label data-scale-field hidden>Scale (decimals)<input data-field="scale" type="number" min="-1000" max="1000" value="2" disabled></label>
    </div>
    <div class="table-column-checks">
      <label class="check-label"><input type="checkbox" data-field="use-default">Use default</label>
      <label class="check-label"><input type="checkbox" data-field="nullable" ${column.nullable ? "checked" : ""}>Nullable</label>
      <label class="check-label"><input type="checkbox" data-field="primary" ${column.primary ? "checked" : ""}>Primary key</label>
      <label class="check-label"><input type="checkbox" data-field="array" ${["smallserial", "serial", "bigserial"].includes(column.type) ? "disabled" : ""}>Array</label>
    </div>
    <div class="table-column-default">
      <label>Default literal<input data-field="default" placeholder="Enter a value" disabled></label>
      <button type="button" class="remove-column ghost" data-remove-column>Remove Column ${icon("x")}</button>
    </div>
  </div>`;
}

function sqliteColumnRow(column = { name: "", type: "TEXT", nullable: true, primary: false }) {
  return `<div class="table-column-row">
    <div class="table-column-fields">
      <label>Name<input data-field="name" required maxlength="255" value="${escapeHtml(column.name)}"></label>
      <label>SQLite type<select data-field="type">${["INTEGER", "INT", "REAL", "TEXT", "BLOB", "NUMERIC", "ANY"].map(type => `<option value="${type}" ${type === column.type ? "selected" : ""}>${type}</option>`).join("")}</select></label>
    </div>
    <div class="table-column-checks">
      <label class="check-label"><input type="checkbox" data-field="nullable" ${column.nullable ? "checked" : ""}>Nullable</label>
      <label class="check-label"><input type="checkbox" data-field="primary" ${column.primary ? "checked" : ""}>Primary key</label>
      <label class="check-label"><input type="checkbox" data-field="unique" ${column.primary ? "disabled" : ""}>Unique</label>
      <label class="check-label"><input type="checkbox" data-field="autoIncrement">AUTOINCREMENT</label>
      <label class="check-label"><input type="checkbox" data-field="use-default">Use default</label>
      <label class="check-label"><input type="checkbox" data-field="defaultExpression">Default expression</label>
    </div>
    <div class="table-column-default">
      <label>Default literal<input data-field="default" disabled></label>
      <button type="button" class="remove-column ghost" data-remove-column>Remove Column ${icon("x")}</button>
    </div>
    <div class="table-column-fields">
      <label>CHECK expression<input data-field="check" placeholder="e.g. price >= 0"></label>
      <label>References table<input data-field="referenceTable" list="sqlite-reference-tables" placeholder="Optional"></label>
      <label>References column<input data-field="referenceColumn" placeholder="e.g. id"></label>
    </div>
  </div>`;
}

function createSQLiteTable(id, database) {
  connectToDatabase(id, () => {
    showDialog("Create SQLite table", `<form id="create-sqlite-table" class="stack">
      <p class="hint">SQLite uses type affinity. Declared lengths such as VARCHAR(255) do not enforce a limit.</p>
      <label>Table name<input name="table" required maxlength="255" autofocus></label>
      <div class="row"><label class="check-label"><input type="checkbox" name="strict">STRICT</label>
        <label class="check-label"><input type="checkbox" name="withoutRowid">WITHOUT ROWID</label></div>
      <div id="sqlite-table-columns" class="stack">${sqliteColumnRow({ name: "id", type: "INTEGER", nullable: false, primary: true })}</div>
      <datalist id="sqlite-reference-tables">${(getConnectionCatalog(id, database)?.tables || []).map(table => `<option value="${escapeHtml(table)}">`).join("")}</datalist>
      <button type="button" id="sqlite-add-column">${icon("plus")}Add column</button>
      <pre id="sqlite-create-preview" class="sql-preview"></pre>
      <div id="sqlite-create-error" class="error-text" role="alert"></div>
      <button type="submit" class="primary">Create table</button>
    </form>`, "", true);
    const form = findElement("#create-sqlite-table");
    const columnsNode = findElement("#sqlite-table-columns");
    const readColumns = () => findElements(".table-column-row", columnsNode).map(row => ({
      name: findElement('[data-field="name"]', row).value.trim(), type: findElement('[data-field="type"]', row).value,
      nullable: findElement('[data-field="nullable"]', row).checked,
      primary: findElement('[data-field="primary"]', row).checked,
      unique: findElement('[data-field="unique"]', row).checked,
      autoIncrement: findElement('[data-field="autoIncrement"]', row).checked,
      default: findElement('[data-field="use-default"]', row).checked ? findElement('[data-field="default"]', row).value : null,
      defaultExpression: findElement('[data-field="use-default"]', row).checked && findElement('[data-field="defaultExpression"]', row).checked,
      check: findElement('[data-field="check"]', row).value.trim(),
      referenceTable: findElement('[data-field="referenceTable"]', row).value.trim(),
      referenceColumn: findElement('[data-field="referenceColumn"]', row).value.trim()
    }));
    const preview = () => {
      const columns = readColumns(), table = form.elements.table.value.trim();
      const keys = columns.filter(column => column.primary);
      const inline = keys.length === 1 && keys[0].autoIncrement;
      const definitions = columns.map(column => {
        const defaultValue = column.default === null ? "" : column.defaultExpression
          ? ` DEFAULT ${/^CURRENT_(TIME|DATE|TIMESTAMP)$/i.test(column.default) ? column.default : `(${column.default})`}`
          : column.type === "BLOB" && column.default.startsWith("\\x") ? ` DEFAULT X'${column.default.slice(2)}'`
            : ` DEFAULT '${column.default.replaceAll("'", "''")}'`;
        return `${quoteSQL(column.name)} ${column.type}${inline && column.autoIncrement ? " PRIMARY KEY AUTOINCREMENT" : ""}${!column.nullable && !column.primary ? " NOT NULL" : ""}${column.unique ? " UNIQUE" : ""}${column.referenceTable ? ` REFERENCES ${quoteSQL(column.referenceTable)}(${quoteSQL(column.referenceColumn)})` : ""}${column.check ? ` CHECK (${column.check})` : ""}${defaultValue}`;
      });
      if (keys.length && !inline) definitions.push(`PRIMARY KEY (${keys.map(column => quoteSQL(column.name)).join(", ")})`);
      const options = [form.elements.withoutRowid.checked ? "WITHOUT ROWID" : "", form.elements.strict.checked ? "STRICT" : ""].filter(Boolean);
      findElement("#sqlite-create-preview").textContent = table ? `CREATE TABLE main.${quoteSQL(table)} (${definitions.join(", ")})${options.length ? " " + options.join(", ") : ""};` : "";
    };
    const updateTypes = () => findElements('[data-field="type"]', columnsNode).forEach(select => {
      select.querySelector('[value="NUMERIC"]')?.toggleAttribute("disabled", form.elements.strict.checked);
      select.querySelector('[value="ANY"]')?.toggleAttribute("disabled", !form.elements.strict.checked);
      if (select.selectedOptions[0]?.disabled) select.value = "REAL";
    });
    form.oninput = preview;
    form.onchange = event => {
      const row = event.target.closest(".table-column-row");
      if (row && event.target.dataset.field === "use-default") findElement('[data-field="default"]', row).disabled = !event.target.checked;
      if (row && event.target.dataset.field === "primary") {
        const unique = findElement('[data-field="unique"]', row);
        unique.disabled = event.target.checked;
        if (unique.disabled) unique.checked = false;
      }
      updateTypes();
      preview();
    };
    columnsNode.onclick = event => { if (event.target.closest("[data-remove-column]") && findElements(".table-column-row", columnsNode).length > 1) { event.target.closest(".table-column-row").remove(); preview(); } };
    findElement("#sqlite-add-column").onclick = () => { if (findElements(".table-column-row", columnsNode).length >= 32) return toast("This form supports up to 32 columns."); columnsNode.insertAdjacentHTML("beforeend", sqliteColumnRow()); updateTypes(); preview(); };
    updateTypes();
    preview();
    form.onsubmit = async event => {
      event.preventDefault();
      if (!form.reportValidity()) return;
      const columns = readColumns(), table = form.elements.table.value.trim();
      if (columns.some(column => column.autoIncrement && (!column.primary || column.type !== "INTEGER")) || columns.filter(column => column.autoIncrement).length > 1) return toast("AUTOINCREMENT requires one INTEGER PRIMARY KEY.");
      if (form.elements.withoutRowid.checked && !columns.some(column => column.primary)) return toast("WITHOUT ROWID requires a primary key.");
      if (form.elements.withoutRowid.checked && columns.some(column => column.autoIncrement)) return toast("AUTOINCREMENT is unavailable on WITHOUT ROWID tables.");
      if (columns.some(column => Boolean(column.referenceTable) !== Boolean(column.referenceColumn))) return toast("Enter both referenced table and column.");
      const submit = form.querySelector('[type="submit"]'); submit.disabled = true;
      try {
        await runRecordedSQL("/connections/" + encodeURIComponent(id) + "/tables", { method: "POST", body: { database, schema: "", table, columns, strict: form.elements.strict.checked, withoutRowid: form.elements.withoutRowid.checked } }, historyLocation(id, database), findElement("#sqlite-create-preview").textContent);
        closeDialog(); await refreshDatabaseCatalog(id, database);
        revealTreeDestination(getConnectionById(id), { db: database, schemaName: "", table }, false);
        onOpenTable?.(id, database, table, false, ""); toast("Table created.");
      } catch (error) { findElement("#sqlite-create-error").textContent = error.message; }
      finally { submit.disabled = false; }
    };
  });
}

function createTable(id, database, schemaName) {
  if (getConnectionById(id)?.engine === "SQLite") return createSQLiteTable(id, database);
  if (mysqlFamily(getConnectionById(id)?.engine)) return createMySQLTable(id, database);
  connectToDatabase(id, async () => {
    try {
      const catalog = await loadDatabaseCatalog(id, database);
      if (!catalog) return;
      if (!catalog.schemas.some(item => item.name === schemaName)) return toast("This schema is no longer available. Refresh the database.");
      showDialog("Create table", `<form id="create-table-form" class="stack">
        <p class="hint">${escapeHtml(database)} / ${escapeHtml(schemaName)}</p>
        <label>Table name<input name="table" required maxlength="63" autofocus></label>
        <div id="create-table-columns" class="stack">${columnRow({ name: "id", type: "bigserial", nullable: false, primary: true }, catalog.types)}</div>
        <button type="button" id="create-table-add-column">${icon("plus")}Add column</button>
        <div id="create-table-error" class="error-text" role="alert"></div>
        <button type="submit" class="primary">Create table</button>
      </form>`, "", true);
      const form = findElement("#create-table-form");
      findElement("#create-table-add-column").onclick = () => {
        const rows = findElement("#create-table-columns");
        if (findElements(".table-column-row", rows).length >= 32) return toast("A table can have at most 32 columns in this form.");
        rows.insertAdjacentHTML("beforeend", columnRow(undefined, catalog.types));
      };
      findElement("#create-table-columns").onclick = event => {
        if (event.target.closest("[data-remove-column]") && findElements(".table-column-row", form).length > 1) event.target.closest(".table-column-row").remove();
      };
      findElement("#create-table-columns").onchange = event => {
        const row = event.target.closest(".table-column-row");
        if (!row) return;
        if (event.target.dataset.field === "type") {
          for (const [field, visible] of [["length", lengthTypes.has(event.target.value)], ["precision", numericTypes.has(event.target.value)], ["scale", numericTypes.has(event.target.value)]]) {
            findElement(`[data-${field}-field]`, row).hidden = !visible;
            findElement(`[data-field="${field}"]`, row).disabled = !visible;
            findElement(`[data-field="${field}"]`, row).required = visible;
          }
          const array = findElement('[data-field="array"]', row);
          array.disabled = ["smallserial", "serial", "bigserial"].includes(event.target.value);
          if (array.disabled) array.checked = false;
        }
        if (event.target.dataset.field === "use-default") findElement('[data-field="default"]', row).disabled = !event.target.checked;
      };
      form.onsubmit = async event => {
        event.preventDefault();
        if (!form.reportValidity()) return;
        const columns = findElements(".table-column-row", form).map(row => ({
          name: findElement('[data-field="name"]', row).value.trim(),
          type: columnTypeSQL(findElement('[data-field="type"]', row).value, findElement('[data-field="length"]', row).value, findElement('[data-field="precision"]', row).value, findElement('[data-field="scale"]', row).value, findElement('[data-field="array"]', row).checked),
          nullable: findElement('[data-field="nullable"]', row).checked,
          primary: findElement('[data-field="primary"]', row).checked,
          default: findElement('[data-field="use-default"]', row).checked ? findElement('[data-field="default"]', row).value : null
        }));
        const submit = form.querySelector('[type="submit"]');
        submit.disabled = true;
        try {
          const schema = schemaName, table = form.elements.table.value.trim();
          const definitions = columns.map(column => `${quoteSQL(column.name)} ${column.type}${column.default === null ? "" : " DEFAULT " + literalSQL(column.default)}${!column.nullable || column.primary ? " NOT NULL" : ""}`);
          const primary = columns.filter(column => column.primary).map(column => quoteSQL(column.name));
          if (primary.length) definitions.push(`PRIMARY KEY (${primary.join(", ")})`);
          const sql = `CREATE TABLE ${quoteSQL(schema)}.${quoteSQL(table)} (${definitions.join(", ")});`;
          await runRecordedSQL("/connections/" + encodeURIComponent(id) + "/tables", { method: "POST", body: { database, schema, table, columns } }, historyLocation(id, database, schema), sql);
          closeDialog();
          try { await refreshDatabaseCatalog(id, database); } catch (error) { toast("Table created, but the sidebar could not refresh: " + error.message); }
          revealTreeDestination(getConnectionById(id), { db: database, schemaName: schema, table }, false);
          renderTree();
          onOpenTable?.(id, database, table, false, schema);
          toast("Table created.");
        } catch (error) { findElement("#create-table-error").textContent = error.message; }
        finally { submit.disabled = false; }
      };
    } catch (error) { toast(error.message); }
  });
}

function createMySQLTable(id, database) {
  const row = () => `<div class="table-column-row">
    <div class="table-column-fields">
      <label>Name<input data-field="name" required maxlength="64"></label>
      ${mysqlTypeFields("int", false, false)}
    </div>
    <div class="table-column-checks">
      ${mysqlUnsignedField()}
      <label class="check-label"><input type="checkbox" data-field="nullable" checked>Nullable</label>
      <label class="check-label"><input type="checkbox" data-field="primary">Primary key</label>
      <label class="check-label"><input type="checkbox" data-field="autoIncrement">Auto increment</label>
      <label class="check-label"><input type="checkbox" data-field="use-default">Use default</label>
    </div>
    <div class="table-column-default">
      <label>Default literal<input data-field="default" disabled></label>
      <button type="button" class="remove-column ghost" data-remove-column>Remove Column ${icon("x")}</button>
    </div>
  </div>`;
  showDialog("Create table", `<form id="create-mysql-table" class="stack">
    <p class="hint">${escapeHtml(database)} · ${escapeHtml(getConnectionById(id)?.engine || "MySQL")}</p>
    <label>Table name<input name="table" required maxlength="64" autofocus></label>
    <label>Storage engine<select name="engine">${mysqlEngineOptions(getConnectionById(id)?.engine)}</select></label>
    <div id="mysql-table-columns" class="stack">${row()}</div>
    <button type="button" id="mysql-add-column">${icon("plus")}Add column</button>
    <div id="create-table-error" class="error-text" role="alert"></div>
    <button type="submit" class="primary">Create table</button>
  </form>`, "", true);
  const form = findElement("#create-mysql-table");
  findElement('[data-field="name"]', form).value = "id";
  findElement('[data-field="nullable"]', form).checked = false;
  findElement('[data-field="primary"]', form).checked = true;
  findElement('[data-field="autoIncrement"]', form).checked = true;
  updateMySQLTypeFields(findElement(".table-column-row", form));
  findElement("#mysql-add-column").onclick = () => {
    if (findElements(".table-column-row", form).length < 32) {
      findElement("#mysql-table-columns").insertAdjacentHTML("beforeend", row());
      updateMySQLTypeFields(findElements(".table-column-row", form).at(-1));
    }
  };
  findElement("#mysql-table-columns").onchange = event => {
    if (event.target.dataset.field === "type") updateMySQLTypeFields(event.target.closest(".table-column-row"));
    if (event.target.dataset.field === "use-default") {
      const column = event.target.closest(".table-column-row");
      findElement('[data-field="default"]', column).disabled = !event.target.checked;
    }
  };
  findElement("#mysql-table-columns").oninput = event => {
    if (event.target.dataset.field === "precision") updateMySQLTypeFields(event.target.closest(".table-column-row"));
  };
  findElement("#mysql-table-columns").onclick = event => {
    if (event.target.closest("[data-remove-column]") && findElements(".table-column-row", form).length > 1) event.target.closest(".table-column-row").remove();
  };
  form.onsubmit = async event => {
    event.preventDefault();
    if (!form.reportValidity()) return;
    const table = form.elements.table.value.trim();
    const engine = form.elements.engine.value;
    const columns = findElements(".table-column-row", form).map(element => ({
      name: findElement('[data-field="name"]', element).value.trim(),
      type: mysqlTypeSQL(element),
      nullable: findElement('[data-field="nullable"]', element).checked,
      primary: findElement('[data-field="primary"]', element).checked,
      autoIncrement: findElement('[data-field="autoIncrement"]', element).checked,
      default: findElement('[data-field="use-default"]', element).checked ? findElement('[data-field="default"]', element).value : null
    }));
    const quote = name => "`" + name.replaceAll("`", "``") + "`";
    const definitions = columns.map(column => {
      const defaultValue = column.default === null ? "" : " DEFAULT " + (/text|blob|^json$/i.test(column.type) ? `('${column.default.replaceAll("'", "''")}')` : `'${column.default.replaceAll("'", "''")}'`);
      return `${quote(column.name)} ${column.type} ${column.primary || !column.nullable ? "NOT NULL" : "NULL"}${defaultValue}${column.autoIncrement ? " AUTO_INCREMENT" : ""}`;
    });
    const primary = columns.filter(column => column.primary).map(column => quote(column.name));
    if (primary.length) definitions.push(`PRIMARY KEY (${primary.join(", ")})`);
    const sql = `CREATE TABLE ${quote(database)}.${quote(table)} (${definitions.join(", ")}) ENGINE=${engine};`;
    const submit = form.querySelector('[type="submit"]');
    submit.disabled = true;
    try {
      await runRecordedSQL("/connections/" + encodeURIComponent(id) + "/tables", { method: "POST", body: { database, schema: "", table, engine, columns } }, historyLocation(id, database), sql);
      closeDialog();
      await refreshDatabaseCatalog(id, database);
      revealTreeDestination(getConnectionById(id), { db: database, schemaName: "", table }, false);
      onOpenTable?.(id, database, table, false, "");
      toast("Table created.");
    } catch (error) { findElement("#create-table-error").textContent = error.message; }
    finally { submit.disabled = false; }
  };
}

function updateOpenTabs(id, database, schema, table, newName) {
  if (newName) {
    for (const tab of state.tabs) {
      if (!tab.queryTab && tab.connectionId === id && tab.db === database && tab.schemaName === schema && tab.table === table) {
        tab.table = newName;
        tab.title = newName;
        tab.sql = `SELECT *\nFROM ${qualifiedTable(id, database, schema, newName)}\nLIMIT 100;`;
      }
    }
  } else {
    state.tabs = state.tabs.filter(tab => tab.connectionId !== id || tab.db !== database || (table && (tab.schemaName !== schema || tab.table !== table)));
    if (!state.tabs.some(tab => tab.id === state.activeTabId)) state.activeTabId = state.tabs.at(-1)?.id;
  }
  onWorkspaceChange();
}

function renameTable(id, database, schema, table) {
  const mysql = mysqlFamily(getConnectionById(id)?.engine);
  const currentEngine = mysql ? getConnectionCatalog(id, database)?.tableEngines?.[table] || "" : "";
  showDialog(mysql ? "Edit table" : "Rename table", `<form id="rename-table-form" class="stack">
    <p>${escapeHtml(mysql ? database : schema)}.${escapeHtml(table)}</p>
    <label>New table name<input name="name" required maxlength="${mysqlFamily(getConnectionById(id)?.engine) ? 64 : 63}" value="${escapeHtml(table)}"></label>
    ${mysql ? `<label>Storage engine<select name="engine"><option value="">Keep current${currentEngine ? ` (${escapeHtml(currentEngine)})` : ""}</option>${mysqlEngineOptions(getConnectionById(id)?.engine)}</select></label>` : ""}
    <div id="rename-table-error" class="error-text" role="alert"></div>
    <button type="submit" class="primary">${mysql ? "Save table" : "Rename table"}</button>
  </form>`);
  const form = findElement("#rename-table-form");
  form.onsubmit = async event => {
    event.preventDefault();
    if (!form.reportValidity()) return;
    const newName = form.elements.name.value.trim();
    const engine = mysql ? form.elements.engine.value : "";
    const changeEngine = engine && engine !== currentEngine;
    if (newName === table && !changeEngine) return closeDialog();
    const submit = form.querySelector('[type="submit"]');
    submit.disabled = true;
    try {
      const sql = mysql
        ? changeEngine
          ? `ALTER TABLE ${qualifiedTable(id, database, schema, table)} ENGINE=${engine}${newName !== table ? `, RENAME TO ${qualifiedTable(id, database, schema, newName)}` : ""};`
          : `RENAME TABLE ${qualifiedTable(id, database, schema, table)} TO ${qualifiedTable(id, database, schema, newName)};`
        : getConnectionById(id)?.engine === "SQLite" ? `ALTER TABLE main.${quoteSQL(table)} RENAME TO ${quoteSQL(newName)};`
          : `ALTER TABLE ${quoteSQL(schema)}.${quoteSQL(table)} RENAME TO ${quoteSQL(newName)};`;
      await runRecordedSQL("/connections/" + encodeURIComponent(id) + "/tables", { method: "PATCH", body: { database, schema, table, newName, engine } }, historyLocation(id, database, schema), sql);
      closeDialog();
      try { await refreshDatabaseCatalog(id, database); } catch (error) { toast("Table changed, but the sidebar could not refresh: " + error.message); }
      updateOpenTabs(id, database, schema, table, newName);
      for (const tab of state.tabs) {
        if (!tab.queryTab && tab.connectionId === id && tab.db === database && tab.schemaName === schema && tab.table === newName) onRefreshTable?.(tab, 1);
      }
      toast(mysql ? "Table updated." : "Table renamed.");
    } catch (error) { findElement("#rename-table-error").textContent = error.message; }
    finally { submit.disabled = false; }
  };
}

function deleteTable(id, database, schema, table) {
  confirmAction("Delete table?", `Delete <strong>${escapeHtml(database)}.${escapeHtml(table)}</strong> and all its data? This cannot be undone.`, "Delete Table", async () => {
    try {
      await runRecordedSQL("/connections/" + encodeURIComponent(id) + "/tables", { method: "DELETE", body: { database, schema, table } }, historyLocation(id, database, schema), `DROP TABLE ${qualifiedTable(id, database, schema, table)};`);
      try { await refreshDatabaseCatalog(id, database); } catch (error) { toast("Table deleted, but the sidebar could not refresh: " + error.message); }
      updateOpenTabs(id, database, schema, table);
      toast("Table deleted.");
    } catch (error) { toast(error.message); }
  });
}

function deleteDatabase(id, database) {
  if (getConnectionById(id)?.database === database) return toast("Change this connection's initial database before deleting it.");
  const detail = getConnectionById(id)?.engine === "PostgreSQL" ? " The database must have no other active connections." : "";
  confirmAction("Delete database?", `Delete <strong>${escapeHtml(database)}</strong> and every object and row inside it? This cannot be undone.${detail}`, "Delete Database", async () => {
    try {
      await runRecordedSQL("/connections/" + encodeURIComponent(id) + "/databases", { method: "DELETE", body: { database } }, historyLocation(id, database), `DROP DATABASE ${databaseQuote(id, database)};`);
      const connection = getConnectionById(id);
      connection.databases = connection.databases.filter(name => name !== database);
      const entry = databaseCatalogs.get(id);
      if (entry) { entry.databases = entry.databases.filter(name => name !== database); entry.catalogs.delete(database); entry.pending.delete(database); }
      expanded.delete(id + ":" + database);
      updateOpenTabs(id, database);
      toast("Database deleted.");
    } catch (error) { toast(error.message); }
  });
}

function renameDatabase(id, database) {
  if (getConnectionById(id)?.database === database) return toast("Change this connection's initial database before renaming it.");
  showDialog("Rename database", `<form id="rename-database-form" class="stack">
    <label>New database name<input name="name" required maxlength="63" value="${escapeHtml(database)}"></label>
    <div id="rename-database-error" class="error-text" role="alert"></div>
    <button type="submit" class="primary">Rename database</button>
  </form>`);
  const form = findElement("#rename-database-form");
  form.onsubmit = async event => {
    event.preventDefault();
    if (!form.reportValidity()) return;
    const newName = form.elements.name.value.trim();
    if (newName === database) return closeDialog();
    const submit = form.querySelector('[type="submit"]');
    submit.disabled = true;
    try {
      await runRecordedSQL("/connections/" + encodeURIComponent(id) + "/databases", { method: "PATCH", body: { database, newName } }, historyLocation(id, newName), `ALTER DATABASE ${quoteSQL(database)} RENAME TO ${quoteSQL(newName)};`);
      closeDialog();
      const connection = getConnectionById(id), entry = databaseCatalogs.get(id);
      connection.databases = connection.databases.map(name => name === database ? newName : name).sort((a, b) => a.localeCompare(b));
      if (entry) {
        entry.databases = entry.databases.map(name => name === database ? newName : name).sort((a, b) => a.localeCompare(b));
        entry.catalogs.delete(database);
        entry.pending.delete(database);
      }
      if (expanded.delete(id + ":" + database)) expanded.add(id + ":" + newName);
      for (const key of [...treeOpen.keys()]) if (key.startsWith(id + ":" + database + ":")) {
        treeOpen.set(id + ":" + newName + key.slice((id + ":" + database).length), treeOpen.get(key));
        treeOpen.delete(key);
      }
      for (const tab of state.tabs) if (tab.connectionId === id && tab.db === database) tab.db = newName;
      onWorkspaceChange();
      try { await refreshDatabaseCatalog(id, newName); } catch (error) { toast("Database renamed, but its catalog could not refresh: " + error.message); }
      toast("Database renamed.");
    } catch (error) { findElement("#rename-database-error").textContent = error.message; }
    finally { submit.disabled = false; }
  };
}

function createDatabase(id) {
  if (!state.connectedConnectionIds.has(id) || !databaseCatalogs.has(id)) {
    connectToDatabase(id, () => createDatabase(id));
    return;
  }
  showDialog("Create database", `<form id="create-database-form" class="stack">
    <label>Database name<input name="name" required maxlength="${mysqlFamily(getConnectionById(id)?.engine) ? 64 : 63}" autocomplete="off" placeholder="Database name"></label>
    <pre id="create-database-sql" class="sql-preview"></pre>
    <div id="create-database-error" class="error-text" role="alert"></div>
    <button type="submit" class="primary">Create Database</button>
  </form>`);
  const form = findElement("#create-database-form");
  form.elements.name.oninput = () => {
    const name = form.elements.name.value.trim();
    findElement("#create-database-sql").textContent = name ? `CREATE DATABASE ${databaseQuote(id, name)};` : "";
  };
  form.onsubmit = async event => {
    event.preventDefault();
    if (!form.reportValidity()) return;
    const name = form.elements.name.value.trim();
    if (!name) return;
    const submit = form.querySelector('[type="submit"]');
    submit.disabled = true;
    try {
      await runRecordedSQL("/connections/" + encodeURIComponent(id) + "/databases", {
        method: "POST", body: { database: name }
      }, historyLocation(id, name), `CREATE DATABASE ${databaseQuote(id, name)};`);
      closeDialog();
      const connection = getConnectionById(id);
      const entry = databaseCatalogs.get(id);
      if (connection) connection.databases = [...new Set([...connection.databases, name])].sort((a, b) => a.localeCompare(b));
      if (entry) entry.databases = [...new Set([...entry.databases, name])].sort((a, b) => a.localeCompare(b));
      collapsedConnections.delete(id);
      expanded.add(id + ":" + name);
      renderTree();
      try { await loadDatabaseCatalog(id, name); } catch (error) { toast("Database created, but its catalog could not be loaded: " + error.message); }
      renderTree();
      toast("Database created.");
    } catch (error) { findElement("#create-database-error").textContent = error.message; }
    finally { submit.disabled = false; }
  };
}

function createSchema(id, database) {
  if (!state.connectedConnectionIds.has(id) || !databaseCatalogs.has(id)) {
    connectToDatabase(id, () => createSchema(id, database));
    return;
  }
  showDialog("Create schema", `<form id="create-schema-form" class="stack">
    <p class="hint">Database: ${escapeHtml(database)}</p>
    <label>Schema name<input name="name" required maxlength="63" autocomplete="off" placeholder="Schema name"></label>
    <pre id="create-schema-sql" class="sql-preview"></pre>
    <div id="create-schema-error" class="error-text" role="alert"></div>
    <button type="submit" class="primary">Create Schema</button>
  </form>`);
  const form = findElement("#create-schema-form");
  form.elements.name.oninput = () => {
    const name = form.elements.name.value.trim();
    findElement("#create-schema-sql").textContent = name ? `CREATE SCHEMA ${quoteSQL(name)};` : "";
  };
  form.onsubmit = async event => {
    event.preventDefault();
    if (!form.reportValidity()) return;
    const name = form.elements.name.value.trim();
    if (!name) return;
    const submit = form.querySelector('[type="submit"]');
    submit.disabled = true;
    try {
      await runRecordedSQL("/connections/" + encodeURIComponent(id) + "/schemas", {
        method: "POST", body: { database, schema: name }
      }, historyLocation(id, database, name), `CREATE SCHEMA ${quoteSQL(name)};`);
      closeDialog();
      expanded.add(id + ":" + database);
      treeOpen.set(id + ":" + database + ":" + name, true);
      try { await refreshDatabaseCatalog(id, database); } catch (error) { toast("Schema created, but the sidebar could not refresh: " + error.message); }
      renderTree();
      toast("Schema created.");
    } catch (error) { findElement("#create-schema-error").textContent = error.message; }
    finally { submit.disabled = false; }
  };
}

function renameSchema(id, database, schema) {
  showDialog("Rename schema", `<form id="rename-schema-form" class="stack">
    <p class="hint">Database: ${escapeHtml(database)}</p>
    <label>New schema name<input name="name" required maxlength="63" value="${escapeHtml(schema)}"></label>
    <div id="rename-schema-error" class="error-text" role="alert"></div>
    <button type="submit" class="primary">Rename schema</button>
  </form>`);
  const form = findElement("#rename-schema-form");
  form.onsubmit = async event => {
    event.preventDefault();
    if (!form.reportValidity()) return;
    const newName = form.elements.name.value.trim();
    if (newName === schema) return closeDialog();
    const submit = form.querySelector('[type="submit"]');
    submit.disabled = true;
    try {
      await runRecordedSQL("/connections/" + encodeURIComponent(id) + "/schemas", { method: "PATCH", body: { database, schema, newName } }, historyLocation(id, database, newName), `ALTER SCHEMA ${quoteSQL(schema)} RENAME TO ${quoteSQL(newName)};`);
      closeDialog();
      const oldKey = id + ":" + database + ":" + schema;
      for (const key of [...treeOpen.keys()]) if (key === oldKey || key.startsWith(oldKey + ":")) {
        treeOpen.set(id + ":" + database + ":" + newName + key.slice(oldKey.length), treeOpen.get(key));
        treeOpen.delete(key);
      }
      for (const tab of state.tabs) if (tab.connectionId === id && tab.db === database && tab.schemaName === schema) {
        tab.schemaName = newName;
        if (!tab.queryTab) tab.sql = `SELECT *\nFROM ${quoteSQL(newName)}.${quoteSQL(tab.table)}\nLIMIT 100;`;
      }
      onWorkspaceChange();
      try { await refreshDatabaseCatalog(id, database); } catch (error) { toast("Schema renamed, but the sidebar could not refresh: " + error.message); }
      toast("Schema renamed.");
    } catch (error) { findElement("#rename-schema-error").textContent = error.message; }
    finally { submit.disabled = false; }
  };
}

function deleteSchema(id, database, schema) {
  confirmAction("Delete schema?", `Delete <strong>${escapeHtml(schema)}</strong> from <strong>${escapeHtml(database)}</strong>? The schema must be empty. PostgreSQL will refuse to remove objects inside it.`, "Delete Schema", async () => {
    try {
      await runRecordedSQL("/connections/" + encodeURIComponent(id) + "/schemas", { method: "DELETE", body: { database, schema } }, historyLocation(id, database), `DROP SCHEMA ${quoteSQL(schema)};`);
      const schemaKey = id + ":" + database + ":" + schema;
      for (const key of [...treeOpen.keys()]) if (key === schemaKey || key.startsWith(schemaKey + ":")) treeOpen.delete(key);
      state.tabs = state.tabs.filter(tab => tab.connectionId !== id || tab.db !== database || tab.schemaName !== schema);
      if (!state.tabs.some(tab => tab.id === state.activeTabId)) state.activeTabId = state.tabs.at(-1)?.id;
      onWorkspaceChange();
      try { await refreshDatabaseCatalog(id, database); } catch (error) { toast("Schema deleted, but the sidebar could not refresh: " + error.message); }
      toast("Schema deleted.");
    } catch (error) { toast(error.message); }
  });
}

// -----------------------------------------------------------------------------
// User actions
// -----------------------------------------------------------------------------
export function handleConnectionsAction(action, element) {
  const id = element.dataset.id;
  switch (action) {
    case "add-connection":
      openConnectionForm(null, engines.includes(element.dataset.engine) ? element.dataset.engine : "PostgreSQL");
      break;
    case "edit-connection":
      openConnectionForm(id);
      break;
    case "pick-engine":
      readConnectionForm();
      if (connectionDraft.engine !== element.dataset.engine) {
        sqliteSelectedFile = null;
        sqlitePickerToken = null;
        if (element.dataset.engine === "SQLite") {
          connectionDraft.location = state.nativeFilePickerAllowed ? "Native file" : "Local file";
          connectionDraft.file = "";
        }
      }
      connectionDraft.engine = element.dataset.engine;
      connectionDraft.port = mysqlFamily(element.dataset.engine) ? "3306" : "5432";
      connectionDraft.username = mysqlFamily(element.dataset.engine) ? "root" : "postgres";
      if (mysqlFamily(connectionDraft.engine)) { connectionDraft.ssh = false; delete connectionDraft.database; }
      connectionPane = "General";
      renderConnectionForm();
      break;
    case "choose-sqlite-file":
      chooseSQLiteFile(element);
      break;
    case "connection-pane":
      readConnectionForm();
      connectionPane = element.dataset.pane;
      renderConnectionForm();
      break;
    case "save-connection":
      saveConnection();
      break;
    case "test-connection":
      testConnection(element);
      break;
    case "connect":
      collapsedConnections.delete(id);
      connectToDatabase(id);
      break;
    case "toggle-connection":
      if (!state.connectedConnectionIds.has(id) || (["PostgreSQL", "MySQL", "MariaDB"].includes(getConnectionById(id)?.engine) && !databaseCatalogs.has(id))) {
        collapsedConnections.delete(id);
        connectToDatabase(id);
      } else {
        if (collapsedConnections.has(id)) collapsedConnections.delete(id);
        else collapsedConnections.add(id);
        renderTree();
      }
      break;
    case "delete-connection":
      removeConnection(id);
      break;
    case "disconnect":
      confirmAction("Disconnect?", "Open tabs for this connection will close.", "Disconnect", async () => {
        if (!await setConnected(id, false)) return;
        state.tabs = state.tabs.filter((tab) => tab.connectionId !== id);
        databaseCatalogs.delete(id);
        if (!getActiveTab()) state.activeTabId = state.tabs[0]?.id;
        onWorkspaceChange();
      });
      break;
    case "database": {
      const database = element.dataset.db;
      const key = id + ":" + database;
      if (expanded.has(key)) {
        expanded.delete(key);
        renderTree();
      } else {
        connectToDatabase(id, async () => {
          try {
            if (!await loadDatabaseCatalog(id, database)) return;
            expanded.add(key);
            renderTree();
          } catch (error) { toast(error.message); }
        });
      }
      break;
    }
    case "inspect-object":
      toast("Object details are not available yet.");
      break;
    case "create-table":
      createTable(id, element.dataset.db, element.dataset.schema);
      break;
    case "create-database":
      createDatabase(id);
      break;
    case "create-schema":
      createSchema(id, element.dataset.db);
      break;
    case "rename-schema":
      renameSchema(id, element.dataset.db, element.dataset.schema);
      break;
    case "delete-schema":
      deleteSchema(id, element.dataset.db, element.dataset.schema);
      break;
    case "rename-database":
      renameDatabase(id, element.dataset.db);
      break;
    case "rename-table":
      renameTable(id, element.dataset.db, element.dataset.schema, element.dataset.table);
      break;
    case "delete-table":
      deleteTable(id, element.dataset.db, element.dataset.schema, element.dataset.table);
      break;
    case "delete-database":
      deleteDatabase(id, element.dataset.db);
      break;
    case "database-export":
      exportPreview(id, element.dataset.db);
      break;
    case "database-import":
      importDialog(id, element.dataset.db);
      break;
    default:
      return false;
  }
  return true;
}

// -----------------------------------------------------------------------------
// Initialization
// -----------------------------------------------------------------------------
export function initializeConnections(callbacks) {
  // Render shell dikoordinasikan app.js untuk mencegah circular import.
  onWorkspaceChange = callbacks.onWorkspaceChange;
  onOpenTable = callbacks.onOpenTable;
  onRefreshTable = callbacks.onRefreshTable;
}
