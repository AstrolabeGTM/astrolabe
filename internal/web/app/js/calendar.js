// Content calendar: drafts and posts by the day they are due, with
// "mark posted with its URL".
import { e, api, post, state, query, pageHead, empty, plural, ago, day, words, ext, field, formData, openDialog, productOptions, currentProduct } from "./lib.js";

const PLATFORMS = ["x", "linkedin", "hn", "reddit", "newsletter", "producthunt", "directories"];
const iso = d => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
function shift(ymd, days) { const [y, m, d] = ymd.split("-").map(Number); return iso(new Date(y, m - 1, d + days)); }

function entry(x) {
  const status = x.status === "posted" ? `<span class="tag ok">posted</span>` : x.status === "skipped" ? `<span class="tag">skipped</span>` : x.overdue ? `<span class="tag bad">overdue</span>` : `<span class="tag info">to post</span>`;
  return `<li class="deal" data-key="content:${e(x.id)}"><div class="row">${status}<strong>${e(words(x.platform))}</strong><span class="muted">${e(x.campaign ? `campaign ${x.campaign}` : x.batch)}</span></div>
    ${x.task ? `<p class="deal-next">${e(x.task)}</p>` : ""}
    <details><summary>The text</summary><pre class="message">${e(x.body)}</pre></details>
    <p class="deal-meta">${x.posted_url ? `${ext(x.posted_url, "the post")} · ${plural(x.clicks, "click")} on its tracked link · posted ${ago(x.posted_at)}` : x.link ? `tracked link ${ext(x.link)}` : "no tracked link"}${x.has_due_date ? "" : " · no due date (made that day)"}</p>
    ${x.status === "draft" ? `<div class="deal-actions"><button type="button" class="btn btn-sm btn-primary" data-action="posted" data-id="${e(x.id)}">Mark posted…</button><button type="button" class="btn btn-sm btn-ghost" data-action="skip" data-id="${e(x.id)}">Skip</button></div>` : ""}</li>`;
}

function render(d) {
  const c = d.cal;
  const head = pageHead("Content calendar", "Posts you publish by hand (launch posts, campaign posting tasks, release-note drafts) on the day they are due. Mark each posted with its URL so its clicks count.");
  const nav = `<div class="row toolbar"><a class="btn btn-sm" href="#calendar?from=${shift(c.from, -28)}">← Earlier</a><a class="btn btn-sm" href="#calendar">Now</a><a class="btn btn-sm" href="#calendar?from=${shift(c.from, 28)}">Later →</a>
    <p class="muted">${e(day(c.from))} to ${e(day(c.to))}: ${plural(c.total, "item")}, ${c.posted} of ${c.total} posted, ${c.skipped} skipped, ${c.overdue} overdue.</p></div>`;
  if (!c.entries.length) return head + nav + empty("Nothing in these four weeks", `Campaigns make posting tasks (<a href="#campaigns">Campaigns</a> → Make posting tasks); release notes become drafts with “Draft posts from text”${state.boot?.ai_drafting ? "" : " (needs the Claude API)"}.`);
  const today = iso(new Date());
  const days = [...new Set(c.entries.map(x => x.day))];
  return head + nav + days.map(dy => `<section class="section" aria-labelledby="d-${dy}"><div class="section-head"><h2 id="d-${dy}">${e(day(dy))}${dy === today ? ` <span class="tag info">today</span>` : ""}</h2></div>
    <ul class="deals">${c.entries.filter(x => x.day === dy).map(entry).join("")}</ul></section>`).join("");
}

export default {
  id: "calendar", title: "Content calendar", icon: "calendar", nav: "work",
  load: async params => ({ cal: await api("/api/content/calendar" + query({ product: state.product, from: params.from, days: 28 })) }),
  render,
  pageActions: () => `<button type="button" class="btn btn-sm" data-action="draft">Draft posts from text</button>`,
  actions: {
    posted: b => openDialog({ title: "Mark posted", description: "Where it went live. Its posting task closes, and clicks on its tracked link count toward it.",
      body: `<form class="form" data-form="posted" data-id="${e(b.dataset.id)}">${field("url", "URL of the post", { kind: "url", required: true })}
        <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Mark posted</button></div></form>` }),
    skip: async (b, ctx) => { await post("/api/content/mark", { id: Number(b.dataset.id), status: "skipped" }); await ctx.done("Skipped; its task is closed."); },
    draft: () => openDialog({ title: "Draft posts from text", wide: true,
      description: `Release notes, a changelog or a blog post become a draft per platform in the product's voice, each with a tracked link. Nothing is posted.${state.boot?.ai_drafting ? "" : " This needs the Claude API (ASTROLABE_ANTHROPIC_API_KEY)."}`,
      body: `<form class="form" data-form="draft">${field("product", "Product", { kind: "select", options: productOptions(currentProduct({ required: true })?.id) })}
        ${field("batch", "What it is", { required: true, help: "e.g. v0.4 release" })}${field("source", "The text", { kind: "textarea", required: true, attrs: 'rows="8"' })}
        <fieldset class="field"><legend class="label">Platforms</legend><div class="row">${PLATFORMS.map(p => `<label class="check"><input type="checkbox" name="pl_${p}"${["x", "linkedin", "hn", "newsletter"].includes(p) ? " checked" : ""}> ${e(words(p))}</label>`).join("")}</div></fieldset>
        ${field("launch", "Also make a posting task for each (due today)", { kind: "checkbox" })}
        <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Draft</button></div></form>` }),
  },
  forms: {
    posted: async (f, ctx) => { await post("/api/content/mark", { id: Number(f.dataset.id), status: "posted", url: formData(f).url }); await ctx.done("Marked posted."); },
    draft: async (f, ctx) => {
      const v = formData(f);
      const platforms = PLATFORMS.filter(p => v[`pl_${p}`]);
      const r = await post("/api/content", { product: v.product, batch: v.batch, source: v.source, platforms, launch: !!v.launch });
      await ctx.done(`Drafted ${plural(r.created.length, "post")}.`);
    },
  },
};
