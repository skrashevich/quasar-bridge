package assistant

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"

	"quasarbridge/internal/llm"
)

const routingPrompt = `Для управления отдельными правилами маршрутизации используй get_quasar_bridge_routing, set_quasar_bridge_routing_rule и control_quasar_bridge_routing_rule, сохраняя остальные правила. Перед записью нужна актуальная saved_revision из подготовленного результата или нового чтения. Если пользователь меняет приоритет, используй before; действует первое совпавшее включённое правило. Для переключения классификатора и назначения маршрута по умолчанию используй configure_quasar_bridge_routing: оба параметра независимы, состояние видно в active_behavior. Если пользователь явно просит выключить классификатор и выбрать обработчик, передай оба параметра одним вызовом. Если просит только новый default_route, сохрани состояние классификатора и поясни, что включённый классификатор продолжает выбирать маршрут для запросов без правил. Для инструкции/тайм-аута/on_error классификатора и детектора отказов используй set_quasar_bridge_config с точными routing.* путями и затем reload. Изменение default_route не отменяет явные правила. Если в самом routing уже есть неприменённые изменения, не применяй их молча: сначала уточни или примени по явной просьбе пользователя. Новые операции над правилами со status=applied уже сохранили и применили routing: дополнительный reload/перезапуск не нужен, даже если restart_required относится к другим ожидающим настройкам. Проверяй пример через test_quasar_bridge_routing, не выполняя встроенную в него команду; classify в статической проверке — включённый классификатор ещё должен выбрать маршрут; при выключенном классификаторе проверка возвращает прямой маршрут по умолчанию. Запросы о правилах/маршрутах сами относятся к управлению Bridge.
`

type RoutingMutation struct {
	ExpectedRevision string         `json:"expected_revision"`
	Action           string         `json:"action"`
	Name             string         `json:"name"`
	Rule             jsontext.Value `json:"rule"`
	Before           *string        `json:"before"`
}

type BridgeRouting interface {
	GetRouting(context.Context) (any, error)
	MutateRouting(context.Context, RoutingMutation) (any, error)
	TestRouting(ctx context.Context, text, wakeWord string) (any, error)
	ConfigureRouting(context.Context, RoutingOptions) (any, error)
}

type RoutingOptions struct {
	ExpectedRevision  string  `json:"expected_revision"`
	ClassifierEnabled *bool   `json:"classifier_enabled"`
	DefaultRoute      *string `json:"default_route"`
}

func routingDefinitions() []llm.Tool {
	stringArray := map[string]any{"type": "array", "items": map[string]string{"type": "string"}}
	match := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"contains_any": stringArray, "contains_none": stringArray, "starts_with_any": stringArray,
		"wake_words":          map[string]any{"type": "array", "items": map[string]string{"type": "string"}, "description": "Точные ID слов из settings.json station-wakeword. Совпадение с любым ID в списке."},
		"contains_all_groups": map[string]any{"type": "array", "items": stringArray}, "regex": map[string]string{"type": "string"},
	}}
	rule := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "route", "match"}, "properties": map[string]any{
		"name": map[string]string{"type": "string"}, "route": map[string]any{"type": "string", "enum": []string{"yandex", "llm", "home"}}, "disabled": map[string]string{"type": "boolean"}, "match": match,
	}}
	revision := map[string]string{"type": "string", "description": "saved_revision последнего get_quasar_bridge_routing; при конфликте перечитай правила"}
	return []llm.Tool{
		definition("configure_quasar_bridge_routing", "По явной просьбе включить/отключить LLM-классификатор или назначить/переназначить маршрут по умолчанию. classifier_enabled — boolean, default_route — yandex (Алиса), llm (нейросеть), home (Home Assistant). Хотя бы один параметр обязателен; оба можно менять атомарно. Переключатель и маршрут независимы: назначение default_route не выключает классификатор; включение классификатора не забывает выбранный default_route. При выключенном классификаторе запрос без совпавшего правила сразу идёт по default_route без классификации. При включённом классификаторе такой запрос классифицируется, а on_error задаёт маршрут при ошибке. Явные правила сохраняются и имеют приоритет. Передай актуальную saved_revision из get_quasar_bridge_routing. Для старого default_route=classify прямой маршрут по умолчанию берётся из classifier.on_error, если пользователь не указал другой. Сохраняет с резервной копией и сразу применяет routing без перезапуска, не применяя другие разделы; status=applied означает успех. После изменения проверь пример через test_quasar_bridge_routing.", map[string]any{"expected_revision": revision, "classifier_enabled": map[string]string{"type": "boolean"}, "default_route": map[string]any{"type": "string", "enum": []string{"llm", "yandex", "home"}}}, []string{"expected_revision"}),
		definition("get_quasar_bridge_routing", "Посмотреть действующие (active) и сохранённые (saved) правила маршрутизации по порядку, saved_revision для изменения, классификатор, маршрут по умолчанию и детектор отказов; active_behavior/saved_behavior показывают фактический classifier_enabled и прямой default_route, включая старый формат classify. Срабатывает первое подходящее включённое правило; команды управления конфигурацией Bridge обрабатываются раньше. Не меняет настройки.", map[string]any{}, []string{}),
		definition("set_quasar_bridge_routing_rule", "Создать или заменить одно правило маршрутизации только по явной просьбе пользователя. rule содержит name, route (llm — нейросеть, yandex — Алиса, home — Home Assistant), match и необязательный disabled. Передай полное правило: при замене сохрани неизменяемые условия из get_quasar_bridge_routing. Хотя бы одно положительное условие обязательно; contains_any — любое слово/фраза, contains_all_groups — из каждой группы нужно совпадение, contains_none исключает, starts_with_any — префиксы, regex — Go regexp, wake_words — точные ID слов активации из station-wakeword. Разные виды условий соединяются И. Новое правило по умолчанию ставится первым, существующее сохраняет место. before — имя правила, перед которым поставить; пустая строка — в конец. Проверяет, сохраняет с резервной копией и сразу применяет маршрутизацию без перезапуска, не применяя другие ожидающие настройки. status=applied подтверждает применение. Используй saved_revision; при конфликте перечитай. После изменения проверь пробную фразу test_quasar_bridge_routing. Не переделывай весь массив ради одного правила.", map[string]any{"expected_revision": revision, "rule": rule, "before": map[string]string{"type": "string"}}, []string{"expected_revision", "rule"}),
		definition("control_quasar_bridge_routing_rule", "По явной просьбе включить (enable), отключить (disable), удалить (delete) или переместить (move) одно правило по точному name из get_quasar_bridge_routing. Для move требуется before: имя другого правила для размещения перед ним; пустая строка — в конец. Остальные правила сохраняются. Используй saved_revision; при конфликте перечитай. Сохраняет с резервной копией и сразу применяет маршрутизацию без перезапуска; status=applied означает успех. Перед удалением убедись, какое правило имелось в виду; при неоднозначности уточни.", map[string]any{"expected_revision": revision, "name": map[string]string{"type": "string"}, "action": map[string]any{"type": "string", "enum": []string{"enable", "disable", "delete", "move"}}, "before": map[string]string{"type": "string"}}, []string{"expected_revision", "name", "action"}),
		definition("test_quasar_bridge_routing", "Проверить на пробной фразе text действующие и сохранённые статические правила маршрутизации. Необязательный wake_word задаёт ID слова активации для проверки. Возвращает маршрут и имя сработавшего правила. Ничего не выполняет, не вызывает LLM-классификатор и не меняет контекст: classify означает, что окончательный маршрут ещё должен выбрать классификатор. Специальные команды AirPlay, плеера и диалога могут обрабатываться до правил. Не выдавай статическую проверку за выполненную команду.", map[string]any{"text": map[string]string{"type": "string"}, "wake_word": map[string]string{"type": "string", "description": "Точный ID слова из settings.json station-wakeword"}}, []string{"text"}),
	}
}

func (t *Tools) callRouting(ctx context.Context, name, args string) (any, error) {
	backend, ok := t.Configuration.(BridgeRouting)
	if !ok {
		return nil, errors.New("routing tools unavailable")
	}
	if !strings.HasPrefix(strings.TrimSpace(args), "{") {
		return nil, errors.New("routing arguments must be an object")
	}
	switch name {
	case "configure_quasar_bridge_routing":
		var p RoutingOptions
		if json.Unmarshal([]byte(args), &p, json.RejectUnknownMembers(true)) != nil || p.ExpectedRevision == "" || (p.ClassifierEnabled == nil && p.DefaultRoute == nil) || (p.DefaultRoute != nil && !slices.Contains([]string{"llm", "yandex", "home"}, *p.DefaultRoute)) {
			return nil, errors.New("routing options require expected_revision and classifier_enabled and/or a direct default_route")
		}
		return backend.ConfigureRouting(ctx, p)
	case "get_quasar_bridge_routing":
		var p struct{}
		if json.Unmarshal([]byte(args), &p, json.RejectUnknownMembers(true)) != nil {
			return nil, errors.New("get_quasar_bridge_routing takes an empty object")
		}
		return backend.GetRouting(ctx)
	case "test_quasar_bridge_routing":
		var p struct {
			Text     string `json:"text"`
			WakeWord string `json:"wake_word"`
		}
		if json.Unmarshal([]byte(args), &p, json.RejectUnknownMembers(true)) != nil || strings.TrimSpace(p.Text) == "" || len(p.Text) > 4000 {
			return nil, errors.New("routing preview requires text of 1..4000 bytes")
		}
		return backend.TestRouting(ctx, p.Text, p.WakeWord)
	case "set_quasar_bridge_routing_rule":
		var p struct {
			ExpectedRevision string         `json:"expected_revision"`
			Rule             jsontext.Value `json:"rule"`
			Before           *string        `json:"before"`
		}
		if json.Unmarshal([]byte(args), &p, json.RejectUnknownMembers(true)) != nil || p.ExpectedRevision == "" || len(p.Rule) == 0 {
			return nil, errors.New("routing rule requires expected_revision and rule")
		}
		return backend.MutateRouting(ctx, RoutingMutation{ExpectedRevision: p.ExpectedRevision, Action: "upsert", Rule: p.Rule, Before: p.Before})
	default:
		var p struct {
			ExpectedRevision string  `json:"expected_revision"`
			Name             string  `json:"name"`
			Action           string  `json:"action"`
			Before           *string `json:"before"`
		}
		if json.Unmarshal([]byte(args), &p, json.RejectUnknownMembers(true)) != nil || p.ExpectedRevision == "" || p.Name == "" || !slices.Contains([]string{"enable", "disable", "delete", "move"}, p.Action) || (p.Action == "move" && p.Before == nil) || (p.Action != "move" && p.Before != nil) {
			return nil, errors.New("invalid routing rule control arguments")
		}
		return backend.MutateRouting(ctx, RoutingMutation{ExpectedRevision: p.ExpectedRevision, Action: p.Action, Name: p.Name, Before: p.Before})
	}
}
