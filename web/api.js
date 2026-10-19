// -----------------------------------------------------------------------------
// HTTP API
// -----------------------------------------------------------------------------
let onUnauthorized;

export function initializeApi(callbacks) {
  onUnauthorized = callbacks.onUnauthorized;
}

export async function apiRequest(path, { method = "GET", body, signal } = {}) {
  let response;
  try {
    response = await fetch("/api" + path, {
      method, signal, credentials: "same-origin", cache: "no-store",
      headers: body === undefined ? {} : { "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body)
    });
    const data = response.status === 204 ? null : await response.json();
    if (response.ok) return data;
    if (response.status === 401) onUnauthorized?.();
    const error = new Error(data?.error?.message || "Request failed.");
    error.status = response.status;
    error.code = data?.error?.code;
    error.detail = data?.error?.detail;
    error.sqlState = data?.error?.sqlState;
    error.hint = data?.error?.hint;
    error.databaseMessage = data?.error?.databaseMessage;
    console.error("GoSQL API error", { method, path, status: response.status, ...data?.error }, error);
    throw error;
  } catch (error) {
    if (!response && error.name !== "AbortError") console.error("GoSQL API request failed", { method, path }, error);
    else if (response && error.status === undefined && error.name !== "AbortError") console.error("GoSQL API response failed", { method, path, status: response.status }, error);
    throw error;
  }
}

export async function uploadFile(path, file) {
  const body = new FormData();
  body.append("file", file);
  const response = await fetch("/api" + path, { method: "POST", body, credentials: "same-origin", cache: "no-store" });
  const data = await response.json();
  if (response.status === 401) onUnauthorized?.();
  if (!response.ok) {
    const error = new Error(data?.error?.message || "File upload failed.");
    error.code = data?.error?.code;
    throw error;
  }
  return data;
}

export { uploadFile as uploadSQLite };

export async function downloadFile(path, filename) {
  const response = await fetch("/api" + path, { credentials: "same-origin", cache: "no-store" });
  if (!response.ok) {
    if (response.status === 401) onUnauthorized?.();
    const data = await response.json();
    throw new Error(data?.error?.message || "Export failed.");
  }
  const url = URL.createObjectURL(await response.blob());
  const link = document.createElement("a");
  link.href = url;
  link.download = filename;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 60000);
}

export async function downloadSQLite(id, filename) {
  const path = "/connections/" + encodeURIComponent(id);
  await apiRequest(path + "/catalog");
  const link = document.createElement("a");
  link.href = "/api" + path + "/sqlite-file";
  link.download = filename || "database.sqlite";
  link.click();
}

export function connectionFromProfile(profile) {
  const database = profile.engine === "SQLite"
    ? profile.file ? profile.file.split(/[\\/]/).at(-1).replace(/\.[^.]+$/, "") || "main" : ""
    : profile.database;
  return { ...profile, databases: database ? [database] : [] };
}
