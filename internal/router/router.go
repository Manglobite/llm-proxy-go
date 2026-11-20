// Пакет internal/router — резолв алиаса в цель (SPEC 5). Чистый пакет:
// про сеть не знает ничего (никаких импортов socks/http) — transporter
// добирает сервер из snap.Socks по Target.SocksName.
package router

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"llm-proxy/internal/config"
)

// Target — цель запроса после резолва алиаса (SPEC 5): (провайдер, аккаунт,
// модель апстрима). SocksName — имя SOCKS-пути аккаунта; сервер берёт
// transporter из snap.Socks (для "none" — socks.Direct).
type Target struct {
	ProviderName string
	ProviderType string // 'codex' | 'glm' | 'zcode' | 'openai-like'
	AccountID    string
	Model        string // модель апстрима
	SocksName    string
}

// ErrUnknownAlias — model из запроса не является алиасом. Типизированная
// ошибка: сервер ветвится через errors.Is и отвечает 404 со списком
// доступных алиасов (SV-2).
var ErrUnknownAlias = errors.New("unknown alias")

// Resolve находит алиас в снапшоте и собирает Target. Ссылочная целостность
// алиас → аккаунт проверена при загрузке конфига (CF-2); здесь ошибки возможны
// только у рассинхронизированного снапшота — они уходят наверх как обычные.
func Resolve(snap *config.Snapshot, alias string) (Target, error) {
	a, ok := snap.Cfg.Aliases[alias]
	if !ok {
		// Оборачиваем в %w: вызывающий различает «неизвестный алиас» (404)
		// от внутренних несостыковок конфигурации (500).
		return Target{}, fmt.Errorf("%w: %q", ErrUnknownAlias, alias)
	}
	pname, accID, _ := strings.Cut(a.Account, "/")
	prov, ok := snap.Cfg.Providers[pname]
	if !ok {
		return Target{}, fmt.Errorf("alias %q: провайдер %q не найден в снапшоте", alias, pname)
	}
	socksName, err := accountSocks(prov, accID)
	if err != nil {
		return Target{}, fmt.Errorf("alias %q: %w", alias, err)
	}
	return Target{
		ProviderName: pname,
		ProviderType: prov.Type,
		AccountID:    accID,
		Model:        a.Model,
		SocksName:    socksName,
	}, nil
}

// accountSocks — имя SOCKS-пути аккаунта у провайдера конкретного типа.
func accountSocks(p config.Provider, accID string) (string, error) {
	switch p.Type {
	case config.ProviderCodex:
		for _, a := range p.Codex.Accounts {
			if a.ID == accID {
				return a.Socks, nil
			}
		}
	case config.ProviderGLM:
		for _, a := range p.GLM.Accounts {
			if a.ID == accID {
				return a.Socks, nil
			}
		}
	case config.ProviderZCode:
		for _, a := range p.ZCode.Accounts {
			if a.ID == accID {
				return a.Socks, nil
			}
		}
	case config.ProviderOpenAILike:
		for _, a := range p.OpenAILike.Accounts {
			if a.ID == accID {
				return a.Socks, nil
			}
		}
	default:
		return "", fmt.Errorf("неизвестный тип провайдера %q", p.Type)
	}
	return "", fmt.Errorf("аккаунт %q не найден", accID)
}

// AvailableAliases — отсортированный список алиасов снапшота: для текста 404
// (SV-2) и /v1/models. Сортировка — детерминированный вывод для клиента.
func AvailableAliases(snap *config.Snapshot) []string {
	out := make([]string, 0, len(snap.Cfg.Aliases))
	for name := range snap.Cfg.Aliases {
		out = append(out, name)
	}
	// отсортированный список: 404-сообщение и /v1/models не должны плясать между запросами
	sort.Strings(out)
	return out
}
