-- M8: the updates list (an opt-in newsletter). A sign-up is a pending row
-- until the person confirms; only confirmed rows ever receive a broadcast.

CREATE TABLE updates_subscribers (
    id              bigserial PRIMARY KEY,
    product_id      text   NOT NULL REFERENCES products(id),
    person_id       bigint NOT NULL REFERENCES people(id),
    email           text   NOT NULL,
    status          text   NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'confirmed', 'unsubscribed', 'expired', 'bounced')),
    source          text   NOT NULL DEFAULT 'manual',  -- manual, automation:<id>, ...
    requests        int    NOT NULL DEFAULT 1,         -- how many times a confirmation was asked for
    requested_at    timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz,                       -- while pending
    confirmed_at    timestamptz,
    confirmed_via   text   NOT NULL DEFAULT '',        -- reply, link, manual
    unsubscribed_at timestamptz,
    unsubscribed_via text  NOT NULL DEFAULT '',        -- reply, link, one-click, manual
    UNIQUE (product_id, person_id)
);
CREATE INDEX updates_subscribers_status ON updates_subscribers(product_id, status);

-- A broadcast: one email to every confirmed subscriber. Approval covers the
-- exact sender, subject and body (approved_hash); an edit withdraws it.
CREATE TABLE updates_broadcasts (
    id            bigserial PRIMARY KEY,
    product_id    text NOT NULL REFERENCES products(id),
    subject       text NOT NULL,
    body          text NOT NULL,
    status        text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'approved', 'cancelled')),
    flags         text[] NOT NULL DEFAULT '{}',
    approved_hash text NOT NULL DEFAULT '',
    approved_at   timestamptz,
    period_from   date,
    period_to     date,
    content_ids   bigint[] NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- One row per message to one subscriber: the confirmation, the welcome, or
-- a broadcast. A broadcast has exactly one row per subscriber (unique), and
-- a row is claimed ('sending') in a transaction before the provider is
-- called, so nobody gets a broadcast twice. 'unknown' is never retried.
CREATE TABLE updates_messages (
    id            bigserial PRIMARY KEY,
    product_id    text   NOT NULL REFERENCES products(id),
    subscriber_id bigint NOT NULL REFERENCES updates_subscribers(id),
    kind          text   NOT NULL CHECK (kind IN ('confirm', 'welcome', 'broadcast')),
    broadcast_id  bigint REFERENCES updates_broadcasts(id),
    request_no    int    NOT NULL DEFAULT 1,
    status        text   NOT NULL DEFAULT 'queued'
                  CHECK (status IN ('queued', 'sending', 'sent', 'failed', 'unknown', 'skipped')),
    attempts      int    NOT NULL DEFAULT 0,
    error         text   NOT NULL DEFAULT '',
    message_id    text   NOT NULL DEFAULT '',
    provider_id   text   NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    claimed_at    timestamptz,
    sent_at       timestamptz
);
CREATE UNIQUE INDEX updates_messages_broadcast ON updates_messages(broadcast_id, subscriber_id) WHERE broadcast_id IS NOT NULL;
CREATE UNIQUE INDEX updates_messages_once ON updates_messages(subscriber_id, kind, request_no) WHERE broadcast_id IS NULL;
CREATE INDEX updates_messages_queue ON updates_messages(status, kind, id);
CREATE INDEX updates_messages_msgid ON updates_messages(message_id) WHERE message_id <> '';

-- Replies that confirm or leave a list are labelled 'list'.
ALTER TABLE replies DROP CONSTRAINT IF EXISTS replies_classification_check;
ALTER TABLE replies ADD CONSTRAINT replies_classification_check
    CHECK (classification IN ('', 'interested', 'question', 'not_now', 'no', 'out_of_office', 'unsubscribe', 'bounce', 'list'));
