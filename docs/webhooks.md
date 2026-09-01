# rnv3 event webhooks

rnv3 can push everything that happens at the station to one or more HTTP
receivers: every decoded pass with its images, failed passes, the upcoming
schedule, station health readings and alerts. The receiver decides what to
do with them — store them in a database, mirror images to a CDN, post to a
chat, anything. rnv3 knows nothing about the backend.

## Configuration

```yaml
publish:
  backfill_days: 31              # on start, (re)send decoded passes newer than this
  endpoints:
    - name: my-site
      url: https://example.org/api/station/webhook
      token: change-me            # sent as "Authorization: Bearer change-me"
      events: []                  # empty = all events; or a list, e.g. [pass.decoded, pass.image]
      images: true                # send pass.image events (the files) to this endpoint
```

`rnv3-setup` asks for the URL and the secret in its notifications section.

## Delivery

Every event is one `POST` to the endpoint URL.

| Header | Value |
|---|---|
| `Authorization` | `Bearer <token>` |
| `Content-Type` | `application/json`, or `multipart/form-data` for `pass.image` |
| `X-Rnv3-Event` | the event name (`pass.decoded`, …) |
| `X-Rnv3-Delivery` | a UUID unique to this delivery attempt |
| `X-Rnv3-Station` | the station name from `station.name` |
| `User-Agent` | `rnv3/<version>` |

A `2xx` response means delivered. Anything else — or no response — is a
failure:

- **Pass events** (`pass.decoded`, `pass.image`, `pass.failed`,
  `pass.deleted`) are durable: they are queued in rnv3's database and
  retried with exponential backoff (1 min → 1 h) for up to 7 days. Order is
  preserved per endpoint: a pass's `pass.image` events are sent after its
  `pass.decoded`.
- **State events** (`schedule.updated`, `station.stats`) are not retried; the
  next replan / the next 5-minute sample sends fresh data anyway.
- **`station.alert`** is sent once and not retried.

Receivers must be idempotent: a delivery can be repeated after a timeout,
and after a restart rnv3 re-sends decoded passes newer than `backfill_days`
(a receiver that upserts on the pass id and image name handles this
naturally).

## Envelope

JSON events:

```json
{
  "version": 1,
  "event": "pass.decoded",
  "sent_at": "2026-09-01T12:34:56Z",
  "station": { "name": "raspinoaa", "location": "Copenhagen", "latitude": 55.68, "longitude": 12.57 },
  "data": { ... }
}
```

`pass.image` is `multipart/form-data` with two parts: `payload` (the same
envelope as JSON) and `file` (the image bytes, with its filename and
content type).

## Events

### `pass.decoded`

A pass finished and produced images. `data`:

```json
{
  "pass": {
    "id": 4711,
    "satellite": "METEOR-M2 3",
    "satellite_type": "meteor-lrpt",
    "status": "decoded",
    "start": "2026-09-01T12:00:00Z",
    "end": "2026-09-01T12:11:40Z",
    "max_elevation": 71.2,
    "start_azimuth": 12,
    "azimuth_at_max": 95,
    "direction": "northbound",
    "daylight": true,
    "gain": 40.2,
    "max_snr": 11.5,
    "avg_snr": 7.2,
    "frames_received": 900,
    "frames_expected": 1000,
    "frame_loss_pct": 10.0,
    "largest_frame_gap": 30
  },
  "images": [
    { "name": "METEOR-M2-3-20260901-120000-221_projected.jpg", "kind": "221_projected", "content_type": "image/jpeg", "size": 1834023, "graph": false },
    { "name": "METEOR-M2-3-20260901-120000-polar-azel.svg", "kind": "polar-azel", "content_type": "image/svg+xml", "size": 9120, "graph": true }
  ]
}
```

`id` is stable for the life of the pass and unique per station. Nullable
numbers (`max_snr`, frame stats, `gain` unknown) are `null`. `graph` marks
auxiliary plots (polar plots, spectrogram, histogram) as opposed to
satellite imagery. The website thumbnail is not included.

### `pass.image`

One image of a decoded pass, sent after its `pass.decoded`. `data`:

```json
{ "pass_id": 4711, "image": { "name": "…-221_projected.jpg", "kind": "221_projected", "content_type": "image/jpeg", "size": 1834023, "graph": false } }
```

plus the `file` part. Endpoints with `images: false` receive no
`pass.image` events; they can fetch images from the panel
(`http://<station>/images/<name>`) if reachable.

### `pass.failed`

A pass ran but produced nothing. `data.pass` as in `pass.decoded` with
`"status": "failed"` and `"error": "…"`; no `images`.

### `pass.deleted`

A capture was removed (admin page or retention pruning). `data`:
`{ "pass_id": 4711 }`.

### `schedule.updated`

The upcoming plan after each replan (TLE refresh, config reload, admin
cancel). `data.passes` lists every scheduled pass, soonest first:

```json
{ "satellite": "NOAA 19", "start": "…", "end": "…", "max_elevation": 62.4, "start_azimuth": 190, "azimuth_at_max": 250, "direction": "southbound" }
```

Passes rnv3 decided to skip (overlaps, sun gate) are not included.

### `station.stats`

Every 5 minutes. `data`:

```json
{
  "recorded_at": "…",
  "cpu_temperature_c": 52.1,
  "cpu_usage_percent": 7.4,
  "memory_total_bytes": 3980000000,
  "memory_used_bytes": 900000000,
  "disk_total_bytes": 62000000000,
  "disk_used_bytes": 21000000000,
  "uptime_ms": 86400000,
  "load_1m": 0.42
}
```

Fields the host cannot provide are `null`. Disk figures are for the
filesystem holding the images.

### `station.alert`

A health watchdog alert. `data`: `{ "check": "disk_usage", "message": "…" }`.

## Testing a receiver

`rnv3 -config … -publish-test` sends a synthetic `station.stats` event to
every endpoint and reports the responses.
