# Astrolabe

**Go-to-market for the everyday programmer.** You built something. Astrolabe finds people who have the problem it solves, drafts outreach you approve, follows up, and shows which efforts turn into users and revenue, without learning five sales tools.

- **Talk to it from Claude or Codex** (MCP): "who should I contact this week?", "draft intros for the top 5", "why did activation drop?"
- **Or use the web app** at `/app/`. It can do exactly what the MCP tools can.
- **Configure it like code**: each product is a folder of YAML and Markdown in git. Your editor autocompletes it, and `astrolabe doctor` checks it.
- **Self-host it**: one Docker image plus Postgres. Open source under AGPL-3.0.

Developer terms come first, and sales terms work too: "enroll my top MQLs in the intro cadence" does the same as "start the intro sequence for the top prospects".

## How Astrolabe works for GTM

Go-to-market is a loop: find people who have the problem, reach them with something useful, help the ones who answer, and learn what worked. Astrolabe runs that loop like a CI pipeline. Machines watch, sort and draft; you decide and approve.

```text
monitors ─▶ signals ─▶ people + priority ─▶ automations ─▶ tasks / drafts ─▶ you approve ─▶ send
                                                                                              │
         weekly review ◀── funnel (people) + pipeline (companies) ◀── replies, signups, calls ◀┘
```

1. **Describe the product once.** A folder in git says who it's for, the one offer every message makes, what's true about it (`claims.md`) and how you write (`voice.md`). Nothing goes out while claims and voice are stubs.
2. **Monitors find signals.** On a schedule, Astrolabe watches GitHub searches, your repos (issues, discussions, stars, release downloads), Hacker News, Stack Overflow, RSS, your request forms, and events from your app and Stripe. Each hit is a signal about a person, or a public mention.
3. **People get a priority.** A score from fit (your target rules), recent activity and anything time-sensitive puts each person in A–D. A means contact now.
4. **Automations decide what happens.** Rules like `on a pain-issue signal with fit ≥ 50 → start a sequence` or `on a form request → reply task due in 2 hours`. Most signals become tasks ("answer in the thread"); some start a sequence of messages.
5. **Drafts wait for you.** Claude writes each message from your claims and voice, and Astrolabe flags anything outside them. You approve, edit or skip on **Today**. Nothing sends without approval unless you allow it per product.
6. **Replies close the loop.** A reply stops the sequence and waits for a label (interested, not now, no). "No", bounces and unsubscribes go on a do-not-contact list that every product respects.
7. **Progress is tracked two ways.** The **funnel** follows people (reached → replied → signed up → activated). For b2b, the **pipeline** follows companies through your stages, each with one next action and a due date. Qualification checks must be "yes" with evidence before the gate stage.
8. **The weekly review says what worked.** Counts with their denominators, stuck accounts, themes from calls, public mentions and campaign results. Experiments end with keep, change or stop.

**A day with it** takes 30–60 minutes: open **Today**, answer requests and replies, approve drafts, close due tasks, and log a note after each call. Or ask Claude "what's waiting on me?". Monitors keep polling and approved messages keep sending while you're away.

| You'll see | Sales term | It means |
| --- | --- | --- |
| **Signal** | intent data | An event about a person: an issue they opened about your problem, a star, a form request, a signup |
| **Target / fit rules** | ICP | Who the product is for: sentences for humans, facts with weights for the scorer |
| **Priority A–D** | lead score | Score 0–100 from fit, recent activity and why now. A = contact now |
| **Automation** | play, workflow | `trigger → when → action` in `automations.yaml` |
| **Sequence** | cadence | Timed message steps. `kind: cold` for strangers, `kind: users` for existing users |
| **Offer** | CTA | The one concrete thing every message asks for |
| **Funnel / activation** | lifecycle stages | Your product's stages for people; activation is the first real success, the number to move |
| **Pipeline** | deals, opportunities | Companies moving through your stages, with qualification (BANT, MEDDIC) before the gate stage |
| **Do not contact** | suppression list | People no product will ever message again |

The config accepts sales vocabulary too (`icp`, `plays.yaml`, `kind: outbound`). The full glossary is in [docs/astrolabe.md](docs/astrolabe.md).

## Try it in five minutes (no accounts needed)

```sh
mkdir my-gtm && cd my-gtm
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD:/work" -w /work ghcr.io/astrolabegtm/astrolabe init -template demo
docker compose up -d
```

Open http://localhost:8080/app/ and sign in with `ASTROLABE_PASSWORD` from `.env`. The demo runs in **sandbox mode**: nothing leaves your machine, and every message lands in a local **Outbox**. Try this:

1. **People:** tick a few demo people, then **Start a sequence** with `intro`.
2. **Today:** read the draft (the exact sender, recipient and text), then **Approve**, or **Edit** it first.
3. **Outbox** (in the sidebar in sandbox mode): the message is there. Play the recipient: **Simulate a reply** (or a bounce).
4. **Today:** the sequence stopped and the reply waits to be labelled. Mark it *Interested* and a task appears.
5. **Signals → Add a signal by hand** with type `referral`: the `referral-intro` automation starts a sequence by itself.

Then look at **Pipeline** (open an account and **Log a conversation**), **Campaigns** and the **Weekly review**.

To use it from Claude Code, run `claude mcp add astrolabe -- docker compose -f "$PWD/compose.yaml" exec -T astrolabe astrolabe mcp` and ask "what's waiting on me?". Codex, Claude Desktop and MCP over HTTP are in [docs/setup.md](docs/setup.md#use-it-from-claude-or-codex).

## Your own product

```sh
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD:/work" -w /work ghcr.io/astrolabegtm/astrolabe init -template devtool -name "Hook Check" -url https://hookcheck.dev
```

| Template | For |
| --- | --- |
| `devtool` | Developer tools: GitHub/HN signals become tasks to help in the thread; email for people who gave you an address; help after signup |
| `saas` | B2B SaaS: trials, activation, trial-ending notes, win-back |
| `app` | Consumer apps: product events, push/in-app and email to users |
| `demo` | The fictional product above |

This writes `products/<id>/`:

| File | What goes in it |
| --- | --- |
| `product.yaml` | Who it's for, funnel stages, the offer, sender mailboxes, fit rules, approval and limits; optionally an updates list and an account pipeline |
| `claims.md`, `voice.md` | What's true about the product (drafts may only use this) and how you write |
| `monitors.yaml` | Where to look for signals |
| `automations.yaml` | `trigger → when → action` rules |
| `sequences/*.yaml` | Timed message steps |
| `campaigns/*.yaml` | Optional: a launch week or a small paid test, with a budget cap, dates, a stop rule and creatives |

Each YAML file points at a JSON schema, so VS Code, Zed, Neovim and JetBrains autocomplete keys and flag typos. Or ask Claude to "set up a new product" and it interviews you and writes the folder. Then run `astrolabe doctor` to see what's missing.

To send for real, [go live](docs/setup.md#go-live): a new database in live mode, your mailboxes connected with an app password, and `astrolabe doctor -connect` passing.

## Safe by default

- You approve the exact message. Any edit withdraws the approval, and each message is sent at most once.
- A person known only from GitHub is never emailed, as GitHub's policies require; you help them in the thread.
- No tracking pixels. Tracked links are optional, and off with `links.personal: false`.
- Replies stop sequences. "No", bounces and unsubscribes are honoured across every product.
- `astrolabe forget <email>` erases a person on request.

The details are in [docs/setup.md](docs/setup.md#how-sending-stays-safe).

## Docs

- [docs/setup.md](docs/setup.md): running it with Docker or as a binary, Claude and Codex, mailboxes, going live, webhooks and forms, configuration, development.
- [docs/astrolabe.md](docs/astrolabe.md): the design: who it's for, the glossary, signals, scoring, automations, outreach, measurement, architecture and the build plan.
- [docs/mcp-tools.md](docs/mcp-tools.md): every MCP tool.

## Status

Everything in the design is built. The tests run against Postgres with fake providers, and the Docker quickstart has been run end to end. Mailbox logins have been tested against a real provider; live sending, reply detection, WhatsApp, Stripe and the Claude API have not yet run against real accounts, so the first live run is where rough edges will show. Please open issues.

## Licence

[AGPL-3.0](LICENSE). You can self-host, modify and share it. If you run a modified version as a service for others, you must offer them your changes.
