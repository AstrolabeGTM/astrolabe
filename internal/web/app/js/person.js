// Person: everything known about one person, and what may be done.
import { e, api, post, state, pageHead, empty, ago, day, dateTime, ext, productName, field, formData, openDialog, words, plural } from "./lib.js";
import { startSequenceDialog, actions as seqActions, forms as seqForms } from "./sequences.js";
import { taskDialog } from "./today.js";
import { conversationDialog, submitConversation, notesList } from "./conversation.js";

const SOURCE = {
  github: "found through GitHub", event: "from your product's events", stripe: "from a Stripe payment", reply: "they wrote from it",
  import: "your import", manual: "entered by hand", form: "a form they filled in", enrich: "enrichment", import_company: "company import",
};
const STATUS_TAG = { sent: "ok", draft: "info", approved: "info", sending: "info", skipped: "", blocked: "bad", failed: "bad", unknown: "warn" };

function nameOf(p) { return p.name || p.email || (p.github ? `github.com/${p.github}` : `Person ${p.id}`); }

function section(id, title, body, note = "") {
  return `<section class="section" aria-labelledby="${id}"><div class="section-head"><h2 id="${id}">${e(title)}</h2>${note}</div>${body}</section>`;
}

function emailPolicy(v) {
  const rows = v.email_policy.map(p => {
    const state = !p.has_address ? `<span class="tag warn">No address</span>`
      : p.may_email ? `<span class="tag ok">May email</span>` : `<span class="tag bad">Not emailed</span>`;
    const why = !p.has_address ? "No email address is known, so email steps are held as blocked. Steps done by hand still work."
      : p.may_email ? "The outreach guard allows email (sequences still check do-not-contact, pauses and limits before each send)."
      : e(p.reason);
    return `<tr><td data-label="Product">${e(productName(p.product))}</td><td data-label="Email">${state}</td><td data-label="Why">${why}</td></tr>`;
  }).join("");
  return rows ? `<div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">Product</th><th scope="col">Email</th><th scope="col">Why</th></tr></thead><tbody>${rows}</tbody></table></div>` : `<p class="muted">No products are loaded.</p>`;
}

function scores(v) {
  if (!v.scores.length) return empty("Not scored yet", "A score is computed when a signal, an import or new facts change something, and again every day. Adding facts (right) rescores them now.");
  return v.scores.map(s => {
    const part = (label, n, lines) => `<div><dt>${label} <span class="num">${n}/100</span></dt><dd>${lines?.length ? `<ul>${lines.map(l => `<li>${e(l)}</li>`).join("")}</ul>` : `<span class="muted">nothing yet</span>`}</dd></div>`;
    return `<div class="card"><div class="row"><span class="tag brass">Priority ${e(s.tier)}</span><strong class="num">${s.priority}/100</strong>
      <span class="muted">${e(productName(s.product))}</span>${s.eligible ? "" : `<span class="tag bad">Excluded: ${e(s.breakdown.excluded || "a fit rule excludes them")}</span>`}</div>
      <dl class="score-parts">${part("Fit", s.fit, s.breakdown.fit)}${part("Intent", s.intent, s.breakdown.intent)}${part("Why now", s.why_now, s.breakdown.why_now)}</dl></div>`;
  }).join("");
}

function enrollments(v) {
  if (!v.enrollments.length) return `<p class="muted">Not in any sequence.</p>`;
  return `<div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">Sequence</th><th scope="col">Status</th><th scope="col">Step</th><th scope="col">Started</th></tr></thead><tbody>
    ${v.enrollments.map(x => `<tr><td data-label="Sequence">${e(x.sequence)}<span class="sub">${e(productName(x.product))}${x.automation ? ` · automation ${e(x.automation)}` : ""}</span></td>
      <td data-label="Status"><span class="tag ${x.status === "active" ? "ok" : ""}">${e(x.status)}</span>${x.stop_reason ? `<span class="sub">${e(words(x.stop_reason))}</span>` : ""}</td>
      <td data-label="Step" class="num">${x.status === "active" ? `${x.next_step} of ${x.steps}${x.next_due ? `<span class="sub">due ${ago(x.next_due)}</span>` : ""}` : `${Math.min(x.next_step - 1, x.steps)} of ${x.steps} done`}</td>
      <td data-label="Started">${e(day(x.started_at))}</td></tr>`).join("")}</tbody></table></div>`;
}

function messages(v) {
  if (!v.messages.length) return `<p class="muted">No messages yet.</p>`;
  return `<div class="stack">${v.messages.map(m => `<details class="card"><summary><span class="tag ${STATUS_TAG[m.status] || ""}">${e(m.status)}</span>
      ${e(m.subject || words(m.channel))} <span class="muted">· ${e(words(m.channel))} · ${m.sent_at ? `sent ${ago(m.sent_at)}` : `created ${ago(m.created_at)}`}</span></summary>
      <dl class="envelope"><dt>From</dt><dd>${e(m.sender || "—")}</dd><dt>To</dt><dd>${e(m.recipient || "—")}</dd></dl>
      ${m.error ? `<p class="reason warn">${e(m.error)}</p>` : ""}<pre class="message">${e(m.body || "(no text)")}</pre></details>`).join("")}</div>`;
}

function replies(v) {
  if (!v.replies.length) return `<p class="muted">No replies.</p>`;
  return `<div class="stack">${v.replies.map(r => `<details class="card"><summary>${r.classification ? `<span class="tag">${e(words(r.classification))}</span>` : `<span class="tag warn">to label</span>`}
      ${e(r.subject || "(no subject)")} <span class="muted">· ${ago(r.received_at)}</span></summary><pre class="message">${e(r.body)}</pre></details>`).join("")}</div>`;
}

function signals(v) {
  const f = v.signals;
  if (!f.signals.length) return `<p class="muted">No signals in the last ${f.days} days.</p>`;
  return `<ul class="plain-list">${f.signals.map(s => `<li><span class="tag">${e(s.type)}</span> ${s.url ? ext(s.url, s.title || s.type) : e(s.title || s.type)}
      <span class="muted">· ${e(productName(s.product))} · strength ${s.strength}${s.why_now ? " · why now" : ""} · ${ago(s.occurred_at)}</span></li>`).join("")}</ul>
    <p class="help">${f.total > f.signals.length ? `Showing ${f.signals.length} of ${f.total}` : plural(f.total, "signal")} in the last ${f.days} days.</p>`;
}

function aside(v) {
  const ids = v.identities.map(i => `<li><span class="tag">${e(i.kind)}</span> <span class="ident">${e(i.value)}</span><span class="sub">${e(SOURCE[i.source] || i.source)}</span></li>`).join("");
  const facts = Object.entries(v.facts).sort().map(([k, src]) => `<span class="chip">${e(k)} <small>${e(SOURCE[src] || src)}</small></span>`).join("");
  const briefs = v.briefs.map(b => `<div class="card"><p class="item-meta">${e(productName(b.product))} · written ${ago(b.written)}</p><pre class="message">${e(b.brief)}</pre></div>`).join("");
  const research = state.boot?.ai_drafting && state.boot.products.length
    ? `<form class="row" data-form="research"><label class="sr-only" for="research-product">Product</label><select id="research-product" name="product">${state.boot.products.map(p => `<option value="${e(p.id)}">${e(p.name)}</option>`).join("")}</select><button class="btn btn-sm">Research with Claude</button></form>`
    : `<p class="help">Research notes are written by the Claude API (set ASTROLABE_ANTHROPIC_API_KEY), or by Claude over MCP.</p>`;
  const tasks = v.open_tasks.map(t => `<li>${e(t.next_action)} <span class="muted">· due ${ago(t.due_at)}</span></li>`).join("");
  return section("p-ids", "Where we know them from", ids ? `<ul class="plain-list">${ids}</ul>` : `<p class="muted">No identities.</p>`) +
    section("p-facts", "Facts", `${facts ? `<div class="chips">${facts}</div>` : `<p class="muted">No facts yet.</p>`}
      <form class="form" data-form="facts">${field("facts", "Add facts", { help: "kind:value, separated by commas, e.g. dep:bullmq, team:5-50. Fit rules in product.yaml match on these." })}
      <div class="form-actions"><button class="btn btn-sm">Add facts</button></div></form>`) +
    section("p-research", "Research notes", (briefs || `<p class="muted">None yet.</p>`) + research) +
    section("p-tasks", "Open tasks", tasks ? `<ul class="plain-list">${tasks}</ul><p class="help">Close them on <a href="#today">Today</a>.</p>` : `<p class="muted">None.</p>`);
}

function render(v) {
  const p = v.person;
  const bits = [];
  if (p.title) bits.push(e(p.title));
  const org = [p.company, p.company_domain].filter(Boolean).join(" · ");
  if (org) bits.push(e(org));
  if (p.email) bits.push(e(p.email));
  if (p.github) bits.push(ext(`https://github.com/${p.github}`, `github.com/${p.github}`));
  const sub = bits.join(" · ");
  const dnc = v.do_not_contact
    ? `<p class="notice bad"><strong>Do not contact</strong> since ${e(day(v.do_not_contact.since))}: ${e(words(v.do_not_contact.reason))}${v.do_not_contact.detail ? ` (${e(v.do_not_contact.detail)})` : ""}. No product will message them.</p>` : "";
  return `${pageHead(nameOf(p), "One person across your products. Score is 0.4 × fit + 0.4 × intent + 0.2 × why now, out of 100; priority A is 70 or more. Scores rank people; they never allow contact on their own.")}
    <p class="item-meta">${sub}${v.products.length ? ` · in ${v.products.map(x => e(productName(x))).join(", ")}` : ""}</p>
    ${(v.reported_source || []).map(r => `<p class="item-meta">Said they came from: <q>${e(r.self_reported)}</q> (${e(productName(r.product))}; first touch: ${e(r.first_touch || "unknown")})</p>`).join("")}
    ${p.notes ? `<p>${e(p.notes)}</p>` : ""}${dnc}
    <div class="person-grid"><div>
      ${section("p-email", "Email", emailPolicy(v), `<p class="muted">The outreach guard, per product</p>`)}
      ${section("p-score", "Score", scores(v))}
      ${section("p-seq", "Sequences", enrollments(v))}
      ${section("p-msg", "Messages", messages(v), `<p class="muted">Newest first</p>`)}
      ${section("p-rep", "Replies", replies(v))}
      ${section("p-notes", "Conversations", notesList(v.conversations, { showPerson: false, showCompany: true }))}
      ${section("p-sig", "Signals", signals(v))}
    </div><aside aria-label="Identities, facts and notes">${aside(v)}</aside></div>`;
}

export default {
  id: "person", title: "Person", section: "people",
  load: params => api(`/api/people/${encodeURIComponent(params.id || "")}`),
  render,
  crumbs: v => [{ label: "People", href: "#people" }, { label: v ? nameOf(v.person) : "Person" }],
  pageActions: v => v ? `<button type="button" class="btn btn-sm btn-primary" data-action="person-log">Log a conversation</button>
    <button type="button" class="btn btn-sm" data-action="person-start">Start a sequence</button>
    <button type="button" class="btn btn-sm" data-action="person-task">Add a task</button>
    ${v.do_not_contact ? `<button type="button" class="btn btn-sm" data-action="person-allow">Allow contact again</button>`
      : `<button type="button" class="btn btn-sm btn-danger" data-action="person-dnc">Do not contact</button>`}` : "",
  actions: {
    ...seqActions,
    "person-start": (b, ctx) => startSequenceDialog([ctx.data.person.id], { productId: ctx.data.products[0] || state.boot.products[0]?.id,
      who: nameOf(ctx.data.person), names: { [ctx.data.person.id]: nameOf(ctx.data.person) } }),
    "person-task": (b, ctx) => taskDialog(ctx.data.person.id),
    "person-log": (b, ctx) => conversationDialog({ product: ctx.data.products.length === 1 ? ctx.data.products[0] : "", person: String(ctx.data.person.id), personName: nameOf(ctx.data.person) }),
    "person-dnc": (b, ctx) => openDialog({
      title: `Do not contact ${nameOf(ctx.data.person)}?`,
      description: "No product will message them again: sequences stop and unsent messages are held. You can undo this.",
      body: `<form class="form" data-form="person-dnc">${field("reason", "Note (optional)", { help: "Why, for your own record." })}
        <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Do not contact</button></div></form>`,
    }),
    "person-allow": async (b, ctx) => ctx.done((await post(`/api/people/${ctx.data.person.id}/do-not-contact`, { allow: true })).status),
  },
  forms: {
    ...seqForms,
    "log-conversation": async (f, ctx) => { await submitConversation(f); await ctx.done("Conversation saved."); },
    facts: async (f, ctx) => ctx.done((await post(`/api/people/${ctx.data.person.id}/facts`, { facts: formData(f).facts })).status),
    research: async (f, ctx) => ctx.done((await post(`/api/people/${ctx.data.person.id}/research`, { product: formData(f).product })).status),
    "person-dnc": async (f, ctx) => ctx.done((await post(`/api/people/${ctx.data.person.id}/do-not-contact`, { reason: formData(f).reason })).status),
  },
};
