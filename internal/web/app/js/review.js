// Weekly review: one page of counts for an ISO week, the review questions,
// and a keep, change or stop decision for each running experiment and
// campaign. Saving freezes the week's numbers.
import { e, api, post, state, query, pageHead, empty, plural, ago, day, words, field, formData } from "./lib.js";

const sec = (id, title, body, note = "") => `<section class="section" aria-labelledby="${id}"><div class="section-head"><h2 id="${id}">${title}</h2>${note}</div>${body}</section>`;
const stat = (label, n, sub) => `<div class="stat"><dt>${label}</dt><dd>${n}<span class="sub">${sub}</span></dd></div>`;
const of = (n, total, what = "") => `${n} of ${total}${what ? " " + what : ""}`;

function tallyList(list, total, what) {
  if (!list?.length) return `<p class="muted">None.</p>`;
  return `<ul class="tally">${list.map(t => `<li><span>${e(words(t.name || t.answer || t.source || t.stage))}</span><span class="num">${of(t.count ?? t.requests, total, what)}</span></li>`).join("")}</ul>`;
}

function reach(s) {
  const parts = [];
  const counters = s.public_counters?.repos || [];
  for (const r of counters) {
    const d = r.downloads_total;
    parts.push(stat(`Release downloads, ${e(r.repo)}`, d.last_7_days ?? "—", `in the last 7 days; ${d.total} in all (downloads, not installs or people)`));
    if (r.stars) parts.push(stat("Stars", r.stars.last_7_days ?? "—", `new in 7 days; ${r.stars.total} in all`));
  }
  const u = s.updates_list;
  if (u) parts.push(stat("Updates list", u.confirmed, `confirmed; ${u.new_confirmed} new this week, ${u.pending} waiting to confirm, ${u.unsubscribed} unsubscribed`));
  const m = s.mentions;
  parts.push(stat("Mentions", m.total, `${m.by_tone.positive || 0} positive, ${m.by_tone.mixed || 0} mixed, ${m.by_tone.negative || 0} negative, ${m.by_tone.unknown || 0} not rated; ${m.asking_a_question} ask a question`));
  const q = s.requests_by_reported_source;
  parts.push(stat("Form requests", q.total, `${q.unanswered} of ${q.total} not answered yet`));
  parts.push(stat("New signals", s.new_signals.total, "this week"));
  return `<dl class="stats">${parts.join("")}</dl>
    <div class="grid-2 section"><div><h3>Where requesters say they came from</h3>${tallyList((q.sources || []).map(x => ({ name: x.source, count: x.requests })), q.total, "requests")}</div>
      <div><h3>New signals by type</h3>${tallyList(s.new_signals.by_type, s.new_signals.total)}</div></div>`;
}

function users(u) {
  const rows = u.stages.map(s => `<tr><th scope="row">${e(words(s.stage))}${s.activation ? ` <span class="tag ok">activation</span>` : ""}</th><td class="num" data-label="This week">${s.entered_this_week}</td><td class="num" data-label="Week before">${s.entered_previous_week}</td></tr>`).join("");
  const c = u.four_week_cohort;
  const cohort = c.stages.map(s => `<li><span>${e(words(s.stage))}</span><span class="num">${of(s.reached, s.of_previous_stage, "from the stage before")}</span></li>`).join("");
  const pmf = u.pmf_survey_so_far;
  const a = u.activation;
  return `<div class="grid-2"><div><h3>People entering each stage</h3><div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">Stage</th><th scope="col" class="num">This week</th><th scope="col" class="num">Week before</th></tr></thead><tbody>${rows}</tbody></table></div></div>
    <div><h3>Four-week cohort</h3><p class="muted">${c.people} people who started ${e(day(c.from))} to ${e(day(c.to))}${c.complete ? "" : "; incomplete, the newest are still inside the offer window"}.</p><ul class="tally">${cohort}</ul></div></div>
    <dl class="stats section">${stat("Activated this week", a.activated_this_week, `${a.assisted} assisted, ${a.unassisted} unassisted (stage ${e(words(a.stage))})`)}
      ${pmf ? stat("Fit survey so far", pmf.total_answers, `answers from ${plural(pmf.people, "person", "people")} who used it two weeks or more${pmf.excluded_used_under_two_weeks ? `; ${pmf.excluded_used_under_two_weeks} under two weeks not counted` : ""}`) : ""}
      ${stat("Conversations this week", a.conversation_notes_this_week.total, `${a.conversation_notes_this_week.assisted} assisted, ${a.conversation_notes_this_week.unassisted} unassisted`)}</dl>
    ${pmf?.answers?.length ? `<h3>Fit survey answers</h3>${tallyList(pmf.answers.map(x => ({ name: x.answer, count: x.count })), pmf.total_answers, "answers")}` : ""}`;
}

function accounts(a) {
  if (!a) return "";
  const b = a.board || {};
  const entries = a.entries_this_week.map(x => ({ name: x.stage, count: x.accounts }));
  const totalEntries = entries.reduce((n, x) => n + x.count, 0);
  const medians = a.median_days_in_stage.map(x => `<li><span>${e(words(x.stage))}</span><span class="num">${x.median_days} days (median of ${plural(x.visits, "visit")})</span></li>`).join("");
  const reasons = l => l.length ? tallyList(l.map(x => ({ name: `${x.status}: ${x.reason}`, count: x.accounts })), l.reduce((n, x) => n + x.accounts, 0), "accounts") : `<p class="muted">None.</p>`;
  return sec("r-accounts", "Accounts", `<dl class="stats">${stat("Open accounts", a.open_accounts, "now")}
      ${stat("Stuck", (b.stuck || []).length, `of ${a.open_accounts} open, over ${e(b.stuck_after || "14d")} in one stage`)}
      ${stat("Next action overdue", (b.overdue || []).length, `of ${a.open_accounts} open`)}
      ${stat("Advocates", a.advocates.length, "accounts with someone who speaks for the product")}</dl>
    <div class="grid-2 section"><div><h3>Entered a stage this week</h3>${tallyList(entries, totalEntries, "moves")}</div><div><h3>Time in each stage</h3>${medians ? `<ul class="tally">${medians}</ul>` : `<p class="muted">No completed stages yet.</p>`}</div></div>
    <div class="grid-2 section"><div><h3>Paused or lost this week</h3>${reasons(a.paused_and_lost_this_week)}</div><div><h3>Paused or lost now</h3>${reasons(a.paused_and_lost_now)}</div></div>`, `<p class="muted"><a href="#pipeline">Pipeline</a></p>`);
}

function response(r) {
  const rows = r.by_kind.map(k => `<tr><th scope="row">${e(words(k.kind))}<span class="sub">target ${e(k.target)}</span></th><td class="num" data-label="Arrived">${k.total}</td>
      <td class="num" data-label="Within target">${of(k.within_target, k.total)}</td><td class="num" data-label="Over target">${of(k.over_target, k.total)}</td><td class="num" data-label="Still waiting">${of(k.waiting_inside_target, k.total)}</td></tr>`).join("");
  const slow = r.slowest.map(x => `<li>${e(x.label)} <span class="muted">· ${e(words(x.kind))} · ${Math.round(x.hours)} hours · ${e(x.status)}</span></li>`).join("");
  return `<p class="muted">${e(r.basis)}</p>${rows ? `<div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">Kind</th><th scope="col" class="num">Arrived</th><th scope="col" class="num">Within target</th><th scope="col" class="num">Over target</th><th scope="col" class="num">Waiting, inside target</th></tr></thead><tbody>${rows}</tbody></table></div>` : ""}
    ${slow ? `<h3 class="section">Slowest</h3><ul class="plain-list">${slow}</ul>` : ""}`;
}

function themes(t) {
  const saved = t.saved ? `<p>${e(t.saved.text)}</p>${(t.saved.items || []).map(i => `<div class="card"><strong>${e(i.theme)}</strong>${i.count ? ` <span class="muted">${i.count} mentions</span>` : ""}${(i.quotes || []).map(q => `<p><q>${e(q)}</q></p>`).join("")}${i.note ? `<p class="help">${e(i.note)}</p>` : ""}</div>`).join("")}`
    : `<p class="muted">No themes saved for this week. Ask Claude to run the <code>weekly_themes</code> prompt over the week's conversation notes; it saves them here.</p>`;
  const p = t.pricing_feedback;
  const price = p && p.statements ? `<p class="muted">${plural(p.statements, "pricing statement")} from ${plural(p.people, "person", "people")}. ${e(p.note || "")}</p>
      <div class="grid-2"><div><h3>By offer</h3>${tallyList(p.by_offer, p.statements, "statements")}</div><div><h3>How they would buy</h3>${tallyList(p.by_buying_process, p.statements, "statements")}</div></div>` : `<p class="muted">No pricing statements this week.</p>`;
  return `${saved}<h3 class="section">Pricing feedback</h3>${price}`;
}

function decisions(r, saved) {
  const got = (kind, id) => (saved?.decisions || []).find(d => d.kind === kind && d.id === id) || {};
  const row = (kind, id, label, status) => {
    const d = got(kind, id);
    return `<fieldset class="decision" data-kind="${kind}" data-id="${e(id)}"><legend><strong>${e(label)}</strong> <span class="muted">${status}</span></legend>
      <div class="row">${["keep", "change", "stop"].map(x => `<label class="check"><input type="radio" name="dec-${kind}-${e(id)}" value="${x}"${d.decision === x ? " checked" : ""}> ${x}</label>`).join("")}</div>
      ${field(`note-${kind}-${id}`, "Note", { value: d.note || "" })}</fieldset>`;
  };
  const cs = r.campaigns.map(c => row("campaign", c.id, c.label, `${e(c.spent)} of ${e(c.budget_cap)} ${e(c.currency)} spent · ${e(c.gate)}`)).join("");
  const xs = r.experiments.map(x => row("experiment", x.experiment, x.experiment, `${e(x.verdict)} · ${x.variants.map(v => `${e(v.variant)}: ${of(v.reached_goal, v.sent, "reached the goal")}`).join("; ")}`)).join("");
  return cs || xs ? `${cs}${xs}` : `<p class="muted">No running campaigns or experiments this week.</p>`;
}

function questions(r, saved) {
  return r.questions.map(q => {
    const a = (saved?.answers || []).find(x => x.number === q.number)?.answer ?? q.answer ?? "";
    return field(`q-${q.number}`, `${q.number}. ${q.question}`, { kind: "textarea", value: a, attrs: 'rows="3"' });
  }).join("");
}

function render(d) {
  const head = pageHead("Weekly review", "One page of counts for the week, each with what it is out of, then your answers to the review questions and a keep, change or stop decision for each running campaign and experiment.");
  if (!d.product) return head + empty("No products are loaded", "Add a product first.");
  const r = d.review;
  const saved = d.saved;
  const weeks = [...new Set([d.thisWeek, ...d.list.map(x => x.week)])].sort().reverse();
  const picker = `<form class="toolbar" data-form="review-week">${field("week", "Week", { kind: "select", options: weeks.map(w => `<option${w === r.week ? " selected" : ""}>${e(w)}</option>`).join("") })}<button class="btn">Show</button></form>`;
  const status = saved ? `<p class="notice">Saved ${ago(saved.saved_at)}. The numbers below were frozen when it was first saved (${e(day(r.built_at))}); a past week never drifts.</p>`
    : `<p class="notice warn">Not saved yet: the numbers are live until you save. Window ${e(day(r.window.from))} to ${e(day(r.window.to))}${r.window.partial ? ", week not over" : ""}.</p>`;
  return head + `<p class="item-meta">${e(r.name)} · ${e(r.week)} · ${e(day(r.window.from))} to ${e(day(r.window.to))}${r.window.partial ? " (partial)" : ""} · review day ${e(r.review_day)}</p>${picker}${status}
    ${sec("r-reach", "Reach", reach(r.spread))}
    ${sec("r-users", "Users", users(r.users))}
    ${accounts(r.accounts)}
    ${sec("r-response", "Response times", response(r.response_times))}
    ${sec("r-themes", "What people said", themes(r.themes))}
    <form data-form="review-save" class="form">
      ${sec("r-decide", "Decisions", decisions(r, saved), `<p class="muted">Keep, change or stop</p>`)}
      ${sec("r-questions", "Questions", questions(r, saved))}
      ${saved ? field("refresh", "Rebuild the frozen numbers from today's data", { kind: "checkbox" }) : ""}
      <div class="form-actions"><button class="btn btn-primary" data-hotkey="submit">${saved ? "Save changes" : "Save the review"}</button></div>
    </form>
    ${sec("r-past", "Past weeks", d.list.length ? `<ul class="plain-list">${d.list.map(x => `<li><a href="#review?week=${encodeURIComponent(x.week)}">${e(x.week)}</a> <span class="muted">· ${of(x.answered_questions, x.questions, "questions answered")} · ${plural(x.decisions, "decision")} · saved ${ago(x.saved_at)}</span></li>`).join("")}</ul>` : `<p class="muted">No saved reviews yet.</p>`)}`;
}

function isoWeek(d = new Date()) {
  const t = new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate()));
  const dayNum = t.getUTCDay() || 7;
  t.setUTCDate(t.getUTCDate() + 4 - dayNum);
  const year = t.getUTCFullYear();
  const week = Math.ceil(((t - Date.UTC(year, 0, 1)) / 86400e3 + 1) / 7);
  return `${year}-W${String(week).padStart(2, "0")}`;
}

export default {
  id: "review", title: "Weekly review", icon: "review", nav: "insight",
  load: async params => {
    const p = (state.boot?.products || []).find(x => x.id === state.product) || state.boot?.products[0];
    if (!p) return { product: null };
    const [r, list] = await Promise.all([api("/api/review" + query({ product: p.id, week: params.week })), api("/api/review/saved" + query({ product: p.id }))]);
    const saved = r.saved ? r.saved_review : null;
    return { product: p, review: saved ? saved.review : r.review, saved, list: list.reviews || [], thisWeek: isoWeek() };
  },
  render,
  crumbs: d => [{ label: "Weekly review" }, ...(d?.review ? [{ label: d.review.week }] : [])],
  forms: {
    "review-week": f => { location.hash = "#review" + query({ week: formData(f).week }); },
    "review-save": async (f, ctx) => {
      const v = formData(f);
      const r = ctx.data.review;
      const answers = r.questions.map(q => ({ number: q.number, answer: v[`q-${q.number}`] || "" }));
      const decisions = [...f.querySelectorAll("fieldset.decision")].map(fs => ({ kind: fs.dataset.kind, id: fs.dataset.id,
        decision: fs.querySelector("input[type=radio]:checked")?.value || "", note: v[`note-${fs.dataset.kind}-${fs.dataset.id}`] || "" })).filter(d => d.decision);
      await post("/api/review/save", { product: ctx.data.product.id, week: r.week, answers, decisions, refresh_snapshot: !!v.refresh });
      await ctx.done(`Saved the review for ${r.week}.`);
    },
  },
};
