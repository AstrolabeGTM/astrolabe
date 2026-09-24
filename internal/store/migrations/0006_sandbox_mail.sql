-- Sandbox mode and simple (SMTP/IMAP) email.

-- Instance settings. 'mode' is sandbox or live, fixed on first start so
-- test data and real sends never mix in one database.
CREATE TABLE settings (
    key        text PRIMARY KEY,
    value      text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Everything "sent" in sandbox mode lands here instead of a provider.
CREATE TABLE sandbox_outbox (
    id         bigserial PRIMARY KEY,
    action_id  bigint REFERENCES actions(id),
    channel    text NOT NULL,
    sender     text NOT NULL,
    recipient  text NOT NULL,
    subject    text NOT NULL DEFAULT '',
    body       text NOT NULL,
    message_id text NOT NULL DEFAULT '',
    thread_id  text NOT NULL DEFAULT '',
    sent_at    timestamptz NOT NULL DEFAULT now()
);

-- IMAP polling position (UIDVALIDITY + last UID) per mailbox and folder.
ALTER TABLE mailbox_sync ADD COLUMN uid_validity bigint;
ALTER TABLE mailbox_sync ADD COLUMN last_uid bigint;

-- Replies can reference any of our Message-IDs (In-Reply-To/References).
ALTER TABLE replies ADD COLUMN refs text[] NOT NULL DEFAULT '{}';
CREATE INDEX actions_message_id ON actions(message_id) WHERE message_id <> '';
