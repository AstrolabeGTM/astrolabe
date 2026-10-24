// Signals: recent events about people, the monitors that find them, and
// matches that need a person picked.
import { e, api, post, state, query, pageHead, empty, plural, ago, day, ext, productName, productOptions,
  currentProduct, field, formData, openDialog, words } from "./lib.js";

const DAYS = [7, 14, 30, 90];

function reviews(d) {
  if (!d.reviews.total) return "";
  const items = d.reviews.reviews.map(r => {
    const subject = Object.entries(r.subject || {}).filter(([, v]) => v).map(([k, v]) => `${words(k)} ${v}`).join(", ");
    const pick = r.candidates.map(c => `<button type="button" class="btn btn-sm" data-action="resolve-review" data-id="${e(r.review_id)}" data-person="${e(c.id)}">It's ${e(c.label)}</button>`).join("");
    return `<article class="item near" data-key="review:${e(r.review_id)}" aria-labelledby="rv-${e(r.review_id)}">
      <div class="item-head"><span class="tag warn">Ambiguous</span><h3 id="rv-${e(r.review_id)}">${r.url ? ext(r.url, r.title || r.type) : e(r.title || r.type)}</h3><span class="timing ok">${ago(r.occurred_at)}</span></div>
      <p class="item-meta">${e(productName(r.product))} · ${e(r.type)} · identifies them as ${e(subject || "unknown")}</p>
      <p class="help">These details belong to ${plural(r.candidates.length, "different person", "different people")}. Nothing is guessed: pick who it is about, or dismiss it.</p>
      <div class="item-actions" role="group" aria-label="Who is it about?">${pick}
        <button type="button" class="btn btn-sm btn-danger" data-action="resolve-review" data-id="${e(r.review_id)}" data-person="0">Dismiss</button></div></article>`;
  }).join("");
  return `<section class="section" aria-labelledby="s-reviews"><div class="section-head"><h2 id="s-reviews">Ambiguous matches <span class="muted num">(${d.reviews.total})</span></h2>
    <p class="muted">Signals that point at more than one person</p></div><div class="items">${items}</div></section>`;
}

function monitors(d) {
  const list = d.monitors.monitors;
  const body = !list.length
    ? empty("No monitors yet", `Monitors look for signals on a schedule: GitHub searches, your repo's stars and issues, Hacker News, Stack Overflow and RSS keywords, and <code>github_release</code> counters. Add <code>monitors.yaml</code> to a product folder, then reload products in <a href="#settings">Settings</a>.`)
    : `<div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">Monitor</th><th scope="col">Type</th><th scope="col">Last run</th><th scope="col">Result</th><th scope="col">Next run</th><th scope="col"><span class="sr-only">Run</span></th></tr></thead><tbody>
      ${list.map(m => `<tr data-key="monitor:${e(m.product)}/${e(m.monitor)}">
        <td data-label="Monitor"><strong>${e(m.monitor)}</strong><span class="sub">${e(productName(m.product))} · every ${e(m.every || "1h")}</span></td>
        <td data-label="Type">${e(m.type)}${m.reception ? `<span class="sub">also files mentions</span>` : ""}</td>
        <td data-label="Last run">${m.last_run ? ago(m.last_run) : `<span class="muted">never</span>`}</td>
        <td data-label="Result">${m.error ? `<span class="tag bad">Error</span><span class="sub">${e(m.error)}</span>`
          : !m.last_run ? `<span class="muted">—</span>` : m.counters ? "counts recorded" : `${plural(m.last_new_signals, "new signal")}`}</td>
        <td data-label="Next run">${ago(m.next_run)}</td>
        <td><button type="button" class="btn btn-sm" data-action="run-monitor" data-product="${e(m.product)}" data-monitor="${e(m.monitor)}">Run now<span class="sr-only"> ${e(m.monitor)}</span></button></td></tr>`).join("")}
      </tbody></table></div>`;
  return `<section class="section" aria-labelledby="s-monitors"><div class="section-head"><h2 id="s-monitors">Monitors <span class="muted num">(${list.length})</span></h2>
    <p class="muted">From each product's <code>monitors.yaml</code></p></div>${body}</section>`;
}

function feed(d, params) {
  const f = d.signals;
  const days = Number(params.days) || 14;
  const filters = `<form class="toolbar" data-form="signal-filter" aria-label="Filter signals">
    ${field("type", "Type", { kind: "select", options: `<option value="">All types</option>` + f.types.map(t => `<option${t === params.type ? " selected" : ""}>${e(t)}</option>`).join("") })}
    ${field("tier", "Priority", { kind: "select", options: `<option value="">Anyone</option>` + ["A", "B", "C"].map(t => `<option value="${t}"${t === params.tier ? " selected" : ""}>${t} or better</option>`).join("") })}
    ${field("days", "Window", { kind: "select", options: DAYS.map(n => `<option value="${n}"${n === days ? " selected" : ""}>Last ${n} days</option>`).join("") })}
    <button class="btn">Apply</button></form>`;
  const count = `<p class="muted">${f.total > f.signals.length ? `Showing the newest ${f.signals.length} of ${f.total}` : plural(f.total, "signal")} from ${e(day(f.since))} to now${state.product ? ` for ${e(productName(state.product))}` : ""}.</p>`;
  const rows = f.signals.map(s => `<tr>
      <td data-label="When" class="nowrap">${ago(s.occurred_at)}</td>
      <td data-label="Signal">${s.url ? ext(s.url, s.title || s.type) : e(s.title || s.type)}<span class="sub">${e(s.type)}${s.why_now ? " · why now" : ""} · strength ${s.strength}/100 · ${e(productName(s.product))}</span></td>
      <td data-label="About">${s.person_id ? `<a href="#person/${e(s.person_id)}">${e(s.person || `Person ${s.person_id}`)}</a>` : `<span class="muted">no one matched</span>`}${s.company ? `<span class="sub">${e(s.company)}</span>` : ""}</td>
      <td data-label="Priority">${s.tier ? `<span class="tag brass">${e(s.tier)}</span> <span class="num">${s.priority}/100</span>` : `<span class="muted">—</span>`}</td></tr>`).join("");
  const body = f.signals.length
    ? `${count}<div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">When</th><th scope="col">Signal</th><th scope="col">About</th><th scope="col">Priority</th></tr></thead><tbody>${rows}</tbody></table></div>`
    : empty(`No signals in the last ${days} days`, `Monitors add them as they run; product events and Stripe webhooks add them as they arrive. You can also add one by hand (top right), e.g. a referral.`);
  return `<section class="section" aria-labelledby="s-feed"><div class="section-head"><h2 id="s-feed">Recent signals</h2></div>${filters}${body}</section>`;
}

function addDialog() {
  const p = currentProduct({ required: true });
  openDialog({
    title: "Add a signal by hand", description: "Something you learned yourself, like a referral or a conversation. It raises their intent score and can start an automation (for example the demo's <code>referral</code> type starts a sequence).",
    body: `<form class="form" data-form="add-signal">
      ${field("product", "Product", { kind: "select", options: productOptions(p?.id) })}
      ${field("who", "Who it is about", { required: true, help: "An email address or a GitHub login. A new person is created if needed." })}
      <div class="form-row">${field("type", "Type", { required: true, value: "referral", help: "Stored as manual.&lt;type&gt;." })}
      ${field("strength", "Strength (0 to 100)", { kind: "number", value: "30", attrs: 'min="0" max="100"', help: "25 is a strong buying signal." })}</div>
      ${field("why_now", "Time-bound (a launch, an incident, a new hire)", { kind: "checkbox" })}
      ${field("note", "Note", {})}
      ${field("url", "Link (optional)", { kind: "url" })}
      <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Add signal</button></div></form>`,
  });
}

export default {
  id: "signals", title: "Signals", icon: "signals", nav: "work",
  load: async params => {
    const [signals, mons, revs] = await Promise.all([
      api("/api/signals" + query({ product: state.product, type: params.type, tier: params.tier, days: params.days || 14, limit: 200 })),
      api("/api/monitors"), api("/api/reviews")]);
    if (state.product) mons.monitors = mons.monitors.filter(m => m.product === state.product);
    return { signals, monitors: mons, reviews: revs };
  },
  render: (d, params) => pageHead("Signals", "A signal is something a person did (opened an issue, starred a repo, signed up) that raises their intent score; monitors find them on a schedule.")
    + reviews(d) + feed(d, params) + monitors(d),
  pageActions: () => state.boot?.products.length ? `<button type="button" class="btn btn-sm" data-action="add-signal">Add a signal by hand</button>` : "",
  actions: {
    "add-signal": () => addDialog(),
    "run-monitor": async (b, ctx) => ctx.done((await post("/api/monitors/run", { product: b.dataset.product, monitor: b.dataset.monitor })).status),
    "resolve-review": async (b, ctx) => ctx.done((await post(`/api/reviews/${b.dataset.id}/resolve`, { person_id: Number(b.dataset.person) })).status),
  },
  forms: {
    "signal-filter": f => {
      const v = formData(f);
      location.hash = "#signals" + query({ type: v.type, tier: v.tier, days: v.days === "14" ? "" : v.days });
    },
    "add-signal": async (f, ctx) => {
      const v = formData(f);
      await ctx.done((await post("/api/signals", { product: v.product, who: v.who, type: v.type, strength: Number(v.strength) || 0, why_now: v.why_now, note: v.note, url: v.url })).status);
    },
  },
};
