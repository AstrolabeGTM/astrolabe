// Conversation notes: the "Log a conversation" form (person and account
// pages) and how a logged note is shown. It calls the MCP tool
// log_conversation.
import { e, post, state, day, words, field, formData, openDialog, productOptions } from "./lib.js";

const CHANNELS = ["call", "email", "chat", "in_person", "other"];
const PMF = { very_disappointed: "Very disappointed", somewhat_disappointed: "Somewhat disappointed", not_disappointed: "Not disappointed" };

// conversationDialog opens the form. who: the person (id or email) and a
// name, or people to pick from; account: {id, company, checks, record} when
// it is about an account.
export function conversationDialog({ product, person, personName, people = [], account = null }) {
  const today = new Date().toISOString().slice(0, 10);
  const whoField = person
    ? `<input type="hidden" name="person" value="${e(person)}"><p class="help">With <strong>${e(personName || person)}</strong>.</p>`
    : field("person", "With", { kind: "select", required: true, options: people.map(p => `<option value="${e(p.person_id)}">${e(p.name || p.email)}${p.role ? ` (${e(p.role)})` : ""}</option>`).join(""),
        help: "Add people to the account first if they are not listed." });
  const checks = account ? [...account.checks.map(c => [c, true]), ...(account.record || []).map(c => [c, false])] : [];
  const qual = account && checks.length ? `<details><summary>Qualification answers (optional)</summary>
      <p class="help">Applied to ${e(account.company)} with this conversation as context, like answering the checks on the account page. Evidence is required for yes and no.</p>
      ${checks.map(([c, gating], i) => `<div class="form-row">${field(`q_answer_${i}`, `${c}${gating ? "" : " (recorded only)"}`, { kind: "select",
        options: `<option value="">No change</option><option value="yes">Yes</option><option value="no">No</option><option value="unknown">Not known</option>`, attrs: `data-check="${e(c)}"` })}
        ${field(`q_evidence_${i}`, `Evidence for ${c}`, {})}</div>`).join("")}</details>` : "";
  openDialog({
    title: "Log a conversation", wide: true,
    description: "A call, email thread or chat. Quote people word for word; their words become the voice of the product. All or nothing: if anything is refused, nothing is saved.",
    body: `<form class="form" data-form="log-conversation">
      ${product ? `<input type="hidden" name="product" value="${e(product)}">` : field("product", "Product", { kind: "select", options: productOptions(state.product) })}
      ${account ? `<input type="hidden" name="account" value="${e(account.id)}">` : ""}
      ${whoField}
      <div class="form-row">${field("date", "Date", { kind: "date", value: today })}
        ${field("channel", "Channel", { kind: "select", options: CHANNELS.map(c => `<option value="${c}">${e(words(c))}</option>`).join("") })}
        ${field("duration_minutes", "Minutes (optional)", { kind: "number", attrs: 'min="0"' })}</div>
      ${field("assisted", "Assisted: we coached them through using the product (counted apart, never as unassisted first use)", { kind: "checkbox" })}
      ${account ? "" : field("account", "Account (optional)", { help: "Company domain or account id, if this was about an account in the pipeline." })}
      ${field("summary", "Summary", { kind: "textarea", attrs: 'rows="3"' })}
      ${field("their_words", "Their words", { kind: "textarea", attrs: 'rows="3"', help: "Exact quotes, word for word." })}
      <div class="form-row">${field("problem", "Problem", { kind: "textarea", attrs: 'rows="2"' })}
        ${field("current_approach", "How they do it today", { kind: "textarea", attrs: 'rows="2"' })}</div>
      ${field("next_step", "Next step they agreed", { kind: "textarea", attrs: 'rows="2"' })}
      ${qual}
      <details><summary>Next action (optional)</summary>
        <p class="help">${account ? `Replaces ${e(account.company)}'s one next action.` : "Becomes a task for this person (or the account's next action if you name one)."}</p>
        <div class="form-row">${field("next_action", "Next action", {})}${field("due", "Due", { kind: "date", value: today })}</div></details>
      <details><summary>Product-market fit answer (optional)</summary>
        <p class="help">“How would you feel if you could no longer use the product?” Ask only people who used it for two weeks or more.</p>
        ${field("pmf_answer", "Answer", { kind: "select", options: `<option value="">Not asked</option>` + Object.entries(PMF).map(([k, v]) => `<option value="${k}">${v}</option>`).join("") })}
        ${field("pmf_used", "They used it for two weeks or more", { kind: "checkbox" })}
        <div class="form-row">${field("pmf_alternative", "What they would use instead", {})}${field("pmf_who_else", "Who else would benefit", {})}</div></details>
      <details><summary>Pricing feedback (optional)</summary>
        <p class="help">What they said about price. Statements, not commitments.</p>
        <div class="form-row">${field("price_offer", "Offer named", { help: "e.g. Team plan $29/month" })}${field("price_replaces", "What it would replace", {})}</div>
        <div class="form-row">${field("price_approver", "Who approves", {})}
          ${field("price_process", "How they buy", { kind: "select", options: `<option value="">Not said</option><option value="card">Card</option><option value="invoice">Invoice</option><option value="procurement">Procurement</option>` })}</div>
        ${field("price_reaction", "Their reaction, in their words", {})}</details>
      <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary" data-hotkey="submit">Save conversation</button></div></form>`,
  });
}

// submit turns the form into log_conversation's arguments.
export async function submitConversation(f) {
  const v = formData(f);
  const body = { product: v.product, person: String(v.person), channel: v.channel, date: v.date, assisted: !!v.assisted,
    summary: v.summary, their_words: v.their_words, problem: v.problem, current_approach: v.current_approach, next_step: v.next_step };
  if (v.account) body.account = String(v.account);
  if (v.duration_minutes) body.duration_minutes = Number(v.duration_minutes);
  const answers = [...f.querySelectorAll("select[data-check]")].map((sel, i) => ({ check: sel.dataset.check, answer: sel.value, evidence: v[`q_evidence_${i}`] || "" }))
    .filter(a => a.answer);
  if (answers.length) body.qualification = answers;
  if (v.next_action) Object.assign(body, { next_action: v.next_action, due: v.due });
  if (v.pmf_answer) body.pmf_survey = { answer: v.pmf_answer, used_two_weeks: !!v.pmf_used, alternative: v.pmf_alternative, who_else: v.pmf_who_else };
  if (v.price_offer || v.price_reaction) {
    body.pricing_feedback = [{ offer: v.price_offer, replaces: v.price_replaces, approver: v.price_approver, buying_process: v.price_process, reaction: v.price_reaction }];
  }
  return post("/api/conversations", body);
}

// notesList shows conversation notes, newest first.
export function notesList(list, { showPerson = true, showCompany = false } = {}) {
  if (!list?.length) return `<p class="muted">No conversations logged yet.</p>`;
  const assisted = list.filter(n => n.assisted).length;
  return `<p class="help">${list.length} conversation${list.length === 1 ? "" : "s"}: ${assisted} assisted, ${list.length - assisted} unassisted.</p>
    <div class="stack">${list.map(n => {
      const rows = [["Summary", n.summary], ["Their words", n.their_words], ["Problem", n.problem], ["How they do it today", n.current_approach], ["Next step", n.next_step]]
        .filter(([, x]) => x).map(([k, x]) => `<dt>${k}</dt><dd>${k === "Their words" ? `<q>${e(x)}</q>` : e(x)}</dd>`).join("");
      const pmf = n.pmf_survey ? `<dt>Fit answer</dt><dd>${e(PMF[n.pmf_survey.answer] || n.pmf_survey.answer)}${n.pmf_survey.used_two_weeks ? "" : " (used under two weeks: not counted)"}${n.pmf_survey.alternative ? `; instead: ${e(n.pmf_survey.alternative)}` : ""}</dd>` : "";
      const price = (n.pricing_feedback || []).map(p => `<dt>Pricing</dt><dd>${e([p.offer, p.replaces && `replaces ${p.replaces}`, p.buying_process, p.reaction && `“${p.reaction}”`].filter(Boolean).join(" · "))}</dd>`).join("");
      return `<article class="card note"><div class="row"><strong>${e(day(n.date))}</strong><span class="tag">${e(words(n.channel))}</span>${n.assisted ? `<span class="tag info">assisted</span>` : ""}
        ${showPerson && n.person ? `<a href="#person/${e(n.person_id)}">${e(n.person)}</a>` : ""}${showCompany && n.company ? `<span class="muted">${e(n.company)}</span>` : ""}</div>
        <dl class="kv">${rows}${pmf}${price}</dl></article>`;
    }).join("")}</div>`;
}
