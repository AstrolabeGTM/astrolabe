// Setup and settings: what is loaded and connected, pause switches, and the
// doctor's checks with the fix for each.
import { e, api, post, pageHead, empty, plural, productName } from "./lib.js";

const STATUS = { ok: ["ok", "OK"], warn: ["warn", "Warning"], fail: ["bad", "Problem"] };

function products(s) {
  const list = s.products.map(p => `<tr><th scope="row">${e(p.name)}<span class="sub">${e(p.id)}${p.demo ? " · demo data, sandbox only" : ""}</span></th>
      <td data-label="Folder"><code class="path">${e(s.folders[p.id] || "")}</code></td>
      <td data-label="Contents">${plural(p.sequences.length, "sequence")}, ${plural(p.automations, "automation")}, ${plural(p.monitors, "monitor")}<span class="sub">funnel: ${e(p.funnel.join(" → "))}</span></td></tr>`).join("");
  const problems = s.load_problems.map(i => `<div class="notice bad"><p><strong>${e(i.folder || "Products")}</strong> did not load:</p>
      <ul>${i.problems.map(x => `<li>${e(x)}</li>`).join("")}</ul>${i.fix ? `<p><strong>Fix:</strong> ${e(i.fix)}</p>` : ""}</div>`).join("");
  return `${problems}${list ? `<div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">Product</th><th scope="col">Folder</th><th scope="col">Contents</th></tr></thead><tbody>${list}</tbody></table></div>`
    : empty("No products are loaded", "Create one with <code>astrolabe init -template devtool</code> (or ask Claude to set one up), then reload.")}
    <p class="help">Products are folders of YAML and Markdown. Edit the files, then use Reload products (top right).</p>`;
}

// grouped merges the doctor's per-line product problems ("- url is
// required") into the check above them, as one item with a list.
function grouped(checks) {
  const out = [];
  for (const c of checks) {
    const prev = out[out.length - 1];
    if (prev && c.area === prev.area && c.status === prev.status && c.detail.startsWith("- ")) prev.lines.push(c.detail.slice(2));
    else out.push({ ...c, lines: [] });
  }
  return out;
}

function doctor(d) {
  if (!d) return `<p class="muted">Checks are not available.</p>`;
  const n = s => d.checks.filter(c => c.status === s).length;
  const items = grouped(d.checks).map(c => {
    const [cls, word] = STATUS[c.status] || ["", c.status];
    return `<li><span class="tag ${cls}">${word}</span><strong>${e(c.area)}</strong>
      <div class="check-detail">${e(c.detail)}${c.lines.length ? `<ul>${c.lines.map(l => `<li>${e(l)}</li>`).join("")}</ul>` : ""}
      ${c.fix && c.status !== "ok" ? `<p class="help"><strong>Fix:</strong> ${e(c.fix)}</p>` : ""}</div></li>`;
  }).join("");
  return `<p class="muted">${plural(d.checks.length, "check")}${d.connect ? ", including logins, keys and DNS" : ""}: ${n("ok")} OK, ${plural(n("warn"), "warning")}, ${plural(n("fail"), "problem")}.</p>
    <ul class="checks">${items}</ul>
    <div class="row section"><button type="button" class="btn" data-action="doctor-connect">Also test logins, keys and DNS</button>
    <p class="help">Sends nothing. It can take up to a minute. The same as <code>astrolabe doctor -connect</code>.</p></div>`;
}

function pauses(s) {
  const rows = s.pauses.map(p => `<tr data-key="pause:${e(p.scope)}"><th scope="row">${e(p.label)}</th>
      <td data-label="State">${p.paused ? `<span class="tag warn">Paused</span>` : `<span class="tag ok">Sending</span>`}</td>
      <td><button type="button" class="btn btn-sm${p.paused ? " btn-primary" : ""}" data-action="${p.paused ? "resume" : "pause"}" data-scope="${e(p.scope)}">${p.paused ? "Resume" : "Pause"}<span class="sr-only"> ${e(p.label)}</span></button></td></tr>`).join("");
  return `<div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">Switch</th><th scope="col">State</th><th scope="col"><span class="sr-only">Change</span></th></tr></thead><tbody>${rows}</tbody></table></div>
    <p class="help">A paused send waits and goes out after you resume; nothing is dropped. Pausing everything also holds sends of every product.</p>`;
}

function mailboxes(s) {
  const rows = s.mailboxes.map(m => `<tr><th scope="row">${e(m.address)}</th><td data-label="Used for">${e(m.uses.join(", "))}</td>
      <td data-label="Status">${m.ready ? `<span class="tag ok">Connected</span><span class="sub">${e(m.how)}</span>` : `<span class="tag ${s.mode === "sandbox" ? "" : "bad"}">Not set up</span>`}${m.note ? `<span class="sub">${e(m.note)}</span>` : ""}</td></tr>`).join("");
  return `${rows ? `<div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">Address</th><th scope="col">Used for</th><th scope="col">Status</th></tr></thead><tbody>${rows}</tbody></table></div>` : `<p class="muted">No product names a sender address (<code>sender:</code> in product.yaml).</p>`}
    <p class="help">Connect a mailbox with an app password on the <a href="/settings">classic Settings page</a>, or run <code>astrolabe mailbox add</code>. Passwords are kept in the secrets folder and never pass through this app's API or MCP.</p>`;
}

function webhooks(s) {
  const rows = s.webhooks.map(h => `<tr><th scope="row">${e(productName(h.product))}<span class="sub">${e(h.kind)}</span></th>
      <td data-label="Endpoint"><code class="path">POST ${e(h.path)}</code></td>
      <td data-label="Secret">${h.secret_set ? `<span class="tag ok">Set</span>` : `<span class="tag">Not set</span><span class="sub">set <code>${e(h.secret_env)}</code></span>`}</td></tr>`).join("");
  return `<p class="muted">Public URL: ${s.public_url ? `<code>${e(s.public_url)}</code>` : "not set (ASTROLABE_PUBLIC_URL); webhooks and tracked links need it"}.</p>
    ${rows ? `<div class="table-wrap"><table class="stack-sm"><thead><tr><th scope="col">Product</th><th scope="col">Endpoint</th><th scope="col">Signing secret</th></tr></thead><tbody>${rows}</tbody></table></div>` : ""}
    <p class="help">Your app posts product events, signed with HMAC-SHA256 (<code>Astrolabe-Signature: t=…,v1=…</code>); Stripe signs with its endpoint's whsec_ secret.</p>`;
}

const sec = (id, title, body, note = "") => `<section class="section" aria-labelledby="${id}"><div class="section-head"><h2 id="${id}">${title}</h2>${note}</div>${body}</section>`;

export default {
  id: "settings", title: "Setup and settings", icon: "settings", nav: "setup",
  load: async () => {
    const [setup, checks] = await Promise.all([api("/api/setup"), api("/api/doctor").catch(() => null)]);
    return { setup, checks };
  },
  render: d => pageHead("Setup and settings", "What is loaded and connected. Products are folders you edit; the checks (astrolabe doctor) say what is missing and how to fix it; pause switches hold sending without dropping anything.")
    + sec("s-products", "Products", products(d.setup), `<p class="muted">${plural(d.setup.products.length, "loaded product")}${d.setup.load_problems.length ? `, ${d.setup.load_problems.length} not loaded` : ""}</p>`)
    + sec("s-doctor", "Checks", doctor(d.checks), `<p class="muted">astrolabe doctor</p>`)
    + sec("s-pauses", "Pause switches", pauses(d.setup))
    + sec("s-mail", "Mailboxes", mailboxes(d.setup), `<p class="muted">Sender addresses from product.yaml</p>`)
    + sec("s-hooks", "Webhooks", webhooks(d.setup)),
  pageActions: () => `<button type="button" class="btn btn-sm" data-action="reload-products">Reload products</button>`,
  actions: {
    pause: async (b, ctx) => ctx.done((await post("/api/pause", { scope: b.dataset.scope })).status),
    resume: async (b, ctx) => ctx.done((await post("/api/resume", { scope: b.dataset.scope })).status),
    "reload-products": async (b, ctx) => {
      const r = await post("/api/products/reload");
      await ctx.done(r.status, r.load_problems.length ? "error" : "ok");
    },
    "doctor-connect": async (b, ctx) => {
      b.textContent = "Testing logins, keys and DNS…";
      ctx.data.checks = await api("/api/doctor?connect=1");
      const body = document.querySelector("#s-doctor").closest("section");
      body.outerHTML = sec("s-doctor", "Checks", doctor(ctx.data.checks), `<p class="muted">astrolabe doctor -connect</p>`);
      document.querySelector("#s-doctor").closest("section").querySelector("button")?.focus();
    },
  },
};
