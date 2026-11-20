package webui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"llm-proxy/internal/config"
	"llm-proxy/internal/socks"
)

func (h *Handler) handleModelCatalog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	snap := h.snap()
	token := snap.Cfg.Server.WebUIToken
	if token == "" {
		writeJSONError(w, http.StatusForbidden, "Для загрузки моделей задайте server.webui_token")
		return
	}
	if !validWebUIToken(r, token) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	provider, id, ok := strings.Cut(r.URL.Query().Get("account"), "/")
	p, exists := snap.Cfg.Providers[provider]
	if !ok || id == "" || !exists {
		writeJSONError(w, http.StatusNotFound, "Аккаунт не найден")
		return
	}
	var base, suffix, key, path string
	switch p.Type {
	case config.ProviderOpenAILike:
		for _, a := range p.OpenAILike.Accounts {
			if a.ID == id {
				base, suffix, key, path = p.OpenAILike.BaseURL, "/v1/models", a.Auth.Key, a.Socks
				break
			}
		}
	case config.ProviderGLM:
		for _, a := range p.GLM.Accounts {
			if a.ID == id {
				base, suffix, key, path = p.GLM.BaseURL, "/api/paas/v4/models", a.Auth.Key, a.Socks
				if a.CodingPlan {
					suffix = "/api/coding/paas/v4/models"
				}
				break
			}
		}
	default:
		writeJSONError(w, http.StatusNotImplemented, "Для этого типа провайдера нет поддерживаемого каталога. Введите ID модели вручную.")
		return
	}
	if base == "" {
		writeJSONError(w, http.StatusNotFound, "Аккаунт не найден")
		return
	}
	var transport socks.Transporter = socks.Direct{}
	if path != "none" {
		p := snap.Socks[path]
		if p == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "SOCKS-путь аккаунта недоступен")
			return
		}
		transport = p
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		writeJSONError(w, http.StatusBadRequest, "Некорректный базовый URL провайдера")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+suffix, nil)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "Некорректный URL каталога моделей")
		return
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Accept", "application/json")
	client := *transport.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		var blocked *socks.BlockedError
		if errors.As(err, &blocked) {
			writeJSONError(w, http.StatusServiceUnavailable, "SOCKS-путь аккаунта заблокирован")
		} else {
			writeJSONError(w, http.StatusBadGateway, "Не удалось получить каталог моделей")
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeJSONError(w, http.StatusBadGateway, "Провайдер не вернул каталог: HTTP "+strconv.Itoa(resp.StatusCode))
		return
	}
	const maxCatalog = 2 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalog+1))
	var result struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err != nil || len(body) > maxCatalog || json.Unmarshal(body, &result) != nil || result.Data == nil {
		writeJSONError(w, http.StatusBadGateway, "Провайдер вернул некорректный каталог моделей")
		return
	}
	seen := make(map[string]struct{}, len(result.Data))
	models := make([]string, 0, len(result.Data))
	for _, item := range result.Data {
		if item.ID == "" {
			continue
		}
		if _, ok := seen[item.ID]; !ok {
			seen[item.ID] = struct{}{}
			models = append(models, item.ID)
		}
	}
	sort.Strings(models)
	writeJSON(w, map[string][]string{"models": models})
}
