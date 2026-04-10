package main

import (
	"log"
	"os"
	"strconv"
)

type Config struct {
	TelegramToken      string
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

	return Config{
		TelegramToken:      must("TELEGRAM_TOKEN"),
		GeminiAPIKey:       must("GEMINI_API_KEY"),
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
