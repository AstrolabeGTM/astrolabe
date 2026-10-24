-- M8: public counters (daily snapshots) and the reception log.

-- One value per series per day; a rerun on the same day replaces the day's
-- value, so counts are never double-counted. metric is downloads, stars,
-- forks, watchers, open_issues or discussions. tag and asset are set only
-- for downloads (release tag and asset file name).
CREATE TABLE counter_snapshots (
    product_id text   NOT NULL REFERENCES products(id),
    monitor_id text   NOT NULL,
    repo       text   NOT NULL,
    day        date   NOT NULL,
    metric     text   NOT NULL,
    tag        text   NOT NULL DEFAULT '',
    asset      text   NOT NULL DEFAULT '',
    value      bigint NOT NULL,
    taken_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (product_id, monitor_id, day, metric, tag, asset)
);
CREATE INDEX counter_snapshots_series ON counter_snapshots(product_id, monitor_id, metric, tag, asset, day);

-- Public mentions of a product. One row per URL per product.
CREATE TABLE reception_mentions (
    id            bigserial PRIMARY KEY,
    product_id    text NOT NULL REFERENCES products(id),
    url           text NOT NULL,
    mentioned_at  timestamptz NOT NULL,
    source        text NOT NULL DEFAULT 'manual',   -- hn, stackoverflow, rss, github, manual, ...
    title         text NOT NULL DEFAULT '',
    tone          text NOT NULL DEFAULT 'unknown' CHECK (tone IN ('positive', 'mixed', 'negative', 'unknown')),
    asks_question boolean NOT NULL DEFAULT false,
    found_by      text NOT NULL DEFAULT 'manual',   -- monitor:<id> or manual
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (product_id, url)
);
CREATE INDEX reception_mentions_when ON reception_mentions(product_id, mentioned_at);
