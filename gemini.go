package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const geminiModel = "gemini-2.0-flash-lite"

// Package-level HTTP clients with connection pooling. Creating a new client
// per request forgoes keep-alive connections and wastes sockets.
var (
	tgAPIClient      = &http.Client{Timeout: 15 * time.Second}
	tgDownloadClient = &http.Client{Timeout: 60 * time.Second}
	geminiHTTPClient = &http.Client{Timeout: 30 * time.Second}
)

// errQuotaExceeded is returned by callGemini when the daily API quota is exhausted.
// The worker must not retry such requests — the quota resets at midnight UTC.
var errQuotaExceeded = errors.New("gemini daily quota exceeded")

// errRateLimited is returned by callGemini when the per-minute rate limit is hit.
// The worker should pause ~60 s and retry, NOT sleep until midnight.
var errRateLimited = errors.New("gemini rate limited (RPM)")

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
	resp, err := tgAPIClient.Get(tgFileURL)
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
	imgResp, err := tgDownloadClient.Get(imageURL)
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
	endpoint := base + "/v1beta/models/" + geminiModel + ":generateContent?key=" + apiKey
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("create gemini request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if workerSecret != "" {
		req.Header.Set("X-Worker-Secret", workerSecret)
	}

	geminiResp, err := geminiHTTPClient.Do(req)
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

// describeImageFromBytes sends pre-fetched image bytes to Gemini.
func describeImageFromBytes(cfg Config, imageBytes []byte, mimeType string) (string, error) {
	return callGemini(cfg.GeminiAPIKey, cfg.GeminiWorkerURL, cfg.GeminiWorkerSecret, imageBytes, mimeType)
}
