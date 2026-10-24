// Account: one company in one product's pipeline, with its people,
// qualification, conversations and activity.
import { e, api, post, query, pageHead, plural, ago, day, words, ext, field, formData, openDialog } from "./lib.js";
import { chips, moveDialog, pauseDialog, gateHint } from "./pipeline.js";
import { conversationDialog, submitConversation, notesList } from "./conversation.js";

const ANS = { yes: "ok", no: "bad", unknown: "" };
const sec = (id, title, body, note = "") => `<section class="section" aria-labelledby="${id}"><div class="section-head"><h2 id="${id}">${title}</h2>${note}</div>${body}</section>`;

function qualification(d) {
  const rows = d.checks.map((c, i) => `<tr><th scope="row">${e(c.check)}<span class="sub">${c.gating ? "gating" : "recorded only, never blocks"}</span></th>
      <td data-label="Answer"><label class="sr-only" for="qa-${i}">Answer for ${e(c.check)}</label>
        <select id="qa-${i}" name="answer_${i}" data-check="${e(c.check)}" class="tone ${ANS[c.answer] || ""}">${["unknown", "yes", "no"].map(a => `<option value="${a}"${a === c.answer ? " selected" : ""}>${a === "unknown" ? "not known" : a}</option>`).join("")}</select></td>
      <td data-label="Evidence"><label class="sr-only" for="qe-${i}">Evidence for ${e(c.check)}</label><input type="text" id="qe-${i}" name="evidence_${i}" value="${e(c.evidence || "")}">
        ${c.answered_at ? `<span class="sub">answered ${ago(c.answered_at)}</span>` : ""}</td></tr>`).join("");
  const status = d.qualified ? `<p class="notice">Qualified: every gating check is yes, so ${e(words(d.gate || "the gate stage"))} and later stages are open.</p>`
    : `<p class="notice warn">Not qualified yet: ${e((d.failing_checks || []).join(", "))}.</p>`;
  return `${status}<form data-form="qualify"><div class="table-wrap section"><table class="stack-sm qual-table"><thead><tr><th scope="col">Check</th><th scope="col">Answer</th><th scope="col">Evidence (one line; needed for yes and no)</th></tr></thead><tbody>${rows}</tbody></table></div>
    <div class="form-actions section"><button class="btn btn-primary">Save answers</button></div></form>`;
}

function fields(d) {
  return `<form class="form" data-form="account-fields">
    ${field("goal", "Goal: what they want to achieve", { kind: "textarea", value: d.goal, attrs: 'rows="2"' })}
    ${field("timing", "Timing: what makes it matter now", { kind: "textarea", value: d.timing, attrs: 'rows="2"' })}
    ${field("baseline", "Baseline: how they do it today (a result is judged against it)", { kind: "textarea", value: d.baseline, attrs: 'rows="2"' })}
    <div class="form-actions"><button class="btn">Save</button></div></form>`;
}

function people(d) {
  const rows = d.people.map(m => `<tr data-key="member:${e(m.person_id)}"><th scope="row"><a href="#person/${e(m.person_id)}">${e(m.name || m.email)}</a>${m.title ? `<span class="sub">${e(m.title)}</span>` : ""}</th>
      <td data-label="Role">${e(words(m.role))}</td><td data-label="Email">${e(m.email || "")}</td>
      <td><button type="button" class="btn btn-sm btn-ghost" data-action="remove-member" data-person="${e(m.person_id)}" aria-label="Take ${e(m.name || m.email)} off the account">Remove</button></td></tr>`).join("");
  return `${rows ? `<div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">Person</th><th scope="col">Role</th><th scope="col">Email</th><th scope="col"><span class="sr-only">Remove</span></th></tr></thead><tbody>${rows}</tbody></table></div>` : `<p class="muted">No people on this account yet.</p>`}
    <form class="toolbar section" data-form="add-member">${field("person", "Add a person", { help: "Their email or person id; a new email is added as a new person." })}
      ${field("role", "Role", { kind: "select", options: ["champion", "buyer", "user", "contact"].map(r => `<option>${r}</option>`).join("") })}<button class="btn">Add</button></form>`;
}

function links(d) {
  const list = d.links.map(l => `<li>${ext(l.url, l.note || l.url)}<span class="sub">added ${ago(l.added_at)}</span></li>`).join("");
  return `${list ? `<ul class="plain-list">${list}</ul>` : `<p class="muted">No evidence links yet.</p>`}
    <form class="toolbar section" data-form="add-link"><div class="grow">${field("url", "Add a link", { kind: "url", required: true })}</div>${field("note", "What it shows", {})}<button class="btn">Add</button></form>`;
}

function history(d) {
  return `<ol class="plain-list">${d.history.map(h => `<li><strong>${e(words(h.stage))}</strong> <span class="muted">${e(day(h.entered_at))}</span>
    ${h.evidence ? `<span class="sub">${e(h.evidence)}</span>` : ""}${h.reason ? `<span class="sub">reason: ${e(words(h.reason))}${h.revisit_on ? `, revisit ${e(day(h.revisit_on))}` : ""}</span>` : ""}</li>`).join("")}</ol>`;
}

function activity(d) {
  const sigs = d.signals.signals.map(s => `<li><span class="tag">${e(s.type)}</span> ${s.url ? ext(s.url, s.title || s.type) : e(s.title || s.type)}
      <span class="muted">· ${s.person_id ? `<a href="#person/${e(s.person_id)}">${e(s.person)}</a> · ` : ""}${ago(s.occurred_at)}</span></li>`).join("");
  const msgs = d.messages.map(m => `<details class="card"><summary><span class="tag">${e(m.status)}</span> ${e(m.subject || words(m.channel))}
      <span class="muted">· to ${e(m.person)} · ${m.sent_at ? `sent ${ago(m.sent_at)}` : `created ${ago(m.created_at)}`}</span></summary><pre class="message">${e(m.body || "(no text)")}</pre></details>`).join("");
  return sec("a-signals", "Signals", sigs ? `<ul class="plain-list">${sigs}</ul><p class="help">${d.signals.total > d.signals.signals.length ? `Showing ${d.signals.signals.length} of ${d.signals.total}` : plural(d.signals.total, "signal")} about the company and its people in the last ${d.signals.days} days.</p>` : `<p class="muted">No signals about the company or its people in the last ${d.signals.days} days.</p>`)
    + sec("a-messages", "Messages", msgs ? `<div class="stack">${msgs}</div>` : `<p class="muted">No messages to anyone here yet.</p>`);
}

function render(d) {
  const a = d.detail;
  const status = a.status === "open" ? `${e(words(a.stage))} for ${plural(a.days_in_stage, "day")}${a.stuck ? ` <span class="tag warn">stuck</span>` : ""}`
    : `<span class="tag ${a.status === "lost" ? "bad" : "warn"}">${e(a.status)}</span> from ${e(words(a.stage))}: ${e(words(a.reason))}${a.revisit_on ? `, revisit ${e(day(a.revisit_on))}` : ""}`;
  const next = a.next_action ? `<p class="notice${a.overdue ? " bad" : ""}"><strong>Next action:</strong> ${e(a.next_action)} · due ${e(day(a.next_due))}${a.overdue ? " (overdue)" : ""}
      <button type="button" class="btn btn-sm" data-action="next-action">Change</button></p>` : (a.status === "open" ? `<p class="notice warn">No next action. <button type="button" class="btn btn-sm" data-action="next-action">Set one</button></p>` : "");
  const tasks = a.open_tasks.length ? sec("a-tasks", "Open tasks", `<ul class="plain-list">${a.open_tasks.map(t => `<li>${e(t.next_action)} <span class="muted">· due ${ago(t.due_at)}</span></li>`).join("")}</ul><p class="help">Close them on <a href="#today">Today</a>.</p>`) : "";
  return `${pageHead(a.company, "An account is one company in a product's pipeline: its stage, its people, the qualification checks with evidence, and what was said.")}
    <p class="item-meta">${a.domain ? `${e(a.domain)} · ` : ""}${status} · ${e(d.product)}</p>${next}
    ${chips(d.board, Object.fromEntries(a.checks.map(c => [c.check, c.answer])))}
    <div class="person-grid"><div>
      ${sec("a-qual", "Qualification", qualification({ ...a, gate: d.board.gate }))}
      ${sec("a-notes", "Conversations", notesList(d.detail.conversations, { showPerson: true }))}
      ${activity(a)}
    </div><aside aria-label="Account details">
      ${sec("a-fields", "Goal, timing, baseline", fields(a))}
      ${sec("a-people", "People", people(a))}
      ${sec("a-links", "Evidence links", links(a))}
      ${sec("a-history", "Stage history", history(a))}
      ${tasks}
    </aside></div>`;
}

const ref = ctx => ({ product: ctx.data.product, account: String(ctx.data.detail.id) });

export default {
  id: "account", title: "Account", section: "pipeline",
  load: async params => {
    const product = params.product;
    const [detail, board] = await Promise.all([api("/api/account" + query({ product, account: params.id })), api("/api/pipeline" + query({ product }))]);
    return { product, detail, board };
  },
  render,
  crumbs: d => [{ label: "Pipeline", href: "#pipeline" }, { label: d?.detail.company || "Account" }],
  pageActions: d => d ? `<button type="button" class="btn btn-sm btn-primary" data-action="log">Log a conversation</button>
    ${d.detail.status === "open" ? `<button type="button" class="btn btn-sm" data-action="move">Move…</button><button type="button" class="btn btn-sm" data-action="pause">Pause or lose…</button>`
      : `<button type="button" class="btn btn-sm" data-action="reopen">Reopen…</button>`}` : "",
  actions: {
    log: (b, ctx) => conversationDialog({ product: ctx.data.product, people: ctx.data.detail.people,
      account: { id: ctx.data.detail.id, company: ctx.data.detail.company, checks: ctx.data.board.checks, record: ctx.data.board.record_checks } }),
    move: (b, ctx) => moveDialog(ctx.data.detail, ctx.data.board),
    reopen: (b, ctx) => moveDialog(ctx.data.detail, ctx.data.board, true),
    pause: (b, ctx) => pauseDialog(ctx.data.detail, ctx.data.board),
    "gate-hint": (el, ctx) => gateHint(el, { ...ctx.data.detail, answers: Object.fromEntries(ctx.data.detail.checks.map(c => [c.check, c.answer])) }, ctx.data.board),
    "next-action": (b, ctx) => openDialog({
      title: "Next action", description: "An open account always has exactly one next action; this replaces the current one.",
      body: `<form class="form" data-form="next-action"><div class="form-row">${field("next_action", "Next action", { required: true, value: ctx.data.detail.next_action })}
        ${field("due", "Due", { kind: "date", value: ctx.data.detail.next_due || new Date().toISOString().slice(0, 10) })}</div>
        <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Save</button></div></form>`,
    }),
    "remove-member": async (b, ctx) => {
      await post("/api/accounts/update", { ...ref(ctx), people: [{ person: b.dataset.person, remove: true }] });
      await ctx.done("Taken off the account.");
    },
  },
  forms: {
    "log-conversation": async (f, ctx) => { await submitConversation(f); await ctx.done("Conversation saved."); },
    qualify: async (f, ctx) => {
      const v = formData(f);
      const answers = [...f.querySelectorAll("select[data-check]")].map((sel, i) => ({ check: sel.dataset.check, answer: sel.value, evidence: v[`evidence_${i}`] }));
      await post("/api/accounts/qualify", { ...ref(ctx), answers });
      await ctx.done("Answers saved.");
    },
    "account-fields": async (f, ctx) => {
      const v = formData(f);
      await post("/api/accounts/update", { ...ref(ctx), goal: v.goal, timing: v.timing, baseline: v.baseline });
      await ctx.done("Saved.");
    },
    "add-member": async (f, ctx) => {
      const v = formData(f);
      await post("/api/accounts/update", { ...ref(ctx), people: [{ person: v.person.trim(), role: v.role }] });
      await ctx.done("Added to the account.");
    },
    "add-link": async (f, ctx) => {
      const v = formData(f);
      await post("/api/accounts/update", { ...ref(ctx), links: [{ url: v.url, note: v.note }] });
      await ctx.done("Link added.");
    },
    "next-action": async (f, ctx) => {
      const v = formData(f);
      await post("/api/accounts/next-action", { ...ref(ctx), next_action: v.next_action, due: v.due });
      await ctx.done("Next action set.");
    },
    move: async (f, ctx) => {
      const v = formData(f);
      const body = { ...ref(ctx), stage: v.stage, evidence: v.evidence };
      if (v.next_action) Object.assign(body, { next_action: v.next_action, due: v.due });
      await post("/api/accounts/move", body);
      await ctx.done(`Now in ${words(v.stage)}.`);
    },
    close: async (f, ctx) => {
      const v = formData(f);
      await post("/api/accounts/close", { ...ref(ctx), status: v.status, reason: v.reason, revisit: v.revisit, note: v.note });
      await ctx.done(`Marked ${v.status}; look again on ${day(v.revisit)}.`);
    },
  },
};
