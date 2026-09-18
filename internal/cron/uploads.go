package cron

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/pkg/notify"
)

// uploadGracePeriod is how long a file is left alone regardless of whether
// anything points at it. An image is written to disk by the frontend upload
// action and only becomes referenced when the host saves the quiz — a sweep
// running in that gap would delete the picture out from under someone still
// editing.
const uploadGracePeriod = 48 * time.Hour

// StartUploadCleanupWorker deletes uploaded images that nothing refers to.
//
// There is no uploads table and no per-file owner row: the URL is embedded in
// the JSON of questions.options / questions.content / questions.explanation,
// quizzes.theme_config and templates.questions. So reference counting is not available, and the sweep is
// mark-and-sweep instead — list what is on disk, ask the database which names
// still appear anywhere, delete the rest once they are past the grace period.
//
// Deliberately conservative: a row that is soft-deleted still counts as a
// reference, because questions carry no DeletedAt and a soft-deleted quiz keeps
// its questions. Better to keep a file nobody can see than to delete one a
// restore would need.
func StartUploadCleanupWorker() {
	dir := os.Getenv("UPLOAD_DIR")
	if dir == "" {
		log.Println("Upload cleanup worker disabled: UPLOAD_DIR is not set.")
		return
	}
	if _, err := os.Stat(dir); err != nil {
		log.Printf("Upload cleanup worker disabled: %s is not readable: %v", dir, err)
		return
	}

	log.Printf("Starting orphaned uploads cleanup worker (dir=%s)...", dir)
	go func() {
		time.Sleep(2 * time.Minute)
		sweepOrphanedUploads(dir)

		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()

		for range ticker.C {
			sweepOrphanedUploads(dir)
		}
	}()
}

func sweepOrphanedUploads(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("[CRON ERROR] upload sweep could not read %s: %v", dir, err)
		notify.P1("upload_sweep_dir", "Sweep ảnh không đọc được thư mục %s: %v", dir, err)
		return
	}

	referenced, err := referencedUploadNames()
	if err != nil {
		log.Printf("[CRON ERROR] upload sweep could not load references: %v", err)
		notify.P1("upload_sweep_refs", "Sweep ảnh không đọc được danh sách tham chiếu: %v — đã dừng an toàn (không xoá nhầm), nhưng ảnh rác sẽ tích tụ.", err)
		return
	}

	cutoff := time.Now().Add(-uploadGracePeriod)
	var removed, freed int64

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if _, ok := referenced[name]; ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue // still inside the editing window
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			log.Printf("[CRON ERROR] could not delete orphaned upload %s: %v", name, err)
			continue
		}
		removed++
		freed += info.Size()
	}

	if removed > 0 {
		log.Printf("[CRON] Removed %d orphaned upload(s), freed %d KB.", removed, freed/1024)
	} else {
		log.Println("[CRON] No orphaned uploads to remove.")
	}
}

// referencedUploadNames returns the set of upload file names that appear
// anywhere in the database.
//
// The columns are free-form text holding JSON, so this reads them whole and
// scans for the "/uploads/" marker rather than trying to parse each shape. The
// volume is small (one row per question) and the alternative — a LIKE per file —
// would be one query per file on disk.
func referencedUploadNames() (map[string]struct{}, error) {
	refs := make(map[string]struct{})

	queries := []string{
		`SELECT content     FROM questions WHERE content     LIKE '%/uploads/%'`,
		`SELECT options     FROM questions WHERE options     LIKE '%/uploads/%'`,
		`SELECT theme_config FROM quizzes  WHERE theme_config LIKE '%/uploads/%'`,
		`SELECT questions   FROM templates WHERE questions   LIKE '%/uploads/%'`,
		// Explanation slides embed their image the same way. Without this row
		// a picture used only by a slide looks unreferenced and the sweep
		// deletes it once the grace period expires.
		`SELECT explanation FROM questions WHERE explanation LIKE '%/uploads/%'`,
	}

	for _, q := range queries {
		var blobs []string
		if err := db.DB.Raw(q).Scan(&blobs).Error; err != nil {
			return nil, err
		}
		for _, b := range blobs {
			collectUploadNames(b, refs)
		}
	}
	return refs, nil
}

// collectUploadNames pulls every "/uploads/<name>" file name out of a blob of
// text. Names stop at the first character that cannot appear in one, so a URL
// embedded in JSON ("...\"image\":\"/uploads/img-1.jpg\"...") yields img-1.jpg
// and not the quote after it.
func collectUploadNames(blob string, out map[string]struct{}) {
	const marker = "/uploads/"
	for {
		i := strings.Index(blob, marker)
		if i < 0 {
			return
		}
		blob = blob[i+len(marker):]
		end := strings.IndexFunc(blob, func(r rune) bool {
			return !(r == '-' || r == '_' || r == '.' ||
				(r >= '0' && r <= '9') ||
				(r >= 'a' && r <= 'z') ||
				(r >= 'A' && r <= 'Z'))
		})
		name := blob
		if end >= 0 {
			name = blob[:end]
		}
		if name != "" {
			out[name] = struct{}{}
		}
	}
}
