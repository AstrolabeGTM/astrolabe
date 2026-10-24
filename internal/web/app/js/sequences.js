// Starting a sequence, from the People list or a person page. Refusals come
// back with their reasons (already in a sequence, do-not-contact, or the
// outreach guard), and are shown, never hidden.
import { e, post, state, field, formData, openDialog, productOptions, plural, words } from "./lib.js";

function sequenceOptions(productId) {
  const p = state.boot?.products.find(x => x.id === productId);
  // Cold sequences (for new people) first: that is what the People list is for.
  const list = [...(p?.sequences || [])].sort((a, b) => (a.kind === "users") - (b.kind === "users") || a.id.localeCompare(b.id));
  return list.map(s =>
    `<option value="${e(s.id)}">${e(s.id)} · ${e(s.kind === "users" ? "users" : "cold")} · ${plural(s.steps, "step")} (${e(s.channels.map(words).join(", "))})</option>`).join("");
}

let names = {};

// startSequenceDialog asks which sequence to start for the given people;
// names (id to name) lets refusals name people rather than number them.
export function startSequenceDialog(personIds, { productId, who, names: known = {} }) {
  const ids = personIds.map(Number);
  names = known;
  openDialog({
    title: "Start a sequence",
    description: `For ${e(who)}. A sequence is a series of timed message steps; the first draft appears on Today for you to approve. Nothing is sent without your approval.`,
    body: `<form class="form" data-form="start-sequence" data-ids="${e(ids.join(","))}">
      ${field("product", "Product", { kind: "select", options: productOptions(productId), attrs: 'data-change="sequence-product"' })}
      ${field("sequence", "Sequence", { kind: "select", options: sequenceOptions(productId), required: true,
        help: "cold: for people who don't know you yet (counts toward the daily limit). users: for people already using the product." })}
      <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Start</button></div></form>`,
  });
}

// showRefusals lists who was not started, and why.
export function showRefusals(result) {
  openDialog({
    title: result.started ? "Started, with some refused" : "Not started",
    description: e(result.status),
    body: `<ul class="flags">${result.skipped.map(s => `<li>${e(s.replace(/^person (\d+):/, (m, id) => names[id] ? `${names[id]}:` : m))}</li>`).join("")}</ul>
      <p class="help">People known only from GitHub are never emailed (GitHub's Acceptable Use Policies forbid using GitHub data for unsolicited email). Help them in the issue thread; once they give you an address (a form, a reply, your import), email is allowed.</p>
      <div class="form-actions"><button type="button" class="btn btn-primary" data-close>OK</button></div>`,
  });
}

export const actions = {
  "sequence-product": el => {
    const sel = el.form.querySelector("select[name=sequence]");
    sel.innerHTML = sequenceOptions(el.value);
  },
};

export const forms = {
  "start-sequence": async (f, ctx) => {
    const v = formData(f);
    const r = await post("/api/sequences/start", { product: v.product, sequence: v.sequence, person_ids: f.dataset.ids.split(",").map(Number) });
    if (r.skipped.length) {
      await ctx.done("");
      showRefusals(r);
    } else {
      await ctx.done(r.status);
    }
  },
};
