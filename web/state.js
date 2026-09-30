export const engines = ["PostgreSQL", "MySQL", "MariaDB", "SQLite"];

// -----------------------------------------------------------------------------
// Shared workspace state
// -----------------------------------------------------------------------------
/**
 * @typedef {Object} ConnectionProfile
 * @property {string} id
 * @property {"PostgreSQL"|"MySQL"|"MariaDB"|"SQLite"} engine
 * @property {string} name
 * @property {string[]} databases
 * @property {string} [host]
 * @property {string} [port]
 * @property {string} [file]
 *
 * @typedef {Object} QueryResult
 * @property {Object[]} [rows]
 * @property {string} [error]
 * @property {string} [message]
 * @property {boolean} [empty]
 * @property {number} [duration]
 * @property {number} [affectedRows]
 *
 * @typedef {Object} WorkspaceTab
 * @property {string} id
 * @property {string} connectionId
 * @property {string} db
 * @property {string} schemaName
 * @property {string} table
 * @property {string} title
 * @property {string} sql
 * @property {boolean} queryTab
 * @property {boolean} consoleOpen
 * @property {"Data"|"Structure"|"Indexes"|"Constraints"} view
 * @property {Object[]} rows
 * @property {Object[]} schema
 * @property {Object[]} indexes
 * @property {Object[]} constraints
 * @property {boolean} isView
 * @property {number} page
 * @property {number} pageSize
 * @property {string} search
 * @property {{field: string, dir: "asc"|"desc"}[]} sorts
 * @property {{mode: "AND"|"OR", children: Object[]}|null} filter
 * @property {QueryResult|null} result
 * @property {import("@codemirror/state").EditorState} [editorState]
 * @property {HTMLElement} [searchHost]
 */
// Hanya data lintas fitur. Instance library, timer, observer, dan cache milik modulnya.
export const state = {
  users: [],
  currentUser: null,
  nativeFilePickerAllowed: false,
  /** @type {ConnectionProfile[]} */
  connections: [],
  savedQueries: [],
  queryHistory: [],
  /** @type {WorkspaceTab[]} */
  tabs: [],
  activeTabId: null,
  connectedConnectionIds: new Set(),
  runningQuery: null
};

export function getActiveTab() {
  return state.tabs.find(tab => tab.id === state.activeTabId);
}

export function getConnectionById(id) {
  return state.connections.find(connection => connection.id === id);
}
