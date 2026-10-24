// Pipeline: accounts (companies) by deal stage, and people by funnel stage.
import { $, e, api, post, state, query, pageHead, empty, plural, day, words, field, formData, openDialog } from "./lib.js";

const cap = s => s.charAt(0).toUpperCase() + s.slice(1);
const ANSWER = { yes: ["ok", "yes"], no: ["bad", "no"], unknown: ["", "not known"] };

// pipelineProduct is the product picked in the top bar if it has a
// pipeline, else the first that does.
export function pipelineProduct() {
  const ps = (state.boot?.products || []).filter(p => p.pipeline);
  return ps.find(p => p.id === state.product) || ps[0] || null;
}

export function chips(board, answers) {
  const one = (c, record) => {
    const a = answers?.[c] || "unknown";
    const [cls, word] = ANSWER[a] || ["", a];
    return `<span class="chk ${cls}${record ? " record" : ""}" title="${e(c)}: ${e(word)}${record ? " (recorded, never blocks)" : ""}">${e(c)}<span class="sr-only">: ${e(word)}</span><span aria-hidden="true">${a === "yes" ? " ✓" : a === "no" ? " ✗" : " ?"}</span></span>`;
  };
  return `<span class="chips">${board.checks.map(c => one(c, false)).join("")}${(board.record_checks || []).map(c => one(c, true)).join("")}</span>`;
}

function card(a, b, product) {
  const due = a.next_due ? `due ${e(day(a.next_due))}` : "";
  return `<li class="deal${a.stuck ? " stuck" : ""}${a.overdue ? " overdue" : ""}" data-key="account:${e(a.id)}">
    <h4><a href="#account/${e(a.id)}?product=${encodeURIComponent(product)}">${e(a.company)}</a></h4>
    ${chips(b, a.answers)}
    <p class="deal-meta">${plural(a.days_in_stage, "day")} in stage${a.stuck ? ` <span class="tag warn">stuck</span>` : ""}</p>
    ${a.next_action ? `<p class="deal-next"><span class="sr-only">Next action: </span>${e(a.next_action)}<span class="sub">${due}${a.overdue ? ` <span class="tag bad">overdue</span>` : ""}</span></p>` : ""}
    <div class="deal-actions"><button type="button" class="btn btn-sm" data-action="move" data-id="${e(a.id)}" aria-label="Move ${e(a.company)}">Move…</button>
      <button type="button" class="btn btn-sm btn-ghost" data-action="close" data-id="${e(a.id)}" aria-label="Pause or lose ${e(a.company)}">Pause or lose…</button></div></li>`;
}

function accountsBoard(b, product) {
  const open = b.accounts || [];
  const total = open.length;
  const cols = b.stages.map(s => {
    const list = open.filter(a => a.stage === s.stage);
    const gated = b.gate && b.stages.findIndex(x => x.stage === s.stage) >= b.stages.findIndex(x => x.stage === b.gate);
    return `<section class="column" aria-labelledby="col-${e(s.stage)}"><h3 id="col-${e(s.stage)}">${e(cap(words(s.stage)))} <span class="muted num">${s.accounts}</span>${gated ? `<span class="sr-only"> (needs every gating check yes)</span><span class="gate-mark" title="Needs every gating check answered yes" aria-hidden="true"> ⚑</span>` : ""}</h3>
      ${list.length ? `<ul class="deals">${list.map(a => card(a, b, product)).join("")}</ul>` : `<p class="muted column-empty">None</p>`}</section>`;
  }).join("");
  const stats = `<dl class="stats" aria-label="Accounts">
    <div class="stat"><dt>Open accounts</dt><dd>${total}<span class="sub">in ${plural(b.stages.length, "stage")}</span></dd></div>
    <div class="stat"><dt>Stuck</dt><dd>${b.stuck.length}<span class="sub">of ${total} open, over ${e(b.stuck_after)} in one stage</span></dd></div>
    <div class="stat"><dt>Next action overdue</dt><dd>${b.overdue.length}<span class="sub">of ${total} open</span></dd></div>
    <div class="stat"><dt>Paused</dt><dd>${b.paused}<span class="sub">${b.lost} lost</span></dd></div>
    <div class="stat"><dt>Revisit date reached</dt><dd>${b.revisit_due.length}<span class="sub">of ${b.paused + b.lost} paused or lost</span></dd></div></dl>`;
  const closed = (b.closed || []).map(a => `<tr data-key="account:${e(a.id)}"><th scope="row"><a href="#account/${e(a.id)}?product=${encodeURIComponent(product)}">${e(a.company)}</a></th>
      <td data-label="Status"><span class="tag ${a.status === "lost" ? "bad" : "warn"}">${e(a.status)}</span><span class="sub">from ${e(words(a.stage))}</span></td>
      <td data-label="Reason">${e(words(a.reason))}</td>
      <td data-label="Revisit">${a.revisit_on ? `${e(day(a.revisit_on))}${b.revisit_due.some(x => x.id === a.id) ? ` <span class="tag info">look again now</span>` : ""}` : "—"}</td>
      <td><button type="button" class="btn btn-sm" data-action="reopen" data-id="${e(a.id)}" aria-label="Reopen ${e(a.company)}">Reopen…</button></td></tr>`).join("");
  return `${stats}
    <div class="board" role="region" aria-label="Accounts board" tabindex="0">${cols}</div>
    <p class="help">Chips are the qualification checks: ✓ yes, ✗ no, ? not known; dashed ones are recorded but never block. ⚑ marks the stages that need every gating check (${e(b.checks.join(", ") || "none")}) answered yes.</p>
    ${closed ? `<section class="section" aria-labelledby="s-closed"><div class="section-head"><h3 id="s-closed">Paused and lost <span class="muted num">(${b.paused + b.lost})</span></h3></div>
      <div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">Account</th><th scope="col">Status</th><th scope="col">Reason</th><th scope="col">Revisit on</th><th scope="col"><span class="sr-only">Reopen</span></th></tr></thead><tbody>${closed}</tbody></table></div></section>` : ""}`;
}

function peopleBoard(pb) {
  const cols = pb.stages.map(s => {
    const label = s.stage ? cap(words(s.stage)) : "No stage yet";
    const more = s.people > s.shown.length ? `<p class="help">Showing ${s.shown.length} of ${s.people}, longest first.</p>` : "";
    return `<section class="column" aria-labelledby="pcol-${e(s.stage || "none")}"><h3 id="pcol-${e(s.stage || "none")}">${e(label)}${s.activation ? ` <span class="tag ok">activation</span>` : ""} <span class="muted num">${s.people}</span></h3>
      ${s.shown.length ? `<ul class="deals">${s.shown.map(p => `<li class="deal"><h4><a href="#person/${e(p.id)}">${e(p.name || `Person ${p.id}`)}</a></h4>
        <p class="deal-meta">${p.company ? `${e(p.company)} · ` : ""}${plural(p.days_in_stage, "day")} here</p></li>`).join("")}</ul>${more}` : `<p class="muted column-empty">None</p>`}</section>`;
  }).join("");
  return `<p class="muted">${plural(pb.total, "person", "people")} in the product; each is in the furthest stage they reached, so the columns add up to ${pb.total}.</p>
    <div class="board" role="region" aria-label="People board" tabindex="0">${cols}</div>`;
}

function stageOptions(b, current) {
  return b.stages.map(s => `<option value="${e(s.stage)}"${s.stage === current ? " selected" : ""}>${e(words(s.stage))}</option>`).join("");
}

export function moveDialog(a, b, reopen = false) {
  const idx = b.stages.findIndex(s => s.stage === a.stage);
  const next = reopen ? a.stage : (b.stages[idx + 1]?.stage || a.stage);
  const tomorrow = new Date(Date.now() + 86400e3).toISOString().slice(0, 10);
  openDialog({
    title: reopen ? `Reopen ${a.company}` : `Move ${a.company}`,
    description: `Now in ${e(words(a.stage))} for ${plural(a.days_in_stage, "day")}. Every move needs one line of evidence: what happened.`,
    body: `<form class="form" data-form="move" data-id="${e(a.id)}">
      ${field("stage", "Move to", { kind: "select", options: stageOptions(b, next), attrs: 'data-change="gate-hint"' })}
      <p class="notice warn" id="gate-hint" hidden></p>
      ${field("evidence", "Evidence", { required: true, help: "One line, e.g. “Sam agreed a two-week trial on the payments repo”." })}
      <div class="form-row">${field("next_action", reopen ? "Next action" : "Next action (optional)", { required: reopen, help: reopen ? "" : "Leave empty to keep the current one." })}
      ${field("due", "Due", { kind: "date", value: tomorrow })}</div>
      <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">${reopen ? "Reopen" : "Move"}</button></div></form>`,
  });
  gateHint($("#dialog select[name=stage]"), a, b);
}

// gateHint warns before a move into a gated stage while checks aren't yes.
export function gateHint(sel, a, b) {
  const box = $("#gate-hint");
  if (!box) return;
  const gi = b.stages.findIndex(s => s.stage === b.gate);
  const ti = b.stages.findIndex(s => s.stage === sel.value);
  const failing = b.checks.filter(c => (a.answers?.[c] || "unknown") !== "yes");
  box.hidden = !(b.gate && gi >= 0 && ti >= gi && failing.length);
  box.textContent = box.hidden ? "" : `${words(sel.value)} needs every gating check answered yes. Still open: ${failing.map(c => `${c} (${a.answers?.[c] || "unknown"})`).join(", ")}. Record the answers on the account page first; this move will be refused.`;
}

export function pauseDialog(a, b) {
  const later = new Date(Date.now() + 30 * 86400e3).toISOString().slice(0, 10);
  openDialog({
    title: `Pause or lose ${a.company}`,
    description: "It leaves the board with a reason, and a task to look at it again appears on the revisit date. Its next action is dropped; Reopen brings it back.",
    body: `<form class="form" data-form="close" data-id="${e(a.id)}">
      <fieldset class="field"><legend class="label">Status</legend>
        <label class="check"><input type="radio" name="status" value="paused" checked> Paused: not now, but likely later</label>
        <label class="check"><input type="radio" name="status" value="lost"> Lost: they chose not to</label></fieldset>
      ${field("reason", "Reason", { kind: "select", required: true, options: b.reasons.map(r => `<option value="${e(r)}">${e(words(r))}</option>`).join("") })}
      ${field("revisit", "Revisit on", { kind: "date", value: later, required: true })}
      ${field("note", "Note (optional)", {})}
      <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Save</button></div></form>`,
  });
}

function addDialog(product) {
  const tomorrow = new Date(Date.now() + 86400e3).toISOString().slice(0, 10);
  openDialog({
    title: "Add an account", description: "A company you are working with. It starts at the first stage with one next action.",
    body: `<form class="form" data-form="add-account" data-product="${e(product)}">
      ${field("company", "Company domain or name", { required: true, help: "e.g. acme.example" })}
      ${field("evidence", "Why it belongs here", { required: true, help: "One line: a named person and a specific need or signal." })}
      <div class="form-row">${field("next_action", "Next action", { required: true })}${field("due", "Due", { kind: "date", value: tomorrow })}</div>
      <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Add account</button></div></form>`,
  });
}

const find = (ctx, id) => [...(ctx.data.board.accounts || []), ...(ctx.data.board.closed || [])].find(a => a.id === Number(id));

export default {
  id: "pipeline", title: "Pipeline", icon: "pipeline", nav: "work",
  load: async () => {
    const p = pipelineProduct();
    if (!p) return { product: null };
    const [board, people] = await Promise.all([api("/api/pipeline" + query({ product: p.id })), api("/api/pipeline/people" + query({ product: p.id }))]);
    return { product: p, board, people };
  },
  render: d => {
    const head = pageHead("Pipeline", "Accounts are companies moving through your deal stages, each with one next action; people move through the product's funnel stages on their own board.");
    if (!d.product) return head + empty("No product has a pipeline", "Add <code>pipeline:</code> (stages, qualify checks) to a b2b product's <code>product.yaml</code>, then reload products in <a href=\"#settings\">Settings</a>.");
    return head + `<section class="section" aria-labelledby="s-accounts"><div class="section-head"><h2 id="s-accounts">Accounts in ${e(d.product.name)}</h2>
      <p class="muted">Gate: ${e(words(d.board.gate || "none"))}</p></div>${accountsBoard(d.board, d.product.id)}</section>
      <section class="section" aria-labelledby="s-people"><div class="section-head"><h2 id="s-people">People by funnel stage</h2><p class="muted">From product.yaml <code>funnel</code></p></div>${peopleBoard(d.people)}</section>`;
  },
  pageActions: d => d?.product ? `<button type="button" class="btn btn-sm" data-action="add-account">Add an account</button>
    <button type="button" class="btn btn-sm" data-action="export">Export CSV</button>` : "",
  actions: {
    move: (b, ctx) => moveDialog(find(ctx, b.dataset.id), ctx.data.board),
    reopen: (b, ctx) => moveDialog(find(ctx, b.dataset.id), ctx.data.board, true),
    close: (b, ctx) => pauseDialog(find(ctx, b.dataset.id), ctx.data.board),
    "gate-hint": (el, ctx) => gateHint(el, find(ctx, el.form.dataset.id), ctx.data.board),
    "add-account": (b, ctx) => addDialog(ctx.data.product.id),
    export: async (b, ctx) => {
      const r = await api("/api/pipeline/export" + query({ product: ctx.data.product.id }));
      const url = URL.createObjectURL(new Blob([r.csv], { type: "text/csv" }));
      const link = Object.assign(document.createElement("a"), { href: url, download: `pipeline-${ctx.data.product.id}.csv` });
      document.body.append(link); link.click(); link.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    },
  },
  forms: {
    move: async (f, ctx) => {
      const v = formData(f);
      const a = find(ctx, f.dataset.id);
      const body = { product: ctx.data.product.id, account: String(a.id), stage: v.stage, evidence: v.evidence };
      if (v.next_action) Object.assign(body, { next_action: v.next_action, due: v.due });
      await post("/api/accounts/move", body);
      await ctx.done(`${a.company} is now in ${words(v.stage)}.`);
    },
    close: async (f, ctx) => {
      const v = formData(f);
      const a = find(ctx, f.dataset.id);
      await post("/api/accounts/close", { product: ctx.data.product.id, account: String(a.id), status: v.status, reason: v.reason, revisit: v.revisit, note: v.note });
      await ctx.done(`${a.company} is ${v.status}; look again on ${day(v.revisit)}.`);
    },
    "add-account": async (f, ctx) => {
      const v = formData(f);
      const r = await post("/api/accounts/add", { product: f.dataset.product, company: v.company, evidence: v.evidence, next_action: v.next_action, due: v.due });
      await ctx.done(`${r.company} added in ${words(r.stage)}.`);
    },
  },
};
