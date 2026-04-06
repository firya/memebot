## memebot — управление проектом
## Использование: make <команда>

.PHONY: help up down restart build logs db status

# Цвета
BOLD  := \033[1m
RESET := \033[0m
CYAN  := \033[36m

help: ## Показать это сообщение
	@echo ""
	@echo "  $(BOLD)memebot$(RESET)"
	@echo ""
	@printf "  $(CYAN)%-15s$(RESET) %s\n" "make up"      "Собрать и запустить (фон)"
	@printf "  $(CYAN)%-15s$(RESET) %s\n" "make down"    "Остановить и удалить контейнер"
	@printf "  $(CYAN)%-15s$(RESET) %s\n" "make restart" "Пересобрать и перезапустить"
	@printf "  $(CYAN)%-15s$(RESET) %s\n" "make logs"    "Смотреть логи в реальном времени"
	@printf "  $(CYAN)%-15s$(RESET) %s\n" "make status"  "Статус контейнера"
	@printf "  $(CYAN)%-15s$(RESET) %s\n" "make db"      "Открыть SQLite (кол-во мемов, последние записи)"
	@echo ""
	@echo "  $(BOLD)Файлы:$(RESET)"
	@printf "  $(CYAN)%-15s$(RESET) %s\n" ".env"          "Конфигурация (токены, ID чатов)"
	@printf "  $(CYAN)%-15s$(RESET) %s\n" "data/memes.db" "База данных SQLite с индексом мемов"
	@echo ""
	@echo "  $(BOLD)Команды боту в Telegram (dev-режим):$(RESET)"
	@printf "  $(CYAN)%-15s$(RESET) %s\n" "/index 10"    "Сбросить БД и проиндексировать 10 фото с начала"
	@printf "  $(CYAN)%-15s$(RESET) %s\n" "/index"       "То же самое, дефолт 10 фото"
	@echo ""

up: ## Собрать и запустить в фоне
	docker compose up --build -d
	@echo ""
	@echo "  Запущен. Логи: $(CYAN)make logs$(RESET)"

down: ## Остановить контейнер
	docker compose down

restart: ## Пересобрать и перезапустить
	docker compose down
	docker compose up --build -d
	@echo ""
	@echo "  Перезапущен. Логи: $(CYAN)make logs$(RESET)"

build: ## Только собрать образ без запуска
	docker compose build --no-cache

logs: ## Следить за логами
	docker compose logs -f

status: ## Статус контейнера
	docker compose ps

db: ## Показать статистику БД
	@echo ""
	@echo "  $(BOLD)Всего мемов:$(RESET)"
	@docker compose exec memebot sh -c \
		'sqlite3 /app/data/memes.db "SELECT count(*) FROM memes;"' 2>/dev/null \
		|| echo "  (контейнер не запущен или БД пуста)"
	@echo ""
	@echo "  $(BOLD)Последние 5 записей:$(RESET)"
	@docker compose exec memebot sh -c \
		'sqlite3 /app/data/memes.db "SELECT msg_id, substr(original_desc,1,100) FROM memes ORDER BY rowid DESC LIMIT 5;"' 2>/dev/null \
		|| true
	@echo ""
	@echo "  $(BOLD)Прогресс краулера:$(RESET)"
	@docker compose exec memebot sh -c \
		'sqlite3 /app/data/memes.db "SELECT value FROM crawler_state WHERE key=\"last_crawled_msg_id\";"' 2>/dev/null \
		|| true
	@echo ""
