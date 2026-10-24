// Shared helpers for Astrolabe's browser app. Plain ES modules, no build.
// Everything shown comes from /api/, and every string from there is escaped:
// issue titles, replies and mentions are written by strangers.

export const $ = (sel, root = document) => root.querySelector(sel);
export const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

// state is the app's memory between screens.
export const state = { boot: null, product: "" };

const ESC = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };
export const e = v => String(v ?? "").replace(/[&<>"']/g, c => ESC[c]);

// safeURL returns an http(s) URL, or "" for anything else (javascript:, data:).
export function safeURL(u) {
  try {
    const url = new URL(String(u ?? ""), location.href);
    return url.protocol === "http:" || url.protocol === "https:" ? url.href : "";
  } catch { return ""; }
}

// ext links to a page outside Astrolabe, or shows the text if the URL is unsafe.
export function ext(url, text) {
  const href = safeURL(url);
  const label = text || url;
  return href ? `<a href="${e(href)}" target="_blank" rel="noopener noreferrer">${e(label)}<span class="sr-only"> (opens in a new tab)</span></a>` : e(label);
}

export class APIError extends Error {
  constructor(message, status) { super(message); this.status = status; }
}

// api calls the JSON API. Requests that change something carry the CSRF
// token from /api/bootstrap.
export async function api(path, { method = "GET", body } = {}) {
  const headers = { Accept: "application/json" };
  if (method !== "GET") {
    headers["Content-Type"] = "application/json";
    headers["X-CSRF-Token"] = state.boot?.csrf_token || "";
  }
  let res;
  try {
    res = await fetch(path, { method, headers, credentials: "same-origin", body: body === undefined ? undefined : JSON.stringify(body) });
  } catch {
    throw new APIError("Astrolabe isn't answering. Is the server running?", 0);
  }
  if (res.status === 401) {
    location.assign("/login?next=" + encodeURIComponent("/app/" + location.hash));
    throw new APIError("Sign in to continue.", 401);
  }
  const data = (res.headers.get("content-type") || "").includes("application/json") ? await res.json() : null;
  if (!res.ok) throw new APIError(data?.error || `The request failed (${res.status}).`, res.status);
  return data;
}

export const post = (path, body = {}) => api(path, { method: "POST", body });

// query builds "?a=1&b=2" from the non-empty values.
export function query(params) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== null && v !== "") q.set(k, v);
  const s = q.toString();
  return s ? "?" + s : "";
}

// ---- words and numbers ----

export function plural(n, one, many = one + "s") { return `${n} ${n === 1 ? one : many}`; }
export const words = s => String(s ?? "").replace(/_/g, " ");

// span says a duration in seconds in plain words.
export function span(seconds) {
  const s = Math.abs(Math.round(seconds));
  if (s < 60) return "under a minute";
  const m = Math.round(s / 60);
  if (m < 60) return m === 1 ? "1 minute" : `${m} minutes`;
  if (s % 86400 === 0) return s === 86400 ? "1 day" : `${s / 86400} days`;
  const h = Math.round(s / 3600);
  if (h < 48) return h === 1 ? "1 hour" : `${h} hours`;
  const d = Math.round(s / 86400);
  return `${d} days`;
}

export function dateTime(iso) {
  const d = new Date(iso);
  if (Number.isNaN(d.valueOf())) return "";
  return d.toLocaleString(undefined, { weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" });
}

export function day(iso) {
  // A bare date (YYYY-MM-DD) is a calendar day, not midnight UTC.
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(String(iso ?? ""));
  const d = m ? new Date(+m[1], +m[2] - 1, +m[3]) : new Date(iso);
  if (Number.isNaN(d.valueOf())) return "";
  return d.toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" });
}

// ago is "3 hours ago" or "in 2 days", in a <time> element with the full date.
export function ago(iso) {
  const d = new Date(iso);
  if (!iso || Number.isNaN(d.valueOf()) || d.getFullYear() < 2000) return `<span class="muted">never</span>`;
  const secs = (Date.now() - d.valueOf()) / 1000;
  const text = Math.abs(secs) < 60 ? "just now" : secs > 0 ? `${span(secs)} ago` : `in ${span(secs)}`;
  return `<time datetime="${e(d.toISOString())}" title="${e(dateTime(iso))}">${e(text)}</time>`;
}

// ---- building blocks ----

const ICONS = {
  today: '<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>',
  people: '<circle cx="9" cy="8" r="3.5"/><path d="M2.5 20c.8-3.6 3.4-5.5 6.5-5.5s5.7 1.9 6.5 5.5"/><path d="M16 4.6a3.5 3.5 0 0 1 0 6.8M18.5 14.8c1.6.8 2.6 2.5 3 5.2"/>',
  signals: '<path d="M3 12h3l3-7 4 14 3-7h5"/>',
  reception: '<path d="M4 5h16v10H9l-5 4z"/><path d="M8 9h8M8 12h5"/>',
  settings: '<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1Z"/>',
  classic: '<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M3 9h18M8 4v5"/>',
  pipeline: '<rect x="3" y="4" width="5" height="16" rx="1.5"/><rect x="10" y="4" width="5" height="11" rx="1.5"/><rect x="17" y="4" width="4" height="7" rx="1.5"/>',
  campaigns: '<path d="M3 11v2a1 1 0 0 0 1 1h3l5 4V6L7 10H4a1 1 0 0 0-1 1Z"/><path d="M16 9a4 4 0 0 1 0 6M19 6a8 8 0 0 1 0 12"/>',
  review: '<path d="M9 4h6v3H9z"/><path d="M8 5H6a1 1 0 0 0-1 1v14a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V6a1 1 0 0 0-1-1h-2"/><path d="m9 13 2 2 4-4"/>',
  updates: '<rect x="3" y="5" width="18" height="14" rx="2"/><path d="m3 7 9 6 9-6"/>',
  calendar: '<rect x="3" y="5" width="18" height="16" rx="2"/><path d="M16 3v4M8 3v4M3 10h18"/>',
  funnel: '<path d="M3 4h18l-7 9v6l-4 2v-8z"/>',
  automations: '<circle cx="6" cy="6" r="2.5"/><circle cx="18" cy="18" r="2.5"/><path d="M8.5 6H15a3 3 0 0 1 3 3v6.5M6 8.5V15a3 3 0 0 0 3 3h6.5"/>',
  outbox: '<path d="M4 13h4l2 3h4l2-3h4"/><path d="M4 13 6 5h12l2 8v6H4z"/>',
  digest: '<path d="M6 3h9l4 4v14H6z"/><path d="M14 3v5h5M9 12h7M9 16h7"/>',
  close: '<path d="m6 6 12 12M18 6 6 18"/>',
};
export function icon(name) {
  return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">${ICONS[name] || ""}</svg>`;
}

// pageHead is every screen's title and its one line explaining the terms.
export function pageHead(title, explain) {
  return `<div class="page-head"><h1>${e(title)}</h1><p class="explain">${explain}</p></div>`;
}

// empty says there is nothing here, and what to do next (HTML allowed in next).
export function empty(title, next) {
  return `<div class="empty"><h2>${e(title)}</h2><p>${next}</p></div>`;
}

export function productName(id) {
  return state.boot?.products.find(p => p.id === id)?.name || id || "";
}

export function productOptions(selected, { all = false } = {}) {
  const ps = state.boot?.products || [];
  return (all ? `<option value="">All products</option>` : "") +
    ps.map(p => `<option value="${e(p.id)}"${p.id === selected ? " selected" : ""}>${e(p.name)}</option>`).join("");
}

// currentProduct is the product picked in the top bar, or the first one for
// screens that need exactly one.
export function currentProduct({ required = false } = {}) {
  const ps = state.boot?.products || [];
  if (state.product && ps.some(p => p.id === state.product)) return ps.find(p => p.id === state.product);
  return required ? ps[0] : null;
}

// field renders a labelled control. kind: text, textarea, select, date, number, email, url, checkbox.
let fieldSeq = 0;
export function field(name, label, { kind = "text", value = "", help = "", required = false, options = "", attrs = "" } = {}) {
  const id = `f-${name}-${++fieldSeq}`;
  const helpId = help ? `${id}-help` : "";
  const desc = helpId ? ` aria-describedby="${helpId}"` : "";
  const req = required ? " required" : "";
  const helpHTML = help ? `<p class="help" id="${helpId}">${help}</p>` : "";
  if (kind === "checkbox") {
    return `<div class="field"><label class="check" for="${id}"><input type="checkbox" id="${id}" name="${e(name)}"${value ? " checked" : ""}${desc} ${attrs}> ${e(label)}</label>${helpHTML}</div>`;
  }
  let control;
  if (kind === "textarea") control = `<textarea id="${id}" name="${e(name)}"${req}${desc} ${attrs}>${e(value)}</textarea>`;
  else if (kind === "select") control = `<select id="${id}" name="${e(name)}"${req}${desc} ${attrs}>${options}</select>`;
  else control = `<input type="${kind}" id="${id}" name="${e(name)}" value="${e(value)}"${req}${desc} ${attrs}>`;
  return `<div class="field"><label for="${id}">${e(label)}</label>${control}${helpHTML}</div>`;
}

// formData reads a form into a plain object (checkboxes as booleans).
export function formData(form) {
  const out = {};
  for (const el of form.elements) {
    if (!el.name || el.disabled) continue;
    if (el.type === "checkbox") out[el.name] = el.checked;
    else if (el.type === "radio") { if (el.checked) out[el.name] = el.value; }
    else if (el.type !== "submit" && el.type !== "button") out[el.name] = el.value;
  }
  return out;
}

// ---- dialog ----

let lastFocus = null;

// openDialog shows a modal dialog. The browser keeps focus inside it; Escape
// or Close shuts it and focus returns to what opened it.
export function openDialog({ title, description = "", body, wide = false }) {
  const d = $("#dialog");
  lastFocus = document.activeElement;
  d.className = wide ? "wide" : "";
  d.innerHTML = `<div class="dialog-head"><div><h2 id="dialog-title">${e(title)}</h2>${description ? `<p id="dialog-desc">${description}</p>` : ""}</div>
    <button type="button" class="icon-btn" data-close aria-label="Close">${icon("close")}</button></div>
    <div class="dialog-body">${body}</div>`;
  if (description) d.setAttribute("aria-describedby", "dialog-desc"); else d.removeAttribute("aria-describedby");
  if (!d.open) d.showModal();
  const first = d.querySelector("[autofocus]") || d.querySelector(".dialog-body input:not([type=hidden]):not([readonly]), .dialog-body textarea, .dialog-body select") || d.querySelector(".dialog-body button") || d.querySelector("[data-close]");
  first?.focus();
  return d;
}

let restoreOnClose = true;

// dialogOpener is the element that opened the open dialog.
export const dialogOpener = () => ($("#dialog").open ? lastFocus : null);

// closeDialog shuts the dialog; restore=false leaves focus to the caller.
export function closeDialog(restore = true) {
  const d = $("#dialog");
  restoreOnClose = restore;
  if (d.open) d.close();
}

export function initDialog() {
  const d = $("#dialog");
  d.addEventListener("close", () => {
    // The close event comes a moment later; if another dialog has opened
    // since, it is not this one's to clear.
    if (d.open) return;
    d.innerHTML = "";
    if (restoreOnClose) {
      if (lastFocus?.isConnected) lastFocus.focus();
      else $("#main").focus({ preventScroll: true });
    }
    restoreOnClose = true;
    lastFocus = null;
  });
  d.addEventListener("click", ev => {
    if (ev.target.closest("[data-close]")) closeDialog();
  });
}

// showFormError puts an error at the top of a form (announced to screen readers).
export function showFormError(form, message) {
  let box = form.querySelector(".form-error");
  if (!box) {
    box = document.createElement("p");
    box.className = "form-error";
    box.setAttribute("role", "alert");
    form.prepend(box);
  }
  box.textContent = message;
  box.hidden = !message;
}

// ---- toast ----

let toastTimer;
export function toast(message, kind = "ok") {
  const t = $("#toast");
  t.textContent = message;
  t.dataset.kind = kind;
  t.setAttribute("aria-live", kind === "error" ? "assertive" : "polite");
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, kind === "error" ? 10000 : 6000);
}
