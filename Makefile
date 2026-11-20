.PHONY: build run test check-egress smoke

CONFIG ?= ./config.json

# VERSION — читаемая операция: git describe (версия/хэш коммита), вне git —
# дата сборки. Никаких записей в git (состояние репо не меняется).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || date -u +%Y%m%d%H%M%S)

# Сборка статического бинарника: cgo запрещён (SPEC 13, CODING 1.1).
# Версия попадает в User-Agent codex-запросов (SPEC 7.1) через
# -X llm-proxy/internal/upstream/codex.Version.
build:
	CGO_ENABLED=0 go build -ldflags "-X 'llm-proxy/internal/upstream/codex.Version=$(VERSION)'" -o bin/llm-proxy ./cmd/proxy

run: build
	@if [ "$(CONFIG)" = "./config.json" ] && [ ! -e "$(CONFIG)" ]; then \
		(umask 077; set -C; cat config.local.example.json > "$(CONFIG)") || exit 1; \
	fi
	exec ./bin/llm-proxy -config "$(CONFIG)"

test:
	go test ./... -count=1

# grep-тест инварианта I1 (ARCHITECTURE §4): http.Client/http.Transport для внешних
# хостов создаёт ТОЛЬКО internal/socks; DefaultClient/DefaultTransport запрещены везде.
# Любое совпадение вне internal/socks/ — ошибка сборки (nonzero exit).
check-egress:
	@hits="$$({ grep -REn 'http\.(Client|Transport)\{' --include='*.go' . | grep -v '^./internal/socks/'; grep -REn 'http\.Default(Client|Transport)' --include='*.go' .; })"; if [ -n "$$hits" ]; then echo "check-egress: FAIL — найден http-клиент/транспорт вне internal/socks (инвариант I1):"; printf '%s\n' "$$hits"; exit 1; else echo "check-egress: ok — http-клиенты только в internal/socks"; fi

# Smoke-гейт фазы (SPEC 14): фейковый openai-апстрим + прокси, оба входа
# stream/non-stream, строки в SQLite. Зависимости скрипта: go, curl, python3.
smoke:
	bash scripts/smoke.sh
