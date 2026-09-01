package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestOutboxQueueOrderDeferAndAbandon(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Enqueue(
		OutboxEntry{Endpoint: "a", Event: "pass.decoded", PassID: 1},
		OutboxEntry{Endpoint: "a", Event: "pass.image", PassID: 1, ImageName: "x.jpg"},
		OutboxEntry{Endpoint: "b", Event: "pass.deleted", PassID: 2, Payload: `{"pass_id":2}`},
	); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	due, _ := st.DueOutbox("a", now, 10)
	if len(due) != 2 || due[0].Event != "pass.decoded" || due[1].ImageName != "x.jpg" {
		t.Fatalf("due a = %+v", due)
	}
	eps, _ := st.OutboxEndpoints()
	if len(eps) != 2 {
		t.Errorf("endpoints = %v", eps)
	}
	st.DeferOutbox(due[0].ID, 3, now.Add(time.Hour))
	if due, _ = st.DueOutbox("a", now, 10); len(due) != 1 || due[0].Event != "pass.image" {
		t.Errorf("deferred entry still due, or order lost: %+v", due)
	}
	if due, _ = st.DueOutbox("a", now.Add(2*time.Hour), 10); len(due) != 2 || due[0].Attempts != 3 {
		t.Errorf("after backoff: %+v", due)
	}
	st.DeleteOutbox(due[1].ID)
	if n, _ := st.OutboxCount("a"); n != 1 {
		t.Errorf("count a = %d", n)
	}
	if n, _ := st.DeleteOutboxEndpoint("b"); n != 1 {
		t.Errorf("deleted for b = %d", n)
	}
	st.DB.Exec(`UPDATE outbox SET created_ts = ?`, now.Add(-10*24*time.Hour).Unix())
	if n, _ := st.DeleteOutboxOlderThan(now.Add(-7 * 24 * time.Hour)); n != 1 {
		t.Errorf("abandoned = %d", n)
	}
}

func TestPublishedTracking(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	tx, _ := st.DB.Begin()
	recent, _, _ := st.InsertImported(tx, ImportedPass{Satellite: "NOAA 19", StartTS: now.Unix() - 3600, EndTS: now.Unix() - 3000, MaxElevation: 50, State: StateDecoded, FileBase: "a"})
	old, _, _ := st.InsertImported(tx, ImportedPass{Satellite: "NOAA 19", StartTS: now.Unix() - 90*86400, EndTS: now.Unix() - 90*86400 + 600, MaxElevation: 50, State: StateDecoded, FileBase: "b"})
	st.InsertImported(tx, ImportedPass{Satellite: "NOAA 18", StartTS: now.Unix() - 7200, EndTS: now.Unix() - 6600, MaxElevation: 50, State: StateFailed, ErrorText: "x"})
	tx.Commit()
	since := now.Add(-31 * 24 * time.Hour)
	ids, _ := st.UnpublishedDecoded("site", since)
	if len(ids) != 1 || ids[0] != recent {
		t.Errorf("unpublished = %v (recent=%d old=%d)", ids, recent, old)
	}
	st.MarkPublished("site", recent)
	if ids, _ = st.UnpublishedDecoded("site", since); len(ids) != 0 {
		t.Errorf("still unpublished after marking: %v", ids)
	}
	if ids, _ = st.UnpublishedDecoded("other", since); len(ids) != 1 {
		t.Errorf("published state must be per endpoint: %v", ids)
	}
	// A queued pass.decoded also counts as handled.
	st.Enqueue(OutboxEntry{Endpoint: "other", Event: "pass.decoded", PassID: recent})
	if ids, _ = st.UnpublishedDecoded("other", since); len(ids) != 0 {
		t.Errorf("queued pass re-listed: %v", ids)
	}
	st.ForgetPublished(recent)
	if ids, _ = st.UnpublishedDecoded("site", since); len(ids) != 1 {
		t.Errorf("forget did not work: %v", ids)
	}
}
