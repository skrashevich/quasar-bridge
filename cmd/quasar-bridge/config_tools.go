package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/journal"
	"quasarbridge/internal/update"
)

type voiceSetting struct {
	Path        string `json:"path"`
	Description string `json:"description"`
	Environment string `json:"environment,omitempty"`
	Restart     bool   `json:"restart_required"`
}

// Explicit leaves prevent a parent-object update from smuggling in credentials,
// process commands, network endpoints or filesystem paths.
var voiceSettings = []voiceSetting{
	{"llm_model", "Модель LLM; непустая строка в hybrid. Наличие модели на сервере отдельно не проверяется.", "QUASAR_LLM_MODEL", true},
	{"llm_api", "API: chat_completions, responses или chatgpt. Зависимые параметры меняй одним вызовом.", "QUASAR_LLM_API", true},
	{"llm_reasoning_effort", "Усилие рассуждения: пустая строка, none, minimal, low, medium, high, xhigh, max; только responses/chatgpt.", "QUASAR_LLM_REASONING_EFFORT", true},
	{"llm_max_output_tokens", "Лимит выходных токенов: положительное целое число.", "", true},
	{"llm_hosted_tools", "Массив серверных инструментов: web_search, code_interpreter для responses; для chatgpt только web_search; [] отключает.", "", true},
	{"llm_chat_parameters", "Объект параметров chat_completions: temperature, max_tokens, max_completion_tokens, response_format, chat_template_kwargs, reasoning_effort. {} очищает.", "QUASAR_LLM_CHAT_PARAMETERS", true},
	{"llm_strip_prefixes", "Массив обращений к модели, удаляемых перед отправкой запроса. [] отключает; маршруты настраиваются отдельно.", "", true},
	{"llm_tts_markup", "Логическое: разрешать модели использовать TTS-разметку. При false её ответы озвучиваются обычным голосом.", "", true},
	{"home_warm_area", "Название области Home Assistant для прогрева кэша; строка, пустая отключает прогрев.", "QUASAR_HOME_WARM_AREA", true},
	{"home_default_area", "Название области Home Assistant, где стоит колонка: команды без названия комнаты относятся к ней; пустая строка отключает.", "QUASAR_HOME_DEFAULT_AREA", true},
	{"listen_after_reply", "Логическое: слушать следующий вопрос после локального ответа.", "", true},
	{"dialogue_context.enabled", "Логическое: хранить контекст последовательных вопросов.", "", true},
	{"dialogue_context.ttl_seconds", "Время жизни контекста в секундах: целое число 30..86400.", "", true},
	{"dialogue_context.max_turns", "Максимум пар вопрос/ответ в контексте: целое число 1..20.", "", true},
	{"dialogue_context.max_chars", "Максимум символов контекста: целое число 512..65536.", "", true},
	{"local_progress.enabled", "Логическое: звуковые уведомления при ожидании локального ответа.", "", true},
	{"local_progress.after_ms", "Задержка первого уведомления в миллисекундах: целое число 100..10000.", "", true},
	{"local_progress.interval_ms", "Интервал уведомлений в миллисекундах: целое число 1000..10000.", "", true},
	{"conversation_log.enabled", "Логическое: сохранять запросы и ответы в журнал.", "", false},
	{"conversation_log.max_size_mb", "Максимальный размер файла журнала в МиБ: целое число 1..1024.", "", false},
	{"conversation_log.max_backups", "Число архивных журналов: целое число 0..20.", "", false},
	{"conversation_log.max_text_chars", "Лимит символов записи: целое число 1..64000.", "", false},
	{"routing.default_route", "Маршрут по умолчанию: classify, yandex, llm или home.", "", false},
	{"routing.classifier.enabled", "Логическое: включить LLM-классификатор запросов без совпавшего правила. При false используется маршрут по умолчанию. Если поле отсутствует, действует прежнее поведение default_route.", "", false},
	{"routing.rules", "Массив правил по порядку: name, route (yandex/llm/home), disabled, match с contains_any, contains_all_groups, contains_none, starts_with_any, regex. Перед изменением прочитай текущие правила и сохрани остальные.", "", false},
	{"routing.classifier.prompt", "Непустая строка: инструкция классификатора маршрута.", "", false},
	{"routing.classifier.timeout_ms", "Тайм-аут классификации в миллисекундах: целое число 1..60000.", "", false},
	{"routing.classifier.on_error", "Маршрут при ошибке классификатора: yandex, llm или home.", "", false},
	{"routing.refusal.enabled", "Логическое: передавать запрос LLM после отказа Алисы.", "", false},
	{"routing.refusal.prefixes", "Массив префиксов отказа; при включённом детекторе непустой.", "", false},
	{"routing.refusal.contains_any", "Массив фраз отказа; при включённом детекторе непустой.", "", false},
	{"assistant_tools.web", "Логическое: чтение веб-страниц.", "", true},
	{"assistant_tools.history", "Логическое: поиск в журнале диалогов.", "", true},
	{"assistant_tools.time", "Логическое: инструмент текущего времени.", "", true},
	{"assistant_tools.updates", "Логическое: инструменты проверки и установки обновлений.", "", true},
	{"assistant_tools.play_sound", "Логическое: инструменты воспроизведения звука.", "", true},
	{"assistant_tools.timezone", "Строка: часовой пояс IANA, например Europe/Moscow или UTC.", "", true},
	{"assistant_tools.max_rounds", "Максимум раундов инструментов: целое число 1..5.", "", true},
	{"assistant_tools.max_calls", "Максимум вызовов инструментов: целое число 1..10.", "", true},
	{"assistant_tools.web_timeout_ms", "Тайм-аут веб-чтения в миллисекундах: целое число 100..20000.", "", true},
	{"assistant_tools.web_max_bytes", "Максимум байт веб-ответа: целое число 1024..2097152.", "", true},
	{"assistant_tools.web_max_chars", "Максимум символов текста страницы: целое число 100..20000.", "", true},
	{"assistant_tools.history_max_scan_mb", "Лимит чтения истории в МиБ: целое число 1..64.", "", true},
	{"airplay.disabled", "Логическое: полностью отключить функцию AirPlay; false делает приёмник доступным после перезапуска.", "", true},
	{"airplay.name", "Строка: имя приёмника в списке AirPlay; пустая — имя по умолчанию.", "", true},
	{"airplay.output_delay_ms", "Задержка вывода аудио в миллисекундах: целое число 0..5000; null — по умолчанию.", "", true},
	{"airplay.initial_volume_db", "Начальная громкость AirPlay в децибелах: число -144..0; null — по умолчанию.", "", true},
}

type configurationControl struct {
	mu          sync.Mutex
	path        string
	active      config
	engine      *bridge.Engine
	logs        *journal.Writer
	restart     func(context.Context) (any, error)
	restartDone <-chan struct{}
}

type configurationResult struct {
	Status           string   `json:"status"`
	Changed          []string `json:"changed,omitempty"`
	Pending          []string `json:"pending"`
	RestartRequired  bool     `json:"restart_required"`
	RestartScheduled bool     `json:"restart_scheduled"`
}

func requiresRestart(active, next config) bool {
	active.Routing, next.Routing = bridge.RoutingConfig{}, bridge.RoutingConfig{}
	active.ConversationLog, next.ConversationLog = journal.Config{}, journal.Config{}
	return !reflect.DeepEqual(active, next)
}

func (c *configurationControl) result(status string, next config) configurationResult {
	result := configurationResult{Status: status, Pending: []string{}, RestartRequired: requiresRestart(c.active, next), RestartScheduled: c.restartPending()}
	for _, setting := range voiceSettings {
		if !bytes.Equal(configValue(c.active, setting.Path), configValue(next, setting.Path)) {
			result.Pending = append(result.Pending, setting.Path)
		}
	}
	return result
}

func (c *configurationControl) Get(ctx context.Context, section string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return nil, errors.New("cannot read configuration file")
	}
	next, err := parseConfig(raw, false)
	if err != nil {
		return nil, err
	}
	type entry struct {
		voiceSetting
		Active    jsontext.Value `json:"active"`
		Saved     jsontext.Value `json:"saved"`
		Effective jsontext.Value `json:"effective"`
		Pending   bool           `json:"pending"`
		Writable  bool           `json:"writable"`
	}
	entries := []entry{}
	for _, setting := range voiceSettings {
		if section != "" && setting.Path != section && !strings.HasPrefix(setting.Path, section+".") {
			continue
		}
		active, effective := configValue(c.active, setting.Path), configValue(next, setting.Path)
		saved := configurationValue(raw, setting.Path)
		_, overridden := os.LookupEnv(setting.Environment)
		entries = append(entries, entry{setting, active, saved, effective, !bytes.Equal(active, effective), !overridden})
	}
	if len(entries) == 0 {
		return nil, errors.New("unknown or unavailable configuration section")
	}
	return struct {
		configurationResult
		Settings []entry `json:"settings"`
	}{c.result("ok", next), entries}, nil
}

func (c *configurationControl) Set(ctx context.Context, changes map[string]jsontext.Value) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.setLocked(ctx, changes, nil)
}

func (c *configurationControl) setLocked(ctx context.Context, changes map[string]jsontext.Value, expected []byte) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.restartPending() {
		return nil, errors.New("restart already scheduled; wait for the new process")
	}
	if len(changes) == 0 || len(changes) > 32 {
		return nil, errors.New("changes must contain 1..32 configuration paths")
	}
	info, err := os.Lstat(c.path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("configuration must be a regular file, not a symlink")
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return nil, errors.New("cannot read configuration file")
	}
	if expected != nil && !bytes.Equal(expected, raw) {
		return nil, errors.New("configuration changed externally; read it again before retrying")
	}
	if len(raw) > 1<<20 {
		return nil, errors.New("configuration exceeds voice editing size limit")
	}
	if _, err := parseConfig(raw, false); err != nil {
		return nil, err
	}
	var doc map[string]jsontext.Value
	if json.Unmarshal(raw, &doc) != nil || doc == nil {
		return nil, errors.New("configuration must be an object")
	}
	paths := slices.Sorted(maps.Keys(changes))
	for _, path := range paths {
		i := slices.IndexFunc(voiceSettings, func(s voiceSetting) bool { return s.Path == path })
		if i < 0 {
			return nil, errors.New("unknown or unavailable configuration path")
		}
		setting := voiceSettings[i]
		if setting.Environment != "" {
			if _, ok := os.LookupEnv(setting.Environment); ok {
				return nil, fmt.Errorf("%s is overridden by %s; edit runtime environment manually", path, setting.Environment)
			}
		}
		if !changes[path].IsValid() || (bytes.Equal(bytes.TrimSpace(changes[path]), []byte("null")) && path != "airplay.output_delay_ms" && path != "airplay.initial_volume_db") {
			return nil, fmt.Errorf("invalid value for %s; null is only allowed for optional AirPlay values", path)
		}
		if err := setConfigValue(doc, path, changes[path]); err != nil {
			return nil, err
		}
	}
	data, err := json.Marshal(doc, json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		return nil, errors.New("cannot encode configuration")
	}
	next, err := parseConfig(data, false)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Notice edits made outside this process while the candidate was validated.
	current, err := os.ReadFile(c.path)
	if err != nil || !bytes.Equal(raw, current) {
		return nil, errors.New("configuration changed externally; read it again before retrying")
	}
	if err := writeConfiguration(c.path+".voice-backup", raw, 0600); err != nil {
		return nil, errors.New("cannot save configuration backup")
	}
	if err := writeConfiguration(c.path, append(data, '\n'), info.Mode().Perm()&0600); err != nil {
		return nil, errors.New("cannot replace configuration file; previous configuration remains intact")
	}
	result := c.result("saved", next)
	result.Changed = paths
	return result, nil
}

func (c *configurationControl) Apply(ctx context.Context, action string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.restartPending() {
		return nil, errors.New("restart already scheduled; wait for the new process")
	}
	next, err := readConfig(c.path)
	if err != nil {
		return nil, err
	}
	switch action {
	case "restart":
		if c.restart == nil {
			return nil, errors.New("voice restart is unavailable for this installation; restart the service manually")
		}
		result, err := c.restart(ctx)
		if err == nil {
			if scheduled, ok := result.(update.RestartResult); ok {
				c.restartDone = scheduled.Done
			}
		}
		return result, err
	case "reload":
		if err := applyReload(next, c.active, c.engine, c.logs); err != nil {
			return nil, err
		}
		c.active = next
		return c.result("applied", next), nil
	default:
		return nil, errors.New("configuration action must be reload or restart")
	}
}

func configValue(cfg config, path string) jsontext.Value {
	if path == "routing.classifier.enabled" {
		raw, _ := json.Marshal(cfg.Routing.ClassifierEnabled())
		return raw
	}
	raw, _ := json.Marshal(cfg, json.Deterministic(true))
	return configurationValue(raw, path)
}

func configurationValue(raw []byte, path string) jsontext.Value {
	for part := range strings.SplitSeq(path, ".") {
		var object map[string]jsontext.Value
		_ = json.Unmarshal(raw, &object)
		raw = object[part]
	}
	if len(raw) == 0 {
		return jsontext.Value("null")
	}
	return raw
}

func (c *configurationControl) restartPending() bool {
	if c.restartDone == nil {
		return false
	}
	select {
	case <-c.restartDone:
		return false
	default:
		return true
	}
}

func setConfigValue(doc map[string]jsontext.Value, path string, value jsontext.Value) error {
	head, tail, nested := strings.Cut(path, ".")
	if !nested {
		doc[head] = value
		return nil
	}
	child := map[string]jsontext.Value{}
	if raw, ok := doc[head]; ok && json.Unmarshal(raw, &child) != nil {
		return errors.New("configuration section is not an object")
	}
	if child == nil {
		child = map[string]jsontext.Value{}
	}
	if err := setConfigValue(child, tail, value); err != nil {
		return err
	}
	raw, err := json.Marshal(child, json.Deterministic(true))
	doc[head] = raw
	return err
}

func writeConfiguration(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".quasar-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(mode); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	return os.Rename(file.Name(), path)
}
