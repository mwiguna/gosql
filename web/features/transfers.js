import { state } from "../state.js";
import { downloadFile, uploadFile } from "../api.js";
import { button, closeDialog, escapeHtml, findElement, showDialog, toast } from "../ui.js";

export function transferFormats(engine, table, importing) {
  const native = engine === "SQLite" ? ["sqlite", "SQLite database (.sqlite)"] : ["sql", "SQL dump (.sql)"];
  if (table && (importing || engine === "SQLite")) return [["csv", "CSV data (.csv)"]];
  const formats = [native, ...(engine === "PostgreSQL" ? [["dump", "PostgreSQL archive (.dump)"]] : [])];
  return table || importing ? [...formats, ["csv", "CSV data (.csv)"]] : formats;
}

export function openTransferDialog(connection, database, schema, table, catalog, importing, onImported) {
  const options = transferFormats(connection.engine, table, importing);
  const targets = connection.engine === "PostgreSQL"
    ? catalog.schemas.flatMap(schema => schema.tables.map(table => ({ schema: schema.name, table })))
    : (catalog.tables || []).filter(table => !catalog.virtualTables?.includes(table)).map(table => ({ schema: "", table }));
  const title = `${importing ? "Import" : "Export"} ${table ? "table" : "database"}`;
  showDialog(title, `<form id="transfer-form" class="stack">
    <p class="hint">${escapeHtml(connection.name)} / ${escapeHtml(database)}${schema ? " / " + escapeHtml(schema) : ""}${table ? " / " + escapeHtml(table) : ""}</p>
    <label>Format<select name="format">${options.map(([value, label]) => `<option value="${value}">${label}</option>`).join("")}</select></label>
    ${!table && importing ? `<label id="transfer-target">Destination table<select name="target">${targets.map((target, index) => `<option value="${index}">${escapeHtml(target.schema ? target.schema + "." + target.table : target.table)}</option>`).join("")}</select></label>` : ""}
    <p id="transfer-help" class="hint"></p>
    ${importing ? '<label>File<input name="file" type="file" required></label><label class="check-label"><input name="confirm" type="checkbox" required>I have checked the file and destination.</label>' : ""}
    <div id="transfer-error" class="error-text" role="alert"></div>
    <div class="row">${button("close-dialog", "Cancel", "", "ghost")}<button type="submit" class="primary">${importing ? "Import file" : "Export file"}</button></div>
    <p id="transfer-progress" class="hint" role="status"></p>
  </form>`);
  const form = findElement("#transfer-form");
  const help = form.querySelector("#transfer-help");
  const update = () => {
    const format = form.elements.format.value;
    if (form.elements.target) {
      form.querySelector("#transfer-target").hidden = format !== "csv";
      form.elements.target.disabled = format !== "csv";
      form.elements.target.required = format === "csv";
    }
    if (form.elements.file) form.elements.file.accept = format === "dump" ? ".dump,.backup" : "." + format;
    help.textContent = format === "csv"
      ? "CSV contains all table rows, with column names in the first row. Import appends rows; omitted columns use defaults. NULL is \\N; literal backslashes are doubled. Binary values use \\x followed by hexadecimal digits. Generated columns are omitted."
      : format === "sqlite"
        ? importing ? "Restoring replaces every table, schema object, and row in this SQLite database. Maximum file size: 256 MB."
          : "Downloads a consistent SQLite snapshot, including tables, indexes, triggers, and data."
        : format === "dump"
          ? importing ? "Restore a PostgreSQL archive into an empty database. Requires pg_restore on the GoSQL machine. Table archives are restored from the database menu. Maximum file size: 256 MB."
            : "Exports structure and data using pg_dump on the GoSQL machine. Restore the archive from a database menu. Related objects may be needed when exporting a single table."
          : connection.engine === "PostgreSQL"
            ? importing ? "Restore a PostgreSQL SQL script into an empty database using psql on the GoSQL machine. Maximum file size: 256 MB."
              : "Exports a readable SQL script with structure and data using pg_dump on the GoSQL machine. Related objects may be needed when exporting a single table."
          : importing ? "SQL statements run as supplied in the selected database. MySQL/MariaDB schema changes can remain applied if a later statement fails. Requires mysql or mariadb on the GoSQL machine. Maximum file size: 256 MB."
            : "Exports structure and data using mysqldump or mariadb-dump on the GoSQL machine. Full database exports include routines and events. Install client tools and add their bin folder to PATH.";
    if (importing && format === "csv") help.textContent += " Maximum file size: 256 MB. Errors roll back on transactional tables.";
  };
  form.elements.format.onchange = update;
  update();
  form.onsubmit = async event => {
    event.preventDefault();
    if (!form.reportValidity()) return;
    const format = form.elements.format.value;
    const destination = format === "csv" && !table ? targets[Number(form.elements.target.value)] : { schema, table };
    if (!destination || (format === "csv" && !destination.table)) return;
    const file = form.elements.file?.files[0];
    if (file && file.size > 256 * 1024 ** 2) { form.querySelector("#transfer-error").textContent = "Import file exceeds 256 MB."; return; }
    const params = new URLSearchParams({ database, schema: destination.schema || "", table: destination.table || "", format });
    const path = `/connections/${encodeURIComponent(connection.id)}/transfer?${params}`;
    const submit = form.querySelector('[type="submit"]');
    const userId = state.currentUser?.id;
    submit.disabled = true;
    form.querySelector("#transfer-error").textContent = "";
    form.querySelector("#transfer-progress").textContent = importing ? "Importing file…" : "Preparing download…";
    try {
      if (importing) {
        const result = await uploadFile(path, file);
        if (state.currentUser?.id !== userId) return;
        if (form.isConnected) closeDialog();
        try { await onImported(); } catch (error) { toast("Import completed, but refresh failed: " + error.message); return; }
        toast(format === "csv" ? `${result.rows} rows imported.` : "Database imported.");
      } else {
        await downloadFile(path, `${destination.table || database}.${format}`);
        if (state.currentUser?.id !== userId) return;
        if (form.isConnected) closeDialog();
        toast("Export completed.");
      }
    } catch (error) {
      if (state.currentUser?.id === userId) form.querySelector("#transfer-error").textContent = error.message;
    } finally {
      submit.disabled = false;
      form.querySelector("#transfer-progress").textContent = "";
    }
  };
}
