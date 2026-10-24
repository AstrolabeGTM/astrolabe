// Funnel, Automations, Outbox and Digest: the screens that replace the
// classic pages of the same names.
import { e, api, post, state, query, pageHead, empty, plural, ago, day, words, field, formData, openDialog, currentProduct } from "./lib.js";

const sec = (id, title, body, note = "") => `<section class="section" aria-labelledby="${id}"><div class="section-head"><h2 id="${id}">${title}</h2>${note}</div>${body}</section>`;
const of = (n, total) => `${n} of ${total}`;
const pct = (n, total) => total ? ` (${Math.round(100 * n / total)}%)` : "";
const table = (head, rows) => rows ? `<div class="table-wrap"><table class="stack-sm"><thead><tr>${head.map(h => `<th scope="col"${h.num ? ' class="num"' : ""}>${e(h.t || h)}</th>`).join("")}</tr></thead><tbody>${rows}</tbody></table></div>` : "";

// ---- funnel ----

function attrib(list, act) {
  if (!list?.length) return `<p class="muted">Nothing yet.</p>`;
  return table(["Where from", { t: "People", num: 1 }, { t: "Replied", num: 1 }, { t: act, num: 1 }, { t: "Paid", num: 1 }],
    list.map(a => `<tr><th scope="row">${e(a.key || "unknown")}</th><td class="num" data-label="People">${a.people}</td><td class="num" data-label="Replied">${of(a.replied, a.people)}</td>
      <td class="num" data-label="${e(act)}">${of(a.activated, a.people)}</td><td class="num" data-label="Paid">${of(a.paid, a.people)}</td></tr>`).join(""));
}

function experiments(list) {
  if (!list?.length) return `<p class="muted">No experiments. Add <code>variants</code> to a sequence step to compare versions.</p>`;
  return list.map(x => `<div class="card"><div class="row"><strong>${e(x.experiment)}</strong><span class="muted">goal: ${e(x.goal_stage)}</span></div>
    ${table(["Variant", { t: "Sent", num: 1 }, { t: "Replied", num: 1 }, { t: "Reached the goal", num: 1 }, { t: "95% interval", num: 1 }],
      x.variants.map(v => `<tr><th scope="row">${e(v.variant)}</th><td class="num" data-label="Sent">${v.sent}</td><td class="num" data-label="Replied">${of(v.replied, v.sent)}</td>
        <td class="num" data-label="Reached the goal">${of(v.reached_goal, v.sent)}</td><td class="num" data-label="95% interval">${Math.round(v.ci_low * 100)}–${Math.round(v.ci_high * 100)}%</td></tr>`).join(""))}
    <p>${e(x.verdict)}</p></div>`).join("");
}

function funnelRender(d) {
  const head = pageHead("Funnel", "People per funnel stage from product.yaml (a person counts for a stage they reached or any later one), the time between stages, who is stuck, where people came from and what they paid.");
  if (!d.product) return head + empty("No products are loaded", "Add a product first.");
  const r = d.report, p = d.product;
  const days = [30, 90, 180, 365];
  const top = r.stages[0]?.people || 0;
  const stages = table(["Stage", { t: "People", num: 1 }, { t: "From the stage before", num: 1 }],
    r.stages.map((s, i) => `<tr><th scope="row">${e(words(s.stage))}${s.activation ? ` <span class="tag ok">activation</span>` : ""}</th><td class="num" data-label="People">${s.people}</td>
      <td class="num" data-label="From the stage before">${i ? `${of(s.people, r.stages[i - 1].people)}${pct(s.people, r.stages[i - 1].people)}` : "—"}</td></tr>`).join(""));
  const times = r.time_between_stages.map(t => `<li><span>${e(words(t.from))} → ${e(words(t.to))}</span><span class="num">${t.people ? `${t.median_days.toFixed(1)} days, median of ${plural(t.people, "person", "people")}` : "no data"}</span></li>`).join("");
  const stuck = r.stuck.filter(s => s.count).map(s => `<li><strong>${e(words(s.stage))}</strong>: ${s.count} inactive over ${s.inactive_days} days
    <span class="sub">${s.people.map(x => `<a href="#person/${e(x.id)}">${e(x.name || `Person ${x.id}`)}</a> (${x.days_inactive} days)`).join(", ")}</span></li>`).join("");
  const revenue = r.revenue.length ? table(["Currency", { t: "Payments", num: 1 }, { t: "Refunds", num: 1 }, { t: "Net", num: 1 }],
    r.revenue.map(m => `<tr><th scope="row">${e(m.currency)}</th><td class="num" data-label="Payments">${(m.payments_cents / 100).toFixed(2)} <span class="sub">${plural(m.payments, "payment")}</span></td><td class="num" data-label="Refunds">${(m.refunds_cents / 100).toFixed(2)}</td><td class="num" data-label="Net"><strong>${(m.net_cents / 100).toFixed(2)}</strong></td></tr>`).join(""))
    : `<p class="muted">No payments yet (the Stripe webhook is in <a href="#settings">Settings</a>).</p>`;
  const cohorts = r.weekly_cohorts.length ? table(["Week", { t: "People", num: 1 }, { t: words(p.activation), num: 1 }, { t: "Paid", num: 1 }, "Window"],
    r.weekly_cohorts.map(c => `<tr><th scope="row">${e(day(c.week))}</th><td class="num" data-label="People">${c.people}</td><td class="num" data-label="${e(words(p.activation))}">${of(c.activated, c.people)}</td><td class="num" data-label="Paid">${of(c.paid, c.people)}</td><td data-label="Window">${c.complete ? "complete" : `<span class="tag warn">incomplete</span>`}</td></tr>`).join("")) : `<p class="muted">No cohorts yet.</p>`;
  const sp = r.spend;
  const totalOutcomes = r.task_outcomes.reduce((n, o) => n + o.count, 0);
  const q = r.requests_by_reported_source;
  return head + `<form class="toolbar" data-form="funnel-window">${field("days", "Window", { kind: "select", options: days.map(n => `<option value="${n}"${n === d.days ? " selected" : ""}>Last ${n} days</option>`).join("") })}<button class="btn">Show</button></form>
    <p class="muted">${e(p.name)}, since ${e(day(r.since))}; ${plural(top, "person", "people")} reached the first stage.</p>
    ${sec("f-stages", "Stages", stages)}
    <div class="grid-2">${sec("f-times", "Time between stages", times ? `<ul class="tally">${times}</ul>` : `<p class="muted">No data.</p>`)}
      ${sec("f-stuck", "Stuck", stuck ? `<ul class="plain-list">${stuck}</ul>` : `<p class="muted">Nobody is stuck.</p>`)}</div>
    ${sec("f-sources", "Where people came from (first touch)", attrib(r.by_source, words(p.activation)))}
    ${sec("f-plays", "Automations: who they started, and what happened after", attrib(r.by_play, words(p.activation)))}
    ${sec("f-requests", "Form requests by the source people reported", q.total ? `<p class="muted">${q.total} requests; ${q.unanswered} of ${q.total} left the question blank. What people say, not first-touch attribution.</p><ul class="tally">${q.sources.map(s => `<li><span>${e(s.source)}</span><span class="num">${of(s.requests, q.total)}</span></li>`).join("")}</ul>` : `<p class="muted">No form requests in this window.</p>`)}
    ${sec("f-exp", "Experiments", experiments(d.experiments))}
    ${sec("f-revenue", "Revenue", revenue)}
    ${sec("f-cohorts", "Weekly cohorts", cohorts, `<p class="muted">Incomplete: still inside the offer window</p>`)}
    ${sec("f-spend", "Effort and spend", `<dl class="kv"><dt>Claude API</dt><dd>${plural(sp.llm_calls, "call")}, ${sp.input_tokens} tokens in, ${sp.output_tokens} out${sp.llm_usd != null ? `, about $${sp.llm_usd.toFixed(2)}` : " (set ASTROLABE_LLM_PRICES for dollars)"}</dd>
      <dt>Your time on closed tasks</dt><dd>${sp.operator_minutes} minutes</dd><dt>Activated, paying</dt><dd>${sp.activated}, ${sp.paying}</dd>
      ${sp.minutes_per_activated != null ? `<dt>Minutes per activated</dt><dd>${Math.round(sp.minutes_per_activated)}</dd>` : ""}</dl>
      <p class="help">Observed costs, not proof that an automation caused a purchase. With no activations there is no cost per activation.</p>`)}
    ${sec("f-outcomes", "Task outcomes", totalOutcomes ? `<ul class="tally">${r.task_outcomes.map(o => `<li><span>${e(words(o.outcome))}</span><span class="num">${of(o.count, totalOutcomes)} · ${o.minutes} minutes</span></li>`).join("")}</ul>` : `<p class="muted">No closed tasks yet.</p>`)}`;
}

export const funnel = {
  id: "funnel", title: "Funnel", icon: "funnel", nav: "insight",
  load: async params => {
    const p = currentProduct({ required: true });
    if (!p) return { product: null };
    const days = Number(params.days) || 90;
    const [report, exp] = await Promise.all([api("/api/funnel" + query({ product: p.id, days })), api("/api/experiments" + query({ product: p.id }))]);
    return { product: p, report, experiments: exp.experiments || [], days };
  },
  render: funnelRender,
  pageActions: d => d?.product ? `<button type="button" class="btn btn-sm" data-action="record-stage">Record a stage by hand</button>` : "",
  actions: {
    "record-stage": (b, ctx) => openDialog({ title: "Record a stage", description: "When you know someone reached a stage (they installed, they paid) and no event told Astrolabe. Sequences that stop on that stage stop.",
      body: `<form class="form" data-form="record-stage">${field("person", "Person id or email", { required: true })}
        ${field("stage", "Stage", { kind: "select", options: ctx.data.product.funnel.map(s => `<option value="${e(s)}">${e(words(s))}</option>`).join("") })}
        <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Record</button></div></form>` }),
  },
  forms: {
    "funnel-window": f => { location.hash = "#funnel" + query({ days: formData(f).days }); },
    "record-stage": async (f, ctx) => { const v = formData(f); await post("/api/stage", { product: ctx.data.product.id, person: v.person.trim(), stage: v.stage }); await ctx.done(`Recorded ${words(v.stage)}.`); },
  },
};

// ---- automations ----

const describe = o => Object.entries(o || {}).filter(([, v]) => v !== "" && v != null && v !== false).map(([k, v]) => typeof v === "object" ? `${words(k)}: ${Object.values(v).join(" for ")}` : v === true ? words(k) : `${words(k)}: ${v}`).join(" · ") || "—";

export const automations = {
  id: "automations", title: "Automations", icon: "automations", nav: "insight",
  load: async params => {
    const [list, runs] = await Promise.all([api("/api/automations" + query({ product: state.product })), api("/api/automations/runs" + query({ product: state.product, play: params.automation, limit: 100 }))]);
    return { plays: list.plays || [], runs: runs.runs || [], filter: params.automation || "" };
  },
  render: d => {
    const head = pageHead("Automations", "Rules in each product's automations.yaml: when a trigger happens and the filter matches, start a sequence or make a task. Like CI workflows for outreach; every run is recorded, skips with their reason.");
    if (!d.plays.length) return head + empty("No automations", "Add rules to <code>automations.yaml</code> in a product folder, then reload products in <a href=\"#settings\">Settings</a>.");
    const rows = d.plays.map(p => {
      const total = p.enrolled + p.tasks + p.skipped;
      return `<tr><th scope="row">${e(p.id)}<span class="sub">${e(p.product)}${p.paused ? " · paused" : ""}</span></th><td data-label="When">${e(describe(p.trigger))}<span class="sub">${p.when && Object.values(p.when).some(Boolean) ? e(describe(p.when)) : "no filter"}</span></td>
        <td data-label="Then">${e(describe(p.action))}</td><td class="num" data-label="Runs">${total}<span class="sub">${p.enrolled} started, ${p.tasks} tasks, ${p.skipped} skipped</span></td></tr>`;
    }).join("");
    const runs = d.runs.map(r => `<tr><td data-label="When">${ago(r.at)}</td><th scope="row">${e(r.play)}<span class="sub">${e(r.product)}</span></th>
      <td data-label="Outcome"><span class="tag ${r.outcome === "skipped" ? "warn" : "ok"}">${e(r.outcome)}</span>${r.detail ? `<span class="sub">${e(r.detail)}</span>` : ""}</td>
      <td data-label="Person">${r.person_id ? `<a href="#person/${e(r.person_id)}">Person ${e(r.person_id)}</a>` : "—"}</td></tr>`).join("");
    return head + sec("a-list", "Rules", table(["Automation", "When", "Then", { t: "Runs", num: 1 }], rows))
      + sec("a-runs", "Recent runs", `<form class="toolbar" data-form="runs-filter">${field("automation", "Automation", { kind: "select", options: `<option value="">All</option>` + d.plays.map(p => `<option${p.id === d.filter ? " selected" : ""}>${e(p.id)}</option>`).join("") })}<button class="btn">Show</button></form>
        ${runs ? table(["When", "Automation", "Outcome", "Person"], runs) + `<p class="help">The newest ${d.runs.length} runs.</p>` : `<p class="muted">No runs yet.</p>`}`);
  },
  forms: { "runs-filter": f => { location.hash = "#automations" + query({ automation: formData(f).automation }); } },
};

// ---- outbox (sandbox) ----

export const outbox = {
  id: "outbox", title: "Outbox", icon: "outbox", nav: "work", sandboxOnly: true,
  load: () => api("/api/outbox"),
  render: d => {
    const head = pageHead("Outbox", "Sandbox mode: everything Astrolabe “sent” lands here; nothing left this machine. Play the recipient: a simulated reply or bounce goes through the real reply handling.");
    const list = d.messages || [];
    if (!list.length) return head + empty("The outbox is empty", "Approve a draft on <a href=\"#today\">Today</a>; within a minute it appears here.");
    return head + `<p class="muted">${plural(list.length, "message")}, newest first.</p><div class="stack">${list.map(m => `<article class="card" data-key="outbox:${e(m.id)}" aria-labelledby="ob-${e(m.id)}">
      <div class="row"><span class="tag">${e(m.channel)}</span><h2 id="ob-${e(m.id)}" class="grow-title item-title">${e(m.subject || "(no subject)")}</h2><span class="muted">${ago(m.sent_at)}</span></div>
      <dl class="envelope"><dt>From</dt><dd>${e(m.from)}</dd><dt>To</dt><dd>${m.person_id ? `<a href="#person/${e(m.person_id)}">${e(m.to)}</a>` : e(m.to)}</dd></dl>
      <details><summary>The message</summary><pre class="message">${e(m.body)}</pre></details>
      <div class="item-actions"><button type="button" class="btn" data-action="sim-reply" data-id="${e(m.id)}">Simulate a reply…</button>${m.channel === "email" ? `<button type="button" class="btn btn-danger" data-action="sim-bounce" data-id="${e(m.id)}">Simulate a bounce</button>` : ""}</div></article>`).join("")}</div>`;
  },
  actions: {
    "sim-reply": b => openDialog({ title: "Simulate a reply", description: "As the recipient. The sequence stops and the reply waits on Today to be labelled.",
      body: `<form class="form" data-form="sim-reply" data-id="${e(b.dataset.id)}">${field("body", "Their reply", { kind: "textarea", value: "Thanks, this looks useful. How do I get started?", attrs: 'rows="4"' })}
        <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Deliver the reply</button></div></form>` }),
    "sim-bounce": async (b, ctx) => { await post("/api/outbox/bounce", { outbox_id: Number(b.dataset.id) }); await ctx.done("Bounced: the person is now on the do-not-contact list."); },
  },
  forms: {
    "sim-reply": async (f, ctx) => { await post("/api/outbox/reply", { outbox_id: Number(f.dataset.id), body: formData(f).body }); await ctx.done("Reply delivered through the real reply handling: a sequence it answers stops and the reply waits on Today; a reply to a list confirmation confirms the sign-up."); },
  },
};

// ---- digest ----

export const digest = {
  id: "digest", title: "Digest", icon: "digest", nav: "insight",
  load: async () => {
    const [now, last] = await Promise.all([api("/api/digest"), api("/api/digest?latest=true")]);
    return { now, last };
  },
  render: d => pageHead("Digest", "The weekly summary emailed on Mondays (to ASTROLABE_DIGEST_TO): what moved, what is stuck and up to three actions, built from the numbers by rules, not written by AI.")
    + sec("d-now", "This week so far", `<pre class="message digest-text">${e(d.now.text)}</pre>`)
    + sec("d-last", "The last one made", d.last.text ? `<p class="muted">Made ${ago(d.last.made_at)}.</p><pre class="message digest-text">${e(d.last.text)}</pre>` : `<p class="muted">${e(d.last.note || "None yet.")}</p>`),
};
