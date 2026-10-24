-- M8: the account pipeline with qualification (docs §15.2).

-- A company (account) in one product's pipeline. An open account sits in
-- one configured stage and has exactly one next action with a due date. A
-- paused or lost account has a reason and a revisit date instead, and keeps
-- the stage it left so it can be reopened.
CREATE TABLE accounts (
    id              bigserial PRIMARY KEY,
    product_id      text NOT NULL REFERENCES products(id),
    company_id      bigint NOT NULL REFERENCES companies(id),
    status          text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'paused', 'lost')),
    stage           text NOT NULL,
    entered_at      timestamptz NOT NULL,       -- entered its current stage, or was paused/lost
    next_action     text NOT NULL DEFAULT '',
    next_due        date,
    reason          text NOT NULL DEFAULT '',   -- why it was paused or lost
    revisit_on      date,
    revisit_task_id bigint REFERENCES tasks(id), -- the task made on the revisit date, once
    goal            text NOT NULL DEFAULT '',   -- what the account wants to achieve
    timing          text NOT NULL DEFAULT '',   -- what is driving the timing (why now)
    baseline        text NOT NULL DEFAULT '',   -- how they do it today
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (product_id, company_id),
    CHECK (CASE WHEN status = 'open'
        THEN next_action <> '' AND next_due IS NOT NULL AND reason = '' AND revisit_on IS NULL
        ELSE next_action = '' AND next_due IS NULL AND reason <> '' AND revisit_on IS NOT NULL END)
);
CREATE INDEX accounts_board ON accounts(product_id, status, stage);

-- Every stage an account entered, and every pause or loss, with the note
-- that justified it. The latest row is the account's current stay.
CREATE TABLE account_stages (
    id         bigserial PRIMARY KEY,
    account_id bigint NOT NULL REFERENCES accounts(id),
    stage      text NOT NULL,                  -- a pipeline stage, or paused/lost
    entered_at timestamptz NOT NULL,
    evidence   text NOT NULL DEFAULT '',
    reason     text NOT NULL DEFAULT '',
    revisit_on date
);
CREATE INDEX account_stages_account ON account_stages(account_id, id);

-- Qualification answers, one per check: yes, no or unknown with one line
-- of evidence.
CREATE TABLE account_checks (
    account_id  bigint NOT NULL REFERENCES accounts(id),
    name        text NOT NULL,
    answer      text NOT NULL CHECK (answer IN ('yes', 'no', 'unknown')),
    evidence    text NOT NULL DEFAULT '',
    answered_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, name)
);

CREATE TABLE account_links (
    id         bigserial PRIMARY KEY,
    account_id bigint NOT NULL REFERENCES accounts(id),
    url        text NOT NULL,
    note       text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, url)
);

-- People on the account and their role (champion, buyer, user, ...).
CREATE TABLE account_people (
    account_id bigint NOT NULL REFERENCES accounts(id),
    person_id  bigint NOT NULL REFERENCES people(id),
    role       text NOT NULL DEFAULT 'contact',
    added_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, person_id)
);
CREATE INDEX account_people_person ON account_people(person_id);

-- Tasks and automation runs about an account.
ALTER TABLE tasks ADD COLUMN account_id bigint REFERENCES accounts(id);
CREATE INDEX tasks_account ON tasks(account_id) WHERE account_id IS NOT NULL;
ALTER TABLE play_runs ADD COLUMN account_id bigint REFERENCES accounts(id);

-- The pipeline once a week, one row per account, for CSV export.
CREATE TABLE pipeline_snapshots (
    week          date NOT NULL,
    product_id    text NOT NULL REFERENCES products(id),
    account_id    bigint NOT NULL REFERENCES accounts(id),
    company       text NOT NULL,
    domain        text NOT NULL DEFAULT '',
    status        text NOT NULL,
    stage         text NOT NULL,
    days_in_stage int NOT NULL,
    stuck         boolean NOT NULL,
    next_action   text NOT NULL DEFAULT '',
    next_due      date,
    overdue       boolean NOT NULL,
    reason        text NOT NULL DEFAULT '',
    revisit_on    date,
    checks        jsonb NOT NULL DEFAULT '{}',  -- check name -> yes/no/unknown
    taken_at      timestamptz NOT NULL,
    PRIMARY KEY (week, product_id, account_id)
);
