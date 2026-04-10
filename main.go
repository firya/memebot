package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"math/bits"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kljensen/snowball"
	tele "gopkg.in/telebot.v3"
	_ "modernc.org/sqlite"
)

// ─── Config ───────────────────────────────────────────────────────────────────

type Config struct {
	TelegramToken      string
	AIProvider         string // "claude" | "gemini"
	ClaudeAPIKey       string
	GeminiAPIKey       string
	GeminiWorkerURL    string // Cloudflare Worker URL (replaces direct googleapis.com call)
	GeminiWorkerSecret string // X-Worker-Secret header value
	ChannelUsername    string // e.g. "@mychannel"
	DumpChatID         int64
	AdminID            int64
	DBPath             string
	DevMode            bool // APP_ENV=dev
	CrawlerMaxGap      int  // consecutive misses before history is considered exhausted
}

func loadConfig() Config {
	must := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			log.Fatalf("required env var %s is not set", key)
		}
		return v
	}

	dumpChatID, err := strconv.ParseInt(must("DUMP_CHAT_ID"), 10, 64)
	if err != nil {
		log.Fatalf("invalid DUMP_CHAT_ID: %v", err)
	}

	adminID, err := strconv.ParseInt(must("ADMIN_ID"), 10, 64)
	if err != nil {
		log.Fatalf("invalid ADMIN_ID: %v", err)
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "/app/data/memes.db"
	}

	devMode := os.Getenv("APP_ENV") == "dev"

	crawlerMaxGap := 100
	if v := os.Getenv("CRAWLER_MAX_GAP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			crawlerMaxGap = n
		}
	}

	provider := os.Getenv("AI_PROVIDER")
	if provider == "" {
		provider = "claude"
	}
	var claudeKey, geminiKey string
	switch provider {
	case "claude":
		claudeKey = must("CLAUDE_API_KEY")
	case "gemini":
		geminiKey = must("GEMINI_API_KEY")
	default:
		log.Fatalf("unknown AI_PROVIDER: %s (use 'claude' or 'gemini')", provider)
	}

	return Config{
		TelegramToken:      must("TELEGRAM_TOKEN"),
		AIProvider:         provider,
		ClaudeAPIKey:       claudeKey,
		GeminiAPIKey:       geminiKey,
		GeminiWorkerURL:    os.Getenv("GEMINI_WORKER_URL"),
		GeminiWorkerSecret: os.Getenv("GEMINI_WORKER_SECRET"),
		ChannelUsername:    must("CHANNEL_USERNAME"),
		DumpChatID:         dumpChatID,
		AdminID:            adminID,
		DBPath:             dbPath,
		DevMode:            devMode,
		CrawlerMaxGap:      crawlerMaxGap,
	}
}

// ─── Database ─────────────────────────────────────────────────────────────────

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
	_, err := db.Exec(`
		DELETE FROM memes;
		DELETE FROM indexed_msgs;
		DELETE FROM crawler_state;
		DELETE FROM image_hashes;
		DELETE FROM failed_msgs;
	`)
	return err
}

func isAlreadyIndexed(db *sql.DB, msgID int) (bool, error) {
	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM indexed_msgs WHERE msg_id = ?", msgID).Scan(&n)
	return n > 0, err
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
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			sx := bounds.Min.X + x*srcW/cols
			sy := bounds.Min.Y + y*srcH/rows
			r, g, b, _ := img.At(sx, sy).RGBA()
			grid[y][x] = color.Gray{Y: uint8((19595*r + 38470*g + 7471*b + 1<<15) >> 24)}.Y
		}
	}

	// Each bit: 1 if left pixel is brighter than right neighbour.
	var hash uint64
	for y := 0; y < rows; y++ {
		for x := 0; x < cols-1; x++ {
			if grid[y][x] > grid[y][x+1] {
				hash |= 1 << uint(y*(cols-1)+x)
			}
		}
	}
	return hash
}

// isDuplicateImage returns true when an image with a Hamming distance ≤ dHashThreshold
// already exists in image_hashes.
func isDuplicateImage(db *sql.DB, hash uint64) (bool, error) {
	if hash == 0 {
		return false, nil
	}
	rows, err := db.Query("SELECT phash FROM image_hashes")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var stored uint64
		if err := rows.Scan(&stored); err != nil {
			return false, err
		}
		if bits.OnesCount64(hash^stored) <= dHashThreshold {
			return true, nil
		}
	}
	return false, rows.Err()
}

// storeImageHash saves a perceptual hash so future duplicates are detected.
func storeImageHash(db *sql.DB, hash uint64) error {
	if hash == 0 {
		return nil
	}
	_, err := db.Exec("INSERT INTO image_hashes(phash) VALUES (?)", hash)
	return err
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
		// Telegram caption limit is 1024 characters
		if len(originalDesc) > 1024 {
			originalDesc = originalDesc[:1021] + "..."
		}
		results = append(results, &tele.PhotoResult{
			ResultBase: tele.ResultBase{ID: strconv.FormatInt(rowid, 10)},
			Cache:      fileID,
			Caption:    originalDesc,
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

// ─── NLP: Stemming & FTS query ────────────────────────────────────────────────

var punctRe = regexp.MustCompile(`[^\p{L}\p{N}\s]+`)

// errQuotaExceeded is returned by callGemini when the daily API quota is exhausted.
// The worker must not retry such requests — the quota resets at midnight UTC.
var errQuotaExceeded = errors.New("gemini daily quota exceeded")

// errRateLimited is returned by callGemini when the per-minute rate limit is hit.
// The worker should pause ~60 s and retry, NOT sleep until midnight.
var errRateLimited = errors.New("gemini rate limited (RPM)")

func stripPunct(text string) string {
	return punctRe.ReplaceAllString(text, " ")
}

func stemWord(word string) string {
	s, err := snowball.Stem(word, "russian", true)
	if err != nil || s == "" {
		return word
	}
	return s
}

// buildSearchVector produces a stemmed, punctuation-free string for FTS5 indexing.
// Section labels from the AI response ("Описание:", "Текст:", "Персоны:") are stripped
// so they don't match every search query.
func buildSearchVector(text string) string {
	text = strings.NewReplacer(
		"Описание:", "",
		"Текст:", "",
		"Персоны:", "",
		"Текст отсутствует", "",
	).Replace(text)
	text = strings.ToLower(stripPunct(text))
	words := strings.Fields(text)
	for i, w := range words {
		words[i] = stemWord(w)
	}
	return strings.Join(words, " ")
}

// buildFTSQuery turns a user query into an FTS5 MATCH expression:
// "котики кушают рыбку" → "котик* AND кушат* AND рыбк*"
func buildFTSQuery(query string) string {
	query = strings.ToLower(stripPunct(query))
	words := strings.Fields(query)
	if len(words) == 0 {
		return ""
	}
	terms := make([]string, 0, len(words))
	for _, w := range words {
		terms = append(terms, stemWord(w)+"*")
	}
	return strings.Join(terms, " AND ")
}

// ─── AI Integration ───────────────────────────────────────────────────────────

const aiPrompt = `Проанализируй мем или комикс и ответь в трёх частях:

1. Описание: что происходит на картинке (1-2 предложения на русском). Если на изображении есть узнаваемые люди — назови их (например: "Илон Маск", "Путин", "Киану Ривз").

2. Текст: перепиши дословно весь текст с картинки, сохраняя оригинальный язык и написание. Правила:
   - Названия игр, фильмов, брендов и прочего пиши в оригинальном виде рядом с русской версией, например: "дарк соулс (Dark Souls)", "скайрим (Skyrim)", "майкрософт (Microsoft)".
   - Если в тексте есть намеренно или случайно искажённые слова (опечатки, просторечие, мемные написания), перепиши их как есть, а рядом в скобках укажи правильное написание, например: "коникулы (каникулы)", "ничиво (ничего)", "превет (привет)".
   - Если текста нет — напиши "Текст отсутствует".

3. Персоны: перечисли через запятую всех узнаваемых людей на изображении. Если никого нет — напиши "Нет".

Формат ответа:
Описание: ...
Текст: ...
Персоны: ...`

// fetchImageBytes resolves a Telegram file_id to raw image bytes and MIME type.
func fetchImageBytes(cfg Config, fileID string) ([]byte, string, error) {
	tgFileURL := fmt.Sprintf("https://api.telegram.org/bot%s/getFile?file_id=%s", cfg.TelegramToken, fileID)
	tgClient := &http.Client{Timeout: 15 * time.Second}
	resp, err := tgClient.Get(tgFileURL)
	if err != nil {
		return nil, "", fmt.Errorf("getFile request: %w", err)
	}
	defer resp.Body.Close()

	var fileInfo struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&fileInfo); err != nil {
		return nil, "", fmt.Errorf("decode getFile: %w", err)
	}
	if !fileInfo.OK {
		return nil, "", fmt.Errorf("getFile failed: %s", fileInfo.Description)
	}

	imageURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", cfg.TelegramToken, fileInfo.Result.FilePath)
	imgResp, err := (&http.Client{Timeout: 60 * time.Second}).Get(imageURL)
	if err != nil {
		return nil, "", fmt.Errorf("download image: %w", err)
	}
	defer imgResp.Body.Close()

	imageBytes, err := io.ReadAll(imgResp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read image: %w", err)
	}

	mimeType := "image/jpeg"
	switch {
	case strings.HasSuffix(fileInfo.Result.FilePath, ".png"):
		mimeType = "image/png"
	case strings.HasSuffix(fileInfo.Result.FilePath, ".webp"):
		mimeType = "image/webp"
	case strings.HasSuffix(fileInfo.Result.FilePath, ".gif"):
		mimeType = "image/gif"
	}

	return imageBytes, mimeType, nil
}

// callClaude sends imageBytes to the Claude vision API and returns the description.
func callClaude(apiKey string, imageBytes []byte, mimeType string) (string, error) {
	type imageSource struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	type content struct {
		Type   string       `json:"type"`
		Text   string       `json:"text,omitempty"`
		Source *imageSource `json:"source,omitempty"`
	}
	type message struct {
		Role    string    `json:"role"`
		Content []content `json:"content"`
	}
	reqBody := struct {
		Model     string    `json:"model"`
		MaxTokens int       `json:"max_tokens"`
		Messages  []message `json:"messages"`
	}{
		Model:     "claude-haiku-4-5-20251001",
		MaxTokens: 1000,
		Messages: []message{{
			Role: "user",
			Content: []content{
				{Type: "image", Source: &imageSource{Type: "base64", MediaType: mimeType, Data: base64.StdEncoding.EncodeToString(imageBytes)}},
				{Type: "text", Text: aiPrompt},
			},
		}},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal claude request: %w", err)
	}

	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("create claude request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	claudeResp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("claude HTTP request: %w", err)
	}
	defer claudeResp.Body.Close()

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(claudeResp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode claude response: %w", err)
	}
	if result.Error != nil {
		return "", fmt.Errorf("claude API error [%s]: %s", result.Error.Type, result.Error.Message)
	}
	if len(result.Content) == 0 {
		return "", fmt.Errorf("empty claude response")
	}
	return strings.TrimSpace(result.Content[0].Text), nil
}

// callGemini sends imageBytes to the Gemini vision API and returns the description.
// workerURL overrides the googleapis.com base URL (Cloudflare Worker); empty means direct.
// workerSecret is sent as X-Worker-Secret if set.
func callGemini(apiKey, workerURL, workerSecret string, imageBytes []byte, mimeType string) (string, error) {
	reqBody := struct {
		Contents []struct {
			Parts []any `json:"parts"`
		} `json:"contents"`
	}{
		Contents: []struct {
			Parts []any `json:"parts"`
		}{{
			Parts: []any{
				map[string]any{"inline_data": map[string]string{"mime_type": mimeType, "data": base64.StdEncoding.EncodeToString(imageBytes)}},
				map[string]string{"text": aiPrompt},
			},
		}},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal gemini request: %w", err)
	}

	base := "https://generativelanguage.googleapis.com"
	if workerURL != "" {
		base = workerURL
	}
	endpoint := base + "/v1beta/models/gemini-3.1-flash-lite-preview:generateContent?key=" + apiKey
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("create gemini request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if workerSecret != "" {
		req.Header.Set("X-Worker-Secret", workerSecret)
	}

	geminiResp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini HTTP request: %w", err)
	}
	defer geminiResp.Body.Close()

	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(geminiResp.Body).Decode(&result); err != nil {
		// Body unreadable — fall back to HTTP status code.
		if geminiResp.StatusCode == 429 {
			if geminiResp.Header.Get("Retry-After") != "" {
				return "", errRateLimited
			}
			return "", errQuotaExceeded
		}
		return "", fmt.Errorf("decode gemini response (status %d): %w", geminiResp.StatusCode, err)
	}
	if result.Error != nil {
		msg := result.Error.Message
		msgLower := strings.ToLower(msg)
		if geminiResp.StatusCode == 429 || strings.Contains(msgLower, "quota") || strings.Contains(msg, "RESOURCE_EXHAUSTED") {
			log.Printf("gemini quota/rate error (status=%d, retry-after=%q): %s", geminiResp.StatusCode, geminiResp.Header.Get("Retry-After"), msg)
			// Retry-After present → per-minute RPM limit; absent → daily quota exhausted.
			if geminiResp.Header.Get("Retry-After") != "" {
				return "", errRateLimited
			}
			return "", errQuotaExceeded
		}
		return "", fmt.Errorf("gemini API error: %s", msg)
	}
	if geminiResp.StatusCode == 429 {
		log.Printf("gemini 429 no-body (retry-after=%q)", geminiResp.Header.Get("Retry-After"))
		if geminiResp.Header.Get("Retry-After") != "" {
			return "", errRateLimited
		}
		return "", errQuotaExceeded
	}
	if len(result.Candidates) == 0 || len(result.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty gemini response")
	}
	return strings.TrimSpace(result.Candidates[0].Content.Parts[0].Text), nil
}

// describeImageFromBytes sends pre-fetched image bytes to the configured AI provider.
func describeImageFromBytes(cfg Config, imageBytes []byte, mimeType string) (string, error) {
	if cfg.AIProvider == "gemini" {
		return callGemini(cfg.GeminiAPIKey, cfg.GeminiWorkerURL, cfg.GeminiWorkerSecret, imageBytes, mimeType)
	}
	return callClaude(cfg.ClaudeAPIKey, imageBytes, mimeType)
}

// ─── Admin alerts ─────────────────────────────────────────────────────────────

func sendAdminAlert(bot *tele.Bot, adminID int64, msg string) {
	log.Printf("ADMIN ALERT: %s", msg)
	admin := &tele.User{ID: adminID}
	if _, err := bot.Send(admin, "🚨 "+msg); err != nil {
		log.Printf("failed to deliver admin alert: %v", err)
	}
}

// ─── Worker Pool ──────────────────────────────────────────────────────────────

type indexJob struct {
	fileID  string
	msgID   int
	replyTo int64 // if > 0, send AI result back to this chat (admin DM)
	retries int   // number of AI retry attempts so far
}

// runWorker consumes jobs from jobChan, gated by a ticker to stay within rate limits.
// wake is a buffered channel; sending to it interrupts any rate-limit sleep early.
// quotaUntil is set to the Unix timestamp of the planned wakeup while the worker
// sleeps due to daily quota exhaustion, and reset to 0 when it resumes.
func runWorker(bot *tele.Bot, db *sql.DB, cfg Config, jobChan chan indexJob, wake <-chan struct{}, quotaUntil *atomic.Int64) {
	// 2000 ms is a safe default for Claude paid tiers.
	// If you hit rate limits, increase this to 4500 (like Gemini before).
	ticker := time.NewTicker(2000 * time.Millisecond)
	defer ticker.Stop()

	log.Println("worker pool started (rate limit: 1 req / 2 s)")

	for job := range jobChan {
		<-ticker.C // wait for the rate-limit window

		// Skip dedup for admin DM photos (replyTo > 0, no channel msgID)
		if job.replyTo == 0 {
			already, err := isAlreadyIndexed(db, job.msgID)
			if err != nil {
				log.Printf("worker: db check error msg_id=%d: %v", job.msgID, err)
			} else if already {
				log.Printf("worker: msg_id=%d already indexed, skipping", job.msgID)
				continue
			}
		}

		log.Printf("worker: describing msg_id=%d file_id=%s", job.msgID, job.fileID)

		imageBytes, mimeType, err := fetchImageBytes(cfg, job.fileID)
		if err != nil {
			log.Printf("worker: fetch error msg_id=%d: %v", job.msgID, err)
			continue
		}

		// Perceptual dedup: skip images that look identical to already-indexed ones.
		if job.replyTo == 0 {
			hash := computeDHash(imageBytes)
			dup, err := isDuplicateImage(db, hash)
			if err != nil {
				log.Printf("worker: phash check error msg_id=%d: %v", job.msgID, err)
			} else if dup {
				log.Printf("worker: msg_id=%d is a visual duplicate, skipping", job.msgID)
				// Mark as indexed so the crawler doesn't revisit it.
				if err := storeImageHash(db, hash); err != nil {
					log.Printf("worker: store dup hash error msg_id=%d: %v", job.msgID, err)
				}
				if _, err := db.Exec("INSERT OR IGNORE INTO indexed_msgs(msg_id) VALUES (?)", job.msgID); err != nil {
					log.Printf("worker: mark dup indexed error msg_id=%d: %v", job.msgID, err)
				}
				continue
			}
		}

		desc, err := describeImageFromBytes(cfg, imageBytes, mimeType)
		if err != nil {
			if errors.Is(err, errRateLimited) {
				// Per-minute rate limit — pause up to 60 s (interruptible by /analyze).
				log.Printf("worker: rate limited (RPM) for msg_id=%d, pausing up to 60s", job.msgID)
				jobChan <- job // requeue before sleeping
				select {
				case <-time.After(60 * time.Second):
					log.Println("worker: RPM window passed, resuming")
				case <-wake:
					log.Println("worker: woken by /analyze, resuming immediately")
				}
				continue
			}
			if errors.Is(err, errQuotaExceeded) {
				// Daily quota exhausted — pause until next midnight UTC (interruptible by /analyze).
				now := time.Now().UTC()
				nextMidnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 5, 0, 0, time.UTC)
				waitDur := time.Until(nextMidnight)
				alertMsg := fmt.Sprintf("Gemini daily quota exhausted. Worker paused until %s UTC (~%s). Квота сбросится автоматически.", nextMidnight.Format("2006-01-02 15:04"), waitDur.Round(time.Minute))
				log.Println("worker:", alertMsg)
				sendAdminAlert(bot, cfg.AdminID, alertMsg)
				jobChan <- job // requeue before sleeping
				quotaUntil.Store(nextMidnight.Unix())
				select {
				case <-time.After(waitDur):
					log.Println("worker: quota reset window reached, resuming")
				case <-wake:
					log.Println("worker: woken by /analyze, retrying immediately")
				}
				quotaUntil.Store(0)
				continue
			}
			const maxRetries = 3
			if job.replyTo == 0 && job.retries < maxRetries {
				job.retries++
				delay := time.Duration(job.retries*30) * time.Second
				log.Printf("worker: AI error msg_id=%d, retry %d/%d in %s: %v", job.msgID, job.retries, maxRetries, delay, err)
				go func(j indexJob) {
					time.Sleep(delay)
					jobChan <- j
				}(job)
			} else {
				errMsg := fmt.Sprintf("AI failed for msg_id=%d file_id=%s (retries=%d): %v", job.msgID, job.fileID, job.retries, err)
				sendAdminAlert(bot, cfg.AdminID, errMsg)
				if job.replyTo > 0 {
					chat := &tele.Chat{ID: job.replyTo}
					bot.Send(chat, "❌ Ошибка AI: "+err.Error())
				} else {
					// Persist failed job so it is retried on next restart.
					if _, dbErr := db.Exec("INSERT OR REPLACE INTO failed_msgs(msg_id, file_id) VALUES (?, ?)", job.msgID, job.fileID); dbErr != nil {
						log.Printf("worker: save failed_msg error msg_id=%d: %v", job.msgID, dbErr)
					}
				}
			}
			continue
		}

		// Save to DB always — channel photos and admin DM photos alike
		if err := saveMeme(db, job.fileID, job.msgID, desc); err != nil {
			sendAdminAlert(bot, cfg.AdminID, fmt.Sprintf(
				"DB save failed for msg_id=%d: %v", job.msgID, err,
			))
			continue
		}

		// Store perceptual hash so future duplicates are detected.
		if job.replyTo == 0 {
			if err := storeImageHash(db, computeDHash(imageBytes)); err != nil {
				log.Printf("worker: store phash error msg_id=%d: %v", job.msgID, err)
			}
			// Remove from failed_msgs if this was a previously failed job.
			if _, err := db.Exec("DELETE FROM failed_msgs WHERE msg_id = ?", job.msgID); err != nil {
				log.Printf("worker: clear failed_msg error msg_id=%d: %v", job.msgID, err)
			}
			// Advance the worker checkpoint so the crawler knows how far we've
			// actually processed. On restart the crawler will resume from here
			// (not from last_crawled_msg_id) to recover any jobs that were
			// in-flight when the container stopped.
			if err := setCrawlerState(db, "last_worker_msg_id", strconv.Itoa(job.msgID)); err != nil {
				log.Printf("worker: save checkpoint error msg_id=%d: %v", job.msgID, err)
			}
		}

		preview := desc
		if len(preview) > 100 {
			preview = preview[:100] + "…"
		}
		log.Printf("worker: indexed msg_id=%d — %s", job.msgID, preview)

		// Send result back: admin DM reply, or dev mode notification
		if job.replyTo > 0 {
			chat := &tele.Chat{ID: job.replyTo}
			if _, err := bot.Send(chat, desc); err != nil {
				log.Printf("worker: reply failed: %v", err)
			}
		} else if cfg.DevMode {
			admin := &tele.User{ID: cfg.AdminID}
			if _, err := bot.Send(admin, fmt.Sprintf("✅ msg_id=%d\n\n%s", job.msgID, desc)); err != nil {
				log.Printf("worker: dev notify failed: %v", err)
			}
		}
	}
}

// ─── History Crawler ──────────────────────────────────────────────────────────

// channelMsg implements tele.Editable so we can call bot.Copy on arbitrary
// message IDs from a known channel chat.
type channelMsg struct {
	id     int
	chatID int64
}

func (m channelMsg) MessageSig() (string, int64) {
	return strconv.Itoa(m.id), m.chatID
}

// crawlHistory iterates message IDs from 1 upward, forwarding each post to the
// dump chat to obtain its file_id. It resumes from where it left off on restart.
// photoLimit > 0 stops after that many photos are enqueued (dev mode).
// ctx cancellation stops the crawler gracefully.
func crawlHistory(ctx context.Context, bot *tele.Bot, db *sql.DB, cfg Config, channelID int64, dumpChat *tele.Chat, jobChan chan indexJob, photoLimit int) {
	// Prefer last_worker_msg_id (last msg actually indexed) over last_crawled_msg_id
	// (last msg forwarded by the crawler). On an abrupt shutdown the in-memory
	// job queue is lost, so restarting from the worker checkpoint ensures we
	// re-enqueue any photos that were queued but never indexed.
	lastStr, err := getCrawlerState(db, "last_worker_msg_id")
	if err != nil || lastStr == "" {
		lastStr, _ = getCrawlerState(db, "last_crawled_msg_id")
	}
	if lastStr == "" {
		lastStr = "0"
	}
	startID, _ := strconv.Atoi(lastStr)
	log.Printf("crawler: starting from msg_id=%d (photoLimit=%d, maxGap=%d)", startID+1, photoLimit, cfg.CrawlerMaxGap)

	consecutiveMisses := 0
	consecutiveTransient := 0
	photosEnqueued := 0

	// ctxSleep sleeps for d but returns early if ctx is cancelled.
	ctxSleep := func(d time.Duration) {
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
	}

	for msgID := startID + 1; ; msgID++ {
		// Check for external stop signal
		select {
		case <-ctx.Done():
			log.Println("crawler: stopped by request")
			return
		default:
		}

		if photoLimit > 0 && photosEnqueued >= photoLimit {
			log.Printf("crawler: dev limit reached (%d photos), stopping", photoLimit)
			break
		}

		ctxSleep(60 * time.Millisecond) // ~16 req/s to Telegram, well under flood limit

		copied, err := bot.Forward(dumpChat, channelMsg{id: msgID, chatID: channelID})
		if err != nil {
			errStr := err.Error()
			// Rate limit or transient network error — pause and retry same msgID
			if strings.Contains(errStr, "retry") || strings.Contains(errStr, "Too Many") || strings.Contains(errStr, "timeout") {
				consecutiveTransient++
				// Exponential backoff: 10s, 20s, 40s … capped at 5 min.
				backoff := time.Duration(10<<min(consecutiveTransient-1, 5)) * time.Second
				log.Printf("crawler: transient error #%d at msg_id=%d, pausing %s: %v", consecutiveTransient, msgID, backoff, err)
				ctxSleep(backoff)
				if ctx.Err() != nil {
					log.Println("crawler: stopped by request during backoff")
					return
				}
				msgID-- // retry same ID
				continue
			}
			// Message doesn't exist — count as a miss
			consecutiveMisses++
			if consecutiveMisses >= cfg.CrawlerMaxGap {
				log.Printf("crawler: %d consecutive misses at msg_id=%d — history exhausted", cfg.CrawlerMaxGap, msgID)
				break
			}
			continue
		}
		consecutiveMisses = 0
		consecutiveTransient = 0

		// Persist progress immediately
		if saveErr := setCrawlerState(db, "last_crawled_msg_id", strconv.Itoa(msgID)); saveErr != nil {
			log.Printf("crawler: cannot save state: %v", saveErr)
		}

		if msgID%100 == 0 {
			var total int
			db.QueryRow("SELECT count(*) FROM memes").Scan(&total)
			log.Printf("crawler: progress msg_id=%d | photos enqueued=%d | indexed=%d | queue=%d",
				msgID, photosEnqueued, total, len(jobChan))
		}

		// bot.Forward populates the full message, but ensure Chat is set for Delete
		copied.Chat = dumpChat

		if copied.Photo == nil {
			if delErr := bot.Delete(copied); delErr != nil {
				log.Printf("crawler: delete non-photo copy msg_id=%d: %v", msgID, delErr)
			}
			continue
		}

		fileID := copied.Photo.FileID
		log.Printf("crawler: photo msg_id=%d file_id=%s", msgID, fileID)

		if delErr := bot.Delete(copied); delErr != nil {
			log.Printf("crawler: delete photo copy msg_id=%d: %v", msgID, delErr)
		}

		jobChan <- indexJob{fileID: fileID, msgID: msgID}
		photosEnqueued++
	}

	log.Println("crawler: history scan complete")
}

// ─── Telegram helper ──────────────────────────────────────────────────────────

// resolveChannelID calls getChat to obtain the numeric ID for a channel username.
func resolveChannelID(token, username string) (int64, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getChat?chat_id=%s", token, username)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Get(url)
	if err != nil {
		return 0, fmt.Errorf("getChat request: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			ID int64 `json:"id"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decode getChat: %w", err)
	}
	if !result.OK {
		return 0, fmt.Errorf("getChat failed: %s", result.Description)
	}
	return result.Result.ID, nil
}

// ─── Status helpers ───────────────────────────────────────────────────────────

func buildStatusMsg(db *sql.DB, jobChan chan indexJob, devMode bool, quotaUntil *atomic.Int64) string {
	var totalMemes int
	db.QueryRow("SELECT count(*) FROM memes").Scan(&totalMemes)

	lastCrawled, _ := getCrawlerState(db, "last_crawled_msg_id")
	if lastCrawled == "" {
		lastCrawled = "0"
	}
	lastWorker, _ := getCrawlerState(db, "last_worker_msg_id")
	if lastWorker == "" {
		lastWorker = "0"
	}

	env := "prod"
	if devMode {
		env = "dev"
	}

	msg := fmt.Sprintf(
		"📊 Статус memebot (%s)\n\n"+
			"🗄 Проиндексировано мемов: %d\n"+
			"🔍 Последний просмотренный msg_id: %s\n"+
			"✅ Последний проиндексированный msg_id: %s\n"+
			"⏳ Очередь на обработку: %d",
		env, totalMemes, lastCrawled, lastWorker, len(jobChan),
	)
	if until := quotaUntil.Load(); until != 0 {
		wakeTime := time.Unix(until, 0).UTC()
		msg += fmt.Sprintf("\n💤 Квота Gemini исчерпана, worker спит до %s UTC", wakeTime.Format("2006-01-02 15:04"))
	}
	return msg
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	cfg := loadConfig()

	db, err := initDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("db init: %v", err)
	}
	defer db.Close()

	bot, err := tele.NewBot(tele.Settings{
		Token:  cfg.TelegramToken,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	})
	if err != nil {
		log.Fatalf("bot init: %v", err)
	}

	channelID, err := resolveChannelID(cfg.TelegramToken, cfg.ChannelUsername)
	if err != nil {
		log.Fatalf("resolve channel: %v", err)
	}
	log.Printf("channel %s resolved to ID %d", cfg.ChannelUsername, channelID)

	jobChan := make(chan indexJob, 1000)
	workerWake := make(chan struct{}, 1)
	var workerQuotaUntil atomic.Int64
	go runWorker(bot, db, cfg, jobChan, workerWake, &workerQuotaUntil)

	// Re-queue any jobs that permanently failed in a previous session.
	{
		rows, err := db.Query("SELECT msg_id, file_id FROM failed_msgs ORDER BY msg_id")
		if err != nil {
			log.Printf("startup: load failed_msgs error: %v", err)
		} else {
			var requeued int
			for rows.Next() {
				var job indexJob
				if err := rows.Scan(&job.msgID, &job.fileID); err == nil {
					jobChan <- job
					requeued++
				}
			}
			rows.Close()
			if requeued > 0 {
				log.Printf("startup: re-queued %d previously failed jobs", requeued)
			}
		}
	}

	var crawlerRunning atomic.Bool
	var manuallyStopped atomic.Bool

	// Periodic status reporter: sends /status to admin every 5 minutes while
	// the crawler is running or there are jobs waiting in the queue.
	// Suppressed while the worker is sleeping due to daily quota exhaustion,
	// or after the user has manually stopped the crawler via /stop.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		admin := &tele.User{ID: cfg.AdminID}
		for range ticker.C {
			if manuallyStopped.Load() {
				continue
			}
			if !crawlerRunning.Load() && len(jobChan) == 0 {
				continue
			}
			if workerQuotaUntil.Load() != 0 {
				continue // worker is in quota sleep; no point spamming unchanged status
			}
			if _, err := bot.Send(admin, buildStatusMsg(db, jobChan, cfg.DevMode, &workerQuotaUntil)); err != nil {
				log.Printf("status ticker: send failed: %v", err)
			}
		}
	}()
	crawlerCtx, crawlerCancel := context.WithCancel(context.Background())

	startCrawler := func(photoLimit int) {
		if crawlerRunning.Swap(true) {
			log.Println("crawler: already running, skipping start")
			return
		}
		go func() {
			defer crawlerRunning.Store(false)
			dumpChat := &tele.Chat{ID: cfg.DumpChatID}
			crawlHistory(crawlerCtx, bot, db, cfg, channelID, dumpChat, jobChan, photoLimit)
		}()
	}
	_ = crawlerCancel // used inside /reset handler closure

	// Listen for new photos posted to the channel
	channelUsernamePlain := strings.TrimPrefix(cfg.ChannelUsername, "@")
	bot.Handle(tele.OnChannelPost, func(c tele.Context) error {
		if c.Chat().Username != channelUsernamePlain {
			return nil
		}
		msg := c.Message()
		if msg.Photo == nil {
			return nil
		}
		log.Printf("listener: new channel photo msg_id=%d", msg.ID)
		select {
		case jobChan <- indexJob{fileID: msg.Photo.FileID, msgID: msg.ID}:
		default:
			log.Printf("listener: job queue full, dropping msg_id=%d", msg.ID)
		}
		return nil
	})

	// Admin DM: photo sent directly to the bot → describe via AI and reply
	bot.Handle(tele.OnPhoto, func(c tele.Context) error {
		if c.Chat().ID != cfg.AdminID {
			return nil
		}
		msg := c.Message()
		fileID := msg.Photo.FileID
		log.Printf("admin DM: photo received file_id=%s", fileID)
		if _, err := c.Bot().Send(c.Chat(), "⏳ Добавлено в очередь..."); err != nil {
			log.Printf("admin DM: ack send failed: %v", err)
		}
		jobChan <- indexJob{fileID: fileID, msgID: 0, replyTo: c.Chat().ID}
		return nil
	})

	// Inline search handler
	bot.Handle(tele.OnQuery, func(c tele.Context) error {
		query := strings.TrimSpace(c.Query().Text)
		if query == "" {
			return c.Answer(&tele.QueryResponse{Results: []tele.Result{}, CacheTime: 1})
		}

		ftsQuery := buildFTSQuery(query)
		if ftsQuery == "" {
			return c.Answer(&tele.QueryResponse{Results: []tele.Result{}, CacheTime: 1})
		}

		log.Printf("inline: %q → FTS: %q", query, ftsQuery)

		results, err := searchMemes(db, ftsQuery)
		if err != nil {
			log.Printf("inline: search error: %v", err)
			return c.Answer(&tele.QueryResponse{Results: []tele.Result{}, CacheTime: 1})
		}

		log.Printf("inline: %d result(s) for %q", len(results), query)
		return c.Answer(&tele.QueryResponse{
			Results:   results,
			CacheTime: 30,
		})
	})

	// /status — progress report for admin (dev and prod)
	bot.Handle("/status", func(c tele.Context) error {
		if c.Chat().ID != cfg.AdminID {
			return nil
		}
		return c.Send(buildStatusMsg(db, jobChan, cfg.DevMode, &workerQuotaUntil))
	})

	// /help — command reference for the admin
	bot.Handle("/help", func(c tele.Context) error {
		if c.Chat().ID != cfg.AdminID {
			return nil
		}
		devSection := ""
		if cfg.DevMode {
			devSection = "\n\n*Только dev-режим:*\n" +
				"`/index <n>` — сбросить БД и проиндексировать первые N фото (по умолчанию 10)"
		}
		text := "*Команды memebot*\n\n" +
			"*Индексация:*\n" +
			"`/status` — статистика: мемов в БД, прогресс краулера, длина очереди\n" +
			"`/resume` — продолжить краулер с последнего сохранённого msg\\_id\n" +
			"`/stop` — остановить краулер (прогресс сохраняется, возобновить: /resume)\n" +
			"`/reset` — сбросить БД и запустить краулер с начала канала\n" +
			"`/reset <n>` — сбросить БД и проиндексировать первые N фото (dev)\n\n" +
			"*Воркер / AI:*\n" +
			"`/analyze` — разбудить воркер досрочно, если он спит из-за RPM-лимита\n" +
			"_(при дневном лимите Gemini не поможет — квота сбросится в 00:05 UTC)_\n\n" +
			"*Поиск:*\n" +
			"Inline-поиск работает везде: `@botusername запрос`\n" +
			"Бот понимает русский язык со стеммингом — достаточно части слова" +
			devSection
		return c.Send(text, tele.ModeMarkdown)
	})

	// /analyze — wake the worker from a per-minute rate-limit sleep and retry immediately.
	// Does NOT help with daily quota exhaustion — in that case the worker sleeps until midnight UTC.
	bot.Handle("/analyze", func(c tele.Context) error {
		if c.Chat().ID != cfg.AdminID {
			return nil
		}
		select {
		case workerWake <- struct{}{}:
			log.Println("admin /analyze: woke worker")
			return c.Send("⚡ Сигнал отправлен. Если воркер спит из-за RPM-лимита — проснётся сразу.\n\nЕсли сработал дневной лимит Gemini — /analyze не поможет, квота сбросится автоматически в 00:05 UTC.")
		default:
			return c.Send("ℹ️ Воркер не спит (сигнал уже в очереди или таймаут не активен).")
		}
	})

	// /stop — stop crawler without resetting DB (dev and prod)
	bot.Handle("/stop", func(c tele.Context) error {
		if c.Chat().ID != cfg.AdminID {
			return nil
		}
		if !crawlerRunning.Load() {
			return c.Send("ℹ️ Краулер не запущен.")
		}
		manuallyStopped.Store(true)
		crawlerCancel()
		crawlerCtx, crawlerCancel = context.WithCancel(context.Background())
		return c.Send("⏹ Индексирование остановлено. Прогресс сохранён, возобновить: /resume")
	})

	// /resume — continue crawling from last saved msg_id without resetting DB (dev and prod)
	bot.Handle("/resume", func(c tele.Context) error {
		if c.Chat().ID != cfg.AdminID {
			return nil
		}
		if crawlerRunning.Load() {
			return c.Send("⚠️ Краулер уже запущен.")
		}
		manuallyStopped.Store(false)
		lastCrawled, _ := getCrawlerState(db, "last_crawled_msg_id")
		if lastCrawled == "" {
			lastCrawled = "0"
		}
		startCrawler(0)
		return c.Send(fmt.Sprintf("▶️ Продолжаю индексацию с msg_id=%s...", lastCrawled))
	})

	// /reset — resets DB and restarts crawler (dev and prod)
	bot.Handle("/reset", func(c tele.Context) error {
		if c.Chat().ID != cfg.AdminID {
			return nil
		}
		// Stop current crawler if running
		if crawlerRunning.Load() {
			crawlerCancel()
			time.Sleep(500 * time.Millisecond)
			crawlerCtx, crawlerCancel = context.WithCancel(context.Background())
		}
		// Drain jobs left in the queue from the previous crawl.
		for len(jobChan) > 0 {
			<-jobChan
		}
		if err := resetDB(db); err != nil {
			return c.Send("❌ Ошибка сброса БД: " + err.Error())
		}
		if cfg.DevMode {
			n := 10
			if arg := c.Message().Payload; arg != "" {
				if parsed, err := strconv.Atoi(arg); err == nil && parsed > 0 {
					n = parsed
				}
			}
			startCrawler(n)
			return c.Send(fmt.Sprintf("🔄 БД сброшена. Запускаю индексацию %d фото...", n))
		}
		startCrawler(0)
		return c.Send("🔄 БД сброшена. Краулер запущен с начала канала.")
	})

	if cfg.DevMode {
		// In dev mode crawling only starts on /index <n> command from admin
		bot.Handle("/index", func(c tele.Context) error {
			if c.Chat().ID != cfg.AdminID {
				return nil
			}
			n := 10
			if arg := c.Message().Payload; arg != "" {
				if parsed, err := strconv.Atoi(arg); err == nil && parsed > 0 {
					n = parsed
				}
			}
			log.Printf("admin: /index %d — resetting DB and starting crawl", n)
			if err := resetDB(db); err != nil {
				return c.Send("❌ Ошибка сброса БД: " + err.Error())
			}
			if err := c.Send(fmt.Sprintf("🔄 Запускаю индексацию %d фото с начала канала...", n)); err != nil {
				log.Printf("admin: send failed: %v", err)
			}
			startCrawler(n)
			return nil
		})
		log.Println("DEV MODE: crawler on hold — send /index <n> to start")
	} else {
		startCrawler(0)
	}

	log.Println("memebot running")
	bot.Start()
}
