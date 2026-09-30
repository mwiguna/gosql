import { getActiveTab, getConnectionById, state } from "../state.js";
import {
  findElement, findElements, button, closeDialog, confirmAction, escapeHtml, icon, iconButton,
  showDialog, toast
} from "../ui.js";
import { renderStructure } from "./schema.js";
import { apiRequest } from "../api.js";
import { runRecordedSQL } from "../storage.js";

// -----------------------------------------------------------------------------
// Grid library and private state
// -----------------------------------------------------------------------------
let Tabulator;
let gridLibrary;
let grid = null;
let gridReady = false;
let gridTabId = null;
let gridFields = "";
let gridEditable = false;
let gridRows = null;
let gridSource = null;
let gridSearch = "";
let gridFilter = null;
let gridSorts = null;
let selectedCell = null;
let cellContextMenu = null;
let mutationPending = false;
let searchTimer = null;
let filterDraft;
const collator = new Intl.Collator(undefined, { numeric: true });
const SEARCH_DEBOUNCE_MS = 180;
let onLoadTablePage;
let onReconnectTable;
let onRetryTableCount;

export function initializeGrid(callbacks) {
  onLoadTablePage = callbacks.onLoadTablePage;
  onReconnectTable = callbacks.onReconnectTable;
  onRetryTableCount = callbacks.onRetryTableCount;
}

export function loadGridLibrary() {
  return gridLibrary ??= import("tabulator-tables").then(library => {
    ({ Tabulator } = library);
    Tabulator.registerModule([
      library.EditModule, library.FormatModule, library.SelectRowModule,
      library.ResizeColumnsModule, library.ResizeTableModule, library.InteractionModule
    ]);
  });
}

export function destroyGrid() {
  closeCellMenu();
  clearTimeout(searchTimer);
  grid?.destroy();
  grid = null;
  gridTabId = null;
  gridReady = false;
  selectedCell = null;
  gridRows = null;
  gridSource = null;
  gridFilter = null;
}

// -----------------------------------------------------------------------------
// Results and table rendering
// -----------------------------------------------------------------------------
export function renderData() {
  const activeTab = getActiveTab();
  if (!activeTab) return;
  if (activeTab.remote && !activeTab.queryTab && activeTab.view === "Data" && !activeTab.result) {
    renderRemoteData(activeTab);
    return;
  }
  gridRows = null;
  if (grid) {
    grid.destroy();
    grid = null;
  }
  gridTabId = null;
  gridReady = false;
  const area = findElement("#data-area");
  if (!activeTab.queryTab && activeTab.view !== "Data" && !activeTab.result) {
    renderStructure(activeTab, area);
    return;
  }
  let banner = "";
  if (activeTab.result) {
    const result = activeTab.result;
    const symbol = result.error ? "info" : result.empty ? "terminal" : "check";
    const message = result.error ? escapeHtml(result.error)
      : result.empty ? "Write a query and press Run to see results."
      : result.message || `${result.rows.length} rows returned · ${result.duration} ms${result.notice ? " · " + escapeHtml(result.notice) : ""}`;
    banner = `<div class="query-banner ${result.error ? "error" : ""}">
      ${icon(symbol)}<span class="grow">${message}</span>
      ${activeTab.queryTab ? "" : button("back-table", "Back to Table", "arrow", "ghost")}
    </div>`;
  }
  const editButtons = !activeTab.result && !activeTab.isView
    ? button("delete-rows", "Delete", "trash", "danger", 'id="delete-selected" aria-label="Delete selected rows" disabled')
      + button("add-row", '<span class="button-label">Add row</span>', "plus", "", 'aria-label="Add row"')
    : "";
  area.innerHTML = `${banner}<div class="toolbar">
    <div class="search-input">${icon("search")}
      <input id="table-search" aria-label="Search table"
        placeholder="Search in ${activeTab.result ? "results" : "table"}…" value="${escapeHtml(activeTab.search)}">
    </div>
    ${button("filters", `Filters${activeTab.filter ? " •" : ""}`, "filter", activeTab.filter ? "active" : "")}
    ${button("sort", "Sort", "sort")}
    ${button("reset-filters", "Reset Filters", "x", "", 'id="reset-filters" title="Reset search, filters, and sorting"')}
    ${iconButton("refresh-table", "Refresh table", "refresh")}
    <span class="spacer"></span>${editButtons}
  </div>
  <div class="table-wrap" id="table-wrap"></div>
  <div class="pagination" id="pagination"></div>`;
  findElement("#table-search").oninput = (domEvent) => {
    activeTab.search = domEvent.target.value;
    activeTab.page = 1;
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => {
      if (state.activeTabId === activeTab.id) mountGrid();
    }, SEARCH_DEBOUNCE_MS);
  };
  mountGrid();
}

function renderRemoteData(tab) {
  const area = findElement("#data-area");
  if (!area) return;
  if (!findElement("#remote-status", area)) {
    destroyGrid();
    area.innerHTML = `<div class="toolbar">
      <span id="remote-status" role="status" class="grow"></span>
      ${button("reconnect-table", "Reconnect", "database", "", 'id="reconnect-table" hidden')}
      ${iconButton("add-row", "Add row", "plus", 'id="remote-add-row"')}
      ${iconButton("refresh-table", "Refresh table", "refresh")}
      ${button("delete-rows", "Delete", "trash", "danger", 'id="delete-selected" disabled')}
    </div><div class="table-wrap" id="table-wrap"></div><div class="pagination" id="pagination"></div>`;
  }
  const readOnlyReason = tab.readOnlyReason || "Editing requires a primary key and write access.";
  findElement("#remote-status").textContent = tab.loading ? "Loading table…" : tab.loadError || (tab.editable ? "Double-click a cell to edit · Select rows to delete" : `Read-only data: ${readOnlyReason}${tab.rows.length ? "" : " This page has no rows."}`);
  findElement("#remote-status").classList.toggle("error-text", Boolean(tab.loadError));
  findElement("#reconnect-table").hidden = !tab.connectionRequired;
  findElement("#delete-selected").hidden = !tab.editable;
  findElement("#remote-add-row").hidden = Boolean(tab.queryTab || tab.isView || tab.result);
  findElement("#remote-add-row").disabled = tab.loading;
  findElement('[data-action="refresh-table"]', area).disabled = tab.loading;
  const key = JSON.stringify([tab.editable, ...tab.schema.map(column => column.name)]);
  const data = tab.rows.map((row, index) => Object.fromEntries([["_row", index], ...row.map((value, column) => ["c" + column, value])]));
  if (!tab.loading && !tab.loadError && (gridSource !== tab.rows || gridTabId !== tab.id)) {
    if (grid && gridReady && gridTabId === tab.id && gridFields === key) {
      selectedCell = null;
      grid.replaceData(data);
    } else {
      destroyGrid();
      findElement("#table-wrap").innerHTML = '<div class="table-grid"></div>';
      grid = new Tabulator(findElement("#table-wrap").firstElementChild, {
        height: "100%", layout: "fitData", index: "_row", nestedFieldSeparator: false, editTriggerEvent: "dblclick",
        data, placeholder: "No rows on this page.", rowHeight: 36,
        columnDefaults: { resizable: true, headerSort: false, minWidth: 100 },
        selectableRows: tab.editable,
        selectableRowsCheck: row => Boolean(tab.versions[row.getData()._row]),
        columns: [ ...(tab.editable ? [{ title: "", field: "_select", formatter: "rowSelection", titleFormatter: "rowSelection", width: 42, minWidth: 42, resizable: false, cellClick: (event, cell) => cell.getRow().toggleSelect() }] : []),
          ...tab.schema.map((column, index) => ({
            title: escapeHtml(column.name), field: "c" + index,
            editor: canEditRemoteCell(tab, index) ? cellEditor : false,
            editable: cell => Boolean(tab.versions[cell.getRow().getData()._row]),
            formatter: cell => formatCellValue(cell.getValue())
          })) ]
      });
      const mounted = grid;
      grid.on("tableBuilt", () => {
        if (grid !== mounted) return;
        gridReady = true;
        watchTableOverflow(mounted);
      });
      grid.on("rowSelectionChanged", data => {
        const deleteButton = findElement("#delete-selected");
        if (deleteButton) { deleteButton.disabled = !data.length; deleteButton.innerHTML = icon("trash") + "Delete" + (data.length ? " (" + data.length + ")" : ""); }
      });
      grid.on("cellEdited", cell => {
        const index = Number(cell.getField().slice(1));
        const rowIndex = cell.getRow().getData()._row;
        const original = tab.rows[rowIndex]?.[index];
        if (original === undefined || original === cell.getValue()) return;
        if (mutationPending) { onLoadTablePage(tab, tab.page); return; }
        mutateRemoteRow(tab, "edit", { row: selectedRemoteRow(tab, rowIndex), column: tab.schema[index].name, value: cell.getValue() });
      });
      grid.on("cellContext", (event, cell) => { event.preventDefault(); if (cell.getField() === "_select") return; selectedCell = cell; openCellMenu(event, cell, tab.schema[Number(cell.getField().slice(1))], canEditRemoteCell(tab, Number(cell.getField().slice(1))) && Boolean(tab.versions[cell.getRow().getData()._row])); });
    }
    gridTabId = tab.id;
    gridFields = key;
    gridSource = tab.rows;
  }
  const disabled = tab.loading ? "disabled" : "";
  const pages = tab.totalRows === null || tab.totalRows === undefined ? null : Math.max(1, Math.ceil(tab.totalRows / tab.pageSize));
  const firstRow = tab.rows.length ? (tab.page - 1) * tab.pageSize + 1 : 0;
  const lastRow = firstRow ? firstRow + tab.rows.length - 1 : 0;
  const countStatus = tab.countError
    ? `<span class="error-text" role="status">Row count unavailable: ${escapeHtml(tab.countError)}</span>${button("retry-count", "Retry count", "refresh", "ghost")}`
    : tab.countLoading ? '<span role="status">Counting rows…</span>' : "";
  const pageButtons = tab.cursorPaging || pages === null ? `<span class="pill">${tab.page}</span>` : pageNumbers(tab.page, pages).map(page =>
    page === "…" ? '<span class="page-ellipsis" aria-hidden="true">…</span>' :
      button("page", String(page), "", page === tab.page ? "active" : "", `data-page="${page}" aria-label="Page ${page}" ${page === tab.page ? 'aria-current="page"' : ""} ${disabled}`)
  ).join("");
  findElement("#pagination").innerHTML = `<select id="page-size" aria-label="Rows per page" ${disabled}>
    ${[20, 50, 100].map(size => `<option ${size === tab.pageSize ? "selected" : ""}>${size}</option>`).join("")}
    </select><span class="page-label">rows per page</span><span class="row-count">· ${firstRow}–${lastRow}${pages === null ? "" : ` of ${tab.totalRows}`}</span><span class="spacer"></span>
    ${button("page-prev", "", "chevron", "", `aria-label="Previous page" style="transform:rotate(180deg)" ${disabled} ${tab.page <= 1 ? "disabled" : ""}`)}
    ${pageButtons}
    ${button("page-next", "", "chevron", "", `aria-label="Next page" ${disabled} ${tab.cursorPaging || pages === null ? !tab.hasMore ? "disabled" : "" : tab.page >= pages ? "disabled" : ""}`)}
    ${countStatus}<span class="timing">Query: ${tab.duration ?? 0} ms</span>`;
  findElement("#page-size").onchange = event => { tab.pageSize = Number(event.target.value); onLoadTablePage(tab, 1); };
}

export function canEditRemoteCell(tab, index) {
  const column = tab.schema[index];
  const mysql = ["MySQL", "MariaDB"].includes(getConnectionById(tab.connectionId)?.engine);
  return Boolean(tab.editable && column?.editable && (mysql || !tab.primaryKey.includes(column.name)));
}

export function selectedRemoteRow(tab, index) {
  if (getConnectionById(tab.connectionId)?.engine === "SQLite" && tab.keyValues?.[index])
    return { values: tab.keyValues[index], types: tab.keyTypes[index], version: tab.versions[index] };
  const keyIndexes = tab.primaryKey.map(name => tab.schema.findIndex(column => column.name === name));
  return { values: keyIndexes.map(column => tab.rows[index][column]), version: tab.versions[index] };
}

function rowMutationSQL(tab, action, payload) {
  if (getConnectionById(tab.connectionId)?.engine === "SQLite") {
    const quote = value => '"' + value.replaceAll('"', '""') + '"';
    const table = `main.${quote(tab.table)}`;
    const keys = action === "edit" ? [payload.row] : payload.rows;
    return keys.map(() => (action === "edit" ? `UPDATE ${table} SET ${quote(payload.column)} = ?` : `DELETE FROM ${table}`)
      + ` WHERE ${tab.primaryKey.map(name => `${quote(name)} = ?`).join(" AND ")}; -- row version checked before change`).join("\n");
  }
  if (["MySQL", "MariaDB"].includes(getConnectionById(tab.connectionId)?.engine)) {
    const quote = value => "`" + value.replaceAll("`", "``") + "`";
    const table = `${quote(tab.db)}.${quote(tab.table)}`;
    const keys = action === "edit" ? [payload.row] : payload.rows;
    return keys.map(() => {
      const where = tab.primaryKey.map(name => `${quote(name)} <=> ?`);
      return (action === "edit" ? `UPDATE ${table} SET ${quote(payload.column)} = ?` : `DELETE FROM ${table}`)
        + ` WHERE ${where.join(" AND ")}; -- parameters omitted; row version checked before the change`;
    }).join("\n");
  }
  const quote = value => '"' + value.replaceAll('"', '""') + '"';
  const literal = value => value === null ? "NULL" : "E'" + String(value).replaceAll("\\", "\\\\").replaceAll("'", "''") + "'";
  const table = `${quote(tab.schemaName)}.${quote(tab.table)}`;
  const keys = action === "edit" ? [payload.row] : payload.rows;
  return keys.map(row => {
    const where = tab.primaryKey.map((name, index) => `${quote(name)} = ${literal(row.values[index])}`);
    where.push(`xmin::text = ${literal(row.version)}`);
    return (action === "edit" ? `UPDATE ${table} SET ${quote(payload.column)} = ${literal(payload.value)}` : `DELETE FROM ${table}`)
      + ` WHERE ${where.join(" AND ")} RETURNING 1;`;
  }).join("\n");
}

export async function mutateRemoteRow(tab, action, payload) {
  if (mutationPending) return false;
  mutationPending = true;
  try {
    const result = await runRecordedSQL("/connections/" + encodeURIComponent(tab.connectionId) + "/rows/" + action, {
      method: "POST", body: { database: tab.db, schema: tab.schemaName, table: tab.table, ...payload }
    }, { connectionId: tab.connectionId, db: tab.db, schemaName: tab.schemaName }, rowMutationSQL(tab, action, payload));
    toast(`${result.affectedRows} ${result.affectedRows === 1 ? "row" : "rows"} ${action === "edit" ? "updated" : "deleted"}.`);
    onLoadTablePage(tab, tab.page, true);
    return true;
  } catch (error) {
    toast(error.message);
    onLoadTablePage(tab, tab.page);
    return false;
  } finally { mutationPending = false; }
}

async function insertDefaultRow(tab) {
  if (mutationPending) return;
  mutationPending = true;
  const mysql = ["MySQL", "MariaDB"].includes(getConnectionById(tab.connectionId)?.engine);
  const table = mysql
    ? "`" + tab.db.replaceAll("`", "``") + "`.`" + tab.table.replaceAll("`", "``") + "`"
    : getConnectionById(tab.connectionId)?.engine === "SQLite" ? 'main."' + tab.table.replaceAll('"', '""') + '"'
    : '"' + tab.schemaName.replaceAll('"', '""') + '"."' + tab.table.replaceAll('"', '""') + '"';
  try {
    const result = await runRecordedSQL("/connections/" + encodeURIComponent(tab.connectionId) + "/rows/insert", {
      method: "POST", body: { database: tab.db, schema: tab.schemaName, table: tab.table, values: {} }
    }, { connectionId: tab.connectionId, db: tab.db, schemaName: tab.schemaName }, mysql ? `INSERT INTO ${table} () VALUES ();` : `INSERT INTO ${table} DEFAULT VALUES;`);
    if (Number.isSafeInteger(tab.totalRows)) tab.totalRows++;
    const key = tab.primaryKey?.length === 1 && tab.schema.find(column => column.name === tab.primaryKey[0]);
    const lastPage = key && /^(?:smallint|integer|bigint|numeric)(?:\(|$)/.test(key.type) && Number.isSafeInteger(tab.totalRows);
    const targetPage = lastPage ? Math.max(1, Math.ceil(tab.totalRows / tab.pageSize)) : 1;
    await onLoadTablePage(tab, tab.cursorPaging && !tab.pageCursors?.[targetPage] ? 1 : targetPage);
    if (!mysql && !tab.loadError && result.row && state.tabs.includes(tab)) {
      const visible = tab.rows.some((row, index) => tab.versions[index] === result.version && row.every((value, column) => value === result.row[column]));
      if (!visible) {
        tab.rows = [result.row, ...tab.rows].slice(0, tab.pageSize);
        tab.versions = [result.version, ...tab.versions].slice(0, tab.pageSize);
        tab.truncated = [result.row.map(() => false), ...(tab.truncated || [])].slice(0, tab.pageSize);
        if (state.activeTabId === tab.id) renderRemoteData(tab);
      }
    }
    toast("Row added.");
  } catch (error) {
    toast(error.code === "not_null_violation" ? "A required column has no default. Add a default or use the query console to provide a value." : error.message);
  } finally { mutationPending = false; }
}

function mountGrid() {
  const activeTab = getActiveTab();
  if (!activeTab) return;
  const reset = findElement("#reset-filters");
  if (reset) reset.disabled = !activeTab.search && !activeTab.filter && !activeTab.sorts.length;
  const wrap = findElement("#table-wrap");
  if (!wrap) return;
  if (activeTab.result?.empty || activeTab.result?.error || activeTab.result?.message) {
    if (grid) {
      grid.destroy();
      grid = null;
    }
    gridTabId = null;
    wrap.innerHTML = `<div class="result-empty">${icon(activeTab.result.error ? "info" : activeTab.result.empty ? "terminal" : "check")}<span>${activeTab.result.error ? "Fix the query and try again." : activeTab.result.empty ? "Your results will appear here." : "Statement completed."}</span></div>`;
    findElement("#pagination").innerHTML = "";
    return;
  }
  const source = activeTab.result ? activeTab.result.rows : activeTab.rows;
  const sorts = JSON.stringify(activeTab.sorts);
  // Pagination memakai hasil yang sama; edit sel dan renderData membatalkan cache ini.
  if (gridSource !== source || gridSearch !== activeTab.search || gridFilter !== activeTab.filter || gridSorts !== sorts || !gridRows) {
    gridRows = getFilteredRows(activeTab);
    gridSource = source;
    gridSearch = activeTab.search;
    gridFilter = activeTab.filter;
    gridSorts = sorts;
  }
  const rows = gridRows;
  const pages = Math.max(1, Math.ceil(rows.length / activeTab.pageSize));
  activeTab.page = Math.min(activeTab.page, pages);
  const paged = rows.slice((activeTab.page - 1) * activeTab.pageSize, activeTab.page * activeTab.pageSize);
  const fields = activeTab.result?.columns || Object.keys(source[0] ?? Object.fromEntries(activeTab.schema.map((column) => [column.name, null])));
  const editable = !activeTab.result && !activeTab.queryTab && !activeTab.isView;
  const fieldKey = fields.join("\0");
  const columnTitle = (field) => {
    const priority = activeTab.sorts.findIndex((sort) => sort.field === field);
    if (priority < 0) return escapeHtml(field);
    const arrow = activeTab.sorts[priority].dir === "asc" ? "↑" : "↓";
    return `${escapeHtml(field)}<span class="sort-priority">${priority + 1} ${arrow}</span>`;
  };
  // Pertahankan instance selama tab, kolom, dan kemampuan edit belum berubah.
  if (grid && gridReady && gridTabId === activeTab.id && gridFields === fieldKey && gridEditable === editable) {
    grid.deselectRow();
    selectedCell = null;
    grid.replaceData(structuredClone(paged));
    for (const field of fields) {
      const title = grid.getColumn(field)?.getElement()?.querySelector(".tabulator-col-title");
      if (title) title.innerHTML = columnTitle(field);
    }
  } else {
    if (grid) grid.destroy();
    gridReady = false;
    wrap.innerHTML = '<div class="table-grid"></div>';
    gridTabId = activeTab.id;
    gridFields = fieldKey;
    gridEditable = editable;
    const columns = [{
      title: "", field: "_select", formatter: "rowSelection",
      titleFormatter: "rowSelection", hozAlign: "center",
      headerSort: false, width: 42, minWidth: 42, resizable: false,
      cellClick: (event, cell) => cell.getRow().toggleSelect()
    }, ...fields.map((field) => ({
      title: field, field,
      titleFormatter: () => columnTitle(field),
      headerClick: () => {
        activeTab.sorts = toggleColumnSort(activeTab.sorts, field);
        activeTab.page = 1;
        mountGrid();
      },
      width: field === "id" ? 65 : undefined,
      minWidth: field === "id" ? 65 : field === "email" ? 225 : field === "created_at" ? 175 : 140,
      editor: editable && field !== "id" ? cellEditor : false,
      formatter: cell => formatCellValue(cell.getValue())
    }))];
    grid = new Tabulator(wrap.firstElementChild, {
      height: "100%", autoResize: true, data: structuredClone(paged),
      index: "id", layout: "fitColumns",
      placeholder: "No rows match your search or filters.",
      rowHeight: 36, editTriggerEvent: "dblclick", selectableRows: true,
      columnDefaults: { resizable: true, headerSort: false, minWidth: 100 },
      columns
    });
    const mountedGrid = grid;
    grid.on("tableBuilt", () => {
      if (grid !== mountedGrid) return;
      gridReady = true;
      watchTableOverflow(mountedGrid);
    });
    grid.on("rowSelectionChanged", (data) => {
      const deleteButton = findElement("#delete-selected");
      if (deleteButton) {
        deleteButton.disabled = !data.length;
        deleteButton.innerHTML = icon("trash") + "Delete" + (data.length ? " (" + data.length + ")" : "");
      }
    });
    grid.on("cellEdited", (cell) => {
      const candidateRow = activeTab.rows.find((row) => row.id === cell.getRow().getData().id);
      if (candidateRow) {
        candidateRow[cell.getField()] = cell.getValue();
        gridRows = null;
        toast("1 row updated.");
      }
    });
    grid.on("cellContext", (domEvent, cell) => {
      domEvent.preventDefault();
      if (cell.getField() !== "_select") {
        selectedCell = cell;
        openCellMenu(domEvent, cell, activeTab.schema.find(column => column.name === cell.getField()), editable && cell.getField() !== "id");
      }
    });
  }
  const pageButtons = pageNumbers(activeTab.page, pages).map((page) => {
    if (page === "…") return '<span class="page-ellipsis" aria-hidden="true">…</span>';
    const attributes = `data-page="${page}" aria-label="Page ${page}" ${page === activeTab.page ? 'aria-current="page"' : ""}`;
    return button("page", String(page), "", page === activeTab.page ? "active" : "", attributes);
  }).join("");
  const pageSizes = [20, 50, 100].map((size) =>
    `<option ${size === activeTab.pageSize ? "selected" : ""}>${size}</option>`
  ).join("");
  const firstRow = rows.length ? (activeTab.page - 1) * activeTab.pageSize + 1 : 0;
  const lastRow = Math.min(activeTab.page * activeTab.pageSize, rows.length);
  findElement("#pagination").innerHTML = `
    <select id="page-size" aria-label="Rows per page">${pageSizes}</select>
    <span class="page-label">rows per page</span>
    <span class="row-count">· ${firstRow}–${lastRow} of ${rows.length}</span>
    <span class="spacer"></span>
    ${button("page-prev", "", "chevron", "", `aria-label="Previous page" style="transform:rotate(180deg)" ${activeTab.page === 1 ? "disabled" : ""}`)}
    ${pageButtons}
    ${button("page-next", "", "chevron", "", `aria-label="Next page" ${activeTab.page === pages ? "disabled" : ""}`)}
    <span class="timing" style="margin-left:9px">Query: ${activeTab.result?.duration ?? 24} ms</span>`;
  findElement("#page-size").onchange = (domEvent) => {
    activeTab.pageSize = Number(domEvent.target.value);
    activeTab.page = 1;
    mountGrid();
  };
}

function watchTableOverflow(table) {
  const holder = table.element.querySelector(".tabulator-tableholder");
  const content = holder.querySelector(".tabulator-table");
  let frame;
  const update = () => {
    cancelAnimationFrame(frame);
    frame = requestAnimationFrame(() => {
      const overflow = holder.scrollWidth - holder.clientWidth > 1;
      holder.style.overflowX = overflow ? "auto" : "hidden";
    });
  };
  const observer = new ResizeObserver(update);
  observer.observe(holder);
  observer.observe(content);
  table.on("renderComplete", update);
  table.on("columnResized", update);
  table.on("tableDestroyed", () => {
    observer.disconnect();
    cancelAnimationFrame(frame);
  });
  update();
}

// -----------------------------------------------------------------------------
// Row processing
// -----------------------------------------------------------------------------
export function getFilteredRows(activeTab) {
  let rows = activeTab.result ? activeTab.result.rows : activeTab.rows;
  if (activeTab.search) {
    const term = activeTab.search.toLowerCase();
    rows = rows.filter((candidateRow) => Object.values(candidateRow).some((cellValue) => cellValue !== null && String(cellValue).toLowerCase().includes(term)));
  }
  if (activeTab.filter) rows = rows.filter((candidateRow) => matchesGroup(candidateRow, activeTab.filter));
  if (activeTab.sorts.length) rows = rows.toSorted((leftRow, rightRow) => {
    for (const sort of activeTab.sorts) {
      const comparison = collator.compare(String(leftRow[sort.field] ?? ""), String(rightRow[sort.field] ?? ""));
      if (comparison) return sort.dir === "asc" ? comparison : -comparison;
    }
    return 0;
  });
  return rows;
}

// -----------------------------------------------------------------------------
// Pagination and filter matching
// -----------------------------------------------------------------------------
const initialFilter = () => ({ mode: "AND", children: [{ field: "status", op: "=", value: "active" }] });

export function pageNumbers(page, total) {
  const start = Math.max(1, Math.min(page - 2, total - 4));
  const numbers = [...new Set([1, ...Array.from({ length: Math.min(5, total) }, (_, itemIndex) => start + itemIndex), total])];
  return numbers.flatMap((number, itemIndex) => itemIndex && number - numbers[itemIndex - 1] > 1 ? ["…", number] : [number]);
}

export function toggleColumnSort(sorts, field) {
  const index = sorts.findIndex((sort) => sort.field === field);
  if (index === -1) return [...sorts, { field, dir: "asc" }];
  if (sorts[index].dir === "desc") return sorts.filter((_, itemIndex) => itemIndex !== index);
  return sorts.map((sort, itemIndex) => itemIndex === index ? { ...sort, dir: "desc" } : sort);
}

export function matchesGroup(row, group) {
  const check = (child) => {
    if (child.children) return matchesGroup(row, child);
    const value = row[child.field];
    if (child.op === "IS NULL") return value === null;
    if (child.op === "IS NOT NULL") return value !== null;
    if (value === null) return false;
    const actualValue = String(value).toLowerCase();
    const expectedValue = String(child.value).toLowerCase();
    if (child.op === "contains") return actualValue.includes(expectedValue);
    if (child.op === "!=") return actualValue !== expectedValue;
    if (child.op === ">") return Number(actualValue) > Number(expectedValue);
    if (child.op === "<") return Number(actualValue) < Number(expectedValue);
    return actualValue === expectedValue;
  };
  return group.mode === "AND" ? group.children.every(check) : group.children.some(check);
}

// -----------------------------------------------------------------------------
// Cell editing
// -----------------------------------------------------------------------------
export function formatCellValue(value) {
  if (value === null) return '<span class="null-value">NULL</span>';
  const preview = String(value);
  return escapeHtml(preview.length > 120 ? preview.slice(0, 120) + "..." : preview);
}

export function cellEditor(cell, onRendered, success, cancel) {
  const tab = getActiveTab();
  const columnIndex = tab.remote ? Number(cell.getField().slice(1)) : -1;
  const column = tab.remote ? tab.schema[columnIndex] : tab.schema.find((item) => item.name === cell.getField());
  const rowIndex = tab.remote ? cell.getRow().getData()._row : -1;
  const needsFullValue = Boolean(tab.remote && tab.truncated?.[rowIndex]?.[columnIndex]);
  const multiline = /TEXT|CHAR|CLOB|BYTEA|BLOB|JSON|XML/i.test(column?.type ?? "") || String(cell.getValue() ?? "").length > 120;
  const input = document.createElement(multiline ? "textarea" : "input");
  if (multiline) input.className = "multiline-cell-editor";
  input.style?.setProperty("background", "#fff", "important");
  let original = cell.getValue() ?? "";
  input.value = needsFullValue ? "Loading full value…" : original;
  input.disabled = needsFullValue;
  input.setAttribute("aria-label", column?.name + (multiline ? " — Alt+Enter for a new line" : " — Enter to save"));
  let finished = false;
  const finish = (save) => {
    if (finished) return;
    finished = true;
    if (save && input.value !== String(original)) success(input.value);
    else cancel();
  };
  onRendered(() => {
    if (!needsFullValue) {
      input.focus({ preventScroll: true });
      input.select();
      return;
    }
    apiRequest("/connections/" + encodeURIComponent(tab.connectionId) + "/rows/cell", {
      method: "POST", body: { database: tab.db, schema: tab.schemaName, table: tab.table, column: column.name, row: selectedRemoteRow(tab, rowIndex) }
    }).then(result => {
      if (finished) return;
      original = result.value ?? "";
      input.value = original;
      input.disabled = false;
      input.focus({ preventScroll: true });
      input.select();
    }).catch(error => { if (!finished) { finish(false); toast(error.message); } });
  });
  input.addEventListener("keydown", (event) => {
    event.stopPropagation();
    if (event.key === "Enter" && multiline && (event.altKey || event.shiftKey)) {
      event.preventDefault();
      input.setRangeText("\n", input.selectionStart, input.selectionEnd, "end");
    } else if (event.key === "Enter") {
      event.preventDefault();
      finish(true);
    } else if (event.key === "Escape") {
      event.preventDefault();
      finish(false);
    } else if (event.key === "Tab") {
      event.preventDefault();
      finish(true);
      if (event.shiftKey) cell.navigatePrev();
      else cell.navigateNext();
    }
  });
  input.addEventListener("blur", () => finish(false));
  return input;
}

export function getCellMenuActions(column, editable) {
  const actions = [];
  if (editable) {
    const numeric = /^(tinyint|smallint|mediumint|int|integer|bigint|numeric|decimal|float|real|double|money|boolean)\b/i.test(column?.type ?? "");
    const binary = /^(?:tinyblob|blob|mediumblob|longblob|binary|varbinary)\b/i.test(column?.type ?? "");
    actions.push({ action: "cell-empty", label: numeric ? "Set 0" : binary ? "Set Empty Binary" : "Set Empty String", value: numeric ? "0" : binary ? "\\x" : "" });
    if (column?.nullable) actions.push({ action: "cell-null", label: "Set NULL", value: null });
  }
  actions.push({ action: "cell-copy", label: "Copy" });
  return actions;
}

function closeCellMenu() {
  if (!cellContextMenu) return;
  cellContextMenu.remove();
  cellContextMenu = null;
  document.removeEventListener("click", closeCellMenu);
  document.removeEventListener("keydown", closeCellMenuOnEscape);
  window.removeEventListener("blur", closeCellMenu);
  window.removeEventListener("resize", closeCellMenu);
}

function closeCellMenuOnEscape(event) {
  if (event.key === "Escape") closeCellMenu();
}

function openCellMenu(event, cell, column, editable) {
  closeCellMenu();
  selectedCell = cell;
  const menu = document.createElement("div");
  menu.id = "cell-context-menu";
  menu.className = "context-menu";
  menu.innerHTML = getCellMenuActions(column, editable).map(item => button(item.action, item.label)).join("");
  document.body.append(menu);
  menu.style.left = Math.max(0, Math.min(event.clientX, window.innerWidth - menu.offsetWidth)) + "px";
  menu.style.top = Math.max(0, Math.min(event.clientY, window.innerHeight - menu.offsetHeight)) + "px";
  cellContextMenu = menu;
  document.addEventListener("click", closeCellMenu);
  document.addEventListener("keydown", closeCellMenuOnEscape);
  window.addEventListener("blur", closeCellMenu);
  window.addEventListener("resize", closeCellMenu);
  menu.querySelector("button")?.focus();
}

function setLocalCell(value) {
  const activeTab = getActiveTab();
  const row = activeTab.rows.find((candidateRow) => candidateRow.id === selectedCell.getRow().getData().id);
  if (row) {
    row[selectedCell.getField()] = value;
  }
  renderData();
  toast("1 row updated.");
}

// -----------------------------------------------------------------------------
// Filter and sort dialogs
// -----------------------------------------------------------------------------
function openFilterDialog() {
  const activeTab = getActiveTab();
  filterDraft = structuredClone(activeTab.filter ?? initialFilter());
  const body = `<p class="hint">Combine conditions using AND / OR. Add groups to build nested expressions.</p>
    <div id="filter-builder"></div>`;
  const footer = button("clear-filters", "Clear Filters") + '<span class="spacer"></span>'
    + button("close-dialog", "Cancel") + button("apply-filters", "Apply Filters", "filter", "primary");
  showDialog("Filter rows", body, footer, true);
  renderFilterForm();
}

function getFilterGroup(path) {
  return path ? path.split(".").reduce((group, index) => group.children[Number(index)], filterDraft) : filterDraft;
}

function renderFilterForm() {
  const fields = getActiveTab().schema.map((column) => column.name);
  function groupHTML(group, path = "") {
    const children = group.children.map((child, index) => {
      const next = path ? path + "." + index : String(index);
      if (child.children) return groupHTML(child, next);
      const columns = fields.map((field) =>
        `<option ${field === child.field ? "selected" : ""}>${escapeHtml(field)}</option>`
      ).join("");
      const operators = ["=", "!=", "contains", ">", "<", "IS NULL", "IS NOT NULL"];
      const options = operators.map((operator) =>
        `<option ${operator === child.op ? "selected" : ""}>${escapeHtml(operator)}</option>`
      ).join("");
      return `<div class="filter-condition" data-condition="${next}">
        <select data-field="field" aria-label="Filter column">${columns}</select>
        <select data-field="op" aria-label="Filter operator">${options}</select>
        <input data-field="value" aria-label="Filter value" value="${escapeHtml(child.value)}"
          placeholder="Value" ${child.op.includes("NULL") ? "disabled" : ""}>
        ${iconButton("filter-remove", "Remove condition", "x", `data-path="${next}"`)}
      </div>`;
    }).join("");
    return `<div class="filter-group">
      <header><span>Match</span>
        <select data-filter-mode="${path}" aria-label="Group operator">
          <option ${group.mode === "AND" ? "selected" : ""}>AND</option>
          <option ${group.mode === "OR" ? "selected" : ""}>OR</option>
        </select>
        <span>conditions</span><span class="spacer"></span>
        ${button("filter-add", "Condition", "plus", "", `data-path="${path}"`)}
        ${button("filter-group", "Group", "plus", "", `data-path="${path}"`)}
        ${path ? iconButton("filter-remove", "Remove group", "x", `data-path="${path}"`) : ""}
      </header>${children}
    </div>`;
  }
  findElement("#filter-builder").innerHTML = groupHTML(filterDraft);
  findElements("[data-filter-mode]").forEach((element) => element.onchange = () => getFilterGroup(element.dataset.filterMode).mode = element.value);
  findElements("[data-condition] [data-field]").forEach((element) => element.onchange = () => {
    const condition = getFilterGroup(element.parentElement.dataset.condition);
    condition[element.dataset.field] = element.value;
    if (element.dataset.field === "op") renderFilterForm();
  });
}

function openSortDialog() {
  const activeTab = getActiveTab();
  let sorts = structuredClone(activeTab.sorts.length ? activeTab.sorts : [{ field: "id", dir: "asc" }]);
  const draw = () => {
    const rules = sorts.map((sort, index) => {
      const columns = activeTab.schema.map((column) =>
        `<option ${sort.field === column.name ? "selected" : ""}>${escapeHtml(column.name)}</option>`
      ).join("");
      return `<div class="row">
        <select data-sort-index="${index}" data-sort-field="field"
          aria-label="Sort column" class="grow">${columns}</select>
        <select data-sort-index="${index}" data-sort-field="dir" aria-label="Sort direction">
          <option value="asc" ${sort.dir === "asc" ? "selected" : ""}>Ascending</option>
          <option value="desc" ${sort.dir === "desc" ? "selected" : ""}>Descending</option>
        </select>
        <button class="icon ghost" type="button" data-remove-sort="${index}"
          aria-label="Remove sort">${icon("x")}</button>
      </div>`;
    }).join("");
    const body = `<p class="hint">Rules are applied in order. Click column headers to add sorting rules in order. Click again for descending, then once more to remove the rule.</p>
      <div id="sort-rules" class="stack" style="margin-top:17px">
        ${rules}${button("add-sort", "Add sort rule", "plus")}
      </div>`;
    const footer = button("close-dialog", "Cancel") + button("apply-sort", "Apply Sort", "", "primary");
    showDialog("Sort rows", body, footer);
    findElements("[data-sort-index]").forEach((element) => element.onchange = () => sorts[Number(element.dataset.sortIndex)][element.dataset.sortField] = element.value);
    findElements("[data-remove-sort]").forEach((element) => element.onclick = () => {
      sorts.splice(Number(element.dataset.removeSort), 1);
      draw();
    });
    findElement('[data-action="add-sort"]').onclick = () => {
      sorts.push({ field: activeTab.schema.find((column) => !sorts.some((sort) => sort.field === column.name))?.name || "id", dir: "asc" });
      draw();
    };
    findElement('[data-action="apply-sort"]').onclick = () => {
      activeTab.sorts = sorts;
      activeTab.page = 1;
      closeDialog();
      mountGrid();
    };
  };
  draw();
}

// -----------------------------------------------------------------------------
// User actions
// -----------------------------------------------------------------------------
export async function handleGridAction(action, element) {
  const activeTab = getActiveTab();
  if (["cell-empty", "cell-null", "cell-copy"].includes(action)) {
    if (!selectedCell || !activeTab) return true;
    const cell = selectedCell;
    closeCellMenu();
    if (action === "cell-copy") {
      try {
        await navigator.clipboard.writeText(cell.getValue() === null ? "NULL" : String(cell.getValue()));
        toast("Cell copied.");
      } catch { toast("Clipboard access is unavailable."); }
      return true;
    }
    const index = activeTab.remote ? Number(cell.getField().slice(1)) : -1;
    const column = activeTab.remote ? activeTab.schema[index] : activeTab.schema.find(item => item.name === cell.getField());
    const editable = activeTab.remote ? canEditRemoteCell(activeTab, index) : !activeTab.result && !activeTab.queryTab && !activeTab.isView && cell.getField() !== "id";
    const selected = getCellMenuActions(column, editable).find(item => item.action === action);
    if (!selected) return true;
    if (activeTab.remote) {
      const row = selectedRemoteRow(activeTab, cell.getRow().getData()._row);
      await mutateRemoteRow(activeTab, "edit", { row, column: column.name, value: selected.value });
    } else setLocalCell(selected.value);
    return true;
  }
  if (activeTab?.remote && !activeTab.queryTab) {
    if (action === "retry-count") { onRetryTableCount?.(activeTab); return true; }
    if (["refresh-table", "page-prev", "page-next", "page", "reconnect-table"].includes(action)) {
      if (activeTab.loading) return true;
      if (action === "reconnect-table") onReconnectTable(activeTab);
      else onLoadTablePage(activeTab, action === "page-prev" ? Math.max(1, activeTab.page - 1) : action === "page-next" ? activeTab.page + 1 : action === "page" ? Number(element.dataset.page) : activeTab.page, action === "refresh-table");
      return true;
    }
    if (action === "delete-rows") {
      const selected = grid?.getSelectedData() ?? [];
      if (!activeTab.editable || !selected.length) return true;
      const rows = selected.map(row => selectedRemoteRow(activeTab, row._row));
      confirmAction("Delete selected rows?", `Delete <strong>${rows.length}</strong> selected rows from <strong>${escapeHtml(activeTab.table)}</strong>? This cannot be undone. Database cascades or triggers may affect other rows.`, "Delete Rows", () => mutateRemoteRow(activeTab, "delete", { rows }));
      return true;
    }
    if (action === "add-row") { if (!activeTab.queryTab && !activeTab.isView && !activeTab.result) await insertDefaultRow(activeTab); return true; }
    if (["filters", "sort"].includes(action)) {
      toast("This action is not available for remote tables yet.");
      return true;
    }
  }
  switch (action) {
    case "back-table":
      activeTab.result = null;
      activeTab.search = "";
      activeTab.filter = null;
      activeTab.sorts = [];
      activeTab.page = 1;
      renderData();
      break;
    case "refresh-table":
      activeTab.result = null;
      activeTab.search = "";
      activeTab.page = 1;
      renderData();
      toast("Table refreshed.");
      break;
    case "page-prev":
      activeTab.page--;
      mountGrid();
      break;
    case "page-next":
      activeTab.page++;
      mountGrid();
      break;
    case "page":
      activeTab.page = Number(element.dataset.page);
      mountGrid();
      break;
    case "add-row": {
      const row = Object.fromEntries(activeTab.schema.map((column) => [column.name, column.name === "id" ? String(Math.max(0, ...activeTab.rows.map((candidateRow) => Number(candidateRow.id))) + 1) : null]));
      activeTab.rows.unshift(row);
      activeTab.search = "";
      activeTab.filter = null;
      activeTab.sorts = [];
      activeTab.page = 1;
      renderData();
      toast("1 row inserted. Double-click a cell to enter a value.");
      break;
    }
    case "delete-rows": {
      const rows = grid?.getSelectedData() ?? [];
      if (!rows.length) {
        toast("Select one or more rows first.");
        break;
      }
      const ids = new Set(rows.map((row) => row.id));
      confirmAction("Delete selected rows?", `Delete <strong>${ids.size}</strong> selected ${ids.size === 1 ? "row" : "rows"} from <strong>${escapeHtml(activeTab.table)}</strong>? This action cannot be undone.`, "Delete Rows", () => {
        const before = activeTab.rows.length;
        activeTab.rows = activeTab.rows.filter((row) => !ids.has(row.id));
        const count = before - activeTab.rows.length;
        renderData();
        toast(`${count} ${count === 1 ? "row" : "rows"} deleted.`);
      });
      break;
    }
    case "filters":
      openFilterDialog();
      break;
    case "filter-add":
      getFilterGroup(element.dataset.path).children.push({ field: activeTab.schema[0].name, op: "=", value: "" });
      renderFilterForm();
      break;
    case "filter-group":
      getFilterGroup(element.dataset.path).children.push({ mode: "OR", children: [{ field: activeTab.schema[0].name, op: "=", value: "" }] });
      renderFilterForm();
      break;
    case "filter-remove": {
      const parts = element.dataset.path.split(".");
      const index = Number(parts.pop());
      getFilterGroup(parts.join(".")).children.splice(index, 1);
      renderFilterForm();
      break;
    }
    case "apply-filters":
      activeTab.filter = structuredClone(filterDraft);
      activeTab.page = 1;
      closeDialog();
      renderData();
      break;
    case "clear-filters":
      activeTab.filter = null;
      activeTab.page = 1;
      closeDialog();
      renderData();
      break;
    case "sort":
      openSortDialog();
      break;
    case "reset-filters":
      activeTab.search = "";
      activeTab.filter = null;
      activeTab.sorts = [];
      activeTab.page = 1;
      renderData();
      break;
    default:
      return false;
  }
  return true;
}
