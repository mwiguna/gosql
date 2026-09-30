import test from "node:test";
import assert from "node:assert/strict";
import { columnTypes, columnTypeSQL } from "../features/column_types.js";

test("PostgreSQL column choices include integer variants and configurable numeric types", () => {
  for (const type of ["smallint", "integer", "bigint", "numeric", "decimal", "jsonb", "timestamp with time zone"]) {
    assert.ok(columnTypes.includes(type), `${type} is missing`);
  }
  assert.ok(!columnTypes.includes("pg_lsn"));
  assert.ok(!columnTypes.includes("gtrgm"));
  assert.equal(columnTypeSQL("numeric", "255", "30", "4"), "numeric(30,4)");
  assert.equal(columnTypeSQL("decimal", "255", "12", "0"), "decimal(12,0)");
  assert.equal(columnTypeSQL("varchar", "120", "18", "2"), "varchar(120)");
  assert.equal(columnTypeSQL("numeric", "255", "30", "4", true), "numeric(30,4)[]");
  assert.equal(columnTypeSQL("integer", "255", "18", "2"), "integer");
});
