# memebot
Telegram-бот для индексации и поиска мемов из канала. Анализирует изображения через Gemini Vision, сохраняет описания в SQLite с полнотекстовым поиском, работает как inline-бот.

## Как это работает

1. Бот слушает новые фото в указанном канале и индексирует их
2. При запуске (prod-режим) краулер обходит историю канала; в dev-режиме — по команде `/index`
3. Каждое изображение анализирует Gemini Vision: описание, весь текст с картинки, узнаваемые люди
4. Перцептивный хэш (dHash) отсеивает визуально одинаковые картинки до отправки в AI
5. Результат сохраняется в FTS5-индекс SQLite со стеммингом на русском
6. Пользователи ищут через inline-режим: `@botusername котики`

## Требования

- Docker + Docker Compose
- Telegram Bot Token ([@BotFather](https://t.me/BotFather))
- Gemini API key
- Публичный Telegram-канал

## Быстрый старт

**1. Клонируй репозиторий и создай `.env`:**

```bash
cp .env.example .env
```

**2. Заполни `.env`:**

```env
TELEGRAM_TOKEN=       # токен от @BotFather
GEMINI_API_KEY=       # ключ Gemini API
CHANNEL_USERNAME=@mychannel
DUMP_CHAT_ID=         # ID приватного чата для временного форварда фото
ADMIN_ID=             # твой Telegram user ID
APP_ENV=dev           # dev — без авто-краулера; prod — краулер стартует сразу
```

**3. Запусти:**

```bash
make up
make logs   # смотреть логи
```

**4. В dev-режиме запусти индексацию командой боту в личку:**

```
/index 20
```

## Настройка: получить нужные ID

| Параметр | Как получить |
|---|---|
| `TELEGRAM_TOKEN` | [@BotFather](https://t.me/BotFather) → /newbot |
| `ADMIN_ID` | Переслать любое сообщение [@userinfobot](https://t.me/userinfobot) |
| `DUMP_CHAT_ID` | Создать приватный канал, добавить бота админом, переслать сообщение из него в @userinfobot |

Бот должен быть **администратором** в канале-источнике и в dump-чате.

## Gemini API

Ключ: [aistudio.google.com/apikey](https://aistudio.google.com/apikey)

Используется модель `gemini-3.1-flash-lite-preview`. Gemini идентифицирует известных людей на фото.

**Лимиты (бесплатный тариф):**
- 15 RPM, 500 RPD — воркер работает в режиме `/econom` по умолчанию
- При превышении RPM — воркер делает паузу 60 с и продолжает
- При исчерпании дневной квоты — воркер спит до 00:05 UTC следующего дня

**Платный тариф:** после апгрейда переключи `/boost` (4000 RPM / 150000 RPD).

**Если Gemini заблокирован в твоём регионе** (например, сервер в Нидерландах) — используй Cloudflare Worker как прокси (см. ниже).

## Cloudflare Worker для Gemini (обход геоблока)

Cloudflare Workers работают на edge-сети без региональных ограничений и бесплатны до 100 000 запросов в день.

**1. Создай Worker:**

- Зайди на [dash.cloudflare.com](https://dash.cloudflare.com) → **Workers & Pages** → **Create** → **Create Worker**
- Замени весь код содержимым файла [`gemini-worker/worker.js`](gemini-worker/worker.js)
- Нажми **Deploy**
- Скопируй URL воркера: `https://ИМЯ.АККАУНТ.workers.dev`

**2. Добавь секрет:**

В настройках Worker → **Settings** → **Variables** → **Add variable**:
- Name: `WORKER_SECRET`
- Value: любая случайная строка (`openssl rand -hex 16`)
- Нажми **Encrypt**

**3. Добавь в `.env`:**

```env
GEMINI_WORKER_URL=https://ИМЯ.АККАУНТ.workers.dev
GEMINI_WORKER_SECRET=твой-секрет
```

**Проверить что Worker работает:**

```bash
# Должен вернуть 401 (Worker жив, секрет не передан)
curl -s -o /dev/null -w "%{http_code}" https://ИМЯ.АККАУНТ.workers.dev

# Полная проверка с ключом и секретом (одна строка)
curl -s -X POST "https://ИМЯ.АККАУНТ.workers.dev/v1beta/models/gemini-2.0-flash-lite:generateContent?key=GEMINI_API_KEY" \
  -H "Content-Type: application/json" \
  -H "X-Worker-Secret: WORKER_SECRET" \
  -d '{"contents":[{"parts":[{"text":"скажи привет"}]}]}' | jq .candidates[0].content.parts[0].text
```

## Команды Makefile

```
make up        — собрать и запустить в фоне
make down      — остановить контейнер
make restart   — пересобрать и перезапустить
make logs      — следить за логами
make status    — статус контейнера
make db        — статистика БД (кол-во мемов, последние записи)
```

## Команды бота (в личку, только для ADMIN_ID)

| Команда | Режим | Описание |
|---|---|---|
| `/help` | dev + prod | Справка по всем командам |
| `/status` | dev + prod | Мемов в БД, прогресс краулера, длина очереди |
| `/resume` | dev + prod | Продолжить краулер с последнего сохранённого msg_id |
| `/stop` | dev + prod | Остановить краулер (прогресс сохраняется) |
| `/reset` | dev + prod | Сбросить БД и перезапустить краулер с начала |
| `/reset <n>` | dev | Сбросить БД и проиндексировать первые N фото |
| `/index <n>` | dev | Алиас для `/reset <n>` |
| `/analyze` | dev + prod | Разбудить воркер досрочно при RPM-лимите |
| `/boost` | dev + prod | Платный тариф: 4000 RPM / 150000 RPD |
| `/econom` | dev + prod | Бесплатный тариф: 15 RPM / 500 RPD (по умолчанию) |

Бот также принимает **фото в личку** от администратора — анализирует через Gemini и отвечает описанием (без сохранения в БД).

## Устойчивость при перезапуске

Краулер сохраняет два чекпоинта в БД:
- `last_crawled_msg_id` — последний осмотренный msg_id
- `last_worker_msg_id` — последний **проиндексированный** msg_id (новый уникальный мем)

При рестарте краулер возобновляется от `last_worker_msg_id`, чтобы не потерять фото, которые были в памяти (in-flight queue) на момент остановки. Повторно встреченные уже проиндексированные фото пропускаются.

Задания, которые AI не смог обработать после нескольких попыток, сохраняются в таблице `failed_msgs` и автоматически ставятся в очередь при следующем запуске.

## Переменные окружения

| Переменная | Обязательна | По умолчанию | Описание |
|---|---|---|---|
| `TELEGRAM_TOKEN` | да | — | Токен бота |
| `GEMINI_API_KEY` | да | — | API-ключ Google Gemini |
| `GEMINI_WORKER_URL` | нет | — | URL Cloudflare Worker для Gemini |
| `GEMINI_WORKER_SECRET` | нет | — | Секрет для аутентификации Worker |
| `CHANNEL_USERNAME` | да | — | Username канала с `@` |
| `DUMP_CHAT_ID` | да | — | ID приватного чата для обхода истории |
| `ADMIN_ID` | да | — | Telegram ID администратора |
| `APP_ENV` | нет | `prod` | `dev` — краулер не стартует автоматически |
| `DB_PATH` | нет | `/app/data/memes.db` | Путь к SQLite-базе |
| `CRAWLER_MAX_GAP` | нет | `100` | Подряд пропущенных msg_id до остановки краулера |

## Структура проекта

```
config.go            — Config struct и loadConfig()
main.go              — инициализация, bot handlers, запуск краулера
crawler.go           — обход истории канала (crawlHistory, resolveChannelID)
worker.go            — AI-воркер: скачивает фото, вызывает Gemini, сохраняет в БД
gemini.go            — Gemini Vision API, fetchImageBytes, callGemini
db.go                — initDB, saveMeme, searchMemes, dHash, хеш-дедупликация
nlp.go               — стемминг, FTS-запрос (buildFTSQuery, buildSearchVector)
gemini-worker/
  worker.js          — Cloudflare Worker для проксирования Gemini API
data/
  memes.db           — SQLite с FTS5-индексом (создаётся автоматически)
.env.example         — шаблон конфигурации
compose.yaml         — Docker Compose
Dockerfile           — многоэтапная сборка (Go + Alpine)
```
