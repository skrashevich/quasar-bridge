package main

import (
	"encoding/json/v2"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/home"
	"quasarbridge/internal/journal"
	"quasarbridge/internal/llm"
)

type progressConfig struct {
	Enabled    bool `json:"enabled"`
	AfterMS    int  `json:"after_ms"`
	IntervalMS int  `json:"interval_ms"`
}

type config struct {
	DialogueContext    dialogue.Config      `json:"dialogue_context"`
	LocalProgress      progressConfig       `json:"local_progress"`
	AssistantTools     assistant.Config     `json:"assistant_tools"`
	ConversationLog    journal.Config       `json:"conversation_log"`
	Listen             string               `json:"listen"`
	Upstream           string               `json:"upstream"`
	TLSCert            string               `json:"tls_cert"`
	TLSKey             string               `json:"tls_key"`
	AllowedIPs         []string             `json:"allowed_ips"`
	Mode               string               `json:"mode"`
	LLMBaseURL         string               `json:"llm_base_url"`
	LLMModel           string               `json:"llm_model"`
	LLMAPI             string               `json:"llm_api"`
	LLMReasoningEffort string               `json:"llm_reasoning_effort"`
	LLMMaxOutputTokens int                  `json:"llm_max_output_tokens"`
	LLMHostedTools     []string             `json:"llm_hosted_tools"`
	LLMChatParameters  map[string]any       `json:"llm_chat_parameters"`
	LLMSystemPrompt    string               `json:"llm_system_prompt"`
	HomeWarmArea       string               `json:"home_warm_area"`
	HomeAssistant      home.AssistConfig    `json:"home_assistant"`
	ListenAfterReply   bool                 `json:"listen_after_reply"`
	SpotterWaitMS      int                  `json:"spotter_wait_ms"`
	Routing            bridge.RoutingConfig `json:"routing"`
}

func readConfig(path string) (config, error) {
	cfg := config{DialogueContext: dialogue.DefaultConfig(), Routing: bridge.DefaultRoutingConfig(), ConversationLog: journal.DefaultConfig(), AssistantTools: assistant.DefaultConfig(), LocalProgress: progressConfig{AfterMS: 2000, IntervalMS: 4000}, LLMAPI: "chat_completions", LLMMaxOutputTokens: 4096, ListenAfterReply: true, SpotterWaitMS: 1500}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, errors.New("cannot read configuration file")
	}
	if json.Unmarshal(raw, &cfg, json.RejectUnknownMembers(true)) != nil {
		return cfg, errors.New("invalid configuration JSON")
	}
	for name, target := range map[string]*string{
		"QUASAR_LISTEN": &cfg.Listen, "QUASAR_UPSTREAM": &cfg.Upstream,
		"QUASAR_TLS_CERT": &cfg.TLSCert, "QUASAR_TLS_KEY": &cfg.TLSKey,
		"QUASAR_MODE": &cfg.Mode, "QUASAR_LLM_BASE_URL": &cfg.LLMBaseURL,
		"QUASAR_LLM_MODEL": &cfg.LLMModel, "QUASAR_LLM_SYSTEM_PROMPT": &cfg.LLMSystemPrompt,
		"QUASAR_HOME_WARM_AREA": &cfg.HomeWarmArea,
		"QUASAR_LLM_API":        &cfg.LLMAPI, "QUASAR_LLM_REASONING_EFFORT": &cfg.LLMReasoningEffort,
	} {
		if value, ok := os.LookupEnv(name); ok {
			*target = value
		}
	}
	if value, ok := os.LookupEnv("QUASAR_ALLOWED_IPS"); ok {
		cfg.AllowedIPs = nil
		for item := range strings.SplitSeq(value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				cfg.AllowedIPs = append(cfg.AllowedIPs, item)
			}
		}
	}
	if len(cfg.AllowedIPs) == 0 {
		cfg.AllowedIPs = nil
	}
	if cfg.Listen == "" {
		cfg.Listen = "localhost:8443"
	}
	if cfg.Upstream == "" {
		cfg.Upstream = "wss://uniproxy.alice.yandex.net/uni.ws"
	}
	if cfg.Mode == "" {
		cfg.Mode = "observe"
	}
	if cfg.LLMAPI == "" {
		cfg.LLMAPI = "chat_completions"
	}
	if cfg.LLMAPI != "chat_completions" && cfg.LLMAPI != "responses" {
		return cfg, errors.New("llm_api must be chat_completions or responses")
	}
	if len(cfg.LLMChatParameters) == 0 {
		cfg.LLMChatParameters = nil
	} else {
		if cfg.LLMAPI != "chat_completions" {
			return cfg, errors.New("llm_chat_parameters requires llm_api chat_completions")
		}
		if err := llm.ValidateChatParameters(cfg.LLMChatParameters); err != nil {
			return cfg, err
		}
	}
	if cfg.LLMMaxOutputTokens <= 0 {
		return cfg, errors.New("llm_max_output_tokens must be positive")
	}
	if cfg.LLMReasoningEffort != "" {
		if cfg.LLMAPI != "responses" {
			return cfg, errors.New("llm_reasoning_effort requires llm_api responses")
		}
		if !slices.Contains([]string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}, cfg.LLMReasoningEffort) {
			return cfg, errors.New("invalid llm_reasoning_effort")
		}
	}
	if len(cfg.LLMHostedTools) == 0 {
		cfg.LLMHostedTools = nil
	} else {
		if cfg.LLMAPI != "responses" {
			return cfg, errors.New("llm_hosted_tools requires llm_api responses")
		}
		seen := make(map[string]bool, len(cfg.LLMHostedTools))
		for _, name := range cfg.LLMHostedTools {
			if name != "web_search" && name != "code_interpreter" {
				return cfg, errors.New("llm_hosted_tools supports web_search and code_interpreter")
			}
			if seen[name] {
				return cfg, errors.New("duplicate llm_hosted_tools entry")
			}
			seen[name] = true
		}
	}
	if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
		return cfg, errors.New("listen must include host and port")
	}
	if cfg.Mode != "observe" && cfg.Mode != "hybrid" {
		return cfg, errors.New("mode must be observe or hybrid")
	}
	if !validEndpoint(cfg.Upstream, "ws", "wss") {
		return cfg, errors.New("invalid upstream URL")
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return cfg, errors.New("TLS certificate and key must be set together")
	}
	for _, ip := range cfg.AllowedIPs {
		if _, err := netip.ParseAddr(ip); err != nil {
			return cfg, errors.New("allowed_ips must contain IP addresses")
		}
	}
	if cfg.LLMBaseURL != "" && !validEndpoint(cfg.LLMBaseURL, "http", "https") {
		return cfg, errors.New("invalid LLM URL")
	}
	if value, ok := os.LookupEnv("QUASAR_HOME_ASSISTANT_URL"); ok {
		cfg.HomeAssistant.URL = value
	}
	if value, ok := os.LookupEnv("QUASAR_HOME_ASSISTANT_AGENT_ID"); ok {
		cfg.HomeAssistant.AgentID = value
	}
	cfg.HomeAssistant.Normalize()
	if err := cfg.HomeAssistant.Validate(os.Getenv("QUASAR_HOME_ASSISTANT_TOKEN")); err != nil {
		return cfg, err
	}
	cfg.LLMSystemPrompt = strings.TrimSpace(cfg.LLMSystemPrompt)
	if utf8.RuneCountInString(cfg.LLMSystemPrompt) > 4000 {
		return cfg, errors.New("llm_system_prompt is too long")
	}
	if cfg.Mode == "hybrid" {
		if cfg.LLMBaseURL == "" || cfg.LLMModel == "" {
			return cfg, errors.New("hybrid mode requires LLM URL and model")
		}
		if endpoint := os.Getenv("QUASAR_HOME_MCP_URL"); endpoint != "" && !validEndpoint(endpoint, "http", "https") {
			return cfg, errors.New("invalid QUASAR_HOME_MCP_URL")
		}
	}
	if cfg.LocalProgress.AfterMS < 100 || cfg.LocalProgress.AfterMS > 10000 || cfg.LocalProgress.IntervalMS < 1000 || cfg.LocalProgress.IntervalMS > 10000 {
		return cfg, errors.New("invalid local_progress timing")
	}
	if cfg.SpotterWaitMS < 0 || cfg.SpotterWaitMS > 5000 {
		return cfg, errors.New("spotter_wait_ms must be from 0 to 5000")
	}
	if err := cfg.DialogueContext.Validate(); err != nil {
		return cfg, err
	}
	if err := cfg.AssistantTools.Validate(); err != nil {
		return cfg, err
	}
	if err := cfg.ConversationLog.Validate(); err != nil {
		return cfg, err
	}
	if _, err := bridge.CompileRouting(cfg.Routing); err != nil {
		return cfg, err
	}
	return cfg, nil
}
func validEndpoint(raw string, schemes ...string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	for _, scheme := range schemes {
		if u.Scheme == scheme {
			return true
		}
	}
	return false
}
