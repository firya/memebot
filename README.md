# memebot
Telegram-бот для индексации и поиска мемов из канала. Анализирует изображения через AI (Claude или Gemini), сохраняет описания в SQLite с полнотекстовым поиском, работает как inline-бот.

## Как это работает

1. Бот слушает новые фото в указанном канале и индексирует их
2. При запуске (prod-режим) или по команде `/index` (dev-режим) краулер обходит историю канала
3. Каждое изображение анализирует AI: описание, весь текст с картинки, узнаваемые люди
4. Перцептивный хэш (dHash) отсеивает визуально одинаковые картинки до отправки в AI
5. Результат сохраняется в FTS5-индекс SQLite со стеммингом на русском
6. Пользователи ищут через inline-режим: `@botusername котики`

## Требования

- Docker + Docker Compose
- Telegram Bot Token ([@BotFather](https://t.me/BotFather))
- API-ключ Claude или Gemini
- Публичный Telegram-канал

## Быстрый старт

**1. Клонируй репозиторий и создай `.env`:**

```bash
cp .env.example .env
```

**2. Заполни `.env`:**

```env
TELEGRAM_TOKEN=       # токен от @BotFather
AI_PROVIDER=claude    # или gemini
CLAUDE_API_KEY=       # если AI_PROVIDER=claude
GEMINI_API_KEY=       # если AI_PROVIDER=gemini
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

## AI-провайдеры

### Claude (по умолчанию)

```env
AI_PROVIDER=claude
CLAUDE_API_KEY=sk-ant-...
```

Ключ: [platform.claude.com/settings/keys](https://platform.claude.com/settings/keys)

Используется модель `claude-haiku-4-5-20251001` — быстрая и дешёвая, поддерживает vision.

**Ограничение:** Claude не идентифицирует людей на фото по лицам (политика Anthropic). Имена извлекаются только из текста/подписей самого мема.

### Gemini

```env
AI_PROVIDER=gemini
GEMINI_API_KEY=AIza...
```

Ключ: [aistudio.google.com/apikey](https://aistudio.google.com/apikey)

Используется модель `gemini-3.1-flash-lite-preview`. Gemini идентифицирует известных людей на фото.

**Лимиты Gemini (бесплатный тариф):**
- RPM (запросов в минуту) — при превышении воркер автоматически делает паузу 60 с
- Дневная квота — при исчерпании воркер спит до 00:05 UTC следующего дня и возобновляется автоматически

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
AI_PROVIDER=gemini
GEMINI_API_KEY=AIza...
GEMINI_WORKER_URL=https://ИМЯ.АККАУНТ.workers.dev
GEMINI_WORKER_SECRET=твой-секрет
```

**Проверить что Worker работает:**

```bash
# Должен вернуть 401 (Worker жив, секрет не передан)
curl -s -o /dev/null -w "%{http_code}" https://ИМЯ.АККАУНТ.workers.dev

# Полная проверка с ключом и секретом (одна строка)
curl -s -X POST "https://ИМЯ.АККАУНТ.workers.dev/v1beta/models/gemini-3.1-flash-lite-preview:generateContent?key=GEMINI_API_KEY" -H "Content-Type: application/json" -H "X-Worker-Secret: WORKER_SECRET" -d '{"contents":[{"parts":[{"text":"скажи привет"}]}]}' | jq .candidates[0].content.parts[0].text
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

Бот также принимает **фото в личку** от администратора — анализирует через AI и отвечает описанием (без сохранения в БД).

## Устойчивость при перезапуске

Краулер сохраняет два чекпоинта в БД:
- `last_crawled_msg_id` — последний осмотренный msg_id
- `last_worker_msg_id` — последний **проиндексированный** msg_id

При рестарте краулер возобновляется от `last_worker_msg_id`, чтобы не потерять фото, которые были в памяти (in-flight queue) на момент остановки контейнера. Повторно встреченные уже проиндексированные фото пропускаются.

## Переменные окружения

| Переменная | Обязательна | По умолчанию | Описание |
|---|---|---|---|
| `TELEGRAM_TOKEN` | да | — | Токен бота |
| `AI_PROVIDER` | нет | `claude` | Провайдер: `claude` или `gemini` |
| `CLAUDE_API_KEY` | если `AI_PROVIDER=claude` | — | API-ключ Anthropic |
| `GEMINI_API_KEY` | если `AI_PROVIDER=gemini` | — | API-ключ Google |
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
main.go              — весь код бота
gemini-worker/
  worker.js          — Cloudflare Worker для проксирования Gemini API
data/
  memes.db           — SQLite с FTS5-индексом (создаётся автоматически)
.env.example         — шаблон конфигурации
compose.yaml         — Docker Compose
Dockerfile           — многоэтапная сборка (Go + Alpine)
```
