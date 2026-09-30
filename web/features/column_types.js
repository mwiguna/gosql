// Tipe umum PostgreSQL untuk formulir struktur; server memvalidasi ulang SQL yang dibentuk.
export const columnTypes = [
  "smallint", "integer", "bigint", "numeric", "decimal", "real", "double precision", "money",
  "smallserial", "serial", "bigserial", "boolean", "text", "varchar", "character", "bit", "bit varying", "bytea",
  "date", "time", "time with time zone", "timestamp", "timestamp with time zone", "interval",
  "uuid", "json", "jsonb", "jsonpath", "xml", "inet", "cidr", "macaddr", "macaddr8",
  "point", "line", "lseg", "box", "path", "polygon", "circle",
  "tsvector", "tsquery", "int4range", "int8range", "numrange", "tsrange", "tstzrange", "daterange"
];

export const lengthTypes = new Set(["varchar", "character", "bit", "bit varying"]);
export const numericTypes = new Set(["numeric", "decimal"]);

export function columnTypeSQL(type, length, precision, scale, array = false) {
  const base = lengthTypes.has(type) ? `${type}(${length})`
    : numericTypes.has(type) ? `${type}(${precision},${scale})` : type;
  return base + (array ? "[]" : "");
}

export const mysqlColumnTypes = [
  "tinyint", "smallint", "mediumint", "int", "bigint", "decimal", "float", "double", "bit", "boolean",
  "char", "varchar", "text", "blob", "date", "time", "datetime", "timestamp", "year", "json"
];

const mysqlIntegerTypes = new Set(["tinyint", "smallint", "mediumint", "int", "bigint"]);
const mysqlLengthLimits = { char: 255, varchar: 16383, binary: 255, varbinary: 16383, bit: 64 };

export function parseMySQLType(type) {
  const match = type.toLowerCase().match(/^([a-z]+)(?:\((\d+)(?:,(\d+))?\))?( unsigned)?$/);
  return { base: match?.[1] || "", length: match?.[2] || "", scale: match?.[3] || "", unsigned: Boolean(match?.[4]) };
}

export function mysqlUnsignedField(checked = false) {
  return `<label data-mysql-unsigned hidden class="check-label"><input data-field="unsigned" type="checkbox" ${checked ? "checked" : ""}>Unsigned</label>`;
}

export function mysqlTypeFields(type = "int", editing = false, includeUnsigned = true) {
  const current = parseMySQLType(type);
  const safeType = type.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;").replaceAll('"', "&quot;");
  return `<label>Type<select data-field="type">${editing ? `<option value="">Keep current type (${safeType})</option>` : ""}${mysqlColumnTypes.map(base => `<option value="${base}" ${!editing && base === current.base ? "selected" : ""}>${base}</option>`).join("")}</select></label>
    <label data-mysql-length hidden><span data-mysql-length-title>Length</span><input data-field="length" type="number" min="1" value="${current.length || (mysqlIntegerTypes.has(current.base) ? "" : current.base === "bit" ? "1" : "255")}"></label>
    <label data-mysql-precision hidden>Precision (digits)<input data-field="precision" type="number" min="1" max="65" value="${current.base === "decimal" ? current.length || "18" : "18"}"></label>
    <label data-mysql-scale hidden>Scale (decimals)<input data-field="scale" type="number" min="0" max="30" value="${current.scale || "0"}"></label>
    ${includeUnsigned ? mysqlUnsignedField(current.unsigned) : ""}`;
}

export function updateMySQLTypeFields(container, currentType = "") {
  const selected = container.querySelector('[data-field="type"]').value;
  const base = selected || parseMySQLType(currentType).base;
  const length = container.querySelector('[data-field="length"]');
  if (container.dataset.mysqlBase && container.dataset.mysqlBase !== base) {
    length.value = mysqlIntegerTypes.has(base) ? "" : base === "bit" ? "1" : "255";
    if (base === "decimal") {
      container.querySelector('[data-field="precision"]').value = "18";
      container.querySelector('[data-field="scale"]').value = "2";
    }
  }
  container.dataset.mysqlBase = base;
  const limit = mysqlLengthLimits[base] || (mysqlIntegerTypes.has(base) ? 999 : 0);
  container.querySelector("[data-mysql-length-title]").textContent = mysqlIntegerTypes.has(base) ? "Display width (optional)" : "Length";
  for (const [name, visible] of [["length", Boolean(limit)], ["precision", base === "decimal"], ["scale", base === "decimal"], ["unsigned", mysqlIntegerTypes.has(base) || base === "decimal"]]) {
    const field = container.querySelector(`[data-field="${name}"]`);
    field.closest("label").hidden = !visible;
    field.disabled = !visible;
    field.required = visible && name !== "unsigned" && !(name === "length" && mysqlIntegerTypes.has(base));
  }
  length.max = String(limit);
  if (limit && Number(length.value) > limit) length.value = String(limit);
  if (limit && !mysqlIntegerTypes.has(base) && !length.value) length.value = base === "bit" ? "1" : "255";
  if (base === "decimal") {
    const precision = container.querySelector('[data-field="precision"]');
    container.querySelector('[data-field="scale"]').max = String(Math.min(30, Number(precision.value) || 30));
  }
}

export function mysqlTypeSQL(container, currentType = "") {
  const selected = container.querySelector('[data-field="type"]').value;
  const base = selected || parseMySQLType(currentType).base;
  let type = base;
  if (mysqlLengthLimits[base] || mysqlIntegerTypes.has(base) && container.querySelector('[data-field="length"]').value) type += `(${container.querySelector('[data-field="length"]').value})`;
  if (base === "decimal") type += `(${container.querySelector('[data-field="precision"]').value},${container.querySelector('[data-field="scale"]').value})`;
  if ((mysqlIntegerTypes.has(base) || base === "decimal") && container.querySelector('[data-field="unsigned"]').checked) type += " unsigned";
  return type;
}
