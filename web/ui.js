
// -----------------------------------------------------------------------------
// HTML and DOM helpers
// -----------------------------------------------------------------------------
const paths = {
  database: "M20 6c0 2-3.6 3.5-8 3.5S4 8 4 6s3.6-3.5 8-3.5S20 4 20 6ZM4 6v12c0 2 3.6 3.5 8 3.5s8-1.5 8-3.5V6M4 12c0 2 3.6 3.5 8 3.5s8-1.5 8-3.5",
  monitor: "M3 4h18v14H3ZM12 18v4M8 22h8",
  table: "M3 4h18v16H3ZM3 9h18M9 9v11M15 9v11M3 14h18",
  folder: "M3 6h7l2 2h9v12H3ZM3 6V4h7l2 2",
  plus: "M12 5v14M5 12h14",
  x: "m6 6 12 12M18 6 6 18",
  chevron: "m9 5 7 7-7 7",
  down: "m5 9 7 7 7-7",
  search: "M10.5 18a7.5 7.5 0 1 0 0-15 7.5 7.5 0 0 0 0 15Zm5.5-2 5 5",
  terminal: "m4 5 6 7-6 7M13 19h7",
  play: "m8 4 12 8-12 8Z",
  stop: "M6 6h12v12H6Z",
  check: "m5 12 4 4L20 5",
  clock: "M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20ZM12 6v6l4 2",
  bookmark: "M6 3h12v18l-6-4-6 4Z",
  settings: "M12.000 4.400L13.424 4.535L14.790 2.397L18.293 4.229L17.317 6.570L18.582 8.200L19.177 9.501L21.711 9.615L21.877 13.564L19.361 13.890L18.582 15.800L17.753 16.966L18.921 19.218L15.584 21.336L14.044 19.320L12.000 19.600L10.576 19.465L9.210 21.603L5.707 19.771L6.683 17.430L5.418 15.800L4.823 14.499L2.289 14.385L2.123 10.436L4.639 10.110L5.418 8.200L6.247 7.034L5.079 4.782L8.416 2.664L9.956 4.680ZM15.4 12a3.4 3.4 0 1 0-6.8 0 3.4 3.4 0 0 0 6.8 0Z",
  user: "M16 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0ZM4 21v-3a8 5 0 0 1 16 0v3Z",
  users: "M14 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0ZM2 21v-3a8 5 0 0 1 16 0v3ZM18 3a4 4 0 0 1 0 8M20 14c2 1 2 3 2 6",
  help: "M22 12a10 10 0 1 0-20 0 10 10 0 0 0 20 0ZM9.1 9a3 3 0 0 1 5.8 1c0 2-3 2-3 4M12 17h.01",
  lock: "M5 10h14v11H5ZM8 10V6a4 4 0 0 1 8 0v4M12 14v3",
  key: "M14 10a5 5 0 1 0-4 4l3 3h3v3h4v-4Z",
  filter: "M3 5h18l-7 8v7l-4-2v-5Z",
  sort: "M8 4v16m-4-4 4 4 4-4M16 20V4m-4 4 4-4 4 4",
  refresh: "M20 8a9 9 0 1 0 0 8M20 3v5h-5",
  download: "M12 3v12m-5-5 5 5 5-5M4 16v5h16v-5",
  upload: "M12 16V4m-5 5 5-5 5 5M4 16v5h16v-5",
  trash: "M3 6h18M9 6V3h6v3M5 6l1 15h12l1-15M10 10v7M14 10v7",
  edit: "m4 16 12-12 4 4L8 20H4ZM14 6l4 4",
  file: "M5 2h9l5 5v15H5ZM14 2v6h5M8 13h8M8 17h6",
  menu: "M4 6h16M4 12h16M4 18h16",
  logout: "M9 4H4v16h5M10 12h12m-5-5 5 5-5 5",
  branch: "M6 4v16M6 7h12v13M6 14h12M3 20h6M15 20h6M3 4h6",
  external: "M14 3h7v7M21 3 10 14M10 4H3v17h17v-8",
  copy: "M9 8h12v14H9ZM5 17H2V2h12v3",
  arrow: "M20 12H4m6-6-6 6 6 6",
  eye: "M2 12s4-7 10-7 10 7 10 7-4 7-10 7S2 12 2 12ZM15 12a3 3 0 1 1-6 0 3 3 0 0 1 6 0",
  info: "M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20ZM12 10v7M12 6h.01"
};

export const icon = (name, className = "") => `<svg class="${className}" viewBox="0 0 24 24" aria-hidden="true"><path d="${paths[name] || paths.database}"/></svg>`;

export const escapeHtml = (value) => String(value ?? "").replace(/[&<>"']/g, (character) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[character]);

export const findElement = (selector, root = document) => root.querySelector(selector);

export const findElements = (selector, root = document) => [...root.querySelectorAll(selector)];

export const createId = () => crypto.randomUUID();

const databaseLogos = {
  PostgreSQL: "assets/postgresql.svg",
  MySQL: "assets/mysql.svg",
  MariaDB: "assets/mysql.svg",
  SQLite: "assets/sqlite.svg"
};

export function engineIcon(engine) {
  const source = databaseLogos[engine];
  if (!source) return icon("database");
  // Logo terpisah dari SVG ikon agar warna dan bentuk aslinya tidak ditimpa style stroke.
  return `<img class="database-logo" src="${source}" width="17" height="17" alt="" aria-hidden="true" draggable="false">`;
}

export const button = (action, label, iconName = "", className = "", attributes = "") => `<button type="button" data-action="${action}" class="${className}" ${attributes}>${iconName ? icon(iconName) : ""}${label}</button>`;

export const iconButton = (action, label, iconName, attributes = "") => button(action, "", iconName, "icon ghost", `aria-label="${escapeHtml(label)}" title="${escapeHtml(label)}" ${attributes}`);

export const brand = () => `<div class="brand"><span class="brand-mark">${icon("database")}</span>GoSQL</div>`;

// -----------------------------------------------------------------------------
// Dialogs and notifications
// -----------------------------------------------------------------------------
let toastTimer;

export function toast(message) {
  clearTimeout(toastTimer);
  const notice = findElement("#toast");
  const dialog = findElement("#dialog");
  if (dialog?.open && notice.parentElement !== dialog) dialog.append(notice);
  else if (!dialog?.open && notice.parentElement === dialog) document.body.append(notice);
  notice.textContent = message;
  notice.classList.add("visible");
  toastTimer = setTimeout(() => notice.classList.remove("visible"), 3200);
}

export function showDialog(title, body, footer = "", wide = false) {
  const dialog = findElement("#dialog");
  const notice = findElement("#toast");
  if (notice.parentElement === dialog) document.body.append(notice);
  dialog.innerHTML = `<div class="modal-head"><h2>${title}</h2>${iconButton("close-dialog", "Close dialog", "x")}</div><div class="modal-body">${body}</div>${footer ? `<div class="modal-footer">${footer}</div>` : ""}`;
  dialog.style.width = wide ? "680px" : "500px";
  if (!dialog.open) dialog.showModal();
  dialog.append(notice);
}

export function closeDialog() {
  findElement("#dialog").close();
}

export function confirmAction(title, message, label, callback) {
  showDialog(title, `<p>${message}</p>`, button("close-dialog", "Cancel") + button("confirm-action", label, "", "primary"));
  findElement('#dialog [data-action="confirm-action"]').onclick = () => {
    closeDialog();
    callback();
  };
}

// -----------------------------------------------------------------------------
// Dialog events
// -----------------------------------------------------------------------------
export function initializeDialogs() {
  findElement("#dialog").addEventListener("close", () => {
    findElements('#dialog input[type="password"]').forEach(input => { input.value = ""; });
    const notice = findElement("#toast");
    if (notice.parentElement === findElement("#dialog")) document.body.append(notice);
  });
  findElement("#dialog").addEventListener("click", (event) => {
    if (event.target !== findElement("#dialog")) return;
    const rect = event.target.getBoundingClientRect();
    if (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom) closeDialog();
  });
}
