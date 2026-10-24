// Astrolabe's browser app: the shell, the router and the event wiring.
// Each screen is a module with load(), render() and its actions; this file
// renders the chrome (navigation, breadcrumbs, mode and pause badges) and
// runs every button and form through the same error handling.
import { $, $$, e, api, state, icon, toast, initDialog, closeDialog, dialogOpener, showFormError, plural } from "./lib.js";

const screens = [];

// Every classic page now has a screen here; the classic pages stay
// reachable from the sidebar's foot until they are removed.

let current = { screen: null, params: {}, data: null };
let loading = 0;

function parseHash() {
  const raw = location.hash.replace(/^#/, "");
  const [path, qs] = raw.split("?");
  const parts = path.split("/").filter(Boolean);
  const params = Object.fromEntries(new URLSearchParams(qs || ""));
  if (parts[1]) params.id = decodeURIComponent(parts[1]);
  return { id: parts[0] || "today", params };
}

// ---- chrome ----

function renderNav() {
  const sandbox = state.boot?.mode === "sandbox";
  const shown = s => !s.sandboxOnly || sandbox;
  const work = screens.filter(s => s.nav === "work" && shown(s));
  const insight = screens.filter(s => s.nav === "insight" && shown(s));
  const setup = screens.filter(s => s.nav === "setup");
  const active = current.screen?.section || current.screen?.id;
  const link = s => `<li><a class="nav-link" href="#${s.id}"${s.id === active ? ' aria-current="page"' : ""}>${icon(s.icon)}<span>${e(s.title)}</span></a></li>`;
  const group = (id, label, items) => items ? `<p class="nav-caption" id="nav-${id}">${label}</p><ul class="nav-list" aria-labelledby="nav-${id}">${items}</ul>` : "";
  $("#navigation").innerHTML = `
    ${group("work", "Work", work.map(link).join(""))}
    ${group("insight", "Results", insight.map(link).join(""))}
    ${group("setup", "Setup", setup.map(link).join(""))}
    <div class="sidebar-foot"><p>The <a href="/">classic pages</a> still work for now.</p>
      <form method="post" action="/logout"><button type="submit">Sign out</button></form></div>`;
}

function renderBadges() {
  const b = state.boot;
  if (!b) return;
  const mode = b.mode === "sandbox"
    ? `<span class="badge sandbox" title="${e(b.mode_note)}">Sandbox<span class="sr-only">: ${e(b.mode_note)}</span></span>`
    : `<span class="badge live" title="${e(b.mode_note)}">Live<span class="sr-only">: ${e(b.mode_note)}</span></span>`;
  let pause = "";
  if (b.pauses.length) {
    const what = b.pauses.includes("global") ? "All sending paused" : `Sending paused: ${plural(b.pauses.length, "pause")}`;
    pause = `<a class="badge paused" href="#settings" title="${e(b.pauses.join(", "))}">${e(what)}</a>`;
  }
  $("#status-badges").innerHTML = mode + pause;
  const sel = $("#product-filter");
  const ps = b.products;
  if (!ps.some(p => p.id === state.product)) state.product = "";
  sel.innerHTML = `<option value="">All products</option>` + ps.map(p => `<option value="${e(p.id)}"${p.id === state.product ? " selected" : ""}>${e(p.name)}</option>`).join("");
  sel.closest("label").hidden = ps.length < 2 && !state.product;
}

function renderCrumbs() {
  const s = current.screen;
  const trail = s?.crumbs ? s.crumbs(current.data, current.params) : [{ label: s?.title || "Not found" }];
  $("#breadcrumbs").innerHTML = `<li><a href="#today">Astrolabe</a></li>` + trail.map((c, i) => {
    const last = i === trail.length - 1;
    return `<li>${c.href && !last ? `<a href="${e(c.href)}">${e(c.label)}</a>` : `<span${last ? ' aria-current="page"' : ""}>${e(c.label)}</span>`}</li>`;
  }).join("");
  document.title = `${trail[trail.length - 1]?.label || "Astrolabe"} · Astrolabe`;
  $("#page-actions").innerHTML = s?.pageActions ? s.pageActions(current.data, current.params) : "";
}

export async function refreshBoot() {
  state.boot = await api("/api/bootstrap");
  renderBadges();
  renderNav();
}

// ---- screens ----

function context() {
  return { data: current.data, params: current.params, done, refresh: () => show({ keepFocus: true }) };
}

// done finishes an action: close any dialog, reload, say what happened, and
// put focus back where the operator was (or on the next item).
async function done(message, kind = "ok") {
  const target = describe(dialogOpener() || document.activeElement);
  closeDialog(false);
  await Promise.all([refreshBoot(), show({ keepFocus: true, target })]);
  if (message) toast(message, kind);
}

// describe remembers an element well enough to find it, or its successor,
// after the screen is rendered again: selectors built from its stable
// attributes (most specific first), and the item it was in.
const FOCUSABLE = "button:not([disabled]), a[href], input:not([disabled]):not([type=hidden]), select:not([disabled]), textarea:not([disabled]), [tabindex='0']";

function describe(a) {
  if (!a || a === document.body) return null;
  const q = (k, v) => `[${k}="${CSS.escape(String(v))}"]`;
  const sels = [];
  const d = a.dataset || {};
  // Generated field ids (f-...) change with every render; others are stable.
  if (a.id && !a.id.startsWith("f-")) sels.push(`#${CSS.escape(a.id)}`);
  const act = d.action ? q("data-action", d.action) : d.change ? q("data-change", d.change) : "";
  if (act) {
    const extra = ["id", "item", "person", "scope", "class", "sent", "product", "monitor"].filter(k => d[k] !== undefined).map(k => q(`data-${k}`, d[k])).join("");
    sels.push(act + extra);
  }
  const form = a.closest?.("form[data-form]");
  if (form) {
    const f = `form${q("data-form", form.dataset.form)}${form.dataset.id ? q("data-id", form.dataset.id) : ""}`;
    if (a.name) sels.push(`${f} ${q("name", a.name)}${a.value && a.type === "submit" ? q("value", a.value) : ""}`);
    if (a.tagName === "BUTTON") sels.push(`${f} button:not([type=button])`);
  }
  if (!$("#main").contains(a)) return { sels };
  const item = a.closest("[data-key]");
  const items = $$("[data-key]", $("#main"));
  return { sels, key: item?.dataset.key, index: item ? items.indexOf(item) : -1 };
}

// restoreFocus puts focus on the remembered element if it is still there
// and can take focus, else on the next control in its item, else on the
// next item, else on the main content.
function restoreFocus(t) {
  const main = $("#main");
  if (!t) return;
  const tryFocus = el => { if (!el) return false; el.focus(); return document.activeElement === el; };
  for (const s of t.sels || []) {
    const el = document.querySelector(s);
    if (tryFocus(el)) return;
    // Disabled now (approved, say): the next control near it.
    const near = el?.closest("[data-key], form, .item-actions, section");
    if (near && tryFocus(near.querySelector(FOCUSABLE))) return;
  }
  if (t.key) {
    const item = main.querySelector(`[data-key="${CSS.escape(t.key)}"]`);
    if (item && tryFocus(item.querySelector(FOCUSABLE))) return;
    const items = $$("[data-key]", main);
    const next = items[Math.min(t.index, items.length - 1)];
    if (next && tryFocus(next.querySelector(FOCUSABLE))) return;
  }
  main.focus({ preventScroll: true });
}

let firstShow = true;

async function show({ keepFocus = false, target } = {}) {
  const { id, params } = parseHash();
  const screen = screens.find(s => s.id === id);
  const changed = screen !== current.screen || JSON.stringify(params) !== JSON.stringify(current.params);
  const focus = keepFocus && !changed ? (target === undefined ? describe(document.activeElement) : target) : null;
  const scroll = window.scrollY;
  current = { screen, params, data: changed ? null : current.data };
  const main = $("#main");
  const ticket = ++loading;
  if (!screen) {
    main.innerHTML = `<div class="page-head"><h1>Not found</h1><p class="explain">There is no screen called “${e(id)}”. <a href="#today">Go to Today</a>.</p></div>`;
    renderNav(); renderCrumbs();
    return;
  }
  if (changed) {
    main.innerHTML = `<p class="loading" role="status">Loading ${e(screen.title)}…</p>`;
    renderNav(); renderCrumbs();
  }
  try {
    const data = await screen.load(params);
    if (ticket !== loading) return; // a newer navigation won
    current.data = data;
    main.innerHTML = screen.render(data, params);
    screen.after?.(data, params);
  } catch (err) {
    if (ticket !== loading) return;
    main.innerHTML = `<div class="page-head"><h1>${e(screen.title)}</h1></div><p class="notice bad" role="alert">${e(err.message)}</p>
      <p><button class="btn" data-action="retry" type="button">Try again</button></p>`;
  }
  renderNav();
  renderCrumbs();
  const wasFirst = firstShow;
  firstShow = false;
  if (changed) {
    window.scrollTo(0, 0);
    // Moving between screens puts focus at the new screen's start; the
    // first load leaves it alone, so the first Tab reaches the skip link.
    if (!wasFirst) main.focus({ preventScroll: true });
  } else {
    window.scrollTo(0, scroll);
    restoreFocus(focus);
  }
}

// ---- events ----

function handlerFor(kind, name) {
  return current.screen?.[kind]?.[name] || GLOBAL[kind][name];
}

const GLOBAL = {
  actions: {
    retry: async () => { await show(); },
  },
  forms: {},
};

// Busy controls are marked aria-busy rather than disabled: disabling the
// focused button would throw keyboard focus back to the top of the page.
async function runAction(btn, name = btn.dataset.action) {
  const fn = handlerFor("actions", name);
  if (!fn || btn.getAttribute("aria-busy") === "true") return;
  btn.setAttribute("aria-busy", "true");
  try {
    await fn(btn, context());
  } catch (err) {
    const form = btn.closest("form");
    if (form && $("#dialog").contains(form)) showFormError(form, err.message);
    else toast(err.message, "error");
  } finally {
    btn.removeAttribute("aria-busy");
  }
}

async function runForm(form, submitter) {
  const fn = handlerFor("forms", form.dataset.form);
  if (!fn || form.getAttribute("aria-busy") === "true") return;
  form.setAttribute("aria-busy", "true");
  showFormError(form, "");
  try {
    await fn(form, context(), submitter);
  } catch (err) {
    showFormError(form, err.message);
    form.querySelector(".form-error")?.scrollIntoView({ block: "nearest" });
  } finally {
    form.removeAttribute("aria-busy");
  }
}

function wire() {
  initDialog();
  document.addEventListener("click", ev => {
    // The skip link moves focus; it must not change the route.
    if (ev.target.closest(".skip-link")) {
      ev.preventDefault();
      $("#main").focus();
      return;
    }
    const btn = ev.target.closest("[data-action]");
    if (btn && !btn.disabled) { if (btn.tagName !== "A") ev.preventDefault(); runAction(btn); }
  });
  document.addEventListener("submit", ev => {
    const form = ev.target.closest("form[data-form]");
    if (!form) return;
    ev.preventDefault();
    runForm(form, ev.submitter);
  });
  document.addEventListener("change", ev => {
    const el = ev.target.closest("[data-change]");
    if (el) runAction(el, el.dataset.change);
  });
  // Cmd/Ctrl+Enter in a draft editor saves and approves it.
  document.addEventListener("keydown", ev => {
    if ((ev.metaKey || ev.ctrlKey) && ev.key === "Enter") {
      const form = ev.target.closest?.("form[data-form]");
      const primary = form?.querySelector("[data-hotkey=submit]");
      if (primary) { ev.preventDefault(); primary.click(); }
    }
  });
  $("#product-filter").addEventListener("change", ev => {
    state.product = ev.target.value;
    try { localStorage.setItem("astrolabe.product", state.product); } catch { /* private window */ }
    show();
  });
  const sidebar = $(".sidebar");
  const toggle = $(".menu-toggle");
  toggle.addEventListener("click", () => {
    const open = !sidebar.classList.contains("open");
    sidebar.classList.toggle("open", open);
    toggle.setAttribute("aria-expanded", String(open));
  });
  window.addEventListener("hashchange", () => {
    sidebar.classList.remove("open");
    toggle.setAttribute("aria-expanded", "false");
    show();
  });
  // Keep the badges and the current screen fresh while the tab is visible.
  setInterval(async () => {
    const busy = document.hidden || $("#dialog").open || ["INPUT", "TEXTAREA", "SELECT"].includes(document.activeElement?.tagName);
    if (busy) return;
    try {
      await refreshBoot();
      if (current.screen?.poll) await show({ keepFocus: true });
    } catch { /* the next tick tries again */ }
  }, 60000);
}

export function register(...list) { screens.push(...list); }

export async function start() {
  try { state.product = localStorage.getItem("astrolabe.product") || ""; } catch { /* private window */ }
  wire();
  try {
    await refreshBoot();
  } catch (err) {
    $("#main").innerHTML = `<div class="page-head"><h1>Astrolabe</h1></div><p class="notice bad" role="alert">${e(err.message)}</p>`;
    return;
  }
  await show();
}
