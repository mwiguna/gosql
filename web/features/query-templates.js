export function querySQLTemplate(engine, tab, command) {
  const mysql = ["MySQL", "MariaDB"].includes(engine);
  const quote = name => mysql ? "`" + name.replaceAll("`", "``") + "`" : '"' + name.replaceAll('"', '""') + '"';
  const namespace = engine === "SQLite" ? "main" : engine === "PostgreSQL" ? tab.schemaName || "public" : tab.db;
  const table = [namespace, tab.table || "table_name"].filter(Boolean).map(quote).join(".");
  const columns = (tab.schema || []).filter(column => column.editable !== false && !column.generated && !column.hidden && column.key !== "ROWID");
  const names = columns.length ? columns.map(column => column.name) : ["column_name"];
  const schema = tab.schema || [];
  const validKey = names => names?.length && names.every(name => schema.some(column => column.name === name));
  const primary = schema.filter(column => column.key === "PRIMARY KEY")
    .sort((left, right) => (left.primaryOrder || 0) - (right.primaryOrder || 0)).map(column => column.name);
  const constraints = tab.constraints || [];
  const indexes = tab.indexes || [];
  const declaredPrimary = constraints.find(item => item.type === "PRIMARY KEY" && validKey(item.columns))?.columns;
  const indexedPrimary = indexes.find(item => item.primary && validKey(item.columns))?.columns;
  const identity = validKey(tab.primaryKey) ? tab.primaryKey : [];
  const regularIdentity = identity.filter(name => schema.find(column => column.name === name)?.key !== "ROWID");
  const unique = constraints.find(item => item.type === "UNIQUE" && validKey(item.columns))?.columns
    || indexes.find(item => item.unique && !item.partial && !item.expression && validKey(item.columns))?.columns;
  const keys = primary.length ? primary : declaredPrimary || indexedPrimary
    || (regularIdentity.length ? regularIdentity : null) || unique || (identity.length ? identity : null)
    || schema.filter(column => column.key === "ROWID").map(column => column.name);
  const condition = keys.length ? keys.map(name => {
    const column = schema.find(column => column.name === name);
    const numeric = column.key === "ROWID" || /^(?:INTEGER|NUMERIC|REAL)$/i.test(column.affinity || "")
      || /^(?:tinyint|smallint|mediumint|int(?:eger)?|bigint|int[248]|serial|bigserial|smallserial|numeric|decimal|real|float|double)(?:\b|\()/i.test(column.type || "");
    return `${quote(name)} = ${numeric ? "1" : "'value'"}`;
  }).join(" AND ") : `${quote("key_column")} = 1`;
  switch (command) {
    case "SELECT": return `SELECT *\nFROM ${table}\nLIMIT 100;`;
    case "INSERT": return `-- Replace the sample values before running.\nINSERT INTO ${table} (${names.map(quote).join(", ")})\nVALUES (${names.map(() => "'value'").join(", ")});`;
    case "UPDATE": {
      const column = columns.find(column => !keys.includes(column.name));
      return `-- Replace the sample value and WHERE condition before running.\nUPDATE ${table}\nSET ${quote(column?.name || "column_name")} = 'value'\nWHERE ${condition};`;
    }
    case "DELETE": return `-- Replace the WHERE condition before running.\nDELETE FROM ${table}\nWHERE ${condition};`;
    default: throw new Error("Unknown query template.");
  }
}
