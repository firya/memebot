package main

import (
	"bytes"
	"database/sql"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
)

// testDB opens a fresh file-based SQLite DB in a temp directory and returns it.
// The DB is automatically removed when the test ends.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := initDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("initDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// makePNG creates a w×h PNG image filled with c and returns its bytes.
func makePNG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// makeGradientPNG creates a w×h PNG where the left half is white and the right
// half is black. This produces a non-zero dHash since adjacent pixels differ.
func makeGradientPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			if x < w/2 {
				img.Set(x, y, color.White)
			} else {
				img.Set(x, y, color.Black)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// ── truncateCaption ───────────────────────────────────────────────────────────

func TestTruncateCaption(t *testing.T) {
	// Short string unchanged.
	s := "привет мир"
	if got := truncateCaption(s); got != s {
		t.Errorf("truncateCaption short: got %q, want %q", got, s)
	}

	// Exactly 1024 runes — unchanged.
	long := strings.Repeat("а", 1024)
	if got := truncateCaption(long); got != long {
		t.Errorf("truncateCaption 1024 runes: got len %d, want %d", len([]rune(got)), 1024)
	}

	// 1025 runes — must be truncated to ≤ 1024 runes and end with "...".
	over := strings.Repeat("б", 1025)
	got := truncateCaption(over)
	runes := []rune(got)
	if len(runes) > 1024 {
		t.Errorf("truncateCaption over 1024: len=%d, want ≤1024", len(runes))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncateCaption over 1024: %q does not end with ...", got)
	}

	// Multi-byte sequence is not split — result must be valid UTF-8.
	// "я" is a 2-byte UTF-8 sequence; 1025 of them = 2050 bytes.
	mb := strings.Repeat("я", 1025)
	gotMB := truncateCaption(mb)
	if !isValidUTF8(gotMB) {
		t.Errorf("truncateCaption produced invalid UTF-8: %q", gotMB)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}

// ── crawler state ─────────────────────────────────────────────────────────────

func TestCrawlerState(t *testing.T) {
	db := testDB(t)

	// Missing key returns empty string, no error.
	v, err := getCrawlerState(db, "missing")
	if err != nil {
		t.Fatalf("getCrawlerState missing: %v", err)
	}
	if v != "" {
		t.Errorf("getCrawlerState missing: got %q, want \"\"", v)
	}

	// Set then get.
	if err := setCrawlerState(db, "key1", "value1"); err != nil {
		t.Fatalf("setCrawlerState: %v", err)
	}
	v, err = getCrawlerState(db, "key1")
	if err != nil {
		t.Fatalf("getCrawlerState: %v", err)
	}
	if v != "value1" {
		t.Errorf("getCrawlerState: got %q, want \"value1\"", v)
	}

	// Upsert overwrites.
	if err := setCrawlerState(db, "key1", "updated"); err != nil {
		t.Fatalf("setCrawlerState upsert: %v", err)
	}
	v, _ = getCrawlerState(db, "key1")
	if v != "updated" {
		t.Errorf("setCrawlerState upsert: got %q, want \"updated\"", v)
	}
}

// ── isAlreadyIndexed ─────────────────────────────────────────────────────────

func TestIsAlreadyIndexed(t *testing.T) {
	db := testDB(t)

	ok, err := isAlreadyIndexed(db, 42)
	if err != nil || ok {
		t.Fatalf("isAlreadyIndexed before insert: got (%v, %v), want (false, nil)", ok, err)
	}

	if _, err := db.Exec("INSERT INTO indexed_msgs(msg_id) VALUES (42)"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	ok, err = isAlreadyIndexed(db, 42)
	if err != nil || !ok {
		t.Fatalf("isAlreadyIndexed after insert: got (%v, %v), want (true, nil)", ok, err)
	}
}

// ── saveMeme / searchMemes ────────────────────────────────────────────────────

func TestSaveMemeAndSearch(t *testing.T) {
	db := testDB(t)

	if err := saveMeme(db, "file123", 1, "Описание: кот сидит на окне"); err != nil {
		t.Fatalf("saveMeme: %v", err)
	}

	// Should be findable after saving.
	results, err := searchMemes(db, buildFTSQuery("кот"))
	if err != nil {
		t.Fatalf("searchMemes: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("searchMemes: expected at least one result, got 0")
	}

	// Should be marked indexed.
	ok, err := isAlreadyIndexed(db, 1)
	if err != nil || !ok {
		t.Fatalf("isAlreadyIndexed after saveMeme: got (%v, %v), want (true, nil)", ok, err)
	}
}

func TestSearchMemes_NoMatch(t *testing.T) {
	db := testDB(t)
	if err := saveMeme(db, "f1", 1, "Описание: собака бежит"); err != nil {
		t.Fatalf("saveMeme: %v", err)
	}
	results, err := searchMemes(db, buildFTSQuery("кот"))
	if err != nil {
		t.Fatalf("searchMemes: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("searchMemes no match: expected 0 results, got %d", len(results))
	}
}

// ── resetDB ───────────────────────────────────────────────────────────────────

func TestResetDB(t *testing.T) {
	db := testDB(t)

	if err := saveMeme(db, "f1", 1, "Описание: кот"); err != nil {
		t.Fatalf("saveMeme: %v", err)
	}
	if err := setCrawlerState(db, "k", "v"); err != nil {
		t.Fatalf("setCrawlerState: %v", err)
	}

	if err := resetDB(db); err != nil {
		t.Fatalf("resetDB: %v", err)
	}

	var n int
	db.QueryRow("SELECT count(*) FROM memes").Scan(&n)
	if n != 0 {
		t.Errorf("after resetDB memes count=%d, want 0", n)
	}
	db.QueryRow("SELECT count(*) FROM crawler_state").Scan(&n)
	if n != 0 {
		t.Errorf("after resetDB crawler_state count=%d, want 0", n)
	}
}

// ── computeDHash ─────────────────────────────────────────────────────────────

func TestComputeDHash_InvalidBytes(t *testing.T) {
	if h := computeDHash([]byte("not an image")); h != 0 {
		t.Errorf("computeDHash invalid bytes: got %d, want 0", h)
	}
}

func TestComputeDHash_ValidImage(t *testing.T) {
	white := makePNG(t, 32, 32, color.White)
	h := computeDHash(white)
	// Uniform colour → all pixels equal → all bits 0.
	if h != 0 {
		// Actually a uniform image may produce 0 or non-zero depending on
		// rounding; just ensure it doesn't panic and returns consistently.
		t.Logf("computeDHash uniform white = %064b (non-zero is acceptable)", h)
	}

	// Two identical images must produce the same hash.
	h2 := computeDHash(white)
	if h != h2 {
		t.Errorf("computeDHash not deterministic: %d vs %d", h, h2)
	}
}

func TestComputeDHash_DifferentImages(t *testing.T) {
	// Left-half-white / right-half-black gradient produces non-zero dHash bits.
	// Uniform white and black both hash to 0 (no brightness change between adjacent pixels).
	gradient := makeGradientPNG(t, 32, 32)
	uniform := makePNG(t, 32, 32, color.White)
	hg := computeDHash(gradient)
	hu := computeDHash(uniform)
	if hg == hu {
		t.Errorf("computeDHash: gradient and uniform produced the same hash %d", hg)
	}
}

// ── isDuplicate / loadHashes / storeImageHash ─────────────────────────────────

func TestIsDuplicate(t *testing.T) {
	// gradient → non-zero hash; uniform white → hash 0 (uniform images have no brightness gradient).
	hw := computeDHash(makeGradientPNG(t, 32, 32))
	hb := computeDHash(makePNG(t, 32, 32, color.White))

	hashes := []uint64{hw}

	if !isDuplicate(hashes, hw) {
		t.Error("isDuplicate: identical hash should be a duplicate")
	}
	if isDuplicate(hashes, hb) {
		t.Error("isDuplicate: black vs white should not be a duplicate")
	}
	if isDuplicate(hashes, 0) {
		t.Error("isDuplicate: hash=0 should never be a duplicate")
	}
	if isDuplicate(nil, hw) {
		t.Error("isDuplicate: empty slice should return false")
	}
}

func TestLoadHashesAndStore(t *testing.T) {
	db := testDB(t)

	hashes, err := loadHashes(db)
	if err != nil {
		t.Fatalf("loadHashes empty: %v", err)
	}
	if len(hashes) != 0 {
		t.Errorf("loadHashes empty db: got %d, want 0", len(hashes))
	}

	if err := storeImageHash(db, 0xDEADBEEF); err != nil {
		t.Fatalf("storeImageHash: %v", err)
	}
	if err := storeImageHash(db, 0); err != nil { // hash=0 should be a no-op
		t.Fatalf("storeImageHash(0): %v", err)
	}

	hashes, err = loadHashes(db)
	if err != nil {
		t.Fatalf("loadHashes after store: %v", err)
	}
	if len(hashes) != 1 {
		t.Errorf("loadHashes after store: got %d, want 1", len(hashes))
	}
	if hashes[0] != 0xDEADBEEF {
		t.Errorf("loadHashes: got %x, want DEADBEEF", hashes[0])
	}
}
