package process

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/perhp/rnv3/internal/store"
)

func TestRemoveCaptureKeepsRowWhenAFileCannotBeDeleted(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tx, _ := st.DB.Begin()
	id, _, err := st.InsertImported(tx, store.ImportedPass{Satellite: "NOAA 19", StartTS: 1, EndTS: 2, MaxElevation: 1, State: store.StateDecoded, FileBase: "X"})
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	st.AddImage(id, "MCIR", "X-MCIR.jpg", "X-MCIR.jpg")
	st.AddImage(id, "MSA", "X-MSA.jpg", "")
	for _, p := range []string{"X-MCIR.jpg", "X-MSA.jpg", "thumb/X-MCIR.jpg"} {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), []byte("x"), 0o644)
	}

	stuck := filepath.Join(dir, "X-MSA.jpg")
	orig := removeFile
	removeFile = func(path string) error {
		if path == stuck {
			return errors.New("permission denied")
		}
		return os.Remove(path)
	}
	defer func() { removeFile = orig }()

	if err := RemoveCapture(st, dir, filepath.Join(dir, "thumb"), id); err == nil {
		t.Fatal("expected an error when a file cannot be deleted")
	}
	if p, _ := st.PassByID(id); p == nil {
		t.Error("row deleted although a file survived — retention could never retry it")
	}
	if _, err := os.Stat(filepath.Join(dir, "X-MCIR.jpg")); !os.IsNotExist(err) {
		t.Error("deletable files should still have been removed")
	}
	removeFile = os.Remove
	if err := RemoveCapture(st, dir, filepath.Join(dir, "thumb"), id); err != nil {
		t.Fatalf("retry after the file became deletable: %v", err)
	}
	if p, _ := st.PassByID(id); p != nil {
		t.Error("row should be gone after a clean retry")
	}
}
