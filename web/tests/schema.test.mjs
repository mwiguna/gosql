import assert from "node:assert/strict";
import { test } from "node:test";
import { changeSQL } from "../features/schema.js";
import { state } from "../state.js";

const tab = { schemaName: "odd\"schema", table: "orders" };

test("index preview separates standard and unique indexes and quotes identifiers", () => {
  const value = { name: "orders_idx", columns: ["customer_id", "a.b"], unique: false };
  assert.equal(changeSQL(tab, "index", value), 'CREATE INDEX "orders_idx" ON "odd""schema"."orders" USING btree ("customer_id", "a.b");');
  assert.match(changeSQL(tab, "index", { ...value, unique: true }), /^CREATE UNIQUE INDEX/);
});

test("constraint preview covers all concept types", () => {
  const base = { name: "rule", columns: ["customer_id"] };
  assert.match(changeSQL(tab, "constraint", { ...base, type: "PRIMARY KEY" }), /PRIMARY KEY \("customer_id"\)/);
  assert.match(changeSQL(tab, "constraint", { ...base, type: "UNIQUE" }), /UNIQUE \("customer_id"\)/);
  assert.match(changeSQL(tab, "constraint", { ...base, type: "CHECK", columns: [], expression: "amount >= 0" }), /CHECK \(amount >= 0\)/);
  assert.equal(changeSQL(tab, "constraint", { ...base, type: "FOREIGN KEY", referenceSchema: "public", referenceTable: "customers", referenceColumns: ["id"] }), 'ALTER TABLE "odd""schema"."orders" ADD CONSTRAINT "rule" FOREIGN KEY ("customer_id") REFERENCES "public"."customers" ("id") NOT VALID;\nALTER TABLE "odd""schema"."orders" VALIDATE CONSTRAINT "rule";');
});

test("MySQL previews use database names, backticks, and native constraint SQL", context => {
  state.connections = [{ id: "mysql", engine: "MySQL" }];
  context.after(() => { state.connections = []; });
  const mysqlTab = { connectionId: "mysql", db: "sales`db", schemaName: "", table: "orders" };
  assert.equal(changeSQL(mysqlTab, "index", { name: "by_customer", columns: ["customer_id"], unique: true }),
    "ALTER TABLE `sales``db`.`orders` ADD UNIQUE INDEX `by_customer` (`customer_id`);");
  assert.equal(changeSQL(mysqlTab, "constraint", { name: "fk", type: "FOREIGN KEY", columns: ["customer_id"], referenceTable: "customers", referenceColumns: ["id"] }),
    "ALTER TABLE `sales``db`.`orders` ADD CONSTRAINT `fk` FOREIGN KEY (`customer_id`) REFERENCES `sales``db`.`customers` (`id`);");
  assert.equal(changeSQL(mysqlTab, "constraint", { name: "nonnegative", type: "CHECK", columns: [], expression: "amount >= 0" }),
    "ALTER TABLE `sales``db`.`orders` ADD CONSTRAINT `nonnegative` CHECK (amount >= 0);");
  state.connections = [{ id: "mysql", engine: "MariaDB" }];
  assert.equal(changeSQL(mysqlTab, "constraint", { name: "nonnegative", type: "CHECK", columns: [], expression: "amount >= 0" }),
    "ALTER TABLE `sales``db`.`orders` ADD CONSTRAINT `nonnegative` CHECK (amount >= 0);");
});
