package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"

	tele "gopkg.in/telebot.v3"
	_ "modernc.org/sqlite"
)

func initDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// SQLite supports only one writer at a time. A single connection
	// serialises all operations and eliminates SQLITE_BUSY errors.
	db.SetMaxOpenConns(1)

	schema := `
	CREATE VIRTUAL TABLE IF NOT EXISTS memes USING fts5(
		file_id   UNINDEXED,
		msg_id    UNINDEXED,
		original_desc,
		search_vector,
		tokenize  = "unicode61"
	);

	CREATE TABLE IF NOT EXISTS indexed_msgs (
		msg_id INTEGER PRIMARY KEY
	);

	CREATE TABLE IF NOT EXISTS crawler_state (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS image_hashes (
		phash INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS failed_msgs (
		msg_id  INTEGER PRIMARY KEY,
		file_id TEXT NOT NULL
	);
	`

	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("init schema: %w", err)
	}

	return db, nil
}

func resetDB(db *sql.DB) error {
	for _, table := range []string{"memes", "indexed_msgs", "crawler_state", "image_hashes", "failed_msgs"} {
		if _, err := db.Exec("DELETE FROM " + table); err != nil {
			return err
		}
	}
	return nil
}

func isAlreadyIndexed(db *sql.DB, msgID int) (bool, error) {
	var n int
	err := db.QueryRow("SELECT 1 FROM indexed_msgs WHERE msg_id = ? LIMIT 1", msgID).Scan(&n)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func saveMeme(db *sql.DB, fileID string, msgID int, originalDesc string) error {
	sv := buildSearchVector(originalDesc)

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err = tx.Exec(
		`INSERT INTO memes(file_id, msg_id, original_desc, search_vector) VALUES (?, ?, ?, ?)`,
		fileID, strconv.Itoa(msgID), originalDesc, sv,
	); err != nil {
		return fmt.Errorf("insert meme: %w", err)
	}

	if _, err = tx.Exec("INSERT OR IGNORE INTO indexed_msgs(msg_id) VALUES (?)", msgID); err != nil {
		return fmt.Errorf("insert indexed_msg: %w", err)
	}

	return tx.Commit()
}

// truncateCaption safely trims s to at most 1024 Unicode code points (Telegram caption limit).
func truncateCaption(s string) string {
	const limit = 1024
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit-3]) + "..."
}

func searchMemes(db *sql.DB, ftsQuery string) ([]tele.Result, error) {
	rows, err := db.Query(
		`SELECT file_id, rowid, original_desc FROM memes WHERE search_vector MATCH ? ORDER BY rank LIMIT 50`,
		ftsQuery,
	)
	if err != nil {
		return nil, fmt.Errorf("fts query: %w", err)
	}
	defer rows.Close()

	results := make([]tele.Result, 0)
	for rows.Next() {
		var fileID, originalDesc string
		var rowid int64
		if err := rows.Scan(&fileID, &rowid, &originalDesc); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		results = append(results, &tele.PhotoResult{
			ResultBase: tele.ResultBase{ID: strconv.FormatInt(rowid, 10)},
			Cache:      fileID,
			Caption:    truncateCaption(originalDesc),
		})
	}

	return results, rows.Err()
}

func getCrawlerState(db *sql.DB, key string) (string, error) {
	var value string
	err := db.QueryRow("SELECT value FROM crawler_state WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func setCrawlerState(db *sql.DB, key, value string) error {
	_, err := db.Exec(
		"INSERT OR REPLACE INTO crawler_state(key, value) VALUES (?, ?)",
		key, value,
	)
	return err
}

// ─── Perceptual image hashing (dHash) ────────────────────────────────────────

// dHashThreshold is the max Hamming distance (out of 64) to treat two images
// as duplicates. 8 catches recompressed / resized copies; raise to allow
// more variation, lower for stricter matching.
const dHashThreshold = 8

// computeDHash returns a 64-bit difference hash for the image.
// Returns 0 for unsupported or undecodable formats — callers must skip dedup when hash == 0.
func computeDHash(imgBytes []byte) uint64 {
	img, _, err := image.Decode(bytes.NewReader(imgBytes))
	if err != nil {
		return 0
	}
	const cols, rows = 9, 8
	bounds := img.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()

	// Sample a cols×rows grid using nearest-neighbour and convert to grayscale.
	var grid [rows][cols]uint8
	for y := range rows {
		for x := range cols {
			sx := bounds.Min.X + x*srcW/cols
			sy := bounds.Min.Y + y*srcH/rows
			r, g, b, _ := img.At(sx, sy).RGBA()
			grid[y][x] = color.Gray{Y: uint8((19595*r + 38470*g + 7471*b + 1<<15) >> 24)}.Y
		}
	}

	// Each bit: 1 if left pixel is brighter than right neighbour.
	var hash uint64
	for y := range rows {
		for x := range cols - 1 {
			if grid[y][x] > grid[y][x+1] {
				hash |= 1 << uint(y*(cols-1)+x)
			}
		}
	}
	return hash
}

// loadHashes reads all perceptual hashes from the DB into a slice for
// in-memory duplicate detection. Call once at startup and pass the slice
// to the worker so every check avoids a round-trip to SQLite.
func loadHashes(db *sql.DB) ([]uint64, error) {
	rows, err := db.Query("SELECT phash FROM image_hashes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hashes []uint64
	for rows.Next() {
		var h uint64
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		hashes = append(hashes, h)
	}
	return hashes, rows.Err()
}

// isDuplicate returns true when hash is within dHashThreshold Hamming distance
// of any hash in the supplied slice. O(n) in-memory scan; no DB access.
func isDuplicate(hashes []uint64, hash uint64) bool {
	if hash == 0 {
		return false
	}
	for _, stored := range hashes {
		if bits.OnesCount64(hash^stored) <= dHashThreshold {
			return true
		}
	}
	return false
}

// storeImageHash saves a perceptual hash so future duplicates are detected.
func storeImageHash(db *sql.DB, hash uint64) error {
	if hash == 0 {
		return nil
	}
	_, err := db.Exec("INSERT INTO image_hashes(phash) VALUES (?)", hash)
	return err
}
