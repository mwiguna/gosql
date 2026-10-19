import assert from "node:assert/strict";
import { test } from "node:test";
import { cellEditor } from "../features/grid.js";
import { state } from "../state.js";

function edit(context, column, original, engine = "PostgreSQL") {
  const createElement = tagName => ({
    tagName, value: "", selectedIndex: 0, children: [], events: {},
    append(...children) { this.children.push(...children); },
    setAttribute() {}, focus() {}, select() {}, reportValidity() {},
    addEventListener(name, handler) { this.events[name] = handler; },
    fire(name, details = {}) {
      const event = { prevented: false, preventDefault() { this.prevented = true; }, stopPropagation() {}, ...details };
      this.events[name]?.(event);
      return event;
    }
  });
  globalThis.document = { createElement };
  state.connections = [{ id: "connection", engine }];
  state.tabs = [{ id: "table", connectionId: "connection", remote: true, schema: [{ name: "value", ...column }] }];
  state.activeTabId = "table";
  context.after(() => { delete globalThis.document; state.tabs = []; state.connections = []; state.activeTabId = null; });
  const saved = [];
  let cancelled = 0;
  let navigated = 0;
  const editor = cellEditor({
    getValue: () => original, getField: () => "c0",
    getRow: () => ({ getData: () => ({ _row: 0 }) }),
    navigateNext() { navigated++; }
  }, render => render(), value => saved.push(value), () => { cancelled++; });
  return { editor, input: editor.tagName === "div" ? editor.children[0] : editor, saved,
    get cancelled() { return cancelled; }, get navigated() { return navigated; } };
}

test("numeric editor rejects typing and pasting text and preserves decimal precision", context => {
  const { input, saved } = edit(context, { type: "numeric(30,10)" }, "9007199254740993.1234567890");
  assert.equal(input.inputMode, "decimal");
  input.selectionStart = input.selectionEnd = input.value.length;
  assert.equal(input.fire("beforeinput", { data: "x" }).prevented, true);
  input.value = "not a number";
  input.fire("input");
  assert.equal(input.value, "9007199254740993.1234567890");
  input.value = "9007199254740994.1234567891";
  input.fire("input");
  input.fire("keydown", { key: "Enter" });
  assert.deepEqual(saved, ["9007199254740994.1234567891"]);
});

test("integer editor blocks decimals, unsigned negatives, and incomplete saves", context => {
  const result = edit(context, { type: "bigint unsigned" }, "18446744073709551614", "MySQL");
  const { input, saved } = result;
  for (const invalid of ["-1", "2.5", "abc"]) {
    input.value = invalid;
    input.fire("input");
    assert.equal(input.value, "18446744073709551614");
  }
  input.value = "";
  input.fire("keydown", { key: "Tab" });
  assert.equal(result.navigated, 0);
  assert.deepEqual(saved, []);
  input.value = "18446744073709551615";
  input.fire("keydown", { key: "Tab" });
  assert.equal(result.navigated, 1);
  assert.deepEqual(saved, ["18446744073709551615"]);
});

test("boolean offers only true and false and saves once on selection", context => {
  const result = edit(context, { type: "boolean" }, "f");
  assert.equal(result.input.tagName, "select");
  assert.equal(result.input.value, "false");
  assert.deepEqual(result.input.children.map(option => option.value), ["true", "false"]);
  result.input.value = "true";
  result.input.fire("change");
  result.input.fire("blur");
  assert.deepEqual(result.saved, ["true"]);
  assert.equal(result.cancelled, 0);
});

test("SQLite boolean selections preserve numeric storage", context => {
  const { input, saved } = edit(context, { type: "BOOLEAN" }, "1", "SQLite");
  input.value = "false";
  input.fire("change");
  assert.deepEqual(saved, ["0"]);
});

test("MySQL TINYINT(1) remains numeric and accepts values beyond 0 and 1", context => {
  const { input, saved } = edit(context, { type: "tinyint(1)" }, "0", "MySQL");
  assert.equal(input.tagName, "input");
  assert.equal(input.inputMode, "numeric");
  input.value = "3";
  input.fire("keydown", { key: "Enter" });
  assert.deepEqual(saved, ["3"]);
});

test("PostgreSQL enum dropdown treats labels as text and accepts only database choices", context => {
  const { input, saved } = edit(context, { type: '"public"."status"', enumValues: ["", "pending", '<done "now">'] }, "pending");
  assert.equal(input.tagName, "select");
  assert.equal(input.children[2].textContent, '<done "now">');
  input.value = "invalid";
  input.fire("change");
  assert.deepEqual(saved, []);
  input.value = "";
  input.fire("change");
  assert.deepEqual(saved, [""]);
});

test("MySQL enum handles commas, quotes, backslashes and empty labels", context => {
  const { input, saved } = edit(context, { type: String.raw`enum('','a,b','it''s','a\\b','line\nend','a\'b','line\rend')` }, "a,b", "MySQL");
  assert.equal(input.tagName, "select");
  assert.deepEqual(input.children.map(option => option.value), ["", "a,b", "it's", "a\\b", "line\nend", "a'b", "line\rend"]);
  input.value = "a'b";
  input.fire("change");
  assert.deepEqual(saved, ["a'b"]);
});

test("date selection saves immediately and Escape cancels without saving", context => {
  const result = edit(context, { type: "date" }, "2026-10-19");
  assert.equal(result.input.type, "date");
  result.input.fire("keydown", { key: "Escape" });
  result.input.value = "2026-10-20";
  result.input.fire("change");
  assert.deepEqual(result.saved, []);
  assert.equal(result.cancelled, 1);
});

test("timestamp picker preserves microseconds and zone without conversion", context => {
  const { editor, input, saved } = edit(context, { type: "timestamp(6) with time zone" }, "2026-10-19 23:59:58.123456-05:30");
  assert.equal(input.type, "datetime-local");
  assert.equal(input.value, "2026-10-19T23:59:58");
  assert.equal(editor.children[1].textContent, ".123456-05:30");
  input.value = "2026-10-20T00:01";
  input.fire("change");
  assert.deepEqual(saved, ["2026-10-20 00:01:00.123456-05:30"]);
});

test("time picker preserves fractional seconds and offset", context => {
  const { input, saved } = edit(context, { type: "time(6) with time zone" }, "09:30:45.000001+07");
  assert.equal(input.type, "time");
  assert.equal(input.value, "09:30:45");
  input.value = "10:15:01";
  input.fire("change");
  assert.deepEqual(saved, ["10:15:01.000001+07"]);
});

test("opening and confirming an unchanged timestamp sends no mutation", context => {
  const result = edit(context, { type: "timestamp" }, "2026-10-19 12:30:00.123456");
  result.input.fire("keydown", { key: "Enter" });
  assert.deepEqual(result.saved, []);
  assert.equal(result.cancelled, 1);
});

test("arrays keep their text editor instead of scalar restrictions", context => {
  const { input } = edit(context, { type: "integer[]" }, "{1,2}");
  assert.equal(input.type, "text");
  assert.equal(input.inputMode, undefined);
});
