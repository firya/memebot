package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	tele "gopkg.in/telebot.v3"
)

// channelMsg implements tele.Editable so we can call bot.Forward on arbitrary
// message IDs from a known channel chat.
type channelMsg struct {
	id     int
	chatID int64
}

func (m channelMsg) MessageSig() (string, int64) {
	return strconv.Itoa(m.id), m.chatID
}

// resolveChannelID calls getChat to obtain the numeric ID for a channel username.
// apiURL overrides the default api.telegram.org base (e.g. a Cloudflare Worker).
// secret is sent as X-Worker-Secret if non-empty.
func resolveChannelID(token, username, apiURL, secret string) (int64, error) {
	base := "https://api.telegram.org"
	if apiURL != "" {
		base = strings.TrimRight(apiURL, "/")
	}
	return resolveChannelIDURL(
		fmt.Sprintf("%s/bot%s/getChat?chat_id=%s", base, token, username),
		secret,
	)
}

// resolveChannelIDURL fetches and decodes a getChat response from the given URL.
// Separated from resolveChannelID so tests can inject a mock server URL.
// secret is sent as X-Worker-Secret if non-empty.
func resolveChannelIDURL(rawURL, secret string) (int64, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return 0, fmt.Errorf("getChat request: %w", err)
	}
	if secret != "" {
		req.Header.Set("X-Worker-Secret", secret)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
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
			if strings.Contains(errStr, "retry") || strings.Contains(errStr, "Too Many") || strings.Contains(errStr, "timeout") || strings.Contains(errStr, "Timeout") || strings.Contains(errStr, "deadline exceeded") {
				consecutiveTransient++
				// After too many transient failures on the same message, skip it.
				// This prevents the crawler from hanging forever on a single msgID
				// when Telegram issues large retry-after values repeatedly.
				const maxTransientPerMsg = 5
				if consecutiveTransient > maxTransientPerMsg {
					log.Printf("crawler: too many transient errors for msg_id=%d (%d attempts), skipping", msgID, consecutiveTransient)
					consecutiveTransient = 0
					continue // advance to next msgID
				}
				// Respect Telegram's Retry-After if present (e.g. "retry after 1307").
				// Fall back to exponential backoff: 10s, 20s, 40s … capped at 5 min.
				const maxBackoff = 5 * time.Minute
				backoff := time.Duration(10<<min(consecutiveTransient-1, 5)) * time.Second
				if idx := strings.Index(errStr, "retry after "); idx != -1 {
					rest := errStr[idx+len("retry after "):]
					// rest may be "1307 (429)" — take the numeric prefix
					end := strings.IndexAny(rest, " (")
					if end > 0 {
						rest = rest[:end]
					}
					if secs, parseErr := strconv.Atoi(rest); parseErr == nil && secs > 0 {
						backoff = time.Duration(secs) * time.Second
					}
				}
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
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
