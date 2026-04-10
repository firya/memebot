package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync/atomic"
	"time"

	tele "gopkg.in/telebot.v3"
)

type indexJob struct {
	fileID  string
	msgID   int
	replyTo int64 // if > 0, send AI result back to this chat (admin DM)
	retries int   // number of AI retry attempts so far
}

// workerIntervalEconom and workerIntervalBoost define the request cadence
// for the two rate-limit modes selectable via /econom and /boost.
const (
	workerIntervalEconom = 4000 * time.Millisecond // 15 RPM / 500 RPD (free tier)
	workerIntervalBoost  = 15 * time.Millisecond   // 4000 RPM / 150000 RPD (paid tier)
)

func sendAdminAlert(bot *tele.Bot, adminID int64, msg string) {
	log.Printf("ADMIN ALERT: %s", msg)
	admin := &tele.User{ID: adminID}
	if _, err := bot.Send(admin, "🚨 "+msg); err != nil {
		log.Printf("failed to deliver admin alert: %v", err)
	}
}

// runWorker consumes jobs from jobChan, gated by a ticker to stay within rate limits.
// wake is a buffered channel; sending to it interrupts any rate-limit sleep early.
// quotaUntil is set to the Unix timestamp of the planned wakeup while the worker
// sleeps due to daily quota exhaustion, and reset to 0 when it resumes.
// intervalNs holds the current ticker interval in nanoseconds; writing to it
// causes the worker to reset its ticker on the next iteration.
func runWorker(bot *tele.Bot, db *sql.DB, cfg Config, jobChan chan indexJob, wake <-chan struct{}, quotaUntil *atomic.Int64, intervalNs *atomic.Int64, hashes *[]uint64) {
	currentInterval := workerIntervalEconom
	ticker := time.NewTicker(currentInterval)
	defer ticker.Stop()

	log.Printf("worker started (interval: %s)", currentInterval)

	for job := range jobChan {
		// Apply interval change requested via /boost or /econom.
		if d := time.Duration(intervalNs.Load()); d != currentInterval {
			currentInterval = d
			ticker.Reset(currentInterval)
			log.Printf("worker: interval changed to %s", currentInterval)
		}

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
			const maxFetchRetries = 5
			if job.replyTo == 0 && job.retries < maxFetchRetries {
				job.retries++
				delay := time.Duration(job.retries*30) * time.Second
				log.Printf("worker: fetch retry %d/%d msg_id=%d in %s", job.retries, maxFetchRetries, job.msgID, delay)
				go func(j indexJob) {
					time.Sleep(delay)
					jobChan <- j
				}(job)
			} else if job.replyTo == 0 {
				errMsg := fmt.Sprintf("fetch permanently failed for msg_id=%d (retries=%d): %v", job.msgID, job.retries, err)
				sendAdminAlert(bot, cfg.AdminID, errMsg)
				if _, dbErr := db.Exec("INSERT OR REPLACE INTO failed_msgs(msg_id, file_id) VALUES (?, ?)", job.msgID, job.fileID); dbErr != nil {
					log.Printf("worker: save failed_msg error msg_id=%d: %v", job.msgID, dbErr)
				}
			}
			continue
		}

		// Compute perceptual hash once; reused for both dedup check and storage.
		var imgHash uint64
		if job.replyTo == 0 {
			imgHash = computeDHash(imageBytes)
			if isDuplicate(*hashes, imgHash) {
				log.Printf("worker: msg_id=%d is a visual duplicate, skipping", job.msgID)
				// Mark as indexed so the crawler doesn't revisit it.
				if err := storeImageHash(db, imgHash); err != nil {
					log.Printf("worker: store dup hash error msg_id=%d: %v", job.msgID, err)
				} else {
					*hashes = append(*hashes, imgHash)
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
			if err := storeImageHash(db, imgHash); err != nil {
				log.Printf("worker: store phash error msg_id=%d: %v", job.msgID, err)
			} else if imgHash != 0 {
				*hashes = append(*hashes, imgHash)
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
