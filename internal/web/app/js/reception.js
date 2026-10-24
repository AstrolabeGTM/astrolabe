// Reception: public counters from GitHub releases and the log of public
// mentions. Nothing here is about a person.
import { e, api, post, state, query, pageHead, empty, plural, ago, day, ext, productName, productOptions, currentProduct,
  field, formData, openDialog } from "./lib.js";

const TONES = ["positive", "mixed", "negative", "unknown"];
const TONE_TAG = { positive: "ok", mixed: "warn", negative: "bad", unknown: "" };
const DAYS = [7, 30, 90];

function signed(n) { return n == null ? `<span class="muted">no earlier count</span>` : `${n > 0 ? "+" : ""}${n.toLocaleString()}`; }

function changeRow(label, c, w) {
  if (!c) return "";
  return `<tr><th scope="row">${label}</th><td class="num" data-label="Now">${c.total.toLocaleString()}</td>
    <td class="num" data-label="Last 7 days">${signed(c.last_7_days)}${c.counted_since ? `<span class="sub">since ${e(c.counted_since)}</span>` : ""}</td>
    <td class="num" data-label="7 days before">${signed(c.previous_7_days)}</td></tr>`;
}

function counters(reports) {
  const repos = reports.flatMap(r => r.repos.map(x => ({ ...x, report: r })));
  if (!repos.length) {
    return empty("No public counters yet", `A <code>github_release</code> monitor takes one snapshot a day of a repo's release downloads, stars, forks, watchers, open issues and discussions. Add one to <code>monitors.yaml</code> (<code>type: github_release</code>, <code>repo: you/project</code>), reload products in <a href="#settings">Settings</a>, then run it on <a href="#signals">Signals</a>.`);
  }
  return repos.map(x => {
    const w = x.report;
    const assets = x.downloads_by_asset?.length ? `<details><summary>Downloads by release file (${x.downloads_by_asset.length})</summary>
      <div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">Release file</th><th scope="col" class="num">Total</th><th scope="col" class="num">Last 7 days</th><th scope="col" class="num">7 days before</th></tr></thead><tbody>
      ${x.downloads_by_asset.map(a => changeRow(`${e(a.release)} · ${e(a.asset)}`, a)).join("")}</tbody></table></div></details>` : "";
    return `<div class="card counters-card"><div class="section-head"><h3>${ext(`https://github.com/${x.repo}`, x.repo)}</h3>
        <p class="muted">${e(productName(w.product))} · latest snapshot ${e(x.latest_snapshot)}</p></div>
      <div class="table-wrap"><table class="stack-sm"><caption>Last 7 days: ${e(w.last_7_days.from)} to ${e(w.last_7_days.to)}. The 7 days before: ${e(w.previous_7_days.from)} to ${e(w.previous_7_days.to)}.</caption>
        <thead><tr><th scope="col">Count</th><th scope="col" class="num">Now</th><th scope="col" class="num">Last 7 days</th><th scope="col" class="num">7 days before</th></tr></thead>
        <tbody>${changeRow("Release downloads (downloads, not installs or people)", x.downloads_total)}${changeRow("Stars", x.stars)}${changeRow("Forks", x.forks)}
          ${changeRow("Watchers", x.watchers)}${changeRow("Open issues and pull requests", x.open_issues)}${changeRow("Discussions", x.discussions)}</tbody></table></div>
      ${assets}</div>`;
  }).join("") + `<ul class="help">${[...new Set(reports.flatMap(r => r.notes))].map(n => `<li>${e(n)}</li>`).join("")}</ul>`;
}

function log(reports, days) {
  const all = reports.flatMap(r => r.mentions);
  const total = reports.reduce((n, r) => n + r.total, 0);
  const tone = t => reports.reduce((n, r) => n + (r.by_tone[t] || 0), 0);
  const questions = reports.reduce((n, r) => n + r.asking_a_question, 0);
  const w = reports[0]?.window;
  const filter = `<form class="toolbar" data-form="reception-window" aria-label="Window">
    ${field("days", "Window", { kind: "select", options: DAYS.map(n => `<option value="${n}"${n === days ? " selected" : ""}>Last ${n} days</option>`).join("") })}
    <button class="btn">Show</button></form>`;
  if (!total) {
    return filter + empty(`No mentions in the last ${days} days`, `Log one by hand (top right), or add <code>reception: true</code> to a <code>github_search</code>, <code>github_repo</code> or <code>community</code> monitor so its results are filed here too.`);
  }
  const stats = `<dl class="stats" aria-label="Mentions in the window">
    <div class="stat"><dt>Mentions</dt><dd>${total}<span class="sub">${w ? `${e(w.from)} to ${e(w.to)}` : ""}</span></dd></div>
    ${["positive", "mixed", "negative"].map(t => `<div class="stat"><dt>${t[0].toUpperCase() + t.slice(1)}</dt><dd>${tone(t)}<span class="sub">of ${total} mentions</span></dd></div>`).join("")}
    <div class="stat"><dt>Not rated yet</dt><dd>${tone("unknown")}<span class="sub">of ${total}; tone is never guessed</span></dd></div>
    <div class="stat"><dt>Ask a question</dt><dd>${questions}<span class="sub">of ${total} mentions</span></dd></div></dl>`;
  const rows = all.map(m => `<tr data-key="mention:${e(m.id)}">
      <td data-label="Date" class="nowrap">${e(day(m.date))}</td>
      <td data-label="Mention">${ext(m.url, m.title || m.url)}<span class="sub">${e(m.source)} · ${m.found_by === "manual" ? "logged by hand" : `found by ${e(m.found_by.replace(/^monitor:/, "monitor "))}`}${m.asks_question ? " · asks a question" : ""}${reports.length > 1 ? ` · ${e(productName(m.product))}` : ""}</span></td>
      <td data-label="Tone"><label class="sr-only" for="tone-${e(m.id)}">Tone of ${e(m.title || m.url)}</label>
        <select id="tone-${e(m.id)}" data-change="set-tone" data-id="${e(m.id)}" class="tone ${TONE_TAG[m.tone]}">${TONES.map(t => `<option value="${t}"${t === m.tone ? " selected" : ""}>${t === "unknown" ? "not rated" : t}</option>`).join("")}</select></td></tr>`).join("");
  const shown = all.length < total ? `<p class="help">Showing the newest ${all.length} of ${total}.</p>` : "";
  return filter + stats + `<div class="table-wrap section"><table class="stack-sm"><thead><tr><th scope="col">Date</th><th scope="col">Mention</th><th scope="col">Tone</th></tr></thead><tbody>${rows}</tbody></table></div>${shown}`;
}

function mentionDialog() {
  const p = currentProduct({ required: true });
  openDialog({
    title: "Log a mention", description: "A public page that talks about the product: a thread, a question, a post, an issue. The same link is kept once.",
    body: `<form class="form" data-form="log-mention">
      ${field("product", "Product", { kind: "select", options: productOptions(p?.id) })}
      ${field("url", "Link", { kind: "url", required: true })}
      ${field("title", "Title or a short excerpt", {})}
      <div class="form-row">${field("source", "Source", { value: "manual", help: "hn, stackoverflow, rss, github, blog, manual…" })}
      ${field("date", "Posted on", { kind: "date", value: new Date().toISOString().slice(0, 10) })}
      ${field("tone", "Tone", { kind: "select", options: TONES.map(t => `<option value="${t}"${t === "unknown" ? " selected" : ""}>${t === "unknown" ? "not rated" : t}</option>`).join("") })}</div>
      ${field("asks_question", "It asks a question", { kind: "checkbox" })}
      <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Log mention</button></div></form>`,
  });
}

export default {
  id: "reception", title: "Reception", icon: "reception", nav: "work",
  load: async params => {
    const days = Number(params.days) || 7;
    const [c, r] = await Promise.all([api("/api/counters" + query({ product: state.product })), api("/api/reception" + query({ product: state.product, days }))]);
    return { counters: c.counters, reception: r.reception, days };
  },
  render: d => pageHead("Reception", "How the product is received in public: daily counts from GitHub releases and a log of public mentions with their tone. Downloads count downloads of a file, not installs or people. Nothing here is about a person.")
    + `<section class="section" aria-labelledby="s-counters"><div class="section-head"><h2 id="s-counters">Public counters</h2><p class="muted">Change over the last 7 days against the 7 before</p></div>${counters(d.counters)}</section>`
    + `<section class="section" aria-labelledby="s-log"><div class="section-head"><h2 id="s-log">Reception log</h2><p class="muted">Tone is set by you, never guessed</p></div>${log(d.reception, d.days)}</section>`,
  pageActions: () => state.boot?.products.length ? `<button type="button" class="btn btn-sm" data-action="log-mention">Log a mention</button>` : "",
  actions: {
    "log-mention": () => mentionDialog(),
    "set-tone": async (el, ctx) => ctx.done((await post(`/api/mentions/${el.dataset.id}/tone`, { tone: el.value })).status),
  },
  forms: {
    "reception-window": f => { const v = formData(f); location.hash = "#reception" + query({ days: v.days === "7" ? "" : v.days }); },
    "log-mention": async (f, ctx) => {
      const v = formData(f);
      await ctx.done((await post("/api/mentions", { product: v.product, url: v.url, title: v.title, source: v.source, date: v.date, tone: v.tone, asks_question: v.asks_question })).status);
    },
  },
};
