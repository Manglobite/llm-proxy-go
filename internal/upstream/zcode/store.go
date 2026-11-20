// zcode-tokens.json: хранилище рантайм-состояния OAuth-цепочки zcode
// (SPEC 3.2а). Владелец файла — только этот Store: internal/config про файл
// не знает ничего (SPEC 13), мёрж при SIGHUP — накопленная in-memory карта
// (как у codex.Store, CF-7): ключи исчезнувших из конфига аккаунтов не
// удаляются молча, удалить может только явный Delete.
//
// PAT (короткоживущий токен модельных запросов) в файл НЕ пишется — он живёт
// только в памяти (PATCache, chain.go), как у самого ZCode.
package zcode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// TokenRec — запись одного аккаунта в zcode-tokens.json (форма — SPEC 3.2а).
// business_expires_at/zai_expires_at — RFC3339 (стандартный маршалинг
// time.Time); zai_expires_at опционален (exp-клеймо JWT, если парсится) —
// детектор смерти цепочки работает только по бизнес-коду auth/z/login.
type TokenRec struct {
	ZaiAccessToken    string     `json:"zai_access_token"`         // OAuth-токен z.ai (~2ч, refresh-эндпоинта нет)
	ZcodeJWT          string     `json:"zcode_jwt"`                // zcodejwttoken (HS256, без exp) — маршрут zcode_plan
	UserID            string     `json:"user_id"`                  // из poll-ответа (user.user_id)
	Email             string     `json:"email,omitempty"`          // из poll-ответа (email, fallback name)
	BusinessJWT       string     `json:"business_jwt,omitempty"`   // бизнес-JWT api.z.ai (auth/z/login)
	BusinessExpiresAt time.Time  `json:"business_expires_at"`      // из expires_in ответа auth/z/login (⚠ реальный TTL)
	ZaiExpiresAt      *time.Time `json:"zai_expires_at,omitempty"` // exp-клеймо JWT zai-токена, если парсится
	Org               string     `json:"org,omitempty"`            // локация ключа (getCustomerInfo)
	Project           string     `json:"project,omitempty"`        // локация ключа (getCustomerInfo)
	APIKeyID          string     `json:"api_key_id,omitempty"`     // Key ID «zcode-api-key» (не секрет)
	DeviceMid         string     `json:"device_mid,omitempty"`     // UUID «устройства»: при первом флоу, стабилен
}

// Рантайм-статусы аккаунта (SPEC 7.4 «needs_relogin»): только память, в файл
// не пишутся; сбрасываются успешным OAuth (Set из флоу); проекция в
// /api/status — задача WebUI-фазы.
const (
	StatusNoToken      = "no_token"
	StatusOK           = "ok"
	StatusNeedsRelogin = "needs_relogin"
)

// tokenStatus — рантайм-статус аккаунта: State — константа Status*,
// Err — текст последней фатальной ошибки (для сообщения пользователю).
type tokenStatus struct {
	State string
	Err   string
}

// String — текстовое представление (логи, будущий экран статуса).
func (s tokenStatus) String() string {
	if s.Err != "" {
		return s.State + ": " + s.Err
	}
	return s.State
}

// Store — потокобезопасное хранилище zcode-tokens.json и ЕДИНСТВЕННЫЙ
// владелец файла (SPEC 3.2а/13). Открывается в main ОДИН раз и живёт до
// остановки процесса; SIGHUP-релоад Store не трогает и файл не перечитывает.
type Store struct {
	path   string
	mu     sync.Mutex
	data   map[string]TokenRec    // ключ "zcode/<account_id>" (SPEC 3.2а)
	status map[string]tokenStatus // рантайм-статусы, в файл не пишутся
}

// OpenStore читает zcode-tokens.json; отсутствие файла — не ошибка (аккаунт
// ещё не проходил OAuth). Битый файл — ошибка: молча продолжать с пустой
// картой значило бы тихо потерять доступ ко всем аккаунтам.
func OpenStore(path string) (*Store, error) {
	s := &Store{
		path:   path,
		data:   map[string]TokenRec{},
		status: map[string]tokenStatus{},
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("zcode-tokens.json: %w", err)
	}
	return s, nil
}

// Set сохраняет запись аккаунта целиком: in-memory сразу + атомарная запись
// файла (tmp+rename на той же ФС, 0600 — в файле живые токены). Чужие ключи
// карты не трогаются — OAuth одного аккаунта не затирает токены остальных.
// Успешная запись живых токенов сбрасывает needs_relogin (SPEC 7.4: статус
// сбрасывается успешным OAuth).
func (s *Store) Set(key string, rec TokenRec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = rec
	delete(s.status, key) // ok — статуса в карте нет (Status() отдаёт по факту записи)
	return s.writeLocked()
}

// Get возвращает копию записи аккаунта.
func (s *Store) Get(key string) (TokenRec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.data[key]
	return rec, ok
}

// Delete удаляет ключ. В рантайме v1 автоматически не вызывается (CF-7) —
// точка для явных действий пользователя из WebUI в будущем.
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	delete(s.status, key)
	return s.writeLocked()
}

// Keys — снимок списка ключей (планировщик PAT-тикет обходит аккаунты).
func (s *Store) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.data))
	for k := range s.data {
		out = append(out, k)
	}
	return out
}

// StoreKeyPrefix — literal-префикс ключа записи (SPEC 3.2а): один файл на
// процесс, провайдеров типа zcode в v1 один. Наружу — чтобы consumers
// (webui-валидация ключа аккаунта) не дублировали литерал.
const StoreKeyPrefix = "zcode"

// Status — рантайм-статус аккаунта: no_token (записи нет или она пустая),
// ok, needs_relogin (+текст ошибки). Память Store, в файл не пишется.
func (s *Store) Status(key string) tokenStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.data[key]; !ok || rec.ZaiAccessToken == "" {
		return tokenStatus{State: StatusNoToken}
	}
	if st, ok := s.status[key]; ok {
		return st
	}
	return tokenStatus{State: StatusOK}
}

// setStatus — рантайм-статус ставят цепочка (needs_relogin) и её обитатели;
// сбрасывается успешным Set (OAuth-флоу). Unexported: статус — внутреннее
// состояние пакета, наружу читается через Status.
func (s *Store) setStatus(key, state, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state == StatusOK {
		delete(s.status, key)
		return
	}
	s.status[key] = tokenStatus{State: state, Err: errMsg}
}

// UpdateChainState — точечный мёрж состояния PAT-цепочки в запись аккаунта:
// мутация применяется к АКТУАЛЬНОЙ записи под мьютексом, а не к копии
// вызывающего — иначе параллельный OAuth-флоу, записавший свежие токены
// между Get и Set цепочки, был бы затёрт устаревшей копией.
func (s *Store) UpdateChainState(key string, mut func(*TokenRec)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.data[key]
	if !ok {
		return fmt.Errorf("zcode-tokens.json: запись %q отсутствует", key)
	}
	mut(&cur)
	s.data[key] = cur
	return s.writeLocked()
}

// writeLocked — общая запись файла под mu. tmp+rename атомарны только на
// той же ФС, поэтому tmp — sibling файла (SPEC 3.2а); права 0600.
func (s *Store) writeLocked() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
