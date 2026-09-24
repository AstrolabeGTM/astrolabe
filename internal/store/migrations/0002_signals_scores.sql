-- M2: signals, identity review, enrichment facts, scores; relax M1 limits.

-- More identity kinds. Product user IDs are stored as "<product>:<user id>"
-- so two products' user 42 never collide.
ALTER TABLE identities DROP CONSTRAINT identities_kind_check;
ALTER TABLE identities ADD CONSTRAINT identities_kind_check
    CHECK (kind IN ('email', 'github', 'user', 'phone', 'hn', 'stackoverflow', 'x', 'reddit'));

-- Enrichment facts ("lang:typescript", "dep:bullmq") with where each came from.
ALTER TABLE people ADD COLUMN facts jsonb NOT NULL DEFAULT '{}';
ALTER TABLE people ADD COLUMN enriched_at timestamptz;
ALTER TABLE companies ADD COLUMN github_org text UNIQUE;
ALTER TABLE companies ADD COLUMN facts jsonb NOT NULL DEFAULT '{}';

-- One active sequence per person per product (re-enrolling after a sequence
-- ends is allowed). Enrollments remember what started them, for attribution.
ALTER TABLE enrollments DROP CONSTRAINT enrollments_product_id_person_id_sequence_id_key;
CREATE UNIQUE INDEX enrollments_one_active ON enrollments(product_id, person_id) WHERE status = 'active';
ALTER TABLE enrollments ADD COLUMN play_id text NOT NULL DEFAULT '';
ALTER TABLE enrollments ADD COLUMN signal_id bigint;

CREATE TABLE signals (
    id           bigserial PRIMARY KEY,
    product_id   text NOT NULL REFERENCES products(id),
    monitor_id   text NOT NULL DEFAULT '',
    type         text NOT NULL,
    occurred_at  timestamptz NOT NULL,
    person_id    bigint REFERENCES people(id),
    company_id   bigint REFERENCES companies(id),
    subject      jsonb NOT NULL DEFAULT '{}',
    strength     int NOT NULL CHECK (strength BETWEEN 0 AND 100),
    why_now      boolean NOT NULL DEFAULT false,
    title        text NOT NULL DEFAULT '',
    evidence_url text NOT NULL DEFAULT '',
    data         jsonb NOT NULL DEFAULT '{}',
    dedupe_key   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (product_id, dedupe_key)
);
CREATE INDEX signals_person ON signals(person_id, occurred_at);
CREATE INDEX signals_company ON signals(company_id, occurred_at);
CREATE INDEX signals_feed ON signals(product_id, occurred_at DESC);
ALTER TABLE enrollments ADD CONSTRAINT enrollments_signal_fk FOREIGN KEY (signal_id) REFERENCES signals(id);

-- Signals whose identities point at more than one person. Never guessed.
CREATE TABLE identity_reviews (
    id          bigserial PRIMARY KEY,
    signal_id   bigint NOT NULL REFERENCES signals(id),
    candidates  bigint[] NOT NULL,
    status      text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved', 'dismissed')),
    resolved_to bigint REFERENCES people(id),
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE scores (
    product_id  text NOT NULL REFERENCES products(id),
    person_id   bigint NOT NULL REFERENCES people(id),
    fit         int NOT NULL,
    intent      int NOT NULL,
    why_now     int NOT NULL,
    priority    int NOT NULL,
    tier        text NOT NULL CHECK (tier IN ('A', 'B', 'C', 'D')),
    eligible    boolean NOT NULL,
    breakdown   jsonb NOT NULL,
    computed_at timestamptz NOT NULL,
    PRIMARY KEY (product_id, person_id)
);
CREATE INDEX scores_rank ON scores(product_id, priority DESC);

CREATE TABLE monitor_runs (
    product_id  text NOT NULL,
    monitor_id  text NOT NULL,
    last_run_at timestamptz NOT NULL,
    last_ok_at  timestamptz,
    last_error  text NOT NULL DEFAULT '',
    last_count  int NOT NULL DEFAULT 0,
    PRIMARY KEY (product_id, monitor_id)
);
