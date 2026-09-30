import assert from "node:assert/strict";
import { test } from "node:test";
import { apiRequest, initializeApi, connectionFromProfile, uploadSQLite } from "../api.js";

test("API sends JSON with same-origin cookies and handles empty responses", async context => {
  let received;
  context.mock.method(globalThis, "fetch", async (url, options) => {
    received = { url, ...options };
    return { ok: true, status: 204 };
  });
  assert.equal(await apiRequest("/connections/one", { method: "DELETE" }), null);
  assert.equal(received.url, "/api/connections/one");
  assert.equal(received.credentials, "same-origin");
  assert.equal(received.cache, "no-store");
  await apiRequest("/login", { method: "POST", body: { username: "admin" } });
  assert.equal(received.headers["Content-Type"], "application/json");
  assert.deepEqual(JSON.parse(received.body), { username: "admin" });
});

test("API errors preserve status and invalidate unauthorized sessions", async context => {
  let invalidated = 0;
  const logged = context.mock.method(console, "error", () => {});
  initializeApi({ onUnauthorized: () => invalidated++ });
  context.mock.method(globalThis, "fetch", async () => ({
    ok: false, status: 401, json: async () => ({ error: { code: "unauthorized", message: "Sign in to continue.", detail: "Session expired.", databaseMessage: "Original error" } })
  }));
  await assert.rejects(apiRequest("/connections"), error => error.status === 401 && error.code === "unauthorized" && error.detail === "Session expired." && error.databaseMessage === "Original error");
  assert.equal(invalidated, 1);
  assert.equal(logged.mock.calls.length, 1);
  assert.equal(logged.mock.calls[0].arguments[1].detail, "Session expired.");
});

test("profile mapping uses configured database without inventing discovered databases", () => {
  const profile = { id: "pg", database: "company", engine: "PostgreSQL" };
  assert.deepEqual(connectionFromProfile(profile).databases, ["company"]);
  assert.equal(profile.databases, undefined);
  assert.deepEqual(connectionFromProfile({ engine: "MySQL" }).databases, []);
});

test("SQLite profiles use the file basename and uploads send multipart data", async context => {
  assert.deepEqual(connectionFromProfile({ engine: "SQLite", file: "/srv/sqlite/customer.db", database: "old" }).databases, ["customer"]);
  assert.deepEqual(connectionFromProfile({ engine: "SQLite", file: ".sqlite" }).databases, ["main"]);
  let received;
  context.mock.method(globalThis, "fetch", async (url, options) => {
    received = { url, ...options };
    return { ok: true, status: 200, json: async () => ({ status: "ok" }) };
  });
  const file = new Blob(["SQLite format 3\0"], { type: "application/vnd.sqlite3" });
  await uploadSQLite("/connections/profile/sqlite-file", file);
  assert.equal(received.url, "/api/connections/profile/sqlite-file");
  assert.equal(received.credentials, "same-origin");
  assert.equal(received.headers, undefined);
  assert.equal(received.body.get("file").size, file.size);
});
