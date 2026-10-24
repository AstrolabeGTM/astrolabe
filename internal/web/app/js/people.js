// People: a product's people, best priority first; start sequences; import.
import { $, $$, e, api, post, state, query, pageHead, empty, plural, productOptions, currentProduct, field, formData,
  openDialog } from "./lib.js";
import { startSequenceDialog, actions as seqActions, forms as seqForms } from "./sequences.js";

const PEOPLE_COLUMNS = "email, github, name, first_name, title, company, company_domain, notes, facts";
const COMPANY_COLUMNS = "domain (required), company, facts, notes";

function row(p) {
  const name = p.name || p.email || (p.github ? `github.com/${p.github}` : `Person ${p.id}`);
  const prio = p.priority != null
    ? `<span class="tag brass">${e(p.tier)}</span> <span class="num">${p.priority}/100</span><span class="sub">fit ${p.fit} · intent ${p.intent} · why now ${p.why_now}</span>`
    : `<span class="muted">not scored</span>`;
  const contact = [p.email ? e(p.email) : "", p.github ? `<span class="sub">github.com/${e(p.github)}</span>` : ""].join("");
  const guard = p.do_not_contact ? `<span class="tag bad">Do not contact</span><span class="sub">${e(p.do_not_contact.replace(/_/g, " "))}</span>`
    : p.email_refused ? `<span class="tag bad">Not emailed</span><span class="sub">known only from GitHub</span>`
    : !p.email ? `<span class="tag warn">No address</span>` : `<span class="tag ok">May email</span>`;
  const selectable = !p.do_not_contact;
  return `<tr data-key="person:${e(p.id)}">
    <td class="select">${selectable ? `<input type="checkbox" name="person" value="${e(p.id)}" aria-label="Select ${e(name)}">` : ""}</td>
    <td data-label="Priority">${prio}${p.eligible ? "" : `<span class="sub">excluded: ${e(p.excluded)}</span>`}</td>
    <td data-label="Person"><a href="#person/${e(p.id)}">${e(name)}</a>${p.title ? `<span class="sub">${e(p.title)}</span>` : ""}</td>
    <td data-label="Company">${e(p.company || "")}${p.company_domain ? `<span class="sub">${e(p.company_domain)}</span>` : ""}</td>
    <td data-label="Contact">${contact || `<span class="muted">none</span>`}</td>
    <td data-label="Email">${guard}</td>
    <td data-label="Sequences">${p.sequences ? e(p.sequences) : `<span class="muted">none</span>`}</td>
  </tr>`;
}

function render(d) {
  const p = currentProduct({ required: true });
  const head = pageHead("People", `People in a product, best priority first. Priority A to D ranks who to contact (A is 70 or more out of 100, from fit, intent and why now); it never allows contact on its own. “Not emailed” is the outreach guard: people known only from GitHub get no email.`);
  if (!p) return head + empty("No products are loaded", `Add a product folder (<code>astrolabe init</code>), then check <a href="#settings">Settings</a> for problems.`);
  const toolbar = `<form class="toolbar" data-form="people-search" role="search">
      ${state.boot.products.length > 1 ? field("product", "Product", { kind: "select", options: productOptions(p.id), attrs: 'data-change="people-product"' }) : ""}
      <div class="grow">${field("q", "Search by name, email, GitHub login, company or domain", { kind: "search", value: d.search || "" })}</div>
      <button class="btn">Search</button>${d.search ? `<a class="btn btn-ghost" href="#people">Clear</a>` : ""}
    </form>`;
  if (!d.people.length) {
    return head + toolbar + empty(d.search ? `Nobody matches “${d.search}”` : `No people in ${p.name} yet`,
      `Import a CSV (top right), add a signal by hand on <a href="#signals">Signals</a>, or let monitors and automations find people.`);
  }
  const shown = d.people.length < d.total ? `Showing the top ${d.people.length} of ${d.total}` : `${plural(d.total, "person", "people")}`;
  return head + toolbar + `<form data-form="people-select" aria-label="People in ${e(p.name)}">
    <div class="selection-bar">
      <p class="muted" id="people-count">${e(shown)} in ${e(p.name)}${d.search ? ` matching “${e(d.search)}”` : ""}.</p>
      <p id="selected-count" class="muted" aria-live="polite">None selected.</p>
      <button type="button" class="btn btn-primary btn-sm" data-action="people-start" disabled>Start a sequence…</button>
    </div>
    <div class="table-wrap"><table class="stack-sm people-table">
      <thead><tr><th scope="col" class="select"><input type="checkbox" data-change="people-all" aria-label="Select everyone shown"></th>
        <th scope="col">Priority</th><th scope="col">Person</th><th scope="col">Company</th><th scope="col">Contact</th><th scope="col">Email</th><th scope="col">Sequences</th></tr></thead>
      <tbody>${d.people.map(row).join("")}</tbody></table></div></form>`;
}

function selected() { return $$("input[name=person]:checked", $("#main")).map(i => Number(i.value)); }

function updateSelection() {
  const ids = selected();
  const btn = $("[data-action=people-start]");
  if (btn) btn.disabled = ids.length === 0;
  const out = $("#selected-count");
  if (out) out.textContent = ids.length ? `${plural(ids.length, "person", "people")} selected.` : "None selected.";
}

function importDialog(kind) {
  const p = currentProduct({ required: true });
  const companies = kind === "companies";
  openDialog({
    title: companies ? "Import companies" : "Import people", wide: true,
    description: companies
      ? `A hand-researched account list, no people needed. Columns: ${COMPANY_COLUMNS}. Facts are <code>;</code>-separated <code>kind:value</code>. Companies match by domain, so importing again is safe. You'll see each company's fit for the product.`
      : `Columns: ${PEOPLE_COLUMNS}. Email or github is required; facts are <code>;</code>-separated, e.g. <code>dep:bullmq;team:5-50</code>. Existing people match by email or GitHub and only blank fields are filled. An address you import counts as given to you, so it may be emailed.`,
    body: `<form class="form" data-form="import" data-kind="${kind}">
      ${field("product", companies ? "Score fit for" : "Product", { kind: "select", options: productOptions(p?.id) })}
      ${field("file", "CSV file", { kind: "file", attrs: 'accept=".csv,text/csv"' })}
      ${field("csv", "…or paste CSV", { kind: "textarea", attrs: `rows="6" placeholder="${companies ? "domain,company,facts&#10;acme.example,Acme,ci:github-actions;team:5-50" : "email,name,company&#10;ana@acme.example,Ana Ruiz,Acme"}"` })}
      <div class="form-actions"><button type="button" class="btn" data-close>Cancel</button><button class="btn btn-primary">Import</button></div></form>`,
  });
}

function companyResults(r) {
  const rows = r.result.companies.map(c => `<tr><td data-label="Company">${e(c.name || c.domain)}<span class="sub">${e(c.domain)}</span></td>
      <td data-label="Fit" class="num">${c.fit}/100${c.eligible ? "" : `<span class="sub">excluded: ${e(c.excluded)}</span>`}</td>
      <td data-label="Why">${c.breakdown.length ? e(c.breakdown.join("; ")) : `<span class="muted">no fit rule matched</span>`}</td>
      <td data-label="Facts">${Object.keys(c.facts || {}).length} <span class="sub">${c.new_facts} new</span></td>
      <td data-label="Row">${c.created ? "new" : "updated"}</td></tr>`).join("");
  openDialog({
    title: "Companies imported", wide: true, description: e(r.status) + " Fit is how well each company's own facts match the product's fit rules, out of 100.",
    body: `${rows ? `<div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">Company</th><th scope="col" class="num">Fit</th><th scope="col">Why</th><th scope="col">Facts</th><th scope="col">Row</th></tr></thead><tbody>${rows}</tbody></table></div>` : ""}
      ${r.result.skipped.length ? `<h3 class="section">Skipped</h3><ul class="flags">${r.result.skipped.map(s => `<li>${e(s)}</li>`).join("")}</ul>` : ""}
      <div class="form-actions section"><button type="button" class="btn btn-primary" data-close>Done</button></div>`,
  });
}

export default {
  id: "people", title: "People", icon: "people", nav: "work",
  load: params => {
    const p = currentProduct({ required: true });
    if (!p) return Promise.resolve({ people: [], total: 0 });
    return api("/api/people" + query({ product: p.id, q: params.q, limit: 500 }));
  },
  render,
  after: () => updateSelection(),
  pageActions: () => state.boot?.products.length ? `<button type="button" class="btn btn-sm" data-action="import-people">Import people</button>
    <button type="button" class="btn btn-sm" data-action="import-companies">Import companies</button>` : "",
  actions: {
    ...seqActions,
    "people-product": el => {
      state.product = el.value;
      try { localStorage.setItem("astrolabe.product", state.product); } catch { /* private window */ }
      $("#product-filter").value = el.value;
      $("#product-filter").dispatchEvent(new Event("change"));
    },
    "people-all": el => {
      $$("input[name=person]", $("#main")).forEach(i => { i.checked = el.checked; });
      updateSelection();
    },
    "people-start": (b, ctx) => {
      const ids = selected();
      const names = Object.fromEntries(ctx.data.people.map(p => [p.id, p.name || p.email || p.github || `Person ${p.id}`]));
      startSequenceDialog(ids, { productId: currentProduct({ required: true }).id, who: plural(ids.length, "person", "people"), names });
    },
    "import-people": () => importDialog("people"),
    "import-companies": () => importDialog("companies"),
  },
  forms: {
    ...seqForms,
    "people-search": f => { const q = formData(f).q.trim(); location.hash = q ? `#people?q=${encodeURIComponent(q)}` : "#people"; },
    import: async (f, ctx) => {
      const v = formData(f);
      const file = f.querySelector("input[type=file]").files[0];
      const csv = file ? await file.text() : v.csv;
      if (!csv.trim()) throw new Error("Choose a CSV file or paste CSV text.");
      const companies = f.dataset.kind === "companies";
      const r = await post(companies ? "/api/companies/import" : "/api/people/import", { product: v.product, csv });
      if (companies) { await ctx.done(""); companyResults(r); return; }
      if (r.skipped.length) {
        await ctx.done("");
        openDialog({ title: "People imported", description: e(r.status),
          body: `<h3>Skipped rows</h3><ul class="flags">${r.skipped.map(s => `<li>${e(s)}</li>`).join("")}</ul><div class="form-actions"><button type="button" class="btn btn-primary" data-close>OK</button></div>` });
      } else await ctx.done(r.status);
    },
  },
};

// Selecting rows updates the count and the start button.
document.addEventListener("change", ev => {
  if (ev.target.matches?.("#main input[name=person]")) updateSelection();
});
