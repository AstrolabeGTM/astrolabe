// Campaigns and the creative studio. A campaign is a goal, a landing page,
// a budget cap, dates, a stop rule and a gate checklist; creatives are HTML
// templates with copy, rendered at each size and approved by hash.
import { $$, e, api, post, state, query, pageHead, empty, plural, ago, day, words, ext, field, formData, openDialog } from "./lib.js";

const money = (n, cur) => `${Number(n || 0).toFixed(2)} ${e(cur || "")}`.trim();
const sec = (id, title, body, note = "") => `<section class="section" aria-labelledby="${id}"><div class="section-head"><h2 id="${id}">${title}</h2>${note}</div>${body}</section>`;

// ---- the list ----

let campaignsFile = "";

function listRender(d) {
  const head = pageHead("Campaigns", "A campaign is a planned push (a launch week, a small paid test) with a budget cap, dates, a stop rule and checks to make before spending; every change is kept.");
  if (!d.campaigns.length) {
    return head + empty("No campaigns yet", "Campaigns are files in <code>products/&lt;id&gt;/campaigns/</code>. Add one (see the demo's <code>launch-week.yaml</code>), then reload products in <a href=\"#settings\">Settings</a>.");
  }
  const rows = d.campaigns.map(c => `<tr><th scope="row"><a href="#campaign/${encodeURIComponent(c.id)}?product=${encodeURIComponent(c.product)}">${e(c.label)}</a><span class="sub">${e(c.code)} · ${e(c.product)}</span></th>
      <td data-label="Dates">${e(day(c.start))}${c.end ? ` to ${e(day(c.end))}` : ""}<span class="sub">${c.active ? "running" : c.start > new Date().toISOString().slice(0, 10) ? "not started" : "ended"}</span></td>
      <td data-label="Spent" class="num">${money(c.spent, c.currency)}<span class="sub">of a ${money(c.budget_cap, c.currency)} cap</span></td>
      <td data-label="Gate">${e(c.gate)}</td>
      <td data-label="Warnings">${(c.warnings || []).length ? c.warnings.map(w => `<span class="sub warn-text">${e(w)}</span>`).join("") : `<span class="muted">none</span>`}</td></tr>`).join("");
  return head + `<p class="muted">${plural(d.campaigns.length, "campaign")}${state.product ? ` in ${e(state.product)}` : ""}.</p>
    <div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">Campaign</th><th scope="col">Dates</th><th scope="col" class="num">Spent</th><th scope="col">Gate checks</th><th scope="col">Warnings</th></tr></thead><tbody>${rows}</tbody></table></div>`;
}

export const campaignsList = {
  id: "campaigns", title: "Campaigns", icon: "campaigns", nav: "work",
  load: () => api("/api/campaigns" + query({ product: state.product })),
  render: listRender,
  pageActions: () => `<button type="button" class="btn btn-sm" data-action="campaigns-json">campaigns.json</button>`,
  actions: {
    "campaigns-json": async () => {
      const r = await api("/api/campaigns/json" + query({ product: state.product }));
      openDialog({ title: "campaigns.json", wide: true,
        description: "For a website to prerender a landing page per campaign that has not ended. Nothing is published from here.",
        body: `<pre class="message code-block">${e(r.file)}</pre><div class="form-actions"><button type="button" class="btn" data-action="download-json">Download</button><button type="button" class="btn btn-primary" data-close>Done</button></div>` });
      campaignsFile = r.file;
    },
    "download-json": () => {
      const url = URL.createObjectURL(new Blob([campaignsFile], { type: "application/json" }));
      const a = Object.assign(document.createElement("a"), { href: url, download: "campaigns.json" });
      document.body.append(a); a.click(); a.remove(); setTimeout(() => URL.revokeObjectURL(url), 1000);
    },
  },
};

// ---- one campaign ----

function settingsForm(c) {
  const st = c.status.settings;
  const when = st.stop_rule_when || {};
  return `<form class="form card" data-form="campaign-set">
    <div class="form-row">${field("budget_cap", `Budget cap (${st.currency})`, { kind: "number", value: Number(st.budget_cap).toFixed(2), attrs: 'min="0" step="0.01"' })}
      ${field("currency", "Currency", { value: st.currency, help: "Fixed once spend is recorded." })}</div>
    <div class="form-row">${field("start", "Start", { kind: "date", value: st.start })}${field("end", "End (empty: no end)", { kind: "date", value: st.end || "" })}</div>
    ${field("stop_rule_text", "Stop rule, in words", { value: st.stop_rule_text || "" })}
    <div class="form-row">${field("stop_spend_pct", "Stop when this % of the cap is spent", { kind: "number", value: when.spend_pct ?? "", attrs: 'min="1" max="100"' })}
      ${field("stop_max_events", "…with at most this many success events", { kind: "number", value: when.max_events ?? "", attrs: 'min="0"' })}</div>
    ${field("note", "Why (kept in the history)", {})}
    <div class="form-actions"><button class="btn btn-primary">Save changes</button></div>
    <p class="help">Only what you change is saved. Every change is kept with who, when, the old and the new value. Nothing raises spend except you.</p></form>`;
}

function gateList(c) {
  const g = c.status.gate;
  const done = g.filter(x => x.checked).length;
  return `<p class="muted">${done} of ${g.length} checked. ${c.status.gate_complete ? "The gate is complete." : "Spend recorded before the gate is complete is allowed, but flagged on the entry for good."}</p>
    <ul class="checklist">${g.map((x, i) => `<li><label class="check"><input type="checkbox" data-change="gate" data-item="${i + 1}"${x.checked ? " checked" : ""}> ${e(x.item)}</label>
      ${x.checked ? `<span class="sub">checked by ${e(x.checked_by)} ${ago(x.checked_at)}${x.note ? `: ${e(x.note)}` : ""}</span>` : ""}</li>`).join("")}</ul>`;
}

function history(h) {
  if (!h.length) return `<p class="muted">No changes yet.</p>`;
  return `<div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">When</th><th scope="col">What</th><th scope="col">Old</th><th scope="col">New</th><th scope="col">By</th></tr></thead><tbody>
    ${[...h].reverse().map(x => `<tr><td data-label="When">${ago(x.at)}</td><th scope="row">${e(words(x.field))}${x.note ? `<span class="sub">${e(x.note)}</span>` : ""}</th>
      <td data-label="Old">${e(x.old || "—")}</td><td data-label="New">${e(x.new || "—")}</td><td data-label="By">${e(x.changed_by)}</td></tr>`).join("")}</tbody></table></div>`;
}

function spend(c) {
  const s = c.status.spend;
  const cur = c.status.settings.currency;
  const rows = c.recent_spend.map(x => `<tr><td data-label="Date">${e(day(x.date))}</td><th scope="row">${e(x.platform)}${x.note ? `<span class="sub">${e(x.note)}</span>` : ""}</th>
      <td data-label="Amount" class="num">${money(x.amount, x.currency)}</td><td data-label="Clicks" class="num">${x.clicks ?? "—"}</td>
      <td data-label="Notes">${x.before_gate ? `<span class="tag warn">before the gate</span>` : ""} <span class="muted">${e(x.source)}</span></td></tr>`).join("");
  return `<p class="muted">${money(s.total, cur)} in ${plural(s.entries, "entry", "entries")}; ${s.before_gate_entries} recorded before the gate (${money(s.before_gate_amount, cur)}); ${s.platform_clicks} platform clicks from ${plural(s.entries_with_clicks, "entry", "entries")} that had them.</p>
    ${rows ? `<div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">Date</th><th scope="col">Platform</th><th scope="col" class="num">Amount</th><th scope="col" class="num">Clicks</th><th scope="col">Notes</th></tr></thead><tbody>${rows}</tbody></table></div>` : ""}
    <div class="row section"><button type="button" class="btn" data-action="spend">Record spend…</button><button type="button" class="btn" data-action="import-spend">Import a spend CSV…</button></div>`;
}

function creative(c, cr, product) {
  const frames = cr.sizes.map(size => {
    const [w, h] = size.split("x").map(Number);
    const rendered = cr.renders.find(r => r.size === size && r.format === "png");
    const src = query({ product, campaign: c.id, creative: cr.id, size });
    const label = `${cr.id} at ${w} × ${h}`;
    return `<figure class="creative">
      <div class="creative-frame" data-w="${w}" data-h="${h}">${rendered
        ? `<img src="/api/campaign/creatives/file${src}&amp;format=png" width="${w}" height="${h}" alt="${e(cr.alt)}">`
        : `<iframe src="/api/campaign/creatives/preview${src}" width="${w}" height="${h}" title="Preview of ${e(label)}"></iframe>`}</div>
      <figcaption>${w} × ${h} px <span class="muted" data-scale></span> · ${rendered ? `rendered ${ago(rendered.rendered_at)}` : "live preview, not rendered"}</figcaption></figure>`;
  }).join("");
  const copy = Object.entries(cr.copy).map(([k, v]) => `<dt>${e(words(k))}</dt><dd>${e(v)}</dd>`).join("");
  const status = cr.approved ? `<span class="tag ok">Approved</span><span class="muted"> by ${e(cr.approved_by)} ${ago(cr.approved_at)}</span>` : `<span class="tag warn">${e(words(cr.status || "not approved"))}</span>`;
  return `<article class="card creative-card" data-key="creative:${e(cr.id)}" aria-labelledby="cr-${e(cr.id)}">
    <div class="row"><h3 id="cr-${e(cr.id)}">${e(cr.id)}</h3>${status}</div>
    <div class="creative-frames">${frames}</div>
    ${cr.flags.length ? `<ul class="flags" aria-label="Claims check">${cr.flags.map(f => `<li>Check: ${e(f)}</li>`).join("")}</ul>` : `<p class="help">The claims check found nothing (never-claims, placeholders, numbers without a source).</p>`}
    <dl class="kv">${copy}<dt>Alt text</dt><dd>${e(cr.alt)}</dd>${cr.source ? `<dt>Source</dt><dd>${e(cr.source)}</dd>` : ""}</dl>
    ${cr.not_rendered?.length ? `<p class="help">Not rendered yet: ${e(cr.not_rendered.join(", "))}.</p>` : ""}
    <div class="item-actions"><button type="button" class="btn" data-action="edit-creative" data-id="${e(cr.id)}">Edit copy</button>
      <button type="button" class="btn" data-action="render-creative" data-id="${e(cr.id)}"${c.renderer.configured ? "" : ` aria-describedby="renderer-note"`}>Render</button>
      <button type="button" class="btn btn-primary" data-action="approve-creative" data-id="${e(cr.id)}"${cr.approved || cr.flags.length ? " disabled" : ""}>Approve</button></div></article>`;
}

function posts(c) {
  const held = c.posts_held_by_claims_check || [];
  const rows = c.posts.map(p => `<tr><th scope="row">${e(p.title)}<span class="sub">${e(p.channel)} · day ${p.day}</span></th><td data-label="Due">${e(day(p.due))}</td>
      <td data-label="Status"><span class="tag ${p.status === "posted" ? "ok" : ""}">${e(p.status)}</span>${p.posted_url ? `<span class="sub">${ext(p.posted_url, "the post")}</span>` : ""}</td>
      <td data-label="Text"><span class="sub">${e(p.body || "")}</span></td></tr>`).join("");
  return `${rows ? `<div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">Post</th><th scope="col">Due</th><th scope="col">Status</th><th scope="col">Text</th></tr></thead><tbody>${rows}</tbody></table></div>` : `<p class="muted">No posting tasks in this campaign.</p>`}
    ${held.length ? `<ul class="flags" aria-label="Held by the claims check">${held.map(h => `<li>${e(h.task)}: ${e(h.flags.join("; "))}</li>`).join("")}</ul>` : ""}
    <div class="row section"><button type="button" class="btn" data-action="make-tasks">Make posting tasks</button><p class="help">Each becomes a task with its text and a tracked link; mark it posted with its URL on the <a href="#calendar">Content calendar</a>. Safe to run again.</p></div>`;
}

function report(r) {
  const counts = r.data.counts.map(x => `<div class="stat"><dt>${e(x.label)}</dt><dd>${x.n}<span class="sub">${x.of != null ? `of ${x.of} ${e(x.of_what || "")}` : e(x.of_what || "")}</span></dd></div>`).join("");
  return `<p class="muted">Window ${e(day(r.data.window_from))} to ${e(day(r.data.window_to))}${r.data.window_still_open ? ", still open" : ""}.</p>
    <dl class="stats">${counts}</dl>
    ${r.data.cost_per_success_event ? `<p>Cost per success event: <strong>${e(r.data.cost_per_success_event)}</strong></p>` : `<p class="help">No cost per success event: it is only shown when there are events.</p>`}
    ${r.data.not_measured?.length ? `<details><summary>What is not measured (${r.data.not_measured.length})</summary><ul class="plain-list help">${r.data.not_measured.map(x => `<li>${e(x)}</li>`).join("")}</ul></details>` : ""}`;
}

function detailRender(d) {
  const c = d.campaign;
  const st = c.status;
  const pct = st.percent_of_cap != null ? `${st.percent_of_cap}% of the cap` : "no cap";
  const facts = `<dl class="kv"><dt>Goal</dt><dd>${e(c.goal)}</dd><dt>Audience</dt><dd>${e(c.audience)}</dd><dt>Offer</dt><dd>${e(c.offer)}</dd>
    <dt>Landing page</dt><dd>${ext(c.landing)}</dd><dt>Success event</dt><dd>${e(c.success_event)}</dd><dt>Channels</dt><dd>${e((c.channels || []).join(", "))}</dd><dt>Stop rule</dt><dd>${e(c.stop_rule || "none")}</dd></dl>`;
  const stats = `<dl class="stats">
    <div class="stat"><dt>Spent</dt><dd>${money(st.spend.total, st.settings.currency)}<span class="sub">of ${money(st.settings.budget_cap, st.settings.currency)} (${pct})</span></dd></div>
    <div class="stat"><dt>Gate checks</dt><dd>${st.gate.filter(x => x.checked).length}<span class="sub">of ${st.gate.length} done</span></dd></div>
    <div class="stat"><dt>Success events</dt><dd>${st.events.success}<span class="sub">${e(c.success_event)} tagged campaign=${e(c.code)}</span></dd></div>
    <div class="stat"><dt>Dates</dt><dd class="small">${e(day(st.settings.start))}<span class="sub">${st.settings.end ? `to ${e(day(st.settings.end))}` : "no end date"} · ${st.active ? "running" : "not running"}</span></dd></div></dl>`;
  const warn = (st.warnings || []).length ? st.warnings.map(w => `<p class="notice warn">${e(w)}</p>`).join("") : "";
  const renderer = c.renderer.configured ? "" : `<p class="notice" id="renderer-note">The renderer is not running, so creatives show as a live preview of the template. To make PNG and PDF files: ${e(c.renderer.how_to_enable)}</p>`;
  return `${pageHead(c.label, "Budget, dates and stop rule are edited here with their history kept; the gate is the checklist to finish before spending; creatives are approved exactly as shown.")}
    <p class="item-meta">${e(c.code)} · ${e(d.product)} · file campaigns/${e(c.id)}.yaml</p>${warn}${stats}
    <div class="grid-2 section"><div>${sec("c-settings", "Budget, dates and stop rule", settingsForm(c))}</div>
      <div>${sec("c-gate", "Gate: before spending", gateList(c))}${sec("c-about", "About", facts)}</div></div>
    ${sec("c-history", "History", history(d.history), `<p class="muted">Every change, newest first</p>`)}
    ${sec("c-spend", "Spend", spend(c))}
    ${sec("c-creatives", "Creatives", renderer + `<div class="stack">${c.creatives.map(cr => creative(c, cr, d.product)).join("")}</div>
      <div class="row section"><button type="button" class="btn" data-action="export-pack">Export the approved pack…</button><p class="help">Writes the rendered files, copy, alt text and posts to a folder for posting by hand. Refused while anything is unapproved or flagged. Nothing is posted.</p></div>`)}
    ${sec("c-posts", "Posting tasks", posts(c))}
    ${sec("c-report", "Report", report(d.report), `<p class="muted">Counts with what they are out of; zero is zero</p>`)}`;
}

// scaleFrames fits each creative into its column, keeping its real size
// and proportions, and says the scale.
function scaleFrames() {
  for (const box of $$(".creative-frame")) {
    const w = Number(box.dataset.w), h = Number(box.dataset.h);
    const avail = box.parentElement.clientWidth || w;
    const k = Math.min(1, avail / w);
    const inner = box.firstElementChild;
    box.style.width = `${w * k}px`;
    box.style.height = `${h * k}px`;
    if (inner.tagName === "IFRAME") {
      inner.style.width = `${w}px`;
      inner.style.height = `${h}px`;
      inner.style.transform = `scale(${k})`;
    } else {
      inner.style.width = `${w * k}px`;
      inner.style.height = `${h * k}px`;
    }
    box.parentElement.querySelector("[data-scale]").textContent = `(shown at ${Math.round(k * 100)}%)`;
  }
}

const ref = ctx => ({ product: ctx.data.product, campaign: ctx.data.campaign.id });
const findCreative = (ctx, id) => ctx.data.campaign.creatives.find(c => c.id === id);
const say = r => (r.warnings?.length ? ` Warnings: ${r.warnings.join("; ")}` : "");

export const campaignDetail = {
  id: "campaign", title: "Campaign", section: "campaigns",
  load: async params => {
    const args = { product: params.product, campaign: params.id };
    const [campaign, hist, report] = await Promise.all([api("/api/campaign" + query(args)), api("/api/campaign/history" + query(args)), api("/api/campaign/report" + query(args))]);
    return { product: params.product, campaign, history: hist.history, report };
  },
  render: detailRender,
  after: () => { scaleFrames(); window.addEventListener("resize", scaleFrames, { once: true }); },
  crumbs: d => [{ label: "Campaigns", href: "#campaigns" }, { label: d?.campaign.label || "Campaign" }],
  actions: {
    gate: async (el, ctx) => {
      const r = await post("/api/campaign/gate", { ...ref(ctx), item: el.dataset.item, checked: el.checked });
      await ctx.done(`${r.item.checked ? "Checked" : "Unchecked"}: ${r.item.item}`);
    },
    spend: (b, ctx) => openDialog({
      title: "Record spend", description: `In ${e(ctx.data.campaign.status.settings.currency)}. Warnings (near the cap, over it, before the gate, stop rule met) are shown; nothing is stopped by them.`,
      body: `<form class="form" data-form="spend"><div class="form-row">${field("amount", "Amount", { kind: "number", required: true, attrs: 'min="0.01" step="0.01"' })}
        ${field("platform", "Platform", { required: true, help: "e.g. reddit, newsletter" })}${field("date", "Date", { kind: "date", value: new Date().toISOString().slice(0, 10) })}</div>
        <div class="form-row">${field("clicks", "Platform's clicks (optional)", { kind: "number", attrs: 'min="0"' })}${field("impressions", "Impressions (optional)", { kind: "number", attrs: 'min="0"' })}</div>
        ${field("note", "Note", {})}<div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Record</button></div></form>`,
    }),
    "import-spend": () => openDialog({
      title: "Import a spend CSV", wide: true, description: "A header row with date and spend (or cost, amount), and optionally clicks, impressions and platform. Rows for the same platform and day add up; importing the same file again replaces those days.",
      body: `<form class="form" data-form="import-spend">${field("file", "CSV file", { kind: "file", attrs: 'accept=".csv,text/csv"' })}
        ${field("csv", "…or paste CSV", { kind: "textarea", attrs: 'rows="5" placeholder="date,spend,clicks&#10;2026-11-02,25.00,140"' })}
        ${field("platform", "Platform for every row (if the CSV has no platform column)", {})}
        <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Import</button></div></form>`,
    }),
    "edit-creative": (b, ctx) => {
      const cr = findCreative(ctx, b.dataset.id);
      openDialog({
        title: `Edit ${cr.id}`, wide: true, description: "Any real edit withdraws the approval and drops the rendered files; the claims check runs on the new copy.",
        body: `<form class="form" data-form="edit-creative" data-id="${e(cr.id)}">${Object.entries(cr.copy).map(([k, v]) => field(`copy_${k}`, words(k), { kind: "textarea", value: v, attrs: 'rows="2"' })).join("")}
          ${field("alt", "Alt text (describes the image for people who can't see it)", { kind: "textarea", value: cr.alt, attrs: 'rows="2"' })}
          <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Save</button></div></form>`,
      });
    },
    "render-creative": async (b, ctx) => {
      const r = await post("/api/campaign/creatives/render", { ...ref(ctx), creative: b.dataset.id });
      await ctx.done(r.note);
    },
    "approve-creative": async (b, ctx) => {
      await post("/api/campaign/creatives/approve", { ...ref(ctx), creative: b.dataset.id });
      await ctx.done(`Approved ${b.dataset.id} exactly as shown. Any later edit withdraws it.`);
    },
    "export-pack": async (b, ctx) => {
      const r = await post("/api/campaign/export", ref(ctx));
      openDialog({ title: "Pack exported", description: e(r.note), body: `<p>Folder: <code class="path">${e(r.dir)}</code></p><ul class="plain-list">${r.files.map(f => `<li><code class="path">${e(f)}</code></li>`).join("")}</ul>
        <div class="form-actions"><button type="button" class="btn btn-primary" data-close>Done</button></div>` });
    },
    "make-tasks": async (b, ctx) => {
      const r = await post("/api/campaign/tasks", ref(ctx));
      await ctx.done(`Posting tasks: ${r.created?.length ?? 0} made, ${r.already_made} already there${r.held_by_the_claims_check?.length ? `, ${r.held_by_the_claims_check.length} held by the claims check` : ""}.`);
    },
  },
  forms: {
    "campaign-set": async (f, ctx) => {
      const v = formData(f);
      const st = ctx.data.campaign.status.settings;
      const when = st.stop_rule_when || {};
      const ch = {};
      if (Number(v.budget_cap).toFixed(2) !== Number(st.budget_cap).toFixed(2)) ch.budget_cap = Number(v.budget_cap);
      if (v.currency !== st.currency) ch.currency = v.currency;
      if (v.start !== st.start) ch.start = v.start;
      if (v.end !== (st.end || "")) ch.end = v.end;
      if (v.stop_rule_text !== (st.stop_rule_text || "")) ch.stop_rule_text = v.stop_rule_text;
      if (v.stop_spend_pct === "" && when.spend_pct != null) ch.clear_stop_rule = true;
      else if (v.stop_spend_pct !== "" && (Number(v.stop_spend_pct) !== when.spend_pct || Number(v.stop_max_events || 0) !== (when.max_events || 0))) {
        ch.stop_spend_pct = Number(v.stop_spend_pct);
        ch.stop_max_events = Number(v.stop_max_events || 0);
      }
      if (!Object.keys(ch).length) throw new Error("Nothing changed.");
      const r = await post("/api/campaign/set", { ...ref(ctx), ...ch, note: v.note });
      await ctx.done(`Saved ${plural(r.changed.length, "change")}.${say(r)}`, r.warnings?.length ? "error" : "ok");
    },
    spend: async (f, ctx) => {
      const v = formData(f);
      const body = { ...ref(ctx), amount: Number(v.amount), platform: v.platform, date: v.date, note: v.note };
      if (v.clicks !== "") body.clicks = Number(v.clicks);
      if (v.impressions !== "") body.impressions = Number(v.impressions);
      const r = await post("/api/campaign/spend", body);
      if (!r.warnings?.length) { await ctx.done("Spend recorded."); return; }
      await ctx.done("");
      openDialog({ title: "Spend recorded, with warnings", description: "Nothing was stopped or changed by these; they are for you to act on.",
        body: `<ul class="flags">${r.warnings.map(w => `<li>${e(w)}</li>`).join("")}</ul><div class="form-actions"><button type="button" class="btn btn-primary" data-close>OK</button></div>` });
    },
    "import-spend": async (f, ctx) => {
      const v = formData(f);
      const file = f.querySelector("input[type=file]").files[0];
      const csv = file ? await file.text() : v.csv;
      if (!csv.trim()) throw new Error("Choose a CSV file or paste CSV text.");
      const r = await post("/api/campaign/spend/import", { ...ref(ctx), csv, platform: v.platform });
      await ctx.done("");
      openDialog({ title: "Spend imported", description: `${r.recorded} of ${r.rows} rows recorded; total ${e(r.total)}.`,
        body: `${r.warnings?.length ? `<h3>Warnings</h3><ul class="flags">${r.warnings.map(w => `<li>${e(w)}</li>`).join("")}</ul>` : ""}
          ${r.skipped?.length ? `<h3>Skipped rows</h3><ul class="flags">${r.skipped.map(w => `<li>${e(w)}</li>`).join("")}</ul>` : ""}
          <div class="form-actions"><button type="button" class="btn btn-primary" data-close>OK</button></div>` });
    },
    "edit-creative": async (f, ctx) => {
      const v = formData(f);
      const cr = findCreative(ctx, f.dataset.id);
      const copy = {};
      for (const k of Object.keys(cr.copy)) if (v[`copy_${k}`] !== cr.copy[k]) copy[k] = v[`copy_${k}`];
      const body = { ...ref(ctx), creative: cr.id };
      if (Object.keys(copy).length) body.copy = copy;
      if (v.alt !== cr.alt) body.alt = v.alt;
      const r = await post("/api/campaign/creatives/edit", body);
      await ctx.done(r.flags?.length ? `Saved; the claims check flags: ${r.flags.join("; ")}` : "Saved. Approve it again when it looks right.", r.flags?.length ? "error" : "ok");
    },
  },
};
