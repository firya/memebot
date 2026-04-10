package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	tele "gopkg.in/telebot.v3"
)

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
	var workerIntervalNs atomic.Int64
	workerIntervalNs.Store(int64(workerIntervalEconom))

	hashes, err := loadHashes(db)
	if err != nil {
		log.Fatalf("load image hashes: %v", err)
	}
	log.Printf("loaded %d image hashes into memory", len(hashes))

	go runWorker(bot, db, cfg, jobChan, workerWake, &workerQuotaUntil, &workerIntervalNs, &hashes)

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

	statusMsg := func() string {
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
		if cfg.DevMode {
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
		if until := workerQuotaUntil.Load(); until != 0 {
			wakeTime := time.Unix(until, 0).UTC()
			msg += fmt.Sprintf("\n💤 Квота Gemini исчерпана, worker спит до %s UTC", wakeTime.Format("2006-01-02 15:04"))
		}
		return msg
	}

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
			if _, err := bot.Send(admin, statusMsg()); err != nil {
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

	// ── Bot handlers ──────────────────────────────────────────────────────────

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

	bot.Handle("/status", func(c tele.Context) error {
		if c.Chat().ID != cfg.AdminID {
			return nil
		}
		return c.Send(statusMsg())
	})

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
			"_(при дневном лимите Gemini не поможет — квота сбросится в 00:05 UTC)_\n" +
			"`/boost` — платный тариф (4000 RPM / 150000 RPD)\n" +
			"`/econom` — бесплатный тариф (15 RPM / 500 RPD)\n\n" +
			"*Поиск:*\n" +
			"Inline-поиск работает везде: `@botusername запрос`\n" +
			"Бот понимает русский язык со стеммингом — достаточно части слова" +
			devSection
		return c.Send(text, tele.ModeMarkdown)
	})

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

	bot.Handle("/boost", func(c tele.Context) error {
		if c.Chat().ID != cfg.AdminID {
			return nil
		}
		workerIntervalNs.Store(int64(workerIntervalBoost))
		return c.Send("⚡ Boost mode: 4000 RPM / 150000 RPD (15 мс/запрос). Вернуть: /econom")
	})

	bot.Handle("/econom", func(c tele.Context) error {
		if c.Chat().ID != cfg.AdminID {
			return nil
		}
		workerIntervalNs.Store(int64(workerIntervalEconom))
		return c.Send("🐢 Econom mode: 15 RPM / 500 RPD (4000 мс/запрос).")
	})

	bot.Handle("/reset", func(c tele.Context) error {
		if c.Chat().ID != cfg.AdminID {
			return nil
		}
		if crawlerRunning.Load() {
			crawlerCancel()
			time.Sleep(500 * time.Millisecond)
			crawlerCtx, crawlerCancel = context.WithCancel(context.Background())
		}
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
