package store

import (
	"database/sql"
	"time"
)

// OutboxEntry is one queued webhook delivery.
type OutboxEntry struct {
	ID        int64
	Endpoint  string
	Event     string
	PassID    int64
	ImageName string
	Payload   string
	Attempts  int
	NextTS    int64
	CreatedTS int64
}

// Enqueue appends entries in one transaction, preserving their order.
func (s *Store) Enqueue(entries ...OutboxEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO outbox (endpoint, event, pass_id, image_name, payload) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range entries {
		if _, err := stmt.Exec(e.Endpoint, e.Event, e.PassID, nullIfEmpty(e.ImageName), nullIfEmpty(e.Payload)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DueOutbox lists an endpoint's deliveries whose retry time has come, in
// queue order.
func (s *Store) DueOutbox(endpoint string, now time.Time, limit int) ([]OutboxEntry, error) {
	rows, err := s.DB.Query(`SELECT id, endpoint, event, pass_id, COALESCE(image_name, ''), COALESCE(payload, ''), attempts, next_ts, created_ts
		FROM outbox WHERE endpoint = ? AND next_ts <= ? ORDER BY id LIMIT ?`, endpoint, now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxEntry
	for rows.Next() {
		var e OutboxEntry
		if err := rows.Scan(&e.ID, &e.Endpoint, &e.Event, &e.PassID, &e.ImageName, &e.Payload, &e.Attempts, &e.NextTS, &e.CreatedTS); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// OutboxHead is the oldest queued entry for an endpoint regardless of its
// retry time (nil when the queue is empty). Deliveries never overtake it.
func (s *Store) OutboxHead(endpoint string) (*OutboxEntry, error) {
	var e OutboxEntry
	err := s.DB.QueryRow(`SELECT id, endpoint, event, pass_id, COALESCE(image_name, ''), COALESCE(payload, ''), attempts, next_ts, created_ts
		FROM outbox WHERE endpoint = ? ORDER BY id LIMIT 1`, endpoint).
		Scan(&e.ID, &e.Endpoint, &e.Event, &e.PassID, &e.ImageName, &e.Payload, &e.Attempts, &e.NextTS, &e.CreatedTS)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// OutboxEndpoints lists endpoints that have queued deliveries.
func (s *Store) OutboxEndpoints() ([]string, error) {
	rows, err := s.DB.Query(`SELECT DISTINCT endpoint FROM outbox`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// OutboxCount is the queue depth for an endpoint ("" = all).
func (s *Store) OutboxCount(endpoint string) (int, error) {
	var n int
	var err error
	if endpoint == "" {
		err = s.DB.QueryRow(`SELECT COUNT(*) FROM outbox`).Scan(&n)
	} else {
		err = s.DB.QueryRow(`SELECT COUNT(*) FROM outbox WHERE endpoint = ?`, endpoint).Scan(&n)
	}
	return n, err
}

// DeleteOutbox removes a delivered (or abandoned) entry.
func (s *Store) DeleteOutbox(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM outbox WHERE id = ?`, id)
	return err
}

// DeferOutbox records a failed attempt and the next time to try.
func (s *Store) DeferOutbox(id int64, attempts int, next time.Time) error {
	_, err := s.DB.Exec(`UPDATE outbox SET attempts = ?, next_ts = ? WHERE id = ?`, attempts, next.Unix(), id)
	return err
}

// DeleteOutboxEndpoint drops every queued delivery for an endpoint that is
// no longer configured.
func (s *Store) DeleteOutboxEndpoint(endpoint string) (int64, error) {
	res, err := s.DB.Exec(`DELETE FROM outbox WHERE endpoint = ?`, endpoint)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteOutboxOlderThan abandons deliveries queued before cutoff.
func (s *Store) DeleteOutboxOlderThan(cutoff time.Time) (int64, error) {
	res, err := s.DB.Exec(`DELETE FROM outbox WHERE created_ts < ?`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MarkPublished records that an endpoint has received a pass.
func (s *Store) MarkPublished(endpoint string, passID int64) error {
	_, err := s.DB.Exec(`INSERT INTO published (endpoint, pass_id) VALUES (?, ?)
		ON CONFLICT (endpoint, pass_id) DO UPDATE SET published_ts = strftime('%s','now')`, endpoint, passID)
	return err
}

// ForgetPublished drops the record for a deleted pass.
func (s *Store) ForgetPublished(passID int64) error {
	_, err := s.DB.Exec(`DELETE FROM published WHERE pass_id = ?`, passID)
	return err
}

// UnpublishedDecoded lists decoded passes starting after since that the
// endpoint has not received and that are not already queued (backfill).
func (s *Store) UnpublishedDecoded(endpoint string, since time.Time) ([]int64, error) {
	rows, err := s.DB.Query(`SELECT p.id FROM passes p
		WHERE p.state = ? AND p.start_ts > ?
		  AND NOT EXISTS (SELECT 1 FROM published pu WHERE pu.endpoint = ? AND pu.pass_id = p.id)
		  AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.endpoint = ? AND o.pass_id = p.id AND o.event = 'pass.decoded')
		ORDER BY p.start_ts`, StateDecoded, since.Unix(), endpoint, endpoint)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
