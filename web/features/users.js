import { state } from "../state.js";
import {
  findElement, button, closeDialog, confirmAction, escapeHtml, iconButton, showDialog, toast
} from "../ui.js";
import { refreshSession } from "./session.js";
import { apiRequest } from "../api.js";

// -----------------------------------------------------------------------------
// User accounts
// -----------------------------------------------------------------------------
async function openUsersDialog() {
  if (state.currentUser?.role !== "Super Admin") return;
  const userId = state.currentUser.id;
  const users = await apiRequest("/users");
  if (state.currentUser?.id !== userId) return;
  state.users = users;
  const cards = state.users.map((user) => {
    const own = user.id === state.currentUser.id;
    const actions = own ? "" : `
      ${button("reset-password", "Reset password", "", "ghost", `data-id="${user.id}"`)}
      ${button("toggle-user", user.enabled ? "Disable" : "Enable", "", "ghost", `data-id="${user.id}"`)}
      ${iconButton("delete-user", "Delete " + user.username, "trash", `data-id="${user.id}"`)}`;
    return `<div class="user-item row">
      <span class="avatar">${escapeHtml(user.username.slice(0, 2).toUpperCase())}</span>
      <div class="grow"><h3>${escapeHtml(user.username)}${own ? " (you)" : ""}</h3>
        <small>${user.role} · ${user.enabled ? "Active" : "Disabled"}</small>
      </div>${actions}
    </div>`;
  }).join("");
  const body = `<p class="hint" style="margin-bottom:14px">
    Manage who can access GoSQL. Each user has a private workspace.
  </p>${cards}`;
  const footer = button("close-dialog", "Close") + button("add-user", "Add User", "plus", "primary");
  showDialog("User management", body, footer, true);
}

function openUserForm() {
  showDialog("Add user", `<form id="user-form" class="stack">
    <label>Username<input name="username" required maxlength="64"></label>
    <label>Temporary password<input type="password" name="password" required
      minlength="8" autocomplete="new-password"></label>
    <label>Role<select name="role"><option>User</option><option>Super Admin</option></select></label>
    <button class="primary" type="submit">Create User</button>
  </form>`);
  findElement("#user-form").onsubmit = async (event) => {
    event.preventDefault();
    const formData = new FormData(event.target);
    const username = formData.get("username").trim();
    if (state.users.some((candidateUser) => candidateUser.username === username)) return toast("This username already exists.");
    const submit = event.target.querySelector('[type="submit"]');
    submit.disabled = true;
    try {
      await apiRequest("/users", { method: "POST", body: { username, password: formData.get("password"), role: formData.get("role") } });
      await openUsersDialog();
    } catch (error) { toast(error.message); }
    finally { submit.disabled = false; }
  };
}

function openPasswordDialog(id) {
  const own = id === state.currentUser.id;
  const currentField = own
    ? '<label>Current password<input name="current" type="password" required autocomplete="current-password"></label>'
    : "";
  showDialog(own ? "Change password" : "Reset password", `<form id="change-password-form" class="stack">
    ${currentField}
    <label>New password<input name="password" type="password" required
      minlength="8" autocomplete="new-password"></label>
    <label>Confirm new password<input name="confirm" type="password" required
      minlength="8" autocomplete="new-password"></label>
    <button type="submit" class="primary">Save Password</button>
  </form>`);
  findElement("#change-password-form").onsubmit = async (event) => {
    event.preventDefault();
    const formData = new FormData(event.target);
    if (formData.get("password") !== formData.get("confirm")) return toast("Passwords do not match.");
    const submit = event.target.querySelector('[type="submit"]');
    submit.disabled = true;
    try {
      await apiRequest("/users/" + encodeURIComponent(id) + "/password", {
        method: "POST", body: { current: formData.get("current") || "", password: formData.get("password") }
      });
      closeDialog();
      if (own) await refreshSession();
      toast(own ? "Password updated. Sign in again." : "Password updated. Existing sessions have been signed out.");
    } catch (error) { toast(error.message); }
    finally { submit.disabled = false; }
  };
}

async function toggleUser(id) {
  const candidate = state.users.find(user => user.id === id);
  await apiRequest("/users/" + encodeURIComponent(id), { method: "PATCH", body: { enabled: !candidate.enabled } });
  await openUsersDialog();
}

async function deleteUser(id) {
  await apiRequest("/users/" + encodeURIComponent(id), { method: "DELETE" });
  await openUsersDialog();
}

// -----------------------------------------------------------------------------
// User actions
// -----------------------------------------------------------------------------
export function handleUsersAction(action, element) {
  const id = element.dataset.id;
  switch (action) {
    case "users":
      openUsersDialog().catch(error => toast(error.message));
      break;
    case "add-user":
      openUserForm();
      break;
    case "reset-password":
      openPasswordDialog(id);
      break;
    case "toggle-user": {
      toggleUser(id).catch(error => toast(error.message));
      break;
    }
    case "delete-user":
      confirmAction("Delete user?", "This account and its saved workspace data will be removed.", "Delete User", () => {
        deleteUser(id).catch(error => toast(error.message));
      });
      break;
    case "account":
      showDialog("Your account", `<div class="stack">
        <div class="row">
          <span class="avatar">${escapeHtml(state.currentUser.username.slice(0, 2).toUpperCase())}</span>
          <div><h3>${escapeHtml(state.currentUser.username)}</h3><p class="hint">${state.currentUser.role}</p></div>
        </div>
        ${button("change-password", "Change Password", "key")}${button("logout", "Sign Out", "logout")}
      </div>`);
      break;
    case "change-password":
      openPasswordDialog(state.currentUser.id);
      break;
    default:
      return false;
  }
  return true;
}
