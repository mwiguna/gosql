import { getConnectionById } from "../state.js";
import { button, escapeHtml, findElement } from "../ui.js";

export function sortOverviewTables(tables, sort) {
  if (!sort) return tables;
  return tables.toSorted((a, b) => {
    const left = a[sort.field], right = b[sort.field];
    if (left == null) return right == null ? 0 : 1;
    if (right == null) return -1;
    return (left - right) * (sort.dir === "asc" ? 1 : -1);
  });
}

export function formatTableSize(bytes) {
  if (bytes === null || bytes === undefined) return "—";
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB", "TB"];
  const index = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)) - 1, units.length - 1);
  return `${(bytes / 1024 ** (index + 1)).toLocaleString(undefined, { maximumFractionDigits: 1 })} ${units[index]}`;
}

export function renderOverview(tab) {
  const area = findElement("#data-area");
  if (!area) return;
  const connection = getConnectionById(tab.connectionId);
  const attributes = `data-id="${escapeHtml(tab.connectionId)}" data-db="${escapeHtml(tab.db)}" data-schema="${escapeHtml(tab.schemaName)}"`;
  const protectedDatabase = ["MySQL", "MariaDB"].includes(connection.engine)
    && ["mysql", "information_schema", "performance_schema", "sys"].includes(tab.db.toLowerCase());
  const tables = sortOverviewTables(tab.tables || [], tab.overviewSort);
  const sortHeader = (field, label) => {
    const direction = tab.overviewSort?.field === field ? tab.overviewSort.dir : null;
    return `<th aria-sort="${direction ? direction === "asc" ? "ascending" : "descending" : "none"}">${button("sort-overview", label + (direction === "asc" ? " ↑" : direction === "desc" ? " ↓" : " ↕"), "", "ghost", `data-field="${field}"`)}</th>`;
  };
  area.innerHTML = `<div class="toolbar overview-toolbar">
    <div><h2>${escapeHtml(tab.schemaName || tab.db)}</h2><span class="muted">${connection.engine === "PostgreSQL" ? "Schema" : "Database"} overview · ${tables.length} tables</span></div>
    <span class="spacer"></span>
    ${button("refresh-overview", "Refresh", "refresh", "ghost", tab.loading ? "disabled" : "")}
    ${protectedDatabase ? "" : button("create-table", "Add table", "plus", "primary", attributes)}
  </div>
  ${tab.loadError ? `<div class="notice" role="alert">${escapeHtml(tab.loadError)} ${button("refresh-overview", "Retry")}</div>` : ""}
  ${tab.loading ? '<div class="result-empty" role="status">Loading tables…</div>' : tab.loadError ? "" : `<div class="schema-scroll overview-tables"><table class="schema-table">
    <thead><tr><th>Table</th>${sortHeader("rows", "Rows")}${sortHeader("sizeBytes", "Size (data + indexes)")}<th>Actions</th></tr></thead>
    <tbody>${tables.map(table => {
      const tableAttributes = `${attributes} data-table="${escapeHtml(table.name)}"`;
      const rows = table.rows === null ? "—" : `${table.estimated ? "≈ " : ""}${table.rows.toLocaleString()}`;
      return `<tr><td>${button("open-table", escapeHtml(table.name), "table", "ghost", tableAttributes)}${table.virtual ? '<small class="muted"> Virtual</small>' : ""}</td>
        <td>${rows}</td><td>${formatTableSize(table.sizeBytes)}</td><td>
          ${protectedDatabase || table.virtual ? '<span class="muted">Read-only</span>' : button("edit-overview-table", "Edit", "edit", "ghost", tableAttributes) + button("delete-table", "Delete", "trash", "ghost danger", tableAttributes)}
        </td></tr>`;
    }).join("") || '<tr><td colspan="4" class="muted">No tables yet. Add a table to get started.</td></tr>'}</tbody>
  </table></div>`}`;
}
