-- M8: form intake (docs §15.5). One row per submission of a form, from a
-- Netlify form or a signed webhook, kept once by (product, monitor, source,
-- external id) so re-polls and retries never duplicate anything.
CREATE TABLE form_submissions (
    id              bigserial PRIMARY KEY,
    product_id      text NOT NULL REFERENCES products(id),
    monitor_id      text NOT NULL,
    source          text NOT NULL CHECK (source IN ('netlify', 'webhook')),
    external_id     text NOT NULL,
    submitted_at    timestamptz NOT NULL,
    fields          jsonb NOT NULL DEFAULT '{}',   -- everything submitted, by the form's field names
    reported_source text NOT NULL DEFAULT '',       -- "how did you hear about us", as written
    campaign        text NOT NULL DEFAULT '',
    person_id       bigint REFERENCES people(id),
    company_id      bigint REFERENCES companies(id),
    account_id      bigint REFERENCES accounts(id),
    signal_id       bigint REFERENCES signals(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (product_id, monitor_id, source, external_id)
);
CREATE INDEX form_submissions_recent ON form_submissions(product_id, submitted_at DESC);
CREATE INDEX form_submissions_person ON form_submissions(person_id) WHERE person_id IS NOT NULL;

-- What a person said about how they heard of the product, per product, as
-- they wrote it. Separate from `source`, the first touch Astrolabe saw.
ALTER TABLE product_people ADD COLUMN reported_source text NOT NULL DEFAULT '';
