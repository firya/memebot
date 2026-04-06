package main

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kljensen/snowball"
	_ "github.com/mattn/go-sqlite3"
	tele "gopkg.in/telebot.v3"
)

// ─── Config ───────────────────────────────────────────────────────────────────

type Config struct {
	TelegramToken   string
	GeminiAPIKey    string
	ChannelUsername string // e.g. "@mychannel"
	DumpChatID      int64
	AdminID         int64
	DBPath          string
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

	return Config{
		TelegramToken:   must("TELEGRAM_TOKEN"),
		GeminiAPIKey:    must("GEMINI_API_KEY"),
		ChannelUsername: must("CHANNEL_USERNAME"),
		DumpChatID:      dumpChatID,
		AdminID:         adminID,
		DBPath:          dbPath,
	}
}

// ─── Database ─────────────────────────────────────────────────────────────────

func initDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

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
	`

	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("init schema: %w", err)
	}

	return db, nil
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

func searchMemes(db *sql.DB, ftsQuery string) ([]tele.Result, error) {
	rows, err := db.Query(
		`SELECT file_id, rowid FROM memes WHERE search_vector MATCH ? ORDER BY rank LIMIT 50`,
		ftsQuery,
	)
	if err != nil {
		return nil, fmt.Errorf("fts query: %w", err)
	}
	defer rows.Close()

	results := make([]tele.Result, 0)
	for rows.Next() {
		var fileID string
		var rowid int64
		if err := rows.Scan(&fileID, &rowid); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		results = append(results, &tele.CachedPhoto{
			ResultBase: tele.ResultBase{ID: strconv.FormatInt(rowid, 10)},
			FileID:     fileID,
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
func buildSearchVector(text string) string {
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

// ─── Gemini Integration ───────────────────────────────────────────────────────

type geminiRequest struct {
	Contents []geminiContent `json:"contents"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text       string           `json:"text,omitempty"`
	InlineData *geminiInlineData `json:"inline_data,omitempty"`
}

type geminiInlineData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"` // base64-encoded
}

const geminiPrompt = "Опиши сцену на картинке 1-2 предложениями и полностью перепиши весь текст из комикса или мема. Отвечай только по-русски."

// describeImage fetches an image from Telegram by file_id and sends it to Gemini.
func describeImage(cfg Config, fileID string) (string, error) {
	// Step 1: resolve file_path via Telegram getFile
	tgFileURL := fmt.Sprintf("https://api.telegram.org/bot%s/getFile?file_id=%s", cfg.TelegramToken, fileID)
	resp, err := http.Get(tgFileURL)
	if err != nil {
		return "", fmt.Errorf("getFile request: %w", err)
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
		return "", fmt.Errorf("decode getFile: %w", err)
	}
	if !fileInfo.OK {
		return "", fmt.Errorf("getFile failed: %s", fileInfo.Description)
	}

	// Step 2: download image bytes
	imageURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", cfg.TelegramToken, fileInfo.Result.FilePath)
	imgResp, err := http.Get(imageURL)
	if err != nil {
		return "", fmt.Errorf("download image: %w", err)
	}
	defer imgResp.Body.Close()

	imageBytes, err := io.ReadAll(imgResp.Body)
	if err != nil {
		return "", fmt.Errorf("read image: %w", err)
	}

	// Infer MIME type from extension
	mimeType := "image/jpeg"
	switch {
	case strings.HasSuffix(fileInfo.Result.FilePath, ".png"):
		mimeType = "image/png"
	case strings.HasSuffix(fileInfo.Result.FilePath, ".webp"):
		mimeType = "image/webp"
	case strings.HasSuffix(fileInfo.Result.FilePath, ".gif"):
		mimeType = "image/gif"
	}

	// Step 3: call Gemini REST API
	geminiURL := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:generateContent?key=%s",
		cfg.GeminiAPIKey,
	)

	reqBody := geminiRequest{
		Contents: []geminiContent{{
			Parts: []geminiPart{
				{InlineData: &geminiInlineData{
					MimeType: mimeType,
					Data:     base64.StdEncoding.EncodeToString(imageBytes),
				}},
				{Text: geminiPrompt},
			},
		}},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal gemini request: %w", err)
	}

	geminiResp, err := http.Post(geminiURL, "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("gemini HTTP request: %w", err)
	}
	defer geminiResp.Body.Close()

	var geminiResult struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		Error *struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}

	if err := json.NewDecoder(geminiResp.Body).Decode(&geminiResult); err != nil {
		return "", fmt.Errorf("decode gemini response: %w", err)
	}

	if geminiResult.Error != nil {
		return "", fmt.Errorf("gemini API error %d: %s", geminiResult.Error.Code, geminiResult.Error.Message)
	}

	if len(geminiResult.Candidates) == 0 || len(geminiResult.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty gemini response for file_id=%s", fileID)
	}

	return strings.TrimSpace(geminiResult.Candidates[0].Content.Parts[0].Text), nil
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
	fileID string
	msgID  int
}

// runWorker consumes jobs from jobChan, gated by a 4.5-second ticker to stay
// within Gemini Free Tier limits (15 req/min).
func runWorker(bot *tele.Bot, db *sql.DB, cfg Config, jobChan <-chan indexJob) {
	ticker := time.NewTicker(4500 * time.Millisecond)
	defer ticker.Stop()

	log.Println("worker pool started (rate limit: 1 req / 4.5 s)")

	for job := range jobChan {
		<-ticker.C // wait for the rate-limit window

		already, err := isAlreadyIndexed(db, job.msgID)
		if err != nil {
			log.Printf("worker: db check error msg_id=%d: %v", job.msgID, err)
		} else if already {
			log.Printf("worker: msg_id=%d already indexed, skipping", job.msgID)
			continue
		}

		log.Printf("worker: describing msg_id=%d file_id=%s", job.msgID, job.fileID)

		desc, err := describeImage(cfg, job.fileID)
		if err != nil {
			sendAdminAlert(bot, cfg.AdminID, fmt.Sprintf(
				"Gemini failed for msg_id=%d file_id=%s: %v", job.msgID, job.fileID, err,
			))
			continue
		}

		if err := saveMeme(db, job.fileID, job.msgID, desc); err != nil {
			sendAdminAlert(bot, cfg.AdminID, fmt.Sprintf(
				"DB save failed for msg_id=%d: %v", job.msgID, err,
			))
			continue
		}

		preview := desc
		if len(preview) > 100 {
			preview = preview[:100] + "…"
		}
		log.Printf("worker: indexed msg_id=%d — %s", job.msgID, preview)
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

// crawlHistory iterates message IDs from 1 upward, copying each post to the
// dump chat to obtain its file_id. It resumes from where it left off on restart.
func crawlHistory(bot *tele.Bot, db *sql.DB, cfg Config, channelID int64, dumpChat *tele.Chat, jobChan chan<- indexJob) {
	lastStr, err := getCrawlerState(db, "last_crawled_msg_id")
	if err != nil {
		log.Printf("crawler: cannot read state: %v", err)
		lastStr = "0"
	}
	startID, _ := strconv.Atoi(lastStr)
	log.Printf("crawler: starting from msg_id=%d", startID+1)

	const maxGap = 100 // consecutive misses before we consider history exhausted
	consecutiveMisses := 0

	for msgID := startID + 1; ; msgID++ {
		time.Sleep(60 * time.Millisecond) // ~16 req/s to Telegram, well under flood limit

		copied, err := bot.Copy(dumpChat, channelMsg{id: msgID, chatID: channelID})
		if err != nil {
			consecutiveMisses++
			if consecutiveMisses >= maxGap {
				log.Printf("crawler: %d consecutive misses at msg_id=%d — history exhausted", maxGap, msgID)
				break
			}
			continue
		}
		consecutiveMisses = 0

		// Persist progress immediately
		if saveErr := setCrawlerState(db, "last_crawled_msg_id", strconv.Itoa(msgID)); saveErr != nil {
			log.Printf("crawler: cannot save state: %v", saveErr)
		}

		if copied.Photo == nil {
			// Not a photo — discard the copy
			if delErr := bot.Delete(copied); delErr != nil {
				log.Printf("crawler: delete non-photo copy msg_id=%d: %v", msgID, delErr)
			}
			continue
		}

		fileID := copied.Photo.FileID
		log.Printf("crawler: photo msg_id=%d file_id=%s", msgID, fileID)

		// Remove the temporary copy from the dump chat
		if delErr := bot.Delete(copied); delErr != nil {
			log.Printf("crawler: delete photo copy msg_id=%d: %v", msgID, delErr)
		}

		jobChan <- indexJob{fileID: fileID, msgID: msgID}
	}

	log.Println("crawler: history scan complete")
}

// ─── Telegram helper ──────────────────────────────────────────────────────────

// resolveChannelID calls getChat to obtain the numeric ID for a channel username.
func resolveChannelID(token, username string) (int64, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getChat?chat_id=%s", token, username)
	resp, err := http.Get(url)
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
	go runWorker(bot, db, cfg, jobChan)

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

	dumpChat := &tele.Chat{ID: cfg.DumpChatID}
	go crawlHistory(bot, db, cfg, channelID, dumpChat, jobChan)

	log.Println("memebot running")
	bot.Start()
}
