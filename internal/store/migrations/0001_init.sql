-- M1: products, people, outreach, replies, tasks, funnel.

CREATE TABLE products (
    id         text PRIMARY KEY,
    name       text NOT NULL,
    config     jsonb NOT NULL,
    file_hash  text NOT NULL,
    loaded_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sequences (
    product_id text NOT NULL REFERENCES products(id),
    id         text NOT NULL,
    config     jsonb NOT NULL,
    PRIMARY KEY (product_id, id)
);

CREATE TABLE companies (
    id         bigserial PRIMARY KEY,
    domain     text UNIQUE,
    name       text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE people (
    id         bigserial PRIMARY KEY,
    name       text NOT NULL DEFAULT '',
    first_name text NOT NULL DEFAULT '',
    title      text NOT NULL DEFAULT '',
    company_id bigint REFERENCES companies(id),
    notes      text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Deterministic identity: an email or GitHub login belongs to exactly one person.
CREATE TABLE identities (
    kind      text NOT NULL CHECK (kind IN ('email', 'github')),
    value     text NOT NULL,
    person_id bigint NOT NULL REFERENCES people(id),
    PRIMARY KEY (kind, value)
);
CREATE INDEX identities_person ON identities(person_id);

-- Which products a person is a prospect or user for.
CREATE TABLE product_people (
    product_id text NOT NULL REFERENCES products(id),
    person_id  bigint NOT NULL REFERENCES people(id),
    source     text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (product_id, person_id)
);

-- Do-not-contact, global per person across all products.
CREATE TABLE suppressions (
    person_id  bigint PRIMARY KEY REFERENCES people(id),
    reason     text NOT NULL CHECK (reason IN ('unsubscribed', 'bounced', 'replied_no', 'manual')),
    detail     text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE enrollments (
    id           bigserial PRIMARY KEY,
    product_id   text NOT NULL REFERENCES products(id),
    person_id    bigint NOT NULL REFERENCES people(id),
    sequence_id  text NOT NULL,
    status       text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'stopped', 'completed')),
    stop_reason  text NOT NULL DEFAULT '',
    next_step    int NOT NULL DEFAULT 0,
    next_due_at  timestamptz,
    started_at   timestamptz NOT NULL DEFAULT now(),
    ended_at     timestamptz,
    UNIQUE (product_id, person_id, sequence_id),
    FOREIGN KEY (product_id, sequence_id) REFERENCES sequences(product_id, id)
);
CREATE INDEX enrollments_due ON enrollments(next_due_at) WHERE status = 'active';

-- Every outbound message or manual task. One row per (enrollment, step): the
-- durable record that makes a send happen at most once.
CREATE TABLE actions (
    id            bigserial PRIMARY KEY,
    enrollment_id bigint NOT NULL REFERENCES enrollments(id),
    step          int NOT NULL,
    product_id    text NOT NULL REFERENCES products(id),
    person_id     bigint NOT NULL REFERENCES people(id),
    channel       text NOT NULL,
    sender        text NOT NULL DEFAULT '',
    recipient     text NOT NULL DEFAULT '',
    subject       text NOT NULL DEFAULT '',
    body          text NOT NULL DEFAULT '',
    goal          text NOT NULL DEFAULT '',
    status        text NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft', 'approved', 'sending', 'sent', 'unknown', 'failed', 'skipped', 'blocked')),
    approved_hash text NOT NULL DEFAULT '',
    approved_at   timestamptz,
    message_id    text NOT NULL DEFAULT '',   -- RFC 822 Message-ID we set before sending
    provider_id   text NOT NULL DEFAULT '',   -- Gmail message id
    thread_id     text NOT NULL DEFAULT '',   -- Gmail thread id
    in_reply_to   text NOT NULL DEFAULT '',   -- Message-ID of the previous step
    error         text NOT NULL DEFAULT '',
    claimed_at    timestamptz,
    sent_at       timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (enrollment_id, step)
);
CREATE INDEX actions_status ON actions(status);
CREATE INDEX actions_thread ON actions(thread_id) WHERE thread_id <> '';
CREATE INDEX actions_person_sent ON actions(person_id, sent_at);

CREATE TABLE replies (
    id                  bigserial PRIMARY KEY,
    provider_message_id text NOT NULL UNIQUE,
    mailbox             text NOT NULL,
    thread_id           text NOT NULL DEFAULT '',
    product_id          text REFERENCES products(id),
    person_id           bigint REFERENCES people(id),
    enrollment_id       bigint REFERENCES enrollments(id),
    action_id           bigint REFERENCES actions(id),
    from_addr           text NOT NULL,
    subject             text NOT NULL DEFAULT '',
    snippet             text NOT NULL DEFAULT '',
    body                text NOT NULL DEFAULT '',
    is_bounce           boolean NOT NULL DEFAULT false,
    received_at         timestamptz NOT NULL,
    classification      text NOT NULL DEFAULT ''
                        CHECK (classification IN ('', 'interested', 'question', 'not_now', 'no', 'out_of_office', 'unsubscribe', 'bounce')),
    classified_at       timestamptz
);

-- Human follow-through with one next action and an outcome when closed.
CREATE TABLE tasks (
    id            bigserial PRIMARY KEY,
    product_id    text NOT NULL REFERENCES products(id),
    person_id     bigint REFERENCES people(id),
    enrollment_id bigint REFERENCES enrollments(id),
    reply_id      bigint REFERENCES replies(id),
    next_action   text NOT NULL,
    due_at        timestamptz NOT NULL,
    status        text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'done')),
    outcome       text NOT NULL DEFAULT ''
                  CHECK (outcome IN ('', 'activated', 'paid', 'wrong_fit', 'unclear_value', 'setup_blocked', 'price', 'later', 'other')),
    outcome_note  text NOT NULL DEFAULT '',
    minutes_spent int,
    created_at    timestamptz NOT NULL DEFAULT now(),
    closed_at     timestamptz
);
CREATE INDEX tasks_open ON tasks(due_at) WHERE status = 'open';

CREATE TABLE funnel_events (
    id          bigserial PRIMARY KEY,
    product_id  text NOT NULL REFERENCES products(id),
    person_id   bigint NOT NULL REFERENCES people(id),
    stage       text NOT NULL,
    occurred_at timestamptz NOT NULL,
    source      text NOT NULL,
    dedupe_key  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (product_id, dedupe_key)
);
CREATE INDEX funnel_events_stage ON funnel_events(product_id, stage);

-- Pause switches: 'global', 'product:<id>', 'sequence:<product>/<id>'.
CREATE TABLE pauses (
    scope      text PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Reply polling position per mailbox.
CREATE TABLE mailbox_sync (
    mailbox      text PRIMARY KEY,
    last_poll_at timestamptz NOT NULL
);
