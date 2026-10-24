-- Saved weekly reviews: the operator's answers, a keep/change/stop decision
-- per running experiment and campaign, and the review as built when it was
-- first saved (frozen, so a past week does not drift as new data arrives).
CREATE TABLE weekly_reviews (
    product_id text NOT NULL REFERENCES products(id),
    iso_week   text NOT NULL,                   -- like 2026-W43
    answers    jsonb NOT NULL DEFAULT '[]',     -- [{question, answer}]
    decisions  jsonb NOT NULL DEFAULT '[]',     -- [{kind: campaign|experiment, id, decision: keep|change|stop, note}]
    snapshot   jsonb NOT NULL,                  -- the built review at first save
    created_at timestamptz NOT NULL DEFAULT now(),
    saved_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (product_id, iso_week)
);
