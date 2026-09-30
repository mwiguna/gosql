import { apiRequest } from "../api.js";
import { getActiveTab, getConnectionById } from "../state.js";
import { getConnectionCatalog } from "./connections.js";
import { button, closeDialog, confirmAction, escapeHtml, findElement, findElements, icon, iconButton, showDialog, toast } from "../ui.js";
import { runRecordedSQL } from "../storage.js";
import { columnTypes, columnTypeSQL, lengthTypes, numericTypes, mysqlTypeFields, mysqlTypeSQL, updateMySQLTypeFields } from "./column_types.js";

let refreshTable;

const quoteIdentifier = name => '"' + name.replaceAll('"', '""') + '"';
const mysql = tab => ["MySQL", "MariaDB"].includes(getConnectionById(tab.connectionId)?.engine);
const mariaDB = tab => getConnectionById(tab.connectionId)?.engine === "MariaDB";
const sqlite = tab => getConnectionById(tab.connectionId)?.engine === "SQLite";
const mysqlIdentifier = name => "`" + name.replaceAll("`", "``") + "`";
const quoted = (tab, name) => mysql(tab) ? mysqlIdentifier(name) : quoteIdentifier(name);
const tableName = tab => mysql(tab)
  ? `${mysqlIdentifier(tab.db)}.${mysqlIdentifier(tab.table)}`
  : sqlite(tab) ? `main.${quoteIdentifier(tab.table)}` : `${quoteIdentifier(tab.schemaName)}.${quoteIdentifier(tab.table)}`;
const target = tab => `/connections/${encodeURIComponent(tab.connectionId)}`;
const locationData = tab => ({ database: tab.db, schema: tab.schemaName, table: tab.table });
const quoteLiteral = value => "E'" + value.replaceAll("\\", "\\\\").replaceAll("'", "''") + "'";

function columnChangeSQL(tab, value, editing) {
  const table = tableName(tab), name = quoteIdentifier(value.name);
  if (!editing) return `ALTER TABLE ${table} ADD COLUMN ${name} ${value.type}${value.changeDefault && value.default !== null ? " DEFAULT " + quoteLiteral(value.default) : ""}${value.nullable === false ? " NOT NULL" : ""};`;
  const statements = [];
  const old = quoteIdentifier(value.oldName);
  if (value.name !== value.oldName) statements.push(`ALTER TABLE ${table} RENAME COLUMN ${old} TO ${name};`);
  if (value.changeDefault && value.type) statements.push(`ALTER TABLE ${table} ALTER COLUMN ${name} DROP DEFAULT;`);
  if (value.type) statements.push(`ALTER TABLE ${table} ALTER COLUMN ${name} TYPE ${value.type};`);
  if (value.nullable !== null) statements.push(`ALTER TABLE ${table} ALTER COLUMN ${name} ${value.nullable ? "DROP" : "SET"} NOT NULL;`);
  if (value.changeDefault) statements.push(`ALTER TABLE ${table} ALTER COLUMN ${name} ${value.default === null ? "DROP DEFAULT" : "SET DEFAULT " + quoteLiteral(value.default)};`);
  return statements.join("\n");
}

// -----------------------------------------------------------------------------
// Metadata display
// -----------------------------------------------------------------------------
export function renderStructure(tab, area) {
  if (tab.loading || tab.loadError) {
    area.innerHTML = `<div class="result-empty" role="status">${escapeHtml(tab.loadError || "Loading structure…")}</div>`;
    return;
  }
  const mode = tab.view;
  const protectedDatabase = mysql(tab) && ["mysql", "information_schema", "performance_schema", "sys"].includes(tab.db.toLowerCase());
  const fields = mode === "Structure" ? ["Column", "Type", "Nullable", "Default", "Key"]
    : mode === "Indexes" ? ["Name", "Definition", "Unique"] : ["Name", "Definition", "Type"];
  const values = mode === "Structure" ? tab.schema : mode === "Indexes" ? tab.indexes : tab.constraints;
  const kind = mode === "Structure" ? "column" : mode === "Indexes" ? "index" : "constraint";
  const actions = (mode !== "Structure" || !tab.isView) && !protectedDatabase && !tab.sqliteVirtual;
  const rows = values.map((value, index) => {
    const cells = mode === "Structure"
      ? [escapeHtml(value.name) + (value.generated ? `<small> · ${escapeHtml(value.generated)} generated</small>` : value.hidden ? "<small> · hidden</small>" : ""), `<code>${escapeHtml(value.type)}</code>${sqlite(tab) ? `<small> · ${escapeHtml(value.affinity)} affinity</small>` : ""}`, value.nullable ? "Yes" : "No", `<code>${escapeHtml(value.default)}</code>${value.defaultSource ? `<small> · ${escapeHtml(value.defaultSource)}</small>` : ""}`, value.key === "PRIMARY KEY" ? icon("key") + ` Primary${value.primaryOrder ? ` #${value.primaryOrder}` : ""}` : escapeHtml(value.key)]
      : mode === "Indexes" ? [escapeHtml(value.name), `<code>${escapeHtml(value.definition)}</code>`, value.unique ? "Yes" : "No"]
        : [escapeHtml(value.name), `<code>${escapeHtml(value.definition)}</code>`, escapeHtml(value.type) + (value.validated === false ? " · Not validated" : "")];
    const editable = actions && !tab.isView && (mode === "Structure" ? !sqlite(tab) || value.key !== "ROWID" && value.key !== "PRIMARY KEY"
      : mode === "Indexes" ? value.editable && !value.managed && !value.primary : !sqlite(tab) && ["UNIQUE", "FOREIGN KEY", "CHECK", "PRIMARY KEY"].includes(value.type));
    const canEditDefinition = !((mysql(tab) || sqlite(tab)) && mode === "Structure") || value.definitionEditable;
    const validate = !mysql(tab) && kind === "constraint" && value.type === "FOREIGN KEY" && value.validated === false
      ? iconButton("schema-validate", "Validate " + value.name, "check", `data-kind="${kind}" data-index="${index}"`) : "";
    const controls = editable ? validate + (canEditDefinition && !(sqlite(tab) && mode === "Indexes") ? iconButton("schema-edit", "Edit " + value.name, "edit", `data-kind="${kind}" data-index="${index}"`) : "")
      + iconButton("schema-delete", "Delete " + value.name, "trash", `data-kind="${kind}" data-index="${index}"`) : "";
    return `<tr>${cells.map(cell => `<td>${cell}</td>`).join("")}${actions ? `<td><div class="row">${controls}</div></td>` : ""}</tr>`;
  }).join("") || `<tr><td colspan="${fields.length + Number(actions)}">No ${mode.toLowerCase()} yet.</td></tr>`;
  const add = mode === "Structure" ? button("schema-add", "Add column", "plus", "primary", 'data-kind="column"')
    : mode === "Indexes" ? button("schema-add", "Add index", "plus", "primary", 'data-kind="index"')
    : mode === "Constraints" ? button("schema-add", "Add constraint", "plus", "primary", 'data-kind="constraint"') : "";
  area.innerHTML = `<div class="toolbar"><div><h3>${mode === "Structure" ? "Table structure" : mode}</h3>
    <p class="hint" style="margin-top:5px">${mode === "Structure" ? "Columns and data types for " + escapeHtml(tab.table) + (sqlite(tab) ? ` · ${tab.sqliteStrict ? "STRICT · " : ""}${tab.sqliteWithoutRowid ? "WITHOUT ROWID" : "rowid"}. SQLite affinity does not enforce a length limit.` : "") : mode === "Indexes" ? "Standard and unique indexes, including indexes managed by constraints." : "Rules that keep table data consistent."}</p></div>
    <span class="spacer"></span>${tab.isView || protectedDatabase || tab.sqliteVirtual ? "" : add}</div>
    <div class="schema-scroll"><table class="schema-table"><thead><tr>${fields.map(field => `<th>${field}</th>`).join("")}${actions ? "<th></th>" : ""}</tr></thead><tbody>${rows}</tbody></table></div>
    ${sqlite(tab) && mode === "Structure" && tab.sqliteDDL ? `<details><summary>SQLite definition</summary><pre class="sql-preview">${escapeHtml(tab.sqliteDDL)}</pre></details>` : ""}
    <div class="metadata-foot"><span>DATABASE<strong>${escapeHtml(tab.db)}</strong></span><span>ENGINE<strong>${escapeHtml(getConnectionById(tab.connectionId).engine)}</strong></span><span>ROWS ON PAGE<strong>${tab.rows.length}</strong></span></div>`;
}

// -----------------------------------------------------------------------------
// Ordered columns and foreign key pairs
// -----------------------------------------------------------------------------
function columnPicker(tab, selected) {
  const draw = () => {
    findElement("#schema-columns-options").innerHTML = tab.schema.map((column, index) => sqlite(tab) && column.key === "ROWID" ? "" : `<label class="column-option"><input type="checkbox" data-column="${index}" ${selected.includes(column.name) ? "checked" : ""}><span>${escapeHtml(column.name)}</span><code>${escapeHtml(column.type)}</code></label>`).join("");
    findElement("#schema-columns-order").innerHTML = selected.map((name, index) => `<div class="column-order-row"><span class="pill">${index + 1}</span><span class="grow">${escapeHtml(name)}</span><button type="button" data-move="${index}" data-step="-1" ${index === 0 ? "disabled" : ""}>↑</button><button type="button" data-move="${index}" data-step="1" ${index === selected.length - 1 ? "disabled" : ""}>↓</button></div>`).join("") || '<p class="hint">Select one or more columns.</p>';
    findElements("[data-column]").forEach(input => input.onchange = () => {
      const name = tab.schema[Number(input.dataset.column)].name;
      if (input.checked) selected.push(name); else selected.splice(selected.indexOf(name), 1);
      draw();
    });
    findElements("[data-move]").forEach(control => control.onclick = () => {
      const index = Number(control.dataset.move), next = index + Number(control.dataset.step);
      [selected[index], selected[next]] = [selected[next], selected[index]];
      draw();
    });
  };
  draw();
}

function foreignKeyPicker(tab, existing, pairs) {
  const catalog = getConnectionCatalog(tab.connectionId, tab.db);
  const references = mysql(tab) || sqlite(tab)
    ? (catalog?.tables || []).map(table => ({ schema: "", table }))
    : (catalog?.schemas || []).flatMap(schema => schema.tables.map(table => ({ schema: schema.name, table })));
  const selector = findElement("#reference-table");
  selector.innerHTML = '<option value="">Choose a table…</option>' + references.map((item, index) => `<option value="${index}" ${item.schema === existing?.referenceSchema && item.table === existing?.referenceTable ? "selected" : ""}>${escapeHtml(item.schema)}.${escapeHtml(item.table)}</option>`).join("");
  let referenceColumns = [];
  const pairFieldset = findElement("#foreign-key-pair-fieldset");
  const drawPairs = () => {
    const localOptions = (selected, current) => '<option value="">Choose a column…</option>' + tab.schema.filter(column => !sqlite(tab) || column.key !== "ROWID").map(column => `<option value="${escapeHtml(column.name)}" ${selected === column.name ? "selected" : ""} ${pairs.some((pair, index) => index !== current && pair.local === column.name) ? "disabled" : ""}>${escapeHtml(column.name)} · ${escapeHtml(column.type)}</option>`).join("");
    const foreignOptions = (selected, current) => '<option value="">Choose a column…</option>' + referenceColumns.filter(column => !sqlite(tab) || column.key !== "ROWID").map(column => `<option value="${escapeHtml(column.name)}" ${selected === column.name ? "selected" : ""} ${pairs.some((pair, index) => index !== current && pair.foreign === column.name) ? "disabled" : ""}>${escapeHtml(column.name)} · ${escapeHtml(column.type)}</option>`).join("");
    findElement("#foreign-key-pairs").innerHTML = pairs.map((pair, index) => `<div class="foreign-key-pair"><span class="pill">${index + 1}</span><label>Column<select data-pair="${index}" data-side="local">${localOptions(pair.local, index)}</select></label><span class="pair-arrow">→</span><label>Reference Column<select data-pair="${index}" data-side="foreign">${foreignOptions(pair.foreign, index)}</select></label><button type="button" data-remove-pair="${index}" ${pairs.length === 1 ? "disabled" : ""}>×</button></div>`).join("") + button("schema-add-pair", "Add Column Pair", "plus", "", `id="add-column-pair" ${pairs.length >= Math.min(tab.schema.length, referenceColumns.length) ? "disabled" : ""}`);
    findElements("[data-pair]").forEach(select => select.onchange = () => { pairs[Number(select.dataset.pair)][select.dataset.side] = select.value; drawPairs(); });
    findElements("[data-remove-pair]").forEach(control => control.onclick = () => { pairs.splice(Number(control.dataset.removePair), 1); drawPairs(); });
    findElement("#add-column-pair").onclick = () => { pairs.push({ local: "", foreign: "" }); drawPairs(); };
  };
  const load = async () => {
    const selected = selector.value;
    const reference = references[Number(selected)];
    pairFieldset.disabled = true;
    referenceColumns = [];
    drawPairs();
    if (selected === "" || !reference) return;
    try {
      const query = new URLSearchParams({ database: tab.db, schema: reference.schema, table: reference.table });
      const columns = await apiRequest(target(tab) + "/columns?" + query);
      if (findElement("#reference-table") !== selector || selector.value !== selected) return;
      referenceColumns = columns;
      drawPairs();
      pairFieldset.disabled = false;
    } catch (error) { if (findElement("#reference-table") === selector && selector.value === selected) toast(error.message); }
  };
  selector.onchange = () => { pairs.splice(0, pairs.length, { local: "", foreign: "" }); load(); };
  load();
  return () => selector.value === "" ? null : references[Number(selector.value)];
}

// -----------------------------------------------------------------------------
// Forms, preview, and execution
// -----------------------------------------------------------------------------
export function changeSQL(tab, kind, value) {
  const q = name => quoted(tab, name);
  const columns = value.columns.map(q).join(", ");
  if (mysql(tab)) {
    if (kind === "index") return `ALTER TABLE ${tableName(tab)} ADD ${value.unique ? "UNIQUE " : ""}INDEX ${q(value.name)} (${columns});`;
    if (value.type === "FOREIGN KEY") return `ALTER TABLE ${tableName(tab)} ADD CONSTRAINT ${q(value.name)} FOREIGN KEY (${columns}) REFERENCES ${q(tab.db)}.${q(value.referenceTable)} (${value.referenceColumns.map(q).join(", ")});`;
    if (value.type === "PRIMARY KEY") return `ALTER TABLE ${tableName(tab)} ADD PRIMARY KEY (${columns});`;
    if (value.type === "CHECK") return `ALTER TABLE ${tableName(tab)} ADD CONSTRAINT ${q(value.name)} CHECK (${value.expression});`;
    return `ALTER TABLE ${tableName(tab)} ADD CONSTRAINT ${q(value.name)} UNIQUE (${columns});`;
  }
  if (kind === "index") return `CREATE ${value.unique ? "UNIQUE " : ""}INDEX ${q(value.name)} ON ${tableName(tab)} USING btree (${columns});`;
  let definition = value.type === "CHECK" ? `CHECK (${value.expression})` : `${value.type} (${columns})`;
  if (value.type === "FOREIGN KEY") definition += ` REFERENCES ${q(value.referenceSchema)}.${q(value.referenceTable)} (${value.referenceColumns.map(q).join(", ")})`;
  const add = `ALTER TABLE ${tableName(tab)} ADD CONSTRAINT ${q(value.name)} ${definition}`;
  return value.type === "FOREIGN KEY" ? `${add} NOT VALID;\nALTER TABLE ${tableName(tab)} VALIDATE CONSTRAINT ${q(value.name)};` : add + ";";
}

async function applyChange(tab, kind, method, value, sql) {
  try {
    await runRecordedSQL(target(tab) + (kind === "index" ? "/indexes" : "/constraints"), { method, body: { ...locationData(tab), ...value } }, { connectionId: tab.connectionId, db: tab.db, schemaName: tab.schemaName }, sql);
    closeDialog();
    toast(`${kind === "index" ? "Index" : "Constraint"} ${method === "DELETE" ? "deleted" : method === "PATCH" ? "updated" : "added"}.`);
    refreshTable(tab);
  } catch (error) {
    if (error.code === "foreign_key_unvalidated") {
      closeDialog();
      refreshTable(tab);
    }
    toast(error.message);
  }
}

function openSchemaForm(tab, kind, existing) {
  const selected = [...(existing?.columns || [])];
  const pairs = existing?.type === "FOREIGN KEY" && existing.columns.length ? existing.columns.map((local, index) => ({ local, foreign: existing.referenceColumns[index] || "" })) : [{ local: "", foreign: "" }];
  const editing = Boolean(existing);
  const typeOptions = ["UNIQUE", "FOREIGN KEY", "CHECK", "PRIMARY KEY"].map(type => `<option ${existing?.type === type ? "selected" : ""}>${type}</option>`).join("");
  showDialog(`${editing ? "Edit" : "Add"} ${kind}`, `<form id="schema-form" class="stack">${mysql(tab) ? '<p class="hint">DDL commits immediately and can wait for a metadata lock. Foreign keys can be added here only when the table is empty.</p>' : ""}<label>Name<input name="name" ${mysql(tab) && existing?.type === "PRIMARY KEY" ? "" : "required"} maxlength="64" value="${escapeHtml(existing?.name || "")}" placeholder="${escapeHtml(tab.table)}_${kind}"></label>
    ${kind === "index" ? `<label>Index Mode<select name="indexMode"><option value="standard" ${!existing?.unique ? "selected" : ""}>Standard Index</option><option value="unique" ${existing?.unique ? "selected" : ""}>Unique Index</option></select></label>` : `<label>Constraint Type<select name="type" id="constraint-type" ${mysql(tab) && editing ? "disabled" : ""}>${typeOptions}</select></label>`}
    <fieldset class="column-picker" id="local-columns"><legend>Columns · ${escapeHtml(tab.db)}.${escapeHtml(tab.table)}</legend><div class="column-options" id="schema-columns-options"></div><div class="column-order" id="schema-columns-order"></div></fieldset>
    ${kind === "constraint" ? `<section id="foreign-key-fields" class="stack" hidden><label>Reference Table · ${escapeHtml(tab.db)}<select id="reference-table"></select></label><fieldset class="column-picker" id="foreign-key-pair-fieldset" disabled><legend>Column Pairs</legend><div id="foreign-key-pairs" class="stack"></div></fieldset><p class="hint">Referenced columns must form a primary or unique key.</p></section><label id="check-expression-field" hidden>Check Expression<textarea name="expression" rows="3" placeholder="e.g. price >= 0">${escapeHtml(existing?.expression || "")}</textarea></label>` : ""}
    <div id="schema-error" class="error-text" role="alert"></div><button type="submit" class="primary">Review SQL</button></form>`, "", true);
  columnPicker(tab, selected);
  const reference = kind === "constraint" ? foreignKeyPicker(tab, existing, pairs) : null;
  const updateType = () => {
    const type = findElement("#constraint-type").value;
    if (mysql(tab)) {
      const nameField = findElement('#schema-form [name="name"]');
      nameField.required = type !== "PRIMARY KEY";
      if (type === "PRIMARY KEY") nameField.value = "PRIMARY";
    }
    findElement("#local-columns").hidden = type === "FOREIGN KEY" || type === "CHECK";
    findElement("#foreign-key-fields").hidden = type !== "FOREIGN KEY";
    findElement("#check-expression-field").hidden = type !== "CHECK";
  };
  if (kind === "constraint") { findElement("#constraint-type").onchange = updateType; updateType(); }
  findElement("#schema-form").onsubmit = async event => {
    event.preventDefault();
    const form = new FormData(event.target), name = form.get("name").trim();
    const type = kind === "index" ? "" : mysql(tab) && existing ? existing.type : form.get("type");
    const columns = type === "FOREIGN KEY" ? pairs.map(pair => pair.local) : type === "CHECK" ? [] : [...selected];
    if (type !== "CHECK" && (!columns.length || columns.some(column => !column))) return findElement("#schema-error").textContent = "Select at least one column and complete every pair.";
    if (type === "PRIMARY KEY" && tab.constraints.some(item => item.type === "PRIMARY KEY" && item.name !== existing?.name)) return findElement("#schema-error").textContent = "This table already has a primary key.";
    const value = { name: mysql(tab) && type === "PRIMARY KEY" ? "PRIMARY" : name, oldName: existing?.name, type, columns, unique: form.get("indexMode") === "unique", expression: form.get("expression")?.trim() || "" };
    if (type === "FOREIGN KEY") {
      const ref = reference();
      if (!ref || pairs.some(pair => !pair.foreign) || new Set(pairs.map(pair => pair.local)).size !== pairs.length || new Set(pairs.map(pair => pair.foreign)).size !== pairs.length) return findElement("#schema-error").textContent = "Choose a reference table and distinct, complete column pairs.";
      value.referenceSchema = ref.schema;
      value.referenceTable = ref.table;
      value.referenceColumns = pairs.map(pair => pair.foreign);
    }
    if (type === "CHECK" && !value.expression) return findElement("#schema-error").textContent = "Enter a CHECK expression.";
    if (sqlite(tab)) {
      const submit = event.target.querySelector('[type="submit"]');
      submit.disabled = true;
      try {
        const body = { ...locationData(tab), ...value };
        const preview = await apiRequest(target(tab) + "/constraints", { method: "POST", body: { ...body, previewOnly: true } });
        showDialog("Review constraint change", `<p>SQLite will rebuild <strong>${escapeHtml(tab.table)}</strong> and check its data before committing.</p><pre class="sql-preview">${escapeHtml(preview.sql)}</pre>${preview.objects.length ? `<p class="hint">Objects to restore: ${escapeHtml(preview.objects.join(", "))}</p>` : ""}`, button("close-dialog", "Cancel") + button("schema-confirm", "Apply Change", "check", "primary"));
        findElement('[data-action="schema-confirm"]').onclick = async confirm => {
          confirm.currentTarget.disabled = true;
          try {
            await runRecordedSQL(target(tab) + "/constraints", { method: "POST", body: { ...body, rebuildName: preview.rebuildName, previewHash: preview.previewHash } }, { connectionId: tab.connectionId, db: tab.db, schemaName: "" }, preview.sql);
            closeDialog(); toast("Constraint added."); refreshTable(tab, 1);
          } catch (error) { toast(error.message); confirm.currentTarget.disabled = false; }
        };
      } catch (error) { findElement("#schema-error").textContent = error.message; }
      finally { submit.disabled = false; }
      return;
    }
    const method = editing ? "PATCH" : "POST";
    const drop = editing && !mysql(tab) ? kind === "index" ? `DROP INDEX ${quoteIdentifier(tab.schemaName)}.${quoteIdentifier(existing.name)};\n` : `ALTER TABLE ${tableName(tab)} DROP CONSTRAINT ${quoteIdentifier(existing.name)};\n` : "";
    const mysqlDrop = kind === "index" || existing?.type === "UNIQUE" ? "DROP INDEX " + quoted(tab, existing.name)
      : existing?.type === "PRIMARY KEY" ? "DROP PRIMARY KEY"
      : existing?.type === "FOREIGN KEY" ? "DROP FOREIGN KEY " + quoted(tab, existing.name)
      : (mariaDB(tab) ? "DROP CONSTRAINT " : "DROP CHECK ") + quoted(tab, existing.name);
    const added = changeSQL(tab, kind, value);
    const sql = editing && mysql(tab)
      ? `ALTER TABLE ${tableName(tab)} ${mysqlDrop}, ${added.slice(("ALTER TABLE " + tableName(tab) + " ").length)}`
      : drop + added;
    showDialog(`Review ${kind} change`, `<p>${escapeHtml(tab.db)} / ${escapeHtml(tab.table)}</p><pre class="sql-preview">${escapeHtml(sql)}</pre>`, button("close-dialog", "Cancel") + button("schema-confirm", "Apply Change", "check", "primary"));
    findElement('[data-action="schema-confirm"]').onclick = () => applyChange(tab, kind, method, value, sql);
  };
}

function deleteSchema(tab, kind, value) {
  const sql = sqlite(tab) && kind === "index" ? `DROP INDEX main.${quoteIdentifier(value.name)};` : mysql(tab)
    ? `ALTER TABLE ${tableName(tab)} ${kind === "index" || value.type === "UNIQUE" ? "DROP INDEX " + quoted(tab, value.name) : value.type === "PRIMARY KEY" ? "DROP PRIMARY KEY" : value.type === "FOREIGN KEY" ? "DROP FOREIGN KEY " + quoted(tab, value.name) : (mariaDB(tab) ? "DROP CONSTRAINT " : "DROP CHECK ") + quoted(tab, value.name)};`
    : kind === "index" ? `DROP INDEX ${quoteIdentifier(tab.schemaName)}.${quoteIdentifier(value.name)};` : `ALTER TABLE ${tableName(tab)} DROP CONSTRAINT ${quoteIdentifier(value.name)};`;
  showDialog(`Delete ${kind}?`, `<p>Remove <strong>${escapeHtml(value.name)}</strong> from <strong>${escapeHtml(tab.table)}</strong>? This may affect dependent objects or data rules.</p><pre class="sql-preview">${escapeHtml(sql)}</pre>`, button("close-dialog", "Cancel") + button("schema-confirm-delete", "Delete", "trash", "danger"));
  findElement('[data-action="schema-confirm-delete"]').onclick = () => applyChange(tab, kind, "DELETE", { name: value.name }, sql);
}

function validateConstraint(tab, value) {
  const sql = `ALTER TABLE ${tableName(tab)} VALIDATE CONSTRAINT ${quoteIdentifier(value.name)};`;
  showDialog("Validate foreign key", `<p>Check existing rows against <strong>${escapeHtml(value.name)}</strong>?</p><pre class="sql-preview">${escapeHtml(sql)}</pre>`, button("close-dialog", "Cancel") + button("schema-confirm", "Validate", "check", "primary"));
  findElement('[data-action="schema-confirm"]').onclick = async () => {
    try {
      await runRecordedSQL(target(tab) + "/constraints", { method: "PATCH", body: { ...locationData(tab), name: value.name, validateOnly: true } }, { connectionId: tab.connectionId, db: tab.db, schemaName: tab.schemaName }, sql);
      closeDialog();
      toast("Foreign key validated.");
      refreshTable(tab);
    } catch (error) { toast(error.message); }
  };
}

function openColumnForm(tab, existing) {
  if (sqlite(tab)) return openSQLiteColumnForm(tab, existing);
  if (mysql(tab)) return openMySQLColumnForm(tab, existing);
  const editing = Boolean(existing);
  const availableTypes = [...columnTypes, ...(getConnectionCatalog(tab.connectionId, tab.db)?.types || [])];
  const oldType = existing?.type || "";
  const oldArray = oldType.endsWith("[]"), oldBase = oldType.replace(/\[\]$/, "");
  const oldLength = oldBase.match(/^(?:character varying|character|varchar|bit varying|bit)\((\d+)\)$/)?.[1] || "255";
  const oldNumeric = oldBase.match(/^(?:numeric|decimal)\((\d+)(?:,\s*(-?\d+))?\)$/);
  const oldPrecision = oldNumeric?.[1] || "18", oldScale = oldNumeric ? oldNumeric[2] ?? "0" : "2";
  const hadDefault = existing?.default !== null && existing?.default !== undefined;
  showDialog(editing ? "Edit column" : "Add column", `<form id="column-form" class="stack">
    <p class="hint">${escapeHtml(tab.schemaName)}.${escapeHtml(tab.table)}${editing ? " · " + escapeHtml(existing.type) : ""}</p>
    <label>Column name<input name="name" required maxlength="63" value="${escapeHtml(existing?.name || "")}"></label>
    <label>Type<select name="type">${editing ? `<option value="">Keep current type (${escapeHtml(existing.type)})</option>` : ""}${availableTypes.filter(type => !editing || !["smallserial", "serial", "bigserial"].includes(type)).map(type => `<option value="${escapeHtml(type)}" ${!editing && type === "text" ? "selected" : ""}>${escapeHtml(type)}</option>`).join("")}</select></label>
    <label id="column-length-field" hidden>Length<input name="length" type="number" min="1" max="10485760" value="${oldLength}"></label>
    <label id="column-precision-field" hidden>Precision (digits)<input name="precision" type="number" min="1" max="1000" value="${oldPrecision}"></label>
    <label id="column-scale-field" hidden>Scale (decimals)<input name="scale" type="number" min="-1000" max="1000" value="${oldScale}"></label>
    <label class="check-label"><input type="checkbox" name="nullable" ${!editing || existing.nullable ? "checked" : ""}>Nullable</label>
    <label class="check-label"><input type="checkbox" name="array" ${oldArray ? "checked" : ""}>Array</label>
    <label class="check-label"><input type="checkbox" name="useDefault" ${hadDefault ? "checked" : ""}>Use default</label>
    <label>Default literal<input name="default" ${hadDefault ? `placeholder="Current: ${escapeHtml(existing.default)}"` : 'placeholder="Enter a value"'} ${hadDefault ? "" : "disabled"}></label>
    <div id="column-form-error" class="error-text" role="alert"></div>
    <button type="submit" class="primary">${editing ? "Save column" : "Add column"}</button>
  </form>`);
  const form = findElement("#column-form");
  let defaultDirty = false;
  let modifierDirty = false;
  for (const name of ["length", "precision", "scale"]) form.elements.namedItem(name).oninput = () => { modifierDirty = true; };
  form.elements.default.oninput = () => { defaultDirty = true; };
  form.elements.useDefault.onchange = () => { form.elements.default.disabled = !form.elements.useDefault.checked; };
  const updateModifiers = () => {
    const type = form.elements.type.value || oldBase;
    const base = type.replace(/\(.*$/, "").replace("character varying", "varchar");
    for (const [name, visible] of [["length", lengthTypes.has(base)], ["precision", numericTypes.has(base)], ["scale", numericTypes.has(base)]]) {
      findElement(`#column-${name}-field`).hidden = !visible;
      form.elements.namedItem(name).disabled = !visible;
      form.elements.namedItem(name).required = visible;
    }
    const array = form.elements.array;
    array.disabled = ["smallserial", "serial", "bigserial"].includes(base);
    if (array.disabled) array.checked = false;
  };
  form.elements.type.onchange = updateModifiers;
  updateModifiers();
  form.onsubmit = async event => {
    event.preventDefault();
    if (!form.reportValidity()) return;
    const useDefault = form.elements.useDefault.checked;
    const changeDefault = useDefault !== hadDefault || (useDefault && defaultDirty);
    let type = form.elements.type.value;
    if (type || modifierDirty || form.elements.array.checked !== oldArray || !editing) {
      const base = (type || oldBase).replace(/\(.*$/, "").replace("character varying", "varchar");
      if (lengthTypes.has(base) || numericTypes.has(base)) {
        const length = form.elements.namedItem("length").value;
        const precision = form.elements.namedItem("precision").value;
        const scale = form.elements.namedItem("scale").value;
        type = columnTypeSQL(base, length, precision, scale, form.elements.array.checked);
      } else {
        type = columnTypeSQL(base, "", "", "", form.elements.array.checked);
      }
    }
    const value = {
      ...locationData(tab), name: form.elements.name.value.trim(), oldName: existing?.name || "",
      type,
      nullable: editing && form.elements.nullable.checked === existing.nullable ? null : form.elements.nullable.checked,
      changeDefault, default: useDefault && changeDefault ? form.elements.default.value : null
    };
    const sql = columnChangeSQL(tab, value, editing);
    if (!sql) return findElement("#column-form-error").textContent = "Choose at least one change.";
    const submit = form.querySelector('[type="submit"]');
    submit.disabled = true;
    try {
      await runRecordedSQL(target(tab) + "/table-columns", { method: editing ? "PATCH" : "POST", body: value }, { connectionId: tab.connectionId, db: tab.db, schemaName: tab.schemaName }, sql);
      closeDialog();
      toast(editing ? "Column updated." : "Column added.");
      refreshTable(tab, tab.page);
    } catch (error) { findElement("#column-form-error").textContent = error.message; }
    finally { submit.disabled = false; }
  };
}

function openSQLiteColumnForm(tab, existing) {
  const editing = Boolean(existing);
  const types = ["INTEGER", "INT", "REAL", "TEXT", "BLOB", ...(tab.sqliteStrict ? ["ANY"] : ["NUMERIC"])];
  if (editing && !types.includes(existing.type.toUpperCase())) types.unshift(existing.type);
  showDialog(editing ? "Edit SQLite column" : "Add SQLite column", `<form id="sqlite-column-form" class="stack">
    <p class="hint">SQLite uses type affinity. Changing type, default, or nullability copies the table in a transaction. Existing values must satisfy the new definition.</p>
    <label>Column name<input name="name" required maxlength="255" value="${escapeHtml(existing?.name || "")}"></label>
    <label>Type<select name="type">${types.map(type => `<option value="${escapeHtml(type)}" ${type.toUpperCase() === (existing?.type || "TEXT").toUpperCase() ? "selected" : ""}>${escapeHtml(type)}</option>`).join("")}</select></label>
    <label class="check-label"><input type="checkbox" name="nullable" ${!editing || existing.nullable ? "checked" : ""}>Nullable</label>
    ${editing ? `<label class="check-label"><input type="checkbox" name="changeDefault">Change default ${existing.default == null ? "(currently none)" : `· current SQL: ${escapeHtml(existing.default)}`}</label>` : ""}
    <label class="check-label"><input type="checkbox" name="useDefault" ${editing ? "disabled" : ""}>Use default</label>
    <label class="check-label"><input type="checkbox" name="defaultExpression" disabled>Default is a SQLite expression</label>
    <label>Default literal or expression<input name="default" disabled></label>
    <pre id="sqlite-column-preview" class="sql-preview"></pre>
    <div id="sqlite-column-error" class="error-text" role="alert"></div>
    <button type="submit" class="primary">${editing ? "Review change" : "Add column"}</button>
  </form>`);
  const form = findElement("#sqlite-column-form");
  const updateDefault = () => {
    const enabled = !editing || form.elements.changeDefault.checked;
    form.elements.useDefault.disabled = !enabled;
    form.elements.default.disabled = !enabled || !form.elements.useDefault.checked;
    form.elements.defaultExpression.disabled = form.elements.default.disabled;
  };
  const preview = () => {
    const name = form.elements.name.value.trim();
    if (!name) { findElement("#sqlite-column-preview").textContent = "Enter a column name to preview SQL."; return; }
    if (editing) {
      findElement("#sqlite-column-preview").textContent = name !== existing.name && form.elements.type.value === existing.type && form.elements.nullable.checked === existing.nullable && !form.elements.changeDefault.checked
        ? `ALTER TABLE ${tableName(tab)} RENAME COLUMN ${quoteIdentifier(existing.name)} TO ${quoteIdentifier(name)};`
        : "Review the change to see the full rebuild SQL before applying it.";
      return;
    }
    const nullable = form.elements.nullable.checked ? "" : " NOT NULL";
    const expression = form.elements.default.value.trim();
    const literal = !form.elements.useDefault.checked ? "" : form.elements.defaultExpression.checked
      ? ` DEFAULT ${["CURRENT_TIME", "CURRENT_DATE", "CURRENT_TIMESTAMP"].includes(expression.toUpperCase()) ? expression : `(${expression})`}` : form.elements.type.value === "BLOB" && form.elements.default.value.startsWith("\\x")
        ? ` DEFAULT X'${form.elements.default.value.slice(2)}'` : ` DEFAULT '${form.elements.default.value.replaceAll("'", "''")}'`;
    findElement("#sqlite-column-preview").textContent = `ALTER TABLE ${tableName(tab)} ADD COLUMN ${quoteIdentifier(name)} ${form.elements.type.value}${nullable}${literal};`;
  };
  updateDefault();
  form.oninput = preview;
  form.onchange = () => { updateDefault(); preview(); };
  preview();
  form.onsubmit = async event => {
    event.preventDefault(); if (!form.reportValidity()) return;
    const value = { ...locationData(tab), name: form.elements.name.value.trim(), oldName: existing?.name || "",
      type: editing && form.elements.type.value.toUpperCase() === existing.type.toUpperCase() ? "" : form.elements.type.value,
      nullable: editing && form.elements.nullable.checked === existing.nullable ? null : form.elements.nullable.checked,
      changeDefault: editing ? form.elements.changeDefault.checked : form.elements.useDefault.checked,
      default: form.elements.useDefault.checked ? form.elements.default.value : null,
      defaultExpression: form.elements.defaultExpression.checked };
    const sql = findElement("#sqlite-column-preview").textContent;
    if (editing && value.name === existing.name && !value.type && value.nullable === null && !value.changeDefault) {
      findElement("#sqlite-column-error").textContent = "Choose at least one change."; return;
    }
    const submit = form.querySelector('[type="submit"]'); submit.disabled = true;
    try {
      if (!editing) {
        await runRecordedSQL(target(tab) + "/table-columns", { method: "POST", body: value }, { connectionId: tab.connectionId, db: tab.db, schemaName: "" }, sql);
        closeDialog(); toast("Column added."); refreshTable(tab, tab.page);
        return;
      }
      const previewResult = await apiRequest(target(tab) + "/table-columns", { method: "PATCH", body: { ...value, previewOnly: true } });
      showDialog("Review SQLite column change", `<p>The table is copied in a transaction. Existing values must satisfy the new definition. ${previewResult.objects?.length ? `The following objects are recreated: ${escapeHtml(previewResult.objects.join(", "))}.` : ""}</p><pre class="sql-preview">${escapeHtml(previewResult.sql)}</pre>`,
        button("close-dialog", "Cancel") + button("sqlite-column-confirm", "Apply Change", "check", "primary"));
      findElement('[data-action="sqlite-column-confirm"]').onclick = async event => {
        event.currentTarget.disabled = true;
        try {
          await runRecordedSQL(target(tab) + "/table-columns", { method: "PATCH", body: { ...value, rebuildName: previewResult.rebuildName, previewHash: previewResult.previewHash } }, { connectionId: tab.connectionId, db: tab.db, schemaName: "" }, previewResult.sql);
          closeDialog(); toast("Column updated."); refreshTable(tab, 1);
        } catch (error) { toast(error.message); event.currentTarget.disabled = false; }
      };
    } catch (error) { findElement("#sqlite-column-error").textContent = error.message; }
    finally { submit.disabled = false; }
  };
}

function openSQLiteIndexForm(tab) {
  const available = tab.schema.filter(column => column.key !== "ROWID");
  showDialog("Add SQLite index", `<form id="sqlite-index-form" class="stack">
    <label>Index name<input name="name" required maxlength="255" value="${escapeHtml(tab.table)}_idx"></label>
    <label class="check-label"><input type="checkbox" name="unique">Unique</label>
    <fieldset class="column-picker"><legend>Columns</legend>${available.map((column, index) => `<div class="column-option"><label><input type="checkbox" data-column="${index}"><span>${escapeHtml(column.name)}</span></label><select aria-label="Order for ${escapeHtml(column.name)}" data-direction="${index}"><option value="">Default</option><option>ASC</option><option>DESC</option></select></div>`).join("")}</fieldset>
    <pre id="sqlite-index-preview" class="sql-preview"></pre>
    <div id="sqlite-index-error" class="error-text" role="alert"></div>
    <button type="submit" class="primary">Create index</button>
  </form>`);
  const form = findElement("#sqlite-index-form");
  const preview = () => {
    const selected = findElements("[data-column]:checked", form).map(input => ({ name: available[Number(input.dataset.column)].name, direction: findElement(`[data-direction="${input.dataset.column}"]`, form).value }));
    const name = form.elements.name.value.trim();
    findElement("#sqlite-index-preview").textContent = selected.length && name ? `CREATE ${form.elements.unique.checked ? "UNIQUE " : ""}INDEX main.${quoteIdentifier(name)} ON ${quoteIdentifier(tab.table)} (${selected.map(item => `${quoteIdentifier(item.name)}${item.direction ? ` ${item.direction}` : ""}`).join(", ")});` : "Choose a name and at least one column to preview SQL.";
  };
  form.oninput = preview;
  form.onchange = preview;
  preview();
  form.onsubmit = async event => {
    event.preventDefault(); if (!form.reportValidity()) return;
    const selected = findElements("[data-column]:checked", form).map(input => ({ name: available[Number(input.dataset.column)].name, direction: findElement(`[data-direction="${input.dataset.column}"]`, form).value }));
    const columns = selected.map(item => item.name), directions = selected.map(item => item.direction);
    if (!columns.length) { findElement("#sqlite-index-error").textContent = "Choose at least one column."; return; }
    const name = form.elements.name.value.trim(), unique = form.elements.unique.checked;
    const sql = findElement("#sqlite-index-preview").textContent;
    const submit = form.querySelector('[type="submit"]'); submit.disabled = true;
    try {
      await runRecordedSQL(target(tab) + "/indexes", { method: "POST", body: { ...locationData(tab), name, columns, directions, unique } }, { connectionId: tab.connectionId, db: tab.db, schemaName: "" }, sql);
      closeDialog(); toast("Index created."); refreshTable(tab, tab.page);
    } catch (error) { findElement("#sqlite-index-error").textContent = error.message; }
    finally { submit.disabled = false; }
  };
}

function openMySQLColumnForm(tab, existing) {
  const editing = Boolean(existing);
  showDialog(editing ? "Edit column" : "Add column", `<form id="mysql-column-form" class="stack">
    <p class="hint">${escapeHtml(tab.db)}.${escapeHtml(tab.table)} · DDL may wait for a metadata lock and commits immediately.</p>
    <label>Column name<input name="name" required maxlength="64" value="${escapeHtml(existing?.name || "")}"></label>
    ${mysqlTypeFields(existing?.type || "varchar(255)", editing)}
    <label class="check-label"><input type="checkbox" name="nullable" ${!editing || existing.nullable ? "checked" : ""}>Nullable</label>
    <label class="check-label"><input type="checkbox" name="useDefault" ${existing?.default != null ? "checked" : ""}>Use default</label>
    <label>Default literal<input name="default" ${existing?.default != null ? `value="${escapeHtml(existing.default)}"` : "disabled"}></label>
    <div id="column-form-error" class="error-text" role="alert"></div>
    <button type="submit" class="primary">${editing ? "Save column" : "Add column"}</button>
  </form>`);
  const form = findElement("#mysql-column-form");
  updateMySQLTypeFields(form, existing?.type);
  let typeDirty = false;
  form.onchange = event => {
    if (event.target.dataset.field === "type") { typeDirty = true; updateMySQLTypeFields(form, existing?.type); }
    else if (event.target.dataset.field) typeDirty = true;
  };
  form.oninput = event => {
    if (event.target.dataset.field) typeDirty = true;
    if (event.target.dataset.field === "precision") updateMySQLTypeFields(form, existing?.type);
  };
  form.elements.useDefault.onchange = () => { form.elements.default.disabled = !form.elements.useDefault.checked; };
  form.onsubmit = async event => {
    event.preventDefault();
    if (!form.reportValidity()) return;
    const value = {
      ...locationData(tab), name: form.elements.name.value.trim(), oldName: existing?.name || "",
      type: editing && !typeDirty ? "" : mysqlTypeSQL(form, existing?.type),
      nullable: form.elements.nullable.checked,
      changeDefault: true,
      default: form.elements.useDefault.checked ? form.elements.default.value : null
    };
    const name = quoted(tab, value.name);
    const type = value.type || existing?.type;
    const defaultLiteral = value.default === null ? "" : "'" + value.default.replaceAll("\\", "\\\\").replaceAll("'", "''") + "'";
    const defaultSQL = !defaultLiteral ? "" : /text|blob|^json$/i.test(type) ? ` DEFAULT (${defaultLiteral})` : ` DEFAULT ${defaultLiteral}`;
    let definition = `${name} ${type} ${value.nullable ? "NULL" : "NOT NULL"}${defaultSQL}`;
    if (editing && existing.comment) definition += ` COMMENT '${existing.comment.replaceAll("'", "''")}'`;
    if (editing && existing.collation && /char|text|^enum|^set/i.test(type)) definition += ` COLLATE ${quoted(tab, existing.collation)}`;
    const sql = `ALTER TABLE ${tableName(tab)} ${editing ? "CHANGE COLUMN " + quoted(tab, value.oldName) : "ADD COLUMN"} ${definition};`;
    showDialog("Review column change", `<p>This change commits immediately and may wait for a metadata lock.</p><pre class="sql-preview">${escapeHtml(sql)}</pre>`,
      button("close-dialog", "Cancel") + button("mysql-column-confirm", "Apply Change", "check", "primary"));
    findElement('[data-action="mysql-column-confirm"]').onclick = async event => {
      event.currentTarget.disabled = true;
      try {
        await runRecordedSQL(target(tab) + "/table-columns", { method: editing ? "PATCH" : "POST", body: value }, { connectionId: tab.connectionId, db: tab.db, schemaName: "" }, sql);
        closeDialog();
        toast(editing ? "Column updated." : "Column added.");
        refreshTable(tab, 1);
      } catch (error) { toast(error.message); event.currentTarget.disabled = false; }
    };
  };
}

function deleteColumn(tab, column) {
  confirmAction("Delete column?", `Delete <strong>${escapeHtml(column.name)}</strong> from <strong>${escapeHtml(tab.table)}</strong>? All values in this column will be lost.`, "Delete Column", async () => {
    try {
      await runRecordedSQL(target(tab) + "/table-columns", { method: "DELETE", body: { ...locationData(tab), name: column.name } }, { connectionId: tab.connectionId, db: tab.db, schemaName: tab.schemaName }, `ALTER TABLE ${tableName(tab)} DROP COLUMN ${quoted(tab, column.name)};`);
      toast("Column deleted.");
      refreshTable(tab, tab.page);
    } catch (error) { toast(error.message); }
  });
}

export function handleSchemaAction(action, element, onRefresh) {
  if (!action.startsWith("schema-")) return false;
  refreshTable = onRefresh;
  const tab = getActiveTab(), kind = element.dataset.kind;
  const value = kind === "column" ? tab?.schema[Number(element.dataset.index)] : kind === "index" ? tab?.indexes[Number(element.dataset.index)] : tab?.constraints[Number(element.dataset.index)];
  if (action === "schema-add") kind === "column" ? openColumnForm(tab) : sqlite(tab) && kind === "index" ? openSQLiteIndexForm(tab) : openSchemaForm(tab, kind);
  else if (action === "schema-validate" && value) validateConstraint(tab, value);
  else if (action === "schema-edit" && value) kind === "column" ? openColumnForm(tab, value) : openSchemaForm(tab, kind, value);
  else if (action === "schema-delete" && value) kind === "column" ? deleteColumn(tab, value) : deleteSchema(tab, kind, value);
  return true;
}
