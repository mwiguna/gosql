import assert from "node:assert/strict";
import { test } from "node:test";
import { transferFormats } from "../features/transfers.js";
import { downloadFile, uploadSQLite } from "../api.js";

test("transfer formats match database and table scope for every engine", () => {
  for (const [engine, native] of [["PostgreSQL", "sql"], ["MySQL", "sql"], ["MariaDB", "sql"], ["SQLite", "sqlite"]]) {
    const formats = engine === "PostgreSQL" ? ["sql", "dump"] : [native];
    assert.deepEqual(transferFormats(engine, "", false).map(([value]) => value), formats);
    assert.deepEqual(transferFormats(engine, "", true).map(([value]) => value), [...formats, "csv"]);
    assert.deepEqual(transferFormats(engine, "table", true).map(([value]) => value), ["csv"]);
    assert.deepEqual(transferFormats(engine, "table", false).map(([value]) => value), engine === "SQLite" ? ["csv"] : [...formats, "csv"]);
  }
});

test("exports download real files and surface backend failures", async context => {
  const link = { click: () => { link.clicked = true; } };
  globalThis.document = { createElement: () => link };
  context.after(() => { delete globalThis.document; });
  context.mock.method(URL, "createObjectURL", blob => { assert.equal(blob.size, 4); return "blob:export"; });
  context.mock.method(globalThis, "setTimeout", () => 1);
  context.mock.method(globalThis, "fetch", async (path, options) => {
    assert.equal(options.credentials, "same-origin");
    assert.equal(path, "/api/connections/id/transfer?format=csv");
    return { ok: true, blob: async () => new Blob(["data"]) };
  });
  await downloadFile("/connections/id/transfer?format=csv", "table.csv");
  assert.equal(link.download, "table.csv");
  assert.equal(link.href, "blob:export");
  assert.equal(link.clicked, true);
  context.mock.method(globalThis, "fetch", async () => ({ ok: false, status: 400, json: async () => ({ error: { message: "pg_dump is required" } }) }));
  await assert.rejects(downloadFile("/connections/id/transfer", "db.dump"), /pg_dump is required/);
});

test("import sends a file upload and returns the actual imported row count", async context => {
  context.mock.method(globalThis, "fetch", async (path, options) => {
    assert.equal(path, "/api/connections/id/transfer?format=csv");
    assert.equal(options.method, "POST");
    assert.equal(options.body.get("file").name, "rows.csv");
    return { ok: true, status: 200, json: async () => ({ rows: 42 }) };
  });
  assert.deepEqual(await uploadSQLite("/connections/id/transfer?format=csv", new File(["id\n1"], "rows.csv")), { rows: 42 });
});
