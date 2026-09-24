-- M3: plays, AI briefs and drafts, reply answers, voice examples.

-- Every model call, for audit and spend.
CREATE TABLE llm_calls (
    id            bigserial PRIMARY KEY,
    purpose       text NOT NULL,
    product_id    text,
    model         text NOT NULL,
    input_tokens  int NOT NULL DEFAULT 0,
    output_tokens int NOT NULL DEFAULT 0,
    prompt        text NOT NULL,
    output        text NOT NULL DEFAULT '',
    error         text NOT NULL DEFAULT '',
    duration_ms   int NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX llm_calls_product ON llm_calls(product_id, created_at);

CREATE TABLE briefs (
    id          bigserial PRIMARY KEY,
    product_id  text NOT NULL REFERENCES products(id),
    person_id   bigint NOT NULL REFERENCES people(id),
    body        text NOT NULL,
    llm_call_id bigint REFERENCES llm_calls(id),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX briefs_person ON briefs(product_id, person_id, created_at DESC);

-- A play firing: what it did or why it didn't, once per trigger.
CREATE TABLE play_runs (
    id            bigserial PRIMARY KEY,
    product_id    text NOT NULL REFERENCES products(id),
    play_id       text NOT NULL,
    person_id     bigint REFERENCES people(id),
    signal_id     bigint REFERENCES signals(id),
    trigger_key   text NOT NULL,
    outcome       text NOT NULL,       -- enrolled, task, skipped
    detail        text NOT NULL DEFAULT '',
    enrollment_id bigint REFERENCES enrollments(id),
    task_id       bigint REFERENCES tasks(id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (product_id, play_id, trigger_key)
);
CREATE INDEX play_runs_day ON play_runs(product_id, play_id, created_at);

ALTER TABLE enrollments ADD COLUMN goal_stage text NOT NULL DEFAULT '';

-- AI drafting state. A draft job only fills an action that is still an
-- untouched pending draft, so my edits always win.
ALTER TABLE actions ADD COLUMN ai_state text NOT NULL DEFAULT ''
    CHECK (ai_state IN ('', 'pending', 'done', 'failed'));
ALTER TABLE actions ADD COLUMN ai_body text NOT NULL DEFAULT '';
ALTER TABLE actions ADD COLUMN flags text[] NOT NULL DEFAULT '{}';
-- 'step' actions belong to a sequence step; 'reply' actions answer a reply
-- (step = -reply_id keeps the (enrollment, step) key unique).
ALTER TABLE actions ADD COLUMN kind text NOT NULL DEFAULT 'step' CHECK (kind IN ('step', 'reply'));
ALTER TABLE actions ADD COLUMN reply_id bigint REFERENCES replies(id);

ALTER TABLE replies ADD COLUMN message_id text NOT NULL DEFAULT '';
ALTER TABLE replies ADD COLUMN suggested_class text NOT NULL DEFAULT '';
ALTER TABLE replies ADD COLUMN auto_classified boolean NOT NULL DEFAULT false;

ALTER TABLE tasks ADD COLUMN draft text NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN play_id text NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN signal_id bigint REFERENCES signals(id);

-- My edits to AI drafts, used as examples of the product's voice.
CREATE TABLE voice_examples (
    id         bigserial PRIMARY KEY,
    product_id text NOT NULL REFERENCES products(id),
    action_id  bigint REFERENCES actions(id),
    original   text NOT NULL,
    edited     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
