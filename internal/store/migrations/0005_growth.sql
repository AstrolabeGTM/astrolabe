-- M6: tracked links, experiments, content, digests.

CREATE TABLE links (
    code        text PRIMARY KEY,
    product_id  text NOT NULL REFERENCES products(id),
    destination text NOT NULL,
    label       text NOT NULL DEFAULT '',   -- e.g. content:x:12, or the play/sequence
    person_id   bigint REFERENCES people(id),
    action_id   bigint REFERENCES actions(id),
    utm         jsonb NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE clicks (
    id         bigserial PRIMARY KEY,
    code       text NOT NULL REFERENCES links(code),
    clicked_at timestamptz NOT NULL DEFAULT now(),
    referer    text NOT NULL DEFAULT '',
    user_agent text NOT NULL DEFAULT ''
);
CREATE INDEX clicks_code ON clicks(code, clicked_at);

ALTER TABLE actions ADD COLUMN link_code text REFERENCES links(code);
ALTER TABLE actions ADD COLUMN variant text NOT NULL DEFAULT '';

-- Variant per unit (person or company), fixed once assigned.
CREATE TABLE assignments (
    experiment  text NOT NULL,   -- <product>/<sequence>/<step>
    unit        text NOT NULL,   -- person:<id> or company:<id>
    variant     text NOT NULL,
    assigned_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (experiment, unit)
);

CREATE TABLE content_items (
    id          bigserial PRIMARY KEY,
    product_id  text NOT NULL REFERENCES products(id),
    batch       text NOT NULL,              -- what it was made from, e.g. "v0.4 release"
    platform    text NOT NULL,
    body        text NOT NULL,
    link_code   text REFERENCES links(code),
    status      text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'posted', 'skipped')),
    posted_url  text NOT NULL DEFAULT '',
    task_id     bigint REFERENCES tasks(id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    posted_at   timestamptz
);

CREATE TABLE digests (
    id         bigserial PRIMARY KEY,
    week       date NOT NULL UNIQUE,
    body       text NOT NULL,
    data       jsonb NOT NULL,
    sent_to    text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
