import { findElement, findElements, icon } from "../ui.js";
import { getActiveTab, getConnectionById, state } from "../state.js";
import { querySQLTemplate } from "./query-templates.js";

// -----------------------------------------------------------------------------
// Editor library and private state
// -----------------------------------------------------------------------------
let EditorState, EditorView, keymap, lineNumbers;
let highlightActiveLine, highlightActiveLineGutter, panels;
let defaultKeymap, history, historyKeymap, indentWithTab;
let searchKeymap, highlightSelectionMatches, openSearchPanel, closeSearchPanel;
let searchPanelOpen, search, SearchQuery, getSearchQuery, setSearchQuery;
let findNext, findPrevious, replaceNext, replaceAll;
let autocompletion, completionKeymap, syntaxHighlighting, defaultHighlightStyle;
let sql, PostgreSQL, MySQL, SQLite;
let editorLibrary;
let editor = null;
let editorResizeObserver = null;
let editorTabId = null;

export function loadEditorLibrary() {
  return editorLibrary ??= Promise.all([
    import("@codemirror/state"),
    import("@codemirror/view"),
    import("@codemirror/commands"),
    import("@codemirror/search"),
    import("@codemirror/autocomplete"),
    import("@codemirror/language"),
    import("@codemirror/lang-sql")
  ]).then(([stateModule, viewModule, commands, searchModule, autocompleteModule, language, sqlModule]) => {
    ({ EditorState } = stateModule);
    ({ EditorView, keymap, lineNumbers, highlightActiveLine, highlightActiveLineGutter, panels } = viewModule);
    ({ defaultKeymap, history, historyKeymap, indentWithTab } = commands);
    ({
      searchKeymap, highlightSelectionMatches, openSearchPanel, closeSearchPanel,
      searchPanelOpen, search, SearchQuery, getSearchQuery, setSearchQuery,
      findNext, findPrevious, replaceNext, replaceAll
    } = searchModule);
    ({ autocompletion, completionKeymap } = autocompleteModule);
    ({ syntaxHighlighting, defaultHighlightStyle } = language);
    ({ sql, PostgreSQL, MySQL, SQLite } = sqlModule);
  });
}

// -----------------------------------------------------------------------------
// Editor lifecycle
// -----------------------------------------------------------------------------
export function mountEditor(tab, connection, onRunQuery) {
  if (tab.searchHost) findElement("#query-search").replaceWith(tab.searchHost);
  editorTabId = tab.id;

  let editorState = tab.editorState;
  if (!editorState) {
    const dialect = connection.engine === "PostgreSQL" ? PostgreSQL
      : ["MySQL", "MariaDB"].includes(connection.engine) ? MySQL : SQLite;
    const schema = { [tab.table]: tab.schema.map(column => column.name) };
    const extensions = [
      panels({ topContainer: findElement("#query-search") }),
      search({ top: true, createPanel: querySearchPanel }),
      lineNumbers(), highlightActiveLine(), highlightActiveLineGutter(),
      history(), syntaxHighlighting(defaultHighlightStyle),
      sql({ dialect, schema }), autocompletion(), highlightSelectionMatches(),
      keymap.of([
        { key: "Mod-Enter", run: () => { onRunQuery(); return true; } },
        indentWithTab, ...defaultKeymap, ...historyKeymap,
        ...searchKeymap, ...completionKeymap
      ]),
      EditorView.updateListener.of((update) => {
        if (update.docChanged) tab.sql = update.state.doc.toString();
        tab.findOpen = searchPanelOpen(update.state);
        const query = getSearchQuery(update.state);
        tab.findQuery = {
          search: query.search, replace: query.replace,
          caseSensitive: query.caseSensitive, wholeWord: query.wholeWord,
          regexp: query.regexp
        };
        findElement('[data-action="editor-search"]')?.setAttribute("aria-expanded", String(tab.findOpen));
      }),
      EditorView.contentAttributes.of({ "aria-label": "SQL editor" })
    ];
    editorState = EditorState.create({ doc: tab.sql, extensions });
  }
  editor = new EditorView({ parent: findElement("#editor"), state: editorState });

  const wrap = findElement("#editor");
  if (tab.editorHeight != null) wrap.style.height = tab.editorHeight + "px";
  let observedHeight = wrap.offsetHeight;
  editorResizeObserver = new ResizeObserver(() => {
    const height = wrap.offsetHeight;
    if (height && height !== observedHeight) tab.editorHeight = height;
    observedHeight = height;
  });
  editorResizeObserver.observe(wrap);
  if (!tab.editorState) {
    const reopen = tab.findOpen;
    if (tab.findQuery) editor.dispatch({ effects: setSearchQuery.of(new SearchQuery(tab.findQuery)) });
    if (reopen) openSearchPanel(editor);
  }
}

export function destroyEditor() {
  editorResizeObserver?.disconnect();
  editorResizeObserver = null;
  if (!editor) return;
  // Simpan EditorState sebelum melepas view agar undo history tetap tersedia.
  const tab = state.tabs.find(item => item.id === editorTabId);
  if (tab) {
    tab.editorState = editor.state;
    tab.searchHost = findElement("#query-search");
    tab.searchHost.style.display = "";
  }
  editor.destroy();
  editor = null;
  editorTabId = null;
}

export function getQueryText(tab) {
  const selection = editor?.state.selection.main;
  return selection && !selection.empty ? editor.state.sliceDoc(selection.from, selection.to) : tab.sql;
}

export function setQueryText(tab, text) {
  const view = editorTabId === tab.id ? editor : null;
  const current = view?.state || tab.editorState;
  const transaction = current && {
    changes: { from: 0, to: current.doc.length, insert: text },
    selection: { anchor: 0 }
  };
  if (view) view.dispatch(transaction);
  else if (current) tab.editorState = current.update(transaction).state;
  tab.sql = text;
}

// -----------------------------------------------------------------------------
// Console and search panel
// -----------------------------------------------------------------------------
function toggleConsole(activeTab, onRunQuery) {
  const section = findElement(".console");
  const parts = ["#query-search", "#editor", ".console-caption"];
  if (activeTab.consoleOpen && !findElement("#editor")) {
    section.insertAdjacentHTML("beforeend", `<div id="query-search"></div><div class="editor-wrap" id="editor"></div><div class="console-caption">${icon("info")}Tab to indent · Shift+Tab to unindent · Esc then Tab to leave editor.<span class="spacer"></span><span>SQL</span></div>`);
    mountEditor(activeTab, getConnectionById(activeTab.connectionId), onRunQuery);
  } else for (const selector of parts) {
    const element = findElement(selector, section);
    if (element) element.style.display = activeTab.consoleOpen ? "" : "none";
  }
  const toggle = findElement('[data-action="toggle-console"]', section);
  toggle.innerHTML = icon(activeTab.consoleOpen ? "down" : "chevron") + "Query console";
  findElement('[data-action="editor-search"]', section).setAttribute("aria-expanded", String(Boolean(activeTab.consoleOpen && activeTab.findOpen)));
}

function querySearchPanel(view) {
  const dom = document.createElement("div");
  dom.className = "query-find-panel";
  dom.setAttribute("role", "search");
  dom.setAttribute("aria-label", "Find and replace in SQL");
  dom.innerHTML = `<div class="query-find-row">
    <label class="query-find-field">${icon("search")}
      <input data-search-field="search" main-field="true" aria-label="Find in query"
        placeholder="Find in query…" autocomplete="off" spellcheck="false">
    </label>
    <div class="query-find-options">
      <button type="button" data-search-option="caseSensitive" title="Match Case" aria-label="Match Case" aria-pressed="false">Aa</button>
      <button type="button" data-search-option="wholeWord" title="Whole Word" aria-label="Whole Word" aria-pressed="false">Ab</button>
      <button type="button" data-search-option="regexp" title="Regular Expression" aria-label="Regular Expression" aria-pressed="false">.*</button>
    </div>
    <span class="query-find-count" role="status" aria-live="polite"></span>
    <div class="query-find-navigation">
      <button type="button" data-search-command="previous" class="icon" aria-label="Previous match" title="Previous Match (Shift+Enter)">${icon("chevron")}</button>
      <button type="button" data-search-command="next" class="icon" aria-label="Next match" title="Next Match (Enter)">${icon("chevron")}</button>
      <button type="button" data-search-command="close" class="icon ghost" aria-label="Close find and replace" title="Close (Escape)">${icon("x")}</button>
    </div>
  </div>
  <div class="query-find-row">
    <label class="query-find-field">${icon("edit")}
      <input data-search-field="replace" aria-label="Replace with"
        placeholder="Replace with…" autocomplete="off" spellcheck="false">
    </label>
    <div class="query-replace-actions">
      <button type="button" data-search-command="replace">Replace</button>
      <button type="button" data-search-command="replaceAll">Replace All</button>
    </div>
    <span class="query-find-hint">Changes affect SQL text only.</span>
  </div>`;
  const find = findElement('[data-search-field="search"]', dom);
  const replacement = findElement('[data-search-field="replace"]', dom);
  const countLabel = findElement(".query-find-count", dom);
  let matchCount = 0;
  let matches = [];
  let lastDoc = null;
  let lastPattern = "";
  function sync() {
    const query = getSearchQuery(view.state);
    if (find.value !== query.search) find.value = query.search;
    if (replacement.value !== query.replace) replacement.value = query.replace;
    findElements("[data-search-option]", dom).forEach((buttonElement) => buttonElement.setAttribute("aria-pressed", String(query[buttonElement.dataset.searchOption])));
    const pattern = JSON.stringify([query.search, query.caseSensitive, query.wholeWord, query.regexp]);
    // Perubahan selection cukup memperbarui indeks aktif, tanpa mencari ulang seluruh SQL.
    if (lastDoc !== view.state.doc || lastPattern !== pattern) {
      lastDoc = view.state.doc;
      lastPattern = pattern;
      matches = [];
      if (query.valid) {
        const cursor = query.getCursor(view.state);
        for (let match = cursor.next(); !match.done && matches.length < 1000; match = cursor.next()) matches.push([match.value.from, match.value.to]);
      }
    }
    matchCount = matches.length;
    const selection = view.state.selection.main;
    const selected = matches.findIndex(([from, to]) => selection.from === from && selection.to === to) + 1;
    const invalid = Boolean(query.search) && !query.valid;
    find.setAttribute("aria-invalid", String(invalid));
    countLabel.textContent = invalid ? "Invalid expression" : !query.search ? "Enter text to find" : matchCount === 1000 ? "1,000+ matches" : selected ? `${selected} of ${matchCount}` : `${matchCount} ${matchCount === 1 ? "match" : "matches"}`;
    findElements("[data-search-command]", dom).forEach((buttonElement) => {
      if (buttonElement.dataset.searchCommand !== "close") buttonElement.disabled = !query.valid || !matchCount;
    });
  }
  function updateQuery(changes) {
    const query = getSearchQuery(view.state);
    view.dispatch({ effects: setSearchQuery.of(new SearchQuery({ search: find.value, replace: replacement.value, caseSensitive: query.caseSensitive, wholeWord: query.wholeWord, regexp: query.regexp, ...changes })) });
  }
  find.addEventListener("input", () => updateQuery());
  replacement.addEventListener("input", () => updateQuery());
  dom.addEventListener("click", (event) => {
    const option = event.target.closest("[data-search-option]");
    if (option) updateQuery({ [option.dataset.searchOption]: !getSearchQuery(view.state)[option.dataset.searchOption] });
    const command = event.target.closest("[data-search-command]");
    if (!command || command.disabled) return;
    const action = command.dataset.searchCommand;
    if (action === "close") {
      closeSearchPanel(view);
      view.focus();
    } else ({ previous: findPrevious, next: findNext, replace: replaceNext, replaceAll })[action](view);
  });
  dom.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      closeSearchPanel(view);
      view.focus();
    } else if (event.key === "Enter" && event.target.tagName === "INPUT") {
      event.preventDefault();
      if (event.target === replacement) replaceNext(view);
      else (event.shiftKey ? findPrevious : findNext)(view);
    }
  });
  sync();
  return { dom, top: true, mount: () => find.focus(), update: (update) => {
    if (update.docChanged || update.selectionSet || update.transactions.some((tr) => tr.effects.some((effect) => effect.is(setSearchQuery)))) sync();
  } };
}

// -----------------------------------------------------------------------------
// User actions
// -----------------------------------------------------------------------------
export function handleEditorAction(action, element, onRunQuery) {
  const activeTab = getActiveTab();
  switch (action) {
    case "query-template": {
      const command = element.value;
      const connection = activeTab && getConnectionById(activeTab.connectionId);
      if (!connection || !["SELECT", "INSERT", "UPDATE", "DELETE"].includes(command)) break;
      setQueryText(activeTab, querySQLTemplate(connection.engine, activeTab, command));
      activeTab.queryTemplate = command;
      activeTab.consoleOpen = true;
      toggleConsole(activeTab, onRunQuery);
      editor?.focus();
      break;
    }
    case "toggle-console":
      activeTab.consoleOpen = !activeTab.consoleOpen;
      toggleConsole(activeTab, onRunQuery);
      break;
    case "editor-search":
      if (!editor) {
        activeTab.consoleOpen = true;
        activeTab.findOpen = false;
        toggleConsole(activeTab, onRunQuery);
      } else if (!activeTab.consoleOpen) {
        activeTab.consoleOpen = true;
        toggleConsole(activeTab, onRunQuery);
      }
      if (searchPanelOpen(editor.state)) {
        closeSearchPanel(editor);
        editor.focus();
      } else openSearchPanel(editor);
      break;
    default:
      return false;
  }
  return true;
}
