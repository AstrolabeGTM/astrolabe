-- M8: conversation notes, the product-market-fit survey, pricing feedback
-- and weekly themes (docs §15.6).

-- One call, email thread or chat with a person, optionally about their
-- account. assisted: we coached them through using the product, so it is
-- never counted as unassisted first use.
CREATE TABLE conversations (
    id               bigserial PRIMARY KEY,
    product_id       text NOT NULL REFERENCES products(id),
    person_id        bigint NOT NULL REFERENCES people(id),
    account_id       bigint REFERENCES accounts(id),
    occurred_on      date NOT NULL,
    channel          text NOT NULL CHECK (channel IN ('call', 'email', 'chat', 'in_person', 'other')),
    duration_minutes int CHECK (duration_minutes IS NULL OR duration_minutes > 0),
    assisted         boolean NOT NULL DEFAULT false,
    summary          text NOT NULL DEFAULT '',
    their_words      text NOT NULL DEFAULT '',   -- exact quotes
    problem          text NOT NULL DEFAULT '',
    current_approach text NOT NULL DEFAULT '',   -- what they do today (the baseline)
    next_step        text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX conversations_product ON conversations(product_id, occurred_on DESC, id DESC);
CREATE INDEX conversations_person ON conversations(person_id, occurred_on DESC);
CREATE INDEX conversations_account ON conversations(account_id) WHERE account_id IS NOT NULL;

-- "How would you feel if you could no longer use <product>?" Asked only of
-- people who used it two weeks or more; used_two_weeks is recorded as given.
CREATE TABLE pmf_answers (
    conversation_id  bigint PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
    product_id       text NOT NULL REFERENCES products(id),
    person_id        bigint NOT NULL REFERENCES people(id),
    answered_on      date NOT NULL,
    answer           text NOT NULL CHECK (answer IN ('very_disappointed', 'somewhat_disappointed', 'not_disappointed')),
    used_two_weeks   boolean NOT NULL,
    alternative      text NOT NULL DEFAULT '',   -- what would you use instead? (word for word)
    who_else         text NOT NULL DEFAULT ''    -- who else would benefit? (word for word)
);
CREATE INDEX pmf_answers_product ON pmf_answers(product_id, answered_on);

-- What a person said about price: statements, not commitments.
CREATE TABLE pricing_feedback (
    id              bigserial PRIMARY KEY,
    conversation_id bigint NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    product_id      text NOT NULL REFERENCES products(id),
    person_id       bigint NOT NULL REFERENCES people(id),
    stated_on       date NOT NULL,
    offer           text NOT NULL,              -- the offer named
    replaces        text NOT NULL DEFAULT '',   -- what it would replace (comparison)
    approver        text NOT NULL DEFAULT '',   -- who would approve it
    buying_process  text NOT NULL DEFAULT '' CHECK (buying_process IN ('', 'card', 'invoice', 'procurement')),
    reaction        text NOT NULL DEFAULT ''
);
CREATE INDEX pricing_feedback_product ON pricing_feedback(product_id, stated_on);

-- The week's themes, written by the model from the week's notes. One row per
-- product and ISO week (2026-W43); saving again replaces it.
CREATE TABLE weekly_themes (
    product_id text NOT NULL REFERENCES products(id),
    iso_week   text NOT NULL,
    body       text NOT NULL DEFAULT '',
    items      jsonb NOT NULL DEFAULT '[]',
    saved_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (product_id, iso_week)
);
