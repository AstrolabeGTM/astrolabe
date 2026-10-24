-- M8: campaigns and creative. products/<id>/campaigns/<id>.yaml gives the
-- defaults; everything an operator changes at runtime lives here, with history.
-- No foreign keys to tables other features add (campaigns are keyed by
-- product id and campaign file id).

-- Overrides of the YAML defaults. NULL means "use the YAML value".
CREATE TABLE campaign_settings (
    product_id    text NOT NULL REFERENCES products(id),
    campaign_id   text NOT NULL,
    budget_cap    bigint CHECK (budget_cap >= 0),   -- minor units
    currency      text,
    start_date    date,
    end_date      date,
    end_cleared   boolean NOT NULL DEFAULT false,   -- end date deliberately removed
    stop_text     text,
    stop_spend_pct int,
    stop_max_events int,
    stop_cleared  boolean NOT NULL DEFAULT false,   -- machine rule deliberately removed
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (product_id, campaign_id)
);

-- Every change to a setting or a gate check: who, when, old and new value.
CREATE TABLE campaign_history (
    id          bigserial PRIMARY KEY,
    product_id  text NOT NULL REFERENCES products(id),
    campaign_id text NOT NULL,
    field       text NOT NULL,        -- budget_cap, currency, start, end, stop_rule, gate:<item>
    old_value   text NOT NULL DEFAULT '',
    new_value   text NOT NULL DEFAULT '',
    changed_by  text NOT NULL,        -- mcp, cli, web, ...
    note        text NOT NULL DEFAULT '',
    changed_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX campaign_history_campaign ON campaign_history(product_id, campaign_id, changed_at);

-- Gate items that are checked. A row exists only while the item is checked.
CREATE TABLE campaign_gate (
    product_id  text NOT NULL REFERENCES products(id),
    campaign_id text NOT NULL,
    item        text NOT NULL,
    checked_by  text NOT NULL,
    note        text NOT NULL DEFAULT '',
    checked_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (product_id, campaign_id, item)
);

-- Spend as entered by hand or imported from a platform's CSV. gate_complete
-- is the gate's state when the row was written and never changes afterwards:
-- checking the gate later does not erase that the spend came first.
CREATE TABLE campaign_spend (
    id            bigserial PRIMARY KEY,
    product_id    text NOT NULL REFERENCES products(id),
    campaign_id   text NOT NULL,
    spent_on      date NOT NULL,
    amount        bigint NOT NULL CHECK (amount >= 0),  -- minor units
    currency      text NOT NULL,
    platform      text NOT NULL,
    note          text NOT NULL DEFAULT '',
    clicks        bigint CHECK (clicks >= 0),           -- the platform's own count, as entered
    impressions   bigint CHECK (impressions >= 0),
    source        text NOT NULL DEFAULT 'manual' CHECK (source IN ('manual', 'csv')),
    gate_complete boolean NOT NULL,
    created_by    text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX campaign_spend_campaign ON campaign_spend(product_id, campaign_id, spent_on);
-- Importing the same CSV again replaces its rows instead of adding to them.
CREATE UNIQUE INDEX campaign_spend_csv ON campaign_spend(product_id, campaign_id, platform, spent_on) WHERE source = 'csv';

-- Creative state: edits to the YAML copy, the version and the approval.
-- Effective copy = the YAML copy with copy_overrides on top.
CREATE TABLE creatives (
    product_id     text NOT NULL REFERENCES products(id),
    campaign_id    text NOT NULL,
    creative_id    text NOT NULL,
    copy_overrides jsonb NOT NULL DEFAULT '{}',
    alt_override   text,
    version        int NOT NULL DEFAULT 1,
    approved_hash  text,              -- hash of copy, template and rendered files when approved
    approved_by    text,
    approved_at    timestamptz,
    updated_by     text NOT NULL DEFAULT '',
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (product_id, campaign_id, creative_id)
);

CREATE TABLE creative_renders (
    product_id  text NOT NULL,
    campaign_id text NOT NULL,
    creative_id text NOT NULL,
    size        text NOT NULL,
    format      text NOT NULL CHECK (format IN ('png', 'pdf')),
    content     bytea NOT NULL,
    sha256      text NOT NULL,
    rendered_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (product_id, campaign_id, creative_id, size, format),
    FOREIGN KEY (product_id, campaign_id, creative_id) REFERENCES creatives ON DELETE CASCADE
);

-- Posting tasks made from a campaign reuse content_items and tasks.
ALTER TABLE content_items ADD COLUMN campaign_id text;
ALTER TABLE content_items ADD COLUMN campaign_task text;
CREATE UNIQUE INDEX content_items_campaign_task ON content_items(product_id, campaign_id, campaign_task) WHERE campaign_task IS NOT NULL;
