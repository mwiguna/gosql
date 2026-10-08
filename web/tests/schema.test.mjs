import assert from "node:assert/strict";
import { test } from "node:test";
import { changeSQL, columnUniqueLabel, renderStructure } from "../features/schema.js";
import { state } from "../state.js";

const tab = { schemaName: "odd\"schema", table: "orders" };

test("Structure unique labels distinguish single keys and composite groups without duplicates", () => {
  const metadata = { schema: [{ name: "id", key: "PRIMARY KEY" }, { name: "nik" }, { name: "phone" }, { name: "email" }, { name: "name" }],
    primaryKey: ["id"], constraints: [{ type: "PRIMARY KEY", columns: ["id"] }, { type: "UNIQUE", columns: ["nik", "phone"] }],
    indexes: [{ primary: true, unique: true, columns: ["id"] }, { unique: true, columns: ["nik", "phone"] },
      { unique: true, columns: ["email"] }, { unique: false, columns: ["name"] }] };
  assert.equal(columnUniqueLabel(metadata, "id"), "yes");
  assert.equal(columnUniqueLabel(metadata, "id", false), "no");
  assert.equal(columnUniqueLabel(metadata, "nik"), "yes (nik,phone)");
  assert.equal(columnUniqueLabel(metadata, "phone"), "yes (nik,phone)");
  assert.equal(columnUniqueLabel(metadata, "email"), "yes");
  assert.equal(columnUniqueLabel(metadata, "name"), "no");
  metadata.indexes.push({ unique: true, columns: ["nik"] }, { unique: true, columns: ["nik", "email"] });
  assert.equal(columnUniqueLabel(metadata, "nik"), "yes (nik,phone); yes; yes (nik,email)");
  assert.equal(columnUniqueLabel({ schema: [{ name: "name" }], indexes: [
    { unique: true, partial: true, columns: ["name"] }, { unique: true, expression: true, columns: ["name"] }
  ] }, "name"), "no");
});

test("Structure renders Unique for every engine and escapes grouped column names", context => {
  const previous = state.connections;
  context.after(() => { state.connections = previous; });
  for (const engine of ["PostgreSQL", "MySQL", "MariaDB", "SQLite"]) {
    state.connections = [{ id: "connection", engine }];
    const area = {};
    const metadata = { connectionId: "connection", db: "shop", schemaName: "public", table: "items", view: "Structure", rows: [],
      schema: [{ name: "nik", type: "TEXT", nullable: true }, { name: "phone<script>", type: "TEXT" }, { name: "name", type: "TEXT" }],
      constraints: [{ type: "UNIQUE", columns: ["nik", "phone<script>"] }], indexes: [] };
    renderStructure(metadata, area);
    assert.match(area.innerHTML, /<th>Unique<\/th>/);
    assert.equal((area.innerHTML.match(/<td class="schema-yes">yes \(nik,phone&lt;script&gt;\)<\/td>/g) || []).length, 2);
    assert.match(area.innerHTML, /<td class="schema-no">no<\/td>/);
    assert.match(area.innerHTML, /<td class="schema-yes">Yes<\/td>/);
    assert.match(area.innerHTML, /<td class="schema-no">No<\/td>/);
    assert.doesNotMatch(area.innerHTML, /<script>/);
  }
});

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
  assert.match(changeSQL(tab, "constraint", { ...base, type: "FOREIGN KEY", referenceSchema: "public", referenceTable: "customers", referenceColumns: ["id"], onUpdate: "CASCADE", onDelete: "SET DEFAULT" }), /ON UPDATE CASCADE ON DELETE SET DEFAULT NOT VALID;/);
});

test("MySQL previews use database names, backticks, and native constraint SQL", context => {
  state.connections = [{ id: "mysql", engine: "MySQL" }];
  context.after(() => { state.connections = []; });
  const mysqlTab = { connectionId: "mysql", db: "sales`db", schemaName: "", table: "orders" };
  assert.equal(changeSQL(mysqlTab, "index", { name: "by_customer", columns: ["customer_id"], unique: true }),
    "ALTER TABLE `sales``db`.`orders` ADD UNIQUE INDEX `by_customer` (`customer_id`);");
  assert.equal(changeSQL(mysqlTab, "constraint", { name: "fk", type: "FOREIGN KEY", columns: ["customer_id"], referenceTable: "customers", referenceColumns: ["id"] }),
    "ALTER TABLE `sales``db`.`orders` ADD CONSTRAINT `fk` FOREIGN KEY (`customer_id`) REFERENCES `sales``db`.`customers` (`id`);");
  assert.match(changeSQL(mysqlTab, "constraint", { name: "fk", type: "FOREIGN KEY", columns: ["customer_id"], referenceTable: "customers", referenceColumns: ["id"], onUpdate: "RESTRICT", onDelete: "SET NULL" }), /ON UPDATE RESTRICT ON DELETE SET NULL;$/);
  assert.equal(changeSQL(mysqlTab, "constraint", { name: "nonnegative", type: "CHECK", columns: [], expression: "amount >= 0" }),
    "ALTER TABLE `sales``db`.`orders` ADD CONSTRAINT `nonnegative` CHECK (amount >= 0);");
  state.connections = [{ id: "mysql", engine: "MariaDB" }];
  assert.equal(changeSQL(mysqlTab, "constraint", { name: "nonnegative", type: "CHECK", columns: [], expression: "amount >= 0" }),
    "ALTER TABLE `sales``db`.`orders` ADD CONSTRAINT `nonnegative` CHECK (amount >= 0);");
});
