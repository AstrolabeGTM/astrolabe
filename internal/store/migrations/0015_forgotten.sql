-- Forgetting a person (erase on request). See docs/astrolabe.md, "Forgetting a person".

-- A tombstone per identity of an erased person: a salted SHA-256 hash of
-- "<kind>:<value>" (an email address, a GitHub login, ...). The salt is the
-- 'forget_salt' row of settings. Nothing here can be read back as an
-- address; it can only answer "is this exact value one we erased?".
CREATE TABLE forgotten (
    hash         text PRIMARY KEY,
    kind         text NOT NULL,
    forgotten_at timestamptz NOT NULL DEFAULT now()
);

-- Sends to an erased person that still count toward a rolling 24-hour cap
-- (limits.cold_per_day, updates.per_day). Erasing the message must not let
-- the cap be exceeded, so one anonymous row stays: product, which cap, when.
-- Rows are pruned once they are older than the window.
CREATE TABLE erased_sends (
    id         bigserial PRIMARY KEY,
    product_id text NOT NULL,
    counter    text NOT NULL CHECK (counter IN ('cold', 'broadcast')),
    counted_at timestamptz NOT NULL
);
CREATE INDEX erased_sends_window ON erased_sends(product_id, counter, counted_at);
