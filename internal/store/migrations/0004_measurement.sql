-- M4: product events and Stripe webhooks, payments, channel permissions.

-- Provider events already processed (webhooks retry and duplicate).
CREATE TABLE webhook_events (
    provider    text NOT NULL,
    event_id    text NOT NULL,
    product_id  text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, event_id)
);

-- Money in and out, once per provider transaction. Refunds are negative.
CREATE TABLE payments (
    id           bigserial PRIMARY KEY,
    product_id   text NOT NULL REFERENCES products(id),
    person_id    bigint REFERENCES people(id),
    provider     text NOT NULL,
    provider_id  text NOT NULL,
    kind         text NOT NULL CHECK (kind IN ('payment', 'refund')),
    amount_cents bigint NOT NULL,
    currency     text NOT NULL,
    occurred_at  timestamptz NOT NULL,
    email        text NOT NULL DEFAULT '',
    UNIQUE (provider, provider_id)
);
CREATE INDEX payments_product ON payments(product_id, occurred_at);

-- What a product user allowed us to use for lifecycle messages.
CREATE TABLE channel_permissions (
    product_id text NOT NULL REFERENCES products(id),
    person_id  bigint NOT NULL REFERENCES people(id),
    channel    text NOT NULL,
    allowed    boolean NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (product_id, person_id, channel)
);

-- Stripe customer ids link subscription events (which carry no email) to people.
ALTER TABLE identities DROP CONSTRAINT identities_kind_check;
ALTER TABLE identities ADD CONSTRAINT identities_kind_check
    CHECK (kind IN ('email', 'github', 'user', 'phone', 'hn', 'stackoverflow', 'x', 'reddit', 'stripe'));
