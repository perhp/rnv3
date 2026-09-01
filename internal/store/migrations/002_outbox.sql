-- Event webhook delivery: a durable outbox for pass events (retried with
-- backoff, ordered per endpoint) and the record of which passes each
-- endpoint has received (drives the startup backfill).

CREATE TABLE outbox (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    endpoint   TEXT    NOT NULL,             -- publish.endpoints[].name
    event      TEXT    NOT NULL,             -- pass.decoded | pass.image | pass.failed | pass.deleted
    pass_id    INTEGER NOT NULL,             -- not a foreign key: pass.deleted outlives the row
    image_name TEXT,                         -- pass.image only
    payload    TEXT,                         -- pre-rendered data for events whose source is gone
    attempts   INTEGER NOT NULL DEFAULT 0,
    next_ts    INTEGER NOT NULL DEFAULT 0,   -- earliest next delivery attempt
    created_ts INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);

CREATE INDEX idx_outbox_due ON outbox (endpoint, next_ts, id);

CREATE TABLE published (
    endpoint     TEXT    NOT NULL,
    pass_id      INTEGER NOT NULL,
    published_ts INTEGER NOT NULL DEFAULT (strftime('%s','now')),
    PRIMARY KEY (endpoint, pass_id)
);
