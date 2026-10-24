// The updates list: an opt-in email list with double opt-in, and the
// broadcasts sent to it.
import { e, api, post, state, query, pageHead, empty, plural, ago, day, words, field, formData, openDialog } from "./lib.js";

const STATUSES = ["confirmed", "pending", "unsubscribed", "expired", "bounced"];
const STATE_TAG = { draft: "info", approved: "warn", sending: "warn", sent: "ok", cancelled: "" };
const sec = (id, title, body, note = "") => `<section class="section" aria-labelledby="${id}"><div class="section-head"><h2 id="${id}">${title}</h2>${note}</div>${body}</section>`;

function updatesProduct() {
  const ps = (state.boot?.products || []).filter(p => p.updates);
  return ps.find(p => p.id === state.product) || ps[0] || null;
}

function subscribers(d) {
  const c = d.list.counts;
  const total = STATUSES.reduce((n, s) => n + c[s], 0);
  const stats = `<dl class="stats">${STATUSES.map(s => `<div class="stat"><dt>${words(s)[0].toUpperCase() + words(s).slice(1)}</dt><dd>${c[s]}<span class="sub">of ${total} who asked to join</span></dd></div>`).join("")}</dl>`;
  const filter = `<form class="toolbar section" data-form="subs-filter">${field("status", "Show", { kind: "select", options: `<option value="">Latest requests</option>` + STATUSES.map(s => `<option value="${s}"${s === d.status ? " selected" : ""}>${words(s)}</option>`).join("") })}<button class="btn">Show</button></form>`;
  const rows = (d.list.subscribers || []).map(s => `<tr data-key="sub:${e(s.id)}"><th scope="row">${e(s.email)}${s.name ? `<span class="sub">${e(s.name)}</span>` : ""}</th>
      <td data-label="Status"><span class="tag ${s.status === "confirmed" ? "ok" : s.status === "pending" ? "warn" : ""}">${e(s.status)}</span>
        ${s.status === "pending" && s.expires_at ? `<span class="sub">expires ${ago(s.expires_at)}</span>` : ""}${s.confirmed_at ? `<span class="sub">confirmed ${ago(s.confirmed_at)} by ${e(words(s.confirmed_via))}</span>` : ""}</td>
      <td data-label="Asked">${ago(s.requested_at)}<span class="sub">${e(words(s.source))}</span></td>
      <td>${s.status === "confirmed" || s.status === "pending" ? `<button type="button" class="btn btn-sm btn-ghost" data-action="unsubscribe" data-email="${e(s.email)}" aria-label="Take ${e(s.email)} off the list">Take off</button>` : ""}</td></tr>`).join("");
  return stats + filter + (rows ? `<div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">Email</th><th scope="col">Status</th><th scope="col">Asked to join</th><th scope="col"><span class="sr-only">Take off</span></th></tr></thead><tbody>${rows}</tbody></table></div>`
    : `<p class="muted">Nobody${d.status ? ` is ${e(d.status)}` : " has asked to join yet"}.</p>`);
}

function broadcasts(d) {
  const list = d.broadcasts;
  if (!list.length) return `<p class="muted">No broadcasts yet.</p>`;
  return `<div class="stack">${list.map(b => {
    const total = b.queued + b.sending + b.sent + b.unknown + b.failed + b.skipped;
    const counts = b.state === "draft" ? "not approved; nothing queued" : `${b.sent} of ${total} sent · ${b.queued} queued · ${b.unknown} unknown · ${b.failed} failed · ${b.skipped} skipped`;
    const buttons = b.state === "draft"
      ? `<button type="button" class="btn btn-primary" data-action="approve" data-id="${e(b.id)}"${b.flags?.length ? " disabled" : ""}>Approve and send</button>
         <button type="button" class="btn" data-action="edit" data-id="${e(b.id)}">Edit</button>
         <button type="button" class="btn btn-danger" data-action="cancel" data-id="${e(b.id)}">Cancel</button>`
      : b.state === "approved" || b.state === "sending" ? `<button type="button" class="btn btn-danger" data-action="cancel" data-id="${e(b.id)}">Stop: send no more</button>` : "";
    return `<article class="card" data-key="bc:${e(b.id)}" aria-labelledby="bc-${e(b.id)}"><div class="row"><span class="tag ${STATE_TAG[b.state] || ""}">${e(b.state)}</span><h3 id="bc-${e(b.id)}" class="grow-title">${e(b.subject || "(no subject)")}</h3><span class="muted">created ${ago(b.created_at)}</span></div>
      <p class="item-meta">${counts}${b.period_from ? ` · built from content posted ${e(day(b.period_from))} to ${e(day(b.period_to))}` : ""}</p>
      ${b.note ? `<p class="reason warn">${e(b.note)}</p>` : ""}
      ${b.flags?.length ? `<ul class="flags">${b.flags.map(f => `<li>Check: ${e(f)}</li>`).join("")}</ul>` : ""}
      <details><summary>The text</summary><pre class="message">${e(b.body)}</pre></details>
      ${buttons ? `<div class="item-actions">${buttons}</div>` : ""}</article>`;
  }).join("")}</div>`;
}

function render(d) {
  const head = pageHead("Updates list", "An opt-in email list for news. Someone joins only after confirming by replying to the confirmation email (or opening its link, when a public URL is set); unconfirmed requests expire. Only confirmed people get broadcasts.");
  if (!d.product) return head + empty("No product has an updates list", "Add <code>updates:</code> (a name, <code>per_day</code> and a <code>postal_address</code>) to a product's <code>product.yaml</code>, then reload products in <a href=\"#settings\">Settings</a>.");
  return head + sec("u-subs", `Subscribers to ${e(d.product.name)}`, subscribers(d)) + sec("u-bc", "Broadcasts", broadcasts(d),
    `<p class="muted">One email to everyone confirmed at approval, sent in batches within the daily limit</p>`);
}

export default {
  id: "updates", title: "Updates list", icon: "updates", nav: "work",
  load: async params => {
    const p = updatesProduct();
    if (!p) return { product: null };
    const [list, bc] = await Promise.all([api("/api/updates/subscribers" + query({ product: p.id, status: params.status, limit: 200 })), api("/api/broadcasts" + query({ product: p.id }))]);
    return { product: p, list, broadcasts: bc.broadcasts || [], status: params.status || "" };
  },
  render,
  pageActions: d => d?.product ? `<button type="button" class="btn btn-sm" data-action="add">Ask someone to join</button>
    <button type="button" class="btn btn-sm" data-action="new-broadcast">New broadcast</button>
    <button type="button" class="btn btn-sm" data-action="export">Export confirmed (CSV)</button>` : "",
  actions: {
    add: () => openDialog({ title: "Ask someone to join",
      description: "For someone who asked in person. They get a confirmation email and are <strong>not</strong> on the list until they reply to it; an unconfirmed request expires. Refused for people on do-not-contact.",
      body: `<form class="form" data-form="add">${field("email", "Email", { kind: "email", required: true })}${field("name", "Name (optional)", {})}
        <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Send the confirmation email</button></div></form>` }),
    unsubscribe: async (b, ctx) => { await post("/api/updates/unsubscribe", { product: ctx.data.product.id, email: b.dataset.email }); await ctx.done(`${b.dataset.email} is off the list; nothing queued for them goes out.`); },
    "new-broadcast": () => openDialog({ title: "New broadcast", wide: true,
      description: "Build it from the content you posted in a period, or write it yourself. Nothing is sent until you approve it.",
      body: `<form class="form" data-form="create">${field("period", "Build from content posted in", { kind: "select", options: `<option value="">(write it myself)</option><option value="this-month">this month</option><option value="last-month">last month</option><option value="30d">the last 30 days</option>` })}
        ${field("subject", "Subject", {})}${field("body", "Text", { kind: "textarea", attrs: 'rows="8"', help: "Leave empty to build it from the period's content." })}
        <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Create draft</button></div></form>` }),
    edit: (b, ctx) => {
      const bc = ctx.data.broadcasts.find(x => x.id === Number(b.dataset.id));
      openDialog({ title: "Edit the broadcast", wide: true, description: "Editing an approved broadcast withdraws the approval.",
        body: `<form class="form" data-form="edit" data-id="${e(bc.id)}">${field("subject", "Subject", { value: bc.subject })}${field("body", "Text", { kind: "textarea", value: bc.body, attrs: 'rows="10"' })}
          <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Save</button></div></form>` });
    },
    approve: async (b, ctx) => { const r = await post("/api/broadcasts/approve", { id: Number(b.dataset.id) }); await ctx.done(`Approved; queued for ${plural(r.queued_for ?? 0, "confirmed subscriber")}.`); },
    cancel: async (b, ctx) => { await post("/api/broadcasts/cancel", { id: Number(b.dataset.id) }); await ctx.done("Stopped: nothing more of it is sent."); },
    export: async (b, ctx) => {
      const r = await api("/api/updates/export" + query({ product: ctx.data.product.id }));
      const url = URL.createObjectURL(new Blob([r.csv], { type: "text/csv" }));
      const a = Object.assign(document.createElement("a"), { href: url, download: `updates-${ctx.data.product.id}.csv` });
      document.body.append(a); a.click(); a.remove(); setTimeout(() => URL.revokeObjectURL(url), 1000);
    },
  },
  forms: {
    "subs-filter": f => { location.hash = "#updates" + query({ status: formData(f).status }); },
    add: async (f, ctx) => { const v = formData(f); await post("/api/updates/add", { product: ctx.data.product.id, email: v.email, name: v.name }); await ctx.done(`Confirmation email sent to ${v.email}. They join when they reply to it.`); },
    create: async (f, ctx) => { const v = formData(f); await post("/api/broadcasts/create", { product: ctx.data.product.id, period: v.period, subject: v.subject, body: v.body }); await ctx.done("Draft broadcast created. Read it before approving."); },
    edit: async (f, ctx) => { const v = formData(f); await post("/api/broadcasts/edit", { id: Number(f.dataset.id), subject: v.subject, body: v.body }); await ctx.done("Saved."); },
  },
};
