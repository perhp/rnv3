-- rnv3 initial schema.
-- One unified passes table with a state machine replaces RN2's
-- predict_passes/decoded_passes split; images replaces filesystem globbing.

CREATE TABLE passes (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    satellite         TEXT    NOT NULL,
    start_ts          INTEGER NOT NULL,             -- unix epoch, AOS
    end_ts            INTEGER NOT NULL,             -- unix epoch, LOS
    max_elevation     REAL    NOT NULL,
    start_azimuth     REAL,
    azimuth_at_max    REAL,
    direction         TEXT,                         -- 'northbound' | 'southbound'
    state             TEXT    NOT NULL DEFAULT 'scheduled',
        -- scheduled | capturing | processing | decoded | failed | skipped | cancelled
    error_text        TEXT,
    daylight          INTEGER,                      -- 0/1, classified at capture time
    gain              REAL,
    max_snr           REAL,
    avg_snr           REAL,
    frames_received   INTEGER,
    frames_expected   INTEGER,
    frame_loss_pct    REAL,
    largest_frame_gap INTEGER,
    file_base         TEXT,                         -- image filename base, e.g. NOAA-19-20260831-121314
    created_ts        INTEGER NOT NULL DEFAULT (strftime('%s','now')),
    updated_ts        INTEGER NOT NULL DEFAULT (strftime('%s','now')),
    UNIQUE (satellite, start_ts)
);

CREATE INDEX idx_passes_start ON passes (start_ts);
CREATE INDEX idx_passes_state ON passes (state);

CREATE TABLE images (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    pass_id    INTEGER NOT NULL REFERENCES passes (id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL,  -- enhancement name, 'polar-azel', 'polar-direction', 'website-thumbnail', ...
    path       TEXT    NOT NULL,  -- relative to paths.images
    thumb_path TEXT,              -- relative to paths.thumbs, NULL if none
    created_ts INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);

CREATE INDEX idx_images_pass ON images (pass_id);
