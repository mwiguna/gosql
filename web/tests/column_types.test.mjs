import assert from "node:assert/strict";
import { test } from "node:test";
import { mysqlTypeFields, mysqlTypeSQL, parseMySQLType } from "../features/column_types.js";

test("MySQL type controls retain existing dimensions and unsigned", () => {
  assert.deepEqual(parseMySQLType("BIGINT(20) UNSIGNED"), { base: "bigint", length: "20", scale: "", unsigned: true });
  assert.deepEqual(parseMySQLType("decimal(12,4)"), { base: "decimal", length: "12", scale: "4", unsigned: false });
  const fields = mysqlTypeFields("varchar(80)", true);
  assert.match(fields, /Keep current type \(varchar\(80\)\)/);
  assert.match(fields, /data-field="length"[^>]*value="80"/);
});

test("binary types are hidden from new choices but preserved for existing columns", () => {
  const newFields = mysqlTypeFields();
  assert.doesNotMatch(newFields, /<option value="(?:binary|varbinary)"/);
  const existingFields = mysqlTypeFields("varbinary(64)", true);
  assert.match(existingFields, /Keep current type \(varbinary\(64\)\)/);
  assert.match(existingFields, /data-field="length"[^>]*value="64"/);
});

test("MySQL type SQL uses entered length, precision, and unsigned", () => {
  const values = { type: "varchar", length: "80", precision: "18", scale: "2", unsigned: false };
  const form = { querySelector: selector => {
    const value = values[selector.match(/data-field="([^"]+)"/)[1]];
    return { value, checked: value };
  } };
  assert.equal(mysqlTypeSQL(form), "varchar(80)");
  values.type = "decimal";
  assert.equal(mysqlTypeSQL(form), "decimal(18,2)");
  values.unsigned = true;
  assert.equal(mysqlTypeSQL(form), "decimal(18,2) unsigned");
  values.type = "bigint";
  values.length = "";
  values.unsigned = true;
  assert.equal(mysqlTypeSQL(form), "bigint unsigned");
});
