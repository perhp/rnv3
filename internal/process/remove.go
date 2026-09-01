package process

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/perhp/rnv3/internal/store"
)

// removeFile is a seam for tests that need a deletion to fail.
var removeFile = os.Remove

// OnCaptureRemoved, when set, is told about every capture whose rows were
// deleted (the event webhooks publish pass.deleted from it).
var OnCaptureRemoved func(passID int64)

// RemoveCapture deletes a pass's files (every registered image and
// thumbnail, plus the website thumbnail) and then its database rows. Used by
// the admin page and by retention pruning.
//
// The rows go only once every file is gone: a file that cannot be deleted
// (permissions, transient I/O) keeps its row, so the next pruning run finds
// and retries it instead of leaving an orphan nothing will ever discover.
func RemoveCapture(st *store.Store, imagesDir, thumbsDir string, id int64) error {
	p, err := st.PassByID(id)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("pass %d not found", id)
	}
	images, err := st.ImagesForPass(id)
	if err != nil {
		return err
	}
	var failures []error
	remove := func(path string) {
		if err := removeFile(path); err != nil && !os.IsNotExist(err) {
			failures = append(failures, err)
		}
	}
	for _, im := range images {
		if im.Path != "" {
			remove(filepath.Join(imagesDir, filepath.Base(im.Path)))
		}
		if im.ThumbPath != "" {
			remove(filepath.Join(thumbsDir, filepath.Base(im.ThumbPath)))
		}
	}
	if p.FileBase != "" { // belt and braces for imported captures
		remove(filepath.Join(thumbsDir, p.FileBase+"-website-thumbnail.jpg"))
	}
	if len(failures) > 0 {
		return fmt.Errorf("pass %d kept: %d file(s) could not be deleted: %w", id, len(failures), errors.Join(failures...))
	}
	if err := st.DeletePass(id); err != nil {
		return err
	}
	if OnCaptureRemoved != nil {
		OnCaptureRemoved(id)
	}
	return nil
}
