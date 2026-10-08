import assert from "node:assert/strict";
import { test } from "node:test";
import { querySQLTemplate } from "../features/query-templates.js";
import { setQueryText } from "../features/editor.js";
import { EditorState } from "@codemirror/state";
import { history, undo } from "@codemirror/commands";

test("query templates quote names for every database and omit generated columns", () => {
  const tab = { db: "shop`db", schemaName: 'my"schema', table: 'odd"`table', schema: [
    { name: "rowid", key: "ROWID", editable: false },
    { name: 'col"`name', editable: true },
    { name: "generated", generated: "STORED" },
    { name: "hidden", hidden: true }
  ] };
  for (const engine of ["PostgreSQL", "MySQL", "MariaDB", "SQLite"]) {
    const table = engine === "PostgreSQL" ? '"my""schema"."odd""`table"'
      : engine === "SQLite" ? '"main"."odd""`table"' : '`shop``db`.`odd"``table`';
    assert.equal(querySQLTemplate(engine, tab, "SELECT"), `SELECT *\nFROM ${table}\nLIMIT 100;`);
    const insert = querySQLTemplate(engine, tab, "INSERT");
    assert.ok(insert.includes(`INSERT INTO ${table}`));
    assert.doesNotMatch(insert, /generated|hidden|rowid/);
    for (const command of ["UPDATE", "DELETE"]) {
      assert.ok(querySQLTemplate(engine, tab, command).includes(table));
      assert.match(querySQLTemplate(engine, tab, command), /WHERE ["`]rowid["`] = 1;/);
    }
  }
  assert.match(querySQLTemplate("PostgreSQL", { db: "shop" }, "INSERT"), /"public"\."table_name" \("column_name"\)/);
});

test("mutation templates prefer primary keys, then unique keys, including composite keys", () => {
  for (const engine of ["PostgreSQL", "MySQL", "MariaDB", "SQLite"]) {
    const quote = name => ["MySQL", "MariaDB"].includes(engine) ? `\`${name}\`` : `"${name}"`;
    const tab = { db: "shop", schemaName: "public", table: "items", primaryKey: ["rowid"], schema: [
      { name: "rowid", key: "ROWID", type: "INTEGER", editable: false },
      { name: "id", key: "PRIMARY KEY", type: "bigint", editable: true },
      { name: "email", type: "TEXT", editable: true }
    ], indexes: [{ unique: true, columns: ["email"] }] };
    for (const command of ["UPDATE", "DELETE"]) {
      assert.ok(querySQLTemplate(engine, tab, command).includes(`WHERE ${quote("id")} = 1;`));
    }
    delete tab.schema[1].key;
    assert.ok(querySQLTemplate(engine, tab, "DELETE").includes(`WHERE ${quote("email")} = 'value';`));
    tab.indexes = [{ unique: true, partial: true, columns: ["email"] }, { unique: true, columns: ["id", "email"] }];
    assert.ok(querySQLTemplate(engine, tab, "DELETE").includes(`WHERE ${quote("id")} = 1 AND ${quote("email")} = 'value';`));
    tab.constraints = [{ type: "PRIMARY KEY", columns: ["email", "id"] }];
    assert.ok(querySQLTemplate(engine, tab, "DELETE").includes(`WHERE ${quote("email")} = 'value' AND ${quote("id")} = 1;`));
    assert.ok(querySQLTemplate(engine, { schema: [] }, "DELETE").includes(`WHERE ${quote("key_column")} = 1;`));
  }
});

test("replacing console SQL updates saved editor state and can be undone", () => {
  const original = "SELECT 'previous query';";
  for (const engine of ["PostgreSQL", "MySQL", "MariaDB", "SQLite"]) {
    for (const command of ["SELECT", "INSERT", "UPDATE", "DELETE"]) {
      const tab = { id: "active", db: "shop", schemaName: "public", table: "items", sql: original,
        editorState: EditorState.create({ doc: original, extensions: [history()] }) };
      const template = querySQLTemplate(engine, tab, command);
      setQueryText(tab, template);
      assert.equal(tab.sql, template);
      assert.equal(tab.editorState.doc.toString(), template);
      assert.equal(tab.editorState.selection.main.anchor, 0);
      assert.equal(undo({ state: tab.editorState, dispatch: transaction => { tab.editorState = transaction.state; } }), true);
      assert.equal(tab.editorState.doc.toString(), original);
    }
  }
  const unmounted = { id: "new", sql: original };
  setQueryText(unmounted, "SELECT 2;");
  assert.equal(unmounted.sql, "SELECT 2;");
});
