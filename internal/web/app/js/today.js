// Today: everything waiting on the operator, most urgent first.
import { e, api, post, query, state, pageHead, empty, plural, span, ago, day, dateTime, ext, productName, productOptions,
  currentProduct, field, formData, openDialog, words } from "./lib.js";

const KIND = { reply: "Reply", approval: "Draft", held: "Held send", task: "Task", queued: "Queued", account: "Account", revisit: "Revisit" };
const LABEL = { interested: "Interested", question: "Question", not_now: "Not now…", no: "No", out_of_office: "Out of office", unsubscribe: "Unsubscribe" };
const OUTCOME = {
  activated: "Activated (records the activation stage)", paid: "Paid (records the paid stage)", wrong_fit: "Wrong fit",
  unclear_value: "Value wasn't clear", setup_blocked: "Setup blocked", price: "Price", later: "Later", other: "Other",
};
const CHANNEL = { email: "email", linkedin_task: "LinkedIn, by hand", x_task: "X, by hand", whatsapp: "WhatsApp", push: "push", in_app: "in-app", webhook: "webhook" };

// targetWords says a target like "2-hour" or "1-day".
function targetWords(sec) {
  return sec % 86400 === 0 ? `${sec / 86400}-day` : `${Math.round(sec / 3600)}-hour`;
}

function timing(it) {
  const age = it.age_seconds;
  let text;
  if (it.kind === "task" || it.kind === "account") {
    text = age >= 0 ? `due ${span(age)} ago` : `due in ${span(-age)}`;
  } else if (it.kind === "revisit") {
    text = age >= 86400 ? `revisit date ${span(age)} ago` : "revisit date is today";
  } else if (it.kind === "queued") {
    text = `approved ${span(age)} ago`;
  } else {
    const target = it.target_seconds;
    const t = targetWords(target);
    text = it.state === "past" ? `waiting ${span(age)}: ${span(age - target)} past the ${t} target`
      : it.state === "near" ? `waiting ${span(age)}: ${span(target - age)} left of the ${t} target`
      : `waiting ${span(age)}, within the ${t} target`;
  }
  return `<span class="timing ${e(it.state)}" title="${e(dateTime(it.due))}">${e(text)}</span>`;
}

function person(id, name) {
  return id ? `<a href="#person/${e(id)}">${e(name || `Person ${id}`)}</a>` : e(name || "Someone");
}

function head(it, title, kindTag = `<span class="tag">${KIND[it.kind]}</span>`) {
  const id = `h-${it.key.replace(/\W/g, "-")}`;
  return { id, html: `<div class="item-head">${kindTag}<h3 id="${id}">${title}</h3>${timing(it)}</div>` };
}

function article(it, h, body) {
  return `<article class="item ${e(it.state)}" data-key="${e(it.key)}" aria-labelledby="${h.id}">${h.html}${body}</article>`;
}

// ---- replies ----

function replyItem(it, t) {
  const r = it.reply;
  const h = head(it, `${person(r.person_id, r.person_name || r.from)} <span class="muted">wrote back</span>`);
  const meta = [r.product ? e(productName(r.product)) : "not from your outreach", e(r.from), `received ${ago(r.received_at)}`].join(" · ");
  const buttons = t.reply_labels.map(c => {
    const cls = c === r.suggested_class ? "btn btn-sm btn-primary" : "btn btn-sm";
    const action = c === "not_now" ? "label-later" : "label";
    return `<button type="button" class="${cls}" data-action="${action}" data-id="${e(r.id)}" data-class="${e(c)}">${e(LABEL[c] || words(c))}</button>`;
  }).join("");
  const suggestion = r.suggested_class ? `<p class="item-meta">Claude suggests <strong>${e(words(r.suggested_class))}</strong> (highlighted). Nothing happens until you choose.</p>` : "";
  const answer = r.action_id ? `<button type="button" class="btn btn-sm" data-action="answer" data-id="${e(r.id)}">Answer by email…</button>` : "";
  return article(it, h, `<p class="item-meta">${meta}</p>
    <div class="item-body"><p class="item-meta"><strong>${e(r.subject || "(no subject)")}</strong></p><pre class="message">${e(r.body || r.snippet)}</pre></div>
    ${suggestion}
    <div class="item-actions"><div class="label-group" role="group" aria-label="Label the reply from ${e(r.person_name || r.from)}">${buttons}</div>${answer}</div>
    <p class="help">Interested and Question open a task due now. No and Unsubscribe put them on the do-not-contact list. Not now sets a reminder.</p>`);
}

// ---- drafts ----

function envelope(a) {
  if (a.channel.endsWith("_task")) return `<dl class="envelope"><dt>Do by hand</dt><dd>${e(CHANNEL[a.channel] || a.channel)} to ${e(a.recipient)}</dd></dl>`;
  const rows = [["From", a.sender], ["To", a.recipient]];
  if (a.subject) rows.push([a.channel === "whatsapp" ? "Template" : "Subject", a.subject]);
  return `<dl class="envelope">${rows.map(([k, v]) => `<dt>${k}</dt><dd>${e(v || "(none)")}</dd>`).join("")}</dl>`;
}

function where(a) {
  const step = a.kind === "reply" ? "answer to their reply" : `${a.sequence} step ${a.step + 1}`;
  return [e(productName(a.product)), e(step), e(CHANNEL[a.channel] || a.channel), a.company ? e(a.company) : ""].filter(Boolean).join(" · ");
}

function editor(a, open) {
  const subject = a.channel === "email"
    ? field("subject", "Subject", { value: a.subject, attrs: a.subject_locked ? "readonly" : "", help: a.subject_locked ? "Follow-ups keep the thread's subject." : "" })
    : "";
  return `<form class="form item-body" data-form="edit-draft" data-id="${e(a.id)}"${open ? "" : " hidden"}>
    <p class="help">Saving withdraws any earlier approval; you then approve the saved text. <kbd>Ctrl</kbd>/<kbd>⌘</kbd>+<kbd>Enter</kbd> saves and approves.</p>
    ${subject}
    ${field("body", a.channel.endsWith("_task") ? "Note for yourself" : "Message", { kind: "textarea", value: a.body })}
    <div class="form-actions">
      ${open ? "" : `<button type="button" class="btn" data-action="cancel-edit">Cancel</button>`}
      <button type="submit" class="btn" name="intent" value="save">Save</button>
      <button type="submit" class="btn btn-primary" name="intent" value="approve" data-hotkey="submit">Save and approve</button>
    </div></form>`;
}

function draftItem(it, t) {
  const a = it.action;
  const manual = a.channel.endsWith("_task");
  const h = head(it, `${person(a.person_id, a.person_name)}`, `<span class="tag info">${manual ? "Manual step" : "Draft"}</span>`);
  const notes = [];
  if (a.ai_state === "pending") notes.push(`<p class="reason info">Claude is writing this draft. It appears here when done; you can also write it yourself.</p>`);
  if (a.ai_state === "failed") notes.push(`<p class="reason warn">The AI draft failed: ${e(a.error)}. Write it yourself or ask again.</p>`);
  else if (a.error) notes.push(`<p class="reason warn">${e(a.error)}</p>`);
  const flags = a.flags?.length ? `<ul class="flags" aria-label="Check before approving">${a.flags.map(f => `<li>Check: ${e(f)}</li>`).join("")}</ul>` : "";
  const empty = !a.body.trim();
  const ai = state.boot?.ai_drafting && !manual ? `<button type="button" class="btn btn-sm" data-action="ai-draft" data-id="${e(a.id)}">Ask Claude to rewrite</button>` : "";
  const approveLabel = manual ? "Done" : "Approve";
  const actions = `<div class="item-actions" data-view>
      <button type="button" class="btn btn-primary" data-action="approve" data-id="${e(a.id)}" data-hash="${e(a.content_hash)}"${empty ? " disabled" : ""}>${approveLabel}</button>
      <button type="button" class="btn" data-action="edit" data-id="${e(a.id)}">Edit</button>
      <button type="button" class="btn btn-danger" data-action="skip" data-id="${e(a.id)}">Skip step</button>${ai}</div>
    <p class="help" data-view>${manual ? "Done marks this step as sent; do it yourself first." : `Approve sends exactly this text from ${e(a.sender)} to ${e(a.recipient)}, after the send check (do-not-contact, pauses, limits).`}</p>`;
  return article(it, h, `<p class="item-meta">${where(a)}${a.goal ? ` · goal: ${e(a.goal)}` : ""}</p>
    ${notes.join("")}${flags}
    <div class="item-body" data-view>${envelope(a)}${empty ? `<p class="muted">No text yet. Write it${state.boot?.ai_drafting ? ", or ask Claude" : ""}.</p>` : `<pre class="message">${e(a.body)}</pre>`}</div>
    ${editor(a, empty)}${empty ? "" : actions}${empty && ai ? `<div class="item-actions">${ai}<button type="button" class="btn btn-danger" data-action="skip" data-id="${e(a.id)}">Skip step</button></div>` : ""}`);
}

// ---- held and queued sends ----

const HELD = {
  unknown: ["warn", "Delivery unknown", "The provider's answer was lost after the message was handed over, so it may have gone out. It is never resent automatically."],
  failed: ["bad", "Failed", "The provider refused it; it was not sent."],
  blocked: ["bad", "Blocked", "Astrolabe refused to send it."],
  approved: ["warn", "Held", "Approved and waiting: it goes out when the hold clears."],
};

function heldItem(it) {
  const a = it.action;
  const [cls, label, explain] = HELD[a.status] || ["warn", a.status, ""];
  const h = head(it, person(a.person_id, a.person_name), `<span class="tag ${cls}">${e(label)}</span>`);
  const reason = a.error.replace(/^held: /, "");
  let buttons = "";
  if (a.status === "unknown") {
    buttons = `<button type="button" class="btn btn-primary" data-action="check-sent" data-id="${e(a.id)}">Check Sent mail</button>
      <button type="button" class="btn" data-action="resolve" data-id="${e(a.id)}" data-sent="true">It was sent</button>
      <button type="button" class="btn" data-action="resolve" data-id="${e(a.id)}" data-sent="false">It was not sent: back to drafts</button>`;
  } else if (a.status === "failed") {
    buttons = `<button type="button" class="btn btn-primary" data-action="resolve" data-id="${e(a.id)}" data-sent="false">Back to drafts</button>
      <button type="button" class="btn btn-danger" data-action="skip" data-id="${e(a.id)}">Skip step</button>`;
  } else if (a.status === "blocked") {
    buttons = `<button type="button" class="btn btn-danger" data-action="skip" data-id="${e(a.id)}">Skip step</button>
      <a class="btn" href="#person/${e(a.person_id)}">Open ${e(a.person_name)}</a>`;
  } else {
    buttons = `<button type="button" class="btn btn-danger" data-action="skip" data-id="${e(a.id)}">Cancel this send</button>`;
    if (reason.startsWith("paused")) buttons += ` <a class="btn" href="#settings">Pauses in Settings</a>`;
  }
  return article(it, h, `<p class="item-meta">${where(a)}</p>
    ${reason ? `<p class="reason ${cls === "bad" ? "" : "warn"}">${e(reason)}</p>` : ""}
    <p class="help">${e(explain)}</p>
    <div class="item-body">${envelope(a)}<details><summary>Show the message</summary><pre class="message">${e(a.body)}</pre></details></div>
    <div class="item-actions">${buttons}</div>`);
}

function queuedItem(it) {
  const a = it.action;
  const h = head(it, person(a.person_id, a.person_name), `<span class="tag ok">${a.status === "sending" ? "Sending" : "Queued"}</span>`);
  return article(it, h, `<p class="item-meta">${where(a)} · ${e(a.subject || "")}</p>
    <p class="help">${a.status === "sending" ? "Being handed to the provider now." : "Approved; it goes out at the next send check, within about a minute."}</p>
    ${a.status === "approved" ? `<div class="item-actions"><button type="button" class="btn btn-sm btn-danger" data-action="skip" data-id="${e(a.id)}">Cancel this send</button></div>` : ""}`);
}

// ---- tasks ----

function taskItem(it) {
  const k = it.task;
  const h = head(it, e(k.next_action));
  const meta = [e(productName(k.product))];
  if (k.person_id) meta.push(person(k.person_id, k.person_name));
  if (k.play) meta.push(`from automation ${e(k.play)}`);
  if (k.evidence_url) meta.push(ext(k.evidence_url, "source"));
  if (k.answer_action_id) meta.push(`your answer is draft #${e(k.answer_action_id)}`);
  return article(it, h, `<p class="item-meta">${meta.join(" · ")} · made ${ago(k.created_at)} · due ${e(dateTime(k.due_at))}</p>
    ${k.draft ? `<details class="item-body"><summary>Suggested text</summary><pre class="message">${e(k.draft)}</pre></details>` : ""}
    <div class="item-actions"><button type="button" class="btn" data-action="close-task" data-id="${e(k.id)}">Close with an outcome…</button></div>`);
}

// ---- accounts ----

function accountLink(a) {
  return `<a href="#account/${e(a.id)}?product=${encodeURIComponent(a.product)}">${e(a.company)}</a>`;
}

function accountItem(it) {
  const a = it.account;
  const h = head(it, `${accountLink(a)} <span class="muted">next action overdue</span>`, `<span class="tag brass">Account</span>`);
  return article(it, h, `<p class="item-meta">${e(productName(a.product))} · ${e(words(a.stage))} for ${plural(a.days_in_stage, "day")}${a.stuck ? " · stuck" : ""}</p>
    <p class="reason warn">${e(a.next_action)} (due ${e(day(a.next_due))})</p>
    <div class="item-actions"><a class="btn" href="#account/${e(a.id)}?product=${encodeURIComponent(a.product)}">Open the account</a></div>
    <p class="help">Do it, then set the next one on the account page; an open account always has exactly one next action.</p>`);
}

function revisitItem(it) {
  const a = it.account;
  const h = head(it, `${accountLink(a)} <span class="muted">to look at again</span>`, `<span class="tag brass">Revisit</span>`);
  return article(it, h, `<p class="item-meta">${e(productName(a.product))} · ${e(a.status)} from ${e(words(a.stage))} · reason ${e(words(a.reason))} · revisit on ${e(day(a.revisit_on))}</p>
    <div class="item-actions"><a class="btn" href="#account/${e(a.id)}?product=${encodeURIComponent(a.product)}">Open the account</a></div>
    <p class="help">Reopen it by moving it to a stage, or pause it again with a new revisit date.</p>`);
}

const RENDER = { reply: replyItem, approval: draftItem, held: heldItem, queued: queuedItem, task: taskItem, account: accountItem, revisit: revisitItem };

// ---- the page ----

function stat(label, c, sub) {
  return `<div class="stat"><dt>${label}</dt><dd>${c.total}<span class="sub">${sub}</span></dd></div>`;
}

function render(t) {
  const c = t.counts;
  const tg = t.targets;
  const pastOf = (k, target) => `${c[k].past} of ${c[k].total} past the ${targetWords(target)} target`;
  const top = pageHead("Today", `Replies to label, drafts to approve, held sends, tasks due today (form requests included) and accounts whose next action or revisit date has come, oldest past its target first. Targets: replies ${span(tg.reply_seconds)}, drafts and held sends ${span(tg.approval_seconds)}, tasks and accounts their due date.`);
  const stats = `<dl class="stats" aria-label="Waiting on you${t.product ? ` for ${e(productName(t.product))}` : ""}">
    ${stat("Replies to label", c.reply, pastOf("reply", tg.reply_seconds))}
    ${stat("Drafts to approve", c.approval, pastOf("approval", tg.approval_seconds))}
    ${stat("Held sends", c.held, pastOf("held", tg.held_seconds))}
    ${stat("Tasks due today", c.task, `${c.task.past} of ${c.task.total} overdue · ${t.later_tasks} due later`)}
    ${state.boot?.products.some(p => p.pipeline) ? `<div class="stat"><dt>Accounts needing you</dt><dd>${c.account.total + c.revisit.total}<span class="sub">${c.account.total} next action${c.account.total === 1 ? "" : "s"} overdue · ${c.revisit.total} to revisit</span></dd></div>` : ""}
  </dl>`;
  const waiting = t.items.filter(i => i.kind !== "queued");
  if (!waiting.length && !c.queued.total) {
    return top + stats + `<div class="section">${empty("Nothing is waiting on you",
      `Start a sequence from <a href="#people">People</a>, or add a signal by hand on <a href="#signals">Signals</a>. ${state.boot?.mode === "sandbox" ? `In sandbox mode, approved messages land in the <a href="/outbox">outbox</a>, where you can simulate a reply.` : ""}`)}</div>`;
  }
  const section = (title, items, note = "") => items.length ? `<section class="section" aria-labelledby="s-${title.replace(/\W/g, "")}">
      <div class="section-head"><h2 id="s-${title.replace(/\W/g, "")}">${e(title)} <span class="muted num">(${items.length})</span></h2>${note}</div>
      <div class="items">${items.map(it => RENDER[it.kind](it, t)).join("")}</div></section>` : "";
  const urgent = waiting.filter(i => i.state !== "ok");
  const rest = k => waiting.filter(i => i.state === "ok" && i.kind === k);
  const capped = Object.entries(c).filter(([, v]) => v.total > v.listed && v.total > 0)
    .map(([k, v]) => `${KIND[k]}s: showing ${v.listed} of ${v.total}`).join("; ");
  return top + stats + (capped ? `<p class="notice warn">${e(capped)}.</p>` : "") +
    section("Past or near target", urgent, `<p class="muted">Most overdue first</p>`) +
    section("Replies to label", rest("reply")) +
    section("Drafts to approve", rest("approval"), `<p class="muted">${e(t.note)}</p>`) +
    section("Held sends", rest("held")) +
    section("Tasks due today", rest("task")) +
    section("Accounts", [...rest("account"), ...rest("revisit")]) +
    section("Queued to send", t.items.filter(i => i.kind === "queued"));
}

// ---- dialogs ----

function taskDialog(personId = "") {
  const p = currentProduct({ required: true });
  const due = new Date().toISOString().slice(0, 10);
  openDialog({
    title: "Add a task", description: "A task is one next action only you can do, with a due date.",
    body: `<form class="form" data-form="add-task">
      ${field("product", "Product", { kind: "select", options: productOptions(p?.id) })}
      ${field("next_action", "Next action", { required: true, help: "One concrete thing, e.g. “Send Sam the setup guide”." })}
      <div class="form-row">${field("due", "Due", { kind: "date", value: due })}
      ${field("person_id", "Person id (optional)", { kind: "number", value: personId, attrs: 'min="1"' })}</div>
      <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Add task</button></div></form>`,
  });
}

export const actions = {
  approve: async (b, ctx) => {
    const r = await post(`/api/actions/${b.dataset.id}/approve`, { content_hash: b.dataset.hash });
    await ctx.done(r.status);
  },
  edit: b => {
    const item = b.closest(".item");
    item.querySelectorAll("[data-view]").forEach(el => { el.hidden = true; });
    const form = item.querySelector("form[data-form=edit-draft]");
    form.hidden = false;
    const ta = form.querySelector("textarea");
    ta.focus();
    ta.setSelectionRange(ta.value.length, ta.value.length);
  },
  "cancel-edit": b => {
    const item = b.closest(".item");
    const form = item.querySelector("form[data-form=edit-draft]");
    form.reset();
    form.hidden = true;
    item.querySelectorAll("[data-view]").forEach(el => { el.hidden = false; });
    item.querySelector("[data-action=edit]").focus();
  },
  skip: async (b, ctx) => ctx.done((await post(`/api/actions/${b.dataset.id}/skip`)).status),
  resolve: async (b, ctx) => ctx.done((await post(`/api/actions/${b.dataset.id}/resolve`, { sent: b.dataset.sent === "true" })).status),
  "check-sent": async (b, ctx) => {
    const r = await post(`/api/actions/${b.dataset.id}/check-sent`);
    await ctx.done(r.message, r.found ? "ok" : "info");
  },
  "ai-draft": async (b, ctx) => ctx.done((await post(`/api/actions/${b.dataset.id}/ai-draft`)).status),
  label: async (b, ctx) => ctx.done((await post(`/api/replies/${b.dataset.id}/label`, { class: b.dataset.class })).status),
  "label-later": b => {
    const d = new Date(Date.now() + 30 * 86400e3).toISOString().slice(0, 10);
    openDialog({
      title: "Not now: remind me", description: "The reply is labelled “not now” and a task to check back in is due on this date. The sequence stays stopped.",
      body: `<form class="form" data-form="label-later" data-id="${e(b.dataset.id)}">${field("remind_on", "Remind me on", { kind: "date", value: d, required: true })}
        <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Label not now</button></div></form>`,
    });
  },
  answer: (b, ctx) => {
    const it = ctx.data.items.find(i => i.reply?.id === Number(b.dataset.id));
    const r = it.reply;
    openDialog({
      title: `Answer ${r.person_name || r.from}`, wide: true,
      description: "Your answer is saved as a draft in the same email thread. It is sent only after you approve it.",
      body: `<form class="form" data-form="answer" data-id="${e(r.id)}">
        <details><summary>Their message</summary><pre class="message">${e(r.body || r.snippet)}</pre></details>
        ${field("body", "Your answer", { kind: "textarea", required: true })}
        <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Save as a draft</button></div></form>`,
    });
  },
  "close-task": (b, ctx) => {
    const k = ctx.data.items.find(i => i.task?.id === Number(b.dataset.id)).task;
    openDialog({
      title: "Close the task", description: `${e(k.next_action)}. Record what happened: outcomes show what to fix in the offer or the product.`,
      body: `<form class="form" data-form="close-task" data-id="${e(k.id)}">
        ${field("outcome", "Outcome", { kind: "select", required: true, options: `<option value="">Choose…</option>` + ctx.data.task_outcomes.map(o => `<option value="${e(o)}">${e(OUTCOME[o] || words(o))}</option>`).join("") })}
        ${field("note", "What happened (optional)", { kind: "textarea", attrs: 'rows="3"' })}
        ${field("minutes", "Minutes you spent (optional)", { kind: "number", attrs: 'min="0"' })}
        <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Close task</button></div></form>`,
    });
  },
  "add-task": () => taskDialog(),
};

export const forms = {
  "edit-draft": async (f, ctx, submitter) => {
    const v = formData(f);
    const r = await post(`/api/actions/${f.dataset.id}/edit`, { subject: v.subject || "", body: v.body, approve: submitter?.value === "approve" });
    await ctx.done(r.status);
  },
  "label-later": async (f, ctx) => ctx.done((await post(`/api/replies/${f.dataset.id}/label`, { class: "not_now", remind_on: formData(f).remind_on })).status),
  answer: async (f, ctx) => ctx.done((await post(`/api/replies/${f.dataset.id}/answer`, { body: formData(f).body })).status),
  "close-task": async (f, ctx) => {
    const v = formData(f);
    await ctx.done((await post(`/api/tasks/${f.dataset.id}/close`, { outcome: v.outcome, note: v.note, minutes: v.minutes === "" ? null : Number(v.minutes) })).status);
  },
  "add-task": async (f, ctx) => {
    const v = formData(f);
    await ctx.done((await post("/api/tasks", { product: v.product, next_action: v.next_action, due: v.due, person_id: Number(v.person_id) || 0 })).status);
  },
};

export default {
  id: "today", title: "Today", icon: "today", nav: "work", poll: true,
  load: () => api("/api/today" + query({ product: state.product })),
  render,
  pageActions: () => state.boot?.products.length ? `<button type="button" class="btn btn-sm" data-action="add-task">Add a task</button>` : "",
  actions, forms,
};
export { taskDialog };
