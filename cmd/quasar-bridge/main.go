package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/home"
	"quasarbridge/internal/journal"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/wire"
)

func main() {
	path := flag.String("config", "config.json", "configuration file")
	query := flag.String("query", "", "run one text query for backend diagnostics")
	check := flag.Bool("check-backends", false, "check LLM and configured Home Assistant connectivity")
	checkMCP := flag.Bool("check-mcp", false, "connect to configured MCP servers and list tools without executing them")
	route := flag.String("route", "", "explain a route without executing home actions or generating an answer")
	validate := flag.Bool("check-config", false, "validate configuration without contacting backends")
	speakerAction := flag.String("speaker-endpoint", "", "apply or rollback the on-device loopback endpoint (requires writable firmware)")
	speakerConfig := flag.String("speaker-config", "/system/vendor/quasar/quasar.cfg", "firmware configuration path")
	speakerBackup := flag.String("speaker-backup", "/data/opt/quasar-bridge/endpoint-backup.json", "persistent endpoint backup")
	speakerURL := flag.String("speaker-url", "ws://127.0.0.1:8443/uni.ws", "on-device bridge URL")
	flag.Parse()
	if *speakerAction != "" {
		if err := setSpeakerEndpoint(*speakerConfig, *speakerBackup, *speakerURL, *speakerAction); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("Speaker endpoint updated; restart the voice service to apply")
		return
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	cfg, err := readConfig(*path)
	if err != nil {
		slog.Error("configuration rejected", "error", err)
		os.Exit(1)
	}

	diagnostics := 0
	if *query != "" {
		diagnostics++
	}
	if *route != "" {
		diagnostics++
	}
	if *check {
		diagnostics++
	}
	if *checkMCP {
		diagnostics++
	}
	if *validate {
		diagnostics++
	}
	if diagnostics > 1 {
		slog.Error("diagnostic flags are mutually exclusive")
		os.Exit(2)
	}
	if *validate {
		fmt.Println("configuration is valid")
		return
	}
	if *checkMCP {
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
		defer cancel()
		status, err := assistant.CheckMCP(ctx, cfg.AssistantTools.MCPServers)
		data, _ := json.Marshal(status)
		fmt.Println(string(data))
		if err != nil {
			slog.Error("MCP check failed", "error", err)
			os.Exit(1)
		}
		return
	}
	var clientCount, serverCount atomic.Int64
	proxy := &bridge.Proxy{Upstream: cfg.Upstream, Observer: func(direction string, kind websocket.MessageType, data []byte) {
		d := wire.ClientToServer
		var count int64
		if direction == "server" {
			d = wire.ServerToClient
			count = serverCount.Add(1)
		} else {
			count = clientCount.Add(1)
		}
		frame := wire.Decode(data, d)
		name := frame.Kind()
		if (name != "opaque" && name != "AsrResult") || count <= 8 || count%100 == 0 {
			attrs := []any{"direction", direction, "transport_type", int(kind), "event", name, "bytes", len(data), "count", count}
			if len(frame.Prefix) == 4 {
				attrs = append(attrs, "prefix_tag", string(frame.Prefix))
			}
			if frame.Client != nil {
				e := frame.Client.GetEvent()
				if req := e.GetTextInput(); req != nil {
					attrs = append(attrs, "wait_client_approval", req.GetRequest().GetUniproxyOptions().GetWaitClientApproval())
				}
				if req := e.GetVoiceInput(); req != nil {
					attrs = append(attrs, "wait_client_approval", req.GetRequest().GetUniproxyOptions().GetWaitClientApproval())
				}
			}
			if name == "opaque" && count <= 8 {
				attrs = append(attrs, "framing", wire.Probe(data, d))
			}
			if name == "opaque" && kind == websocket.MessageText {
				// Keys only: never emit request values, audio or authentication.
				var obj map[string]jsontext.Value
				if json.Unmarshal(data, &obj) == nil {
					var keys []string
					for key := range obj {
						if len(key) < 40 && !strings.ContainsAny(key, "\n\r") {
							keys = append(keys, key)
						}
					}
					attrs = append(attrs, "json_keys", keys)
				}
			}
			slog.Info("frame", attrs...)
		}
	}}
	logs := new(journal.Writer)
	lc := &llm.Client{
		BaseURL: cfg.LLMBaseURL, Model: cfg.LLMModel, APIKey: os.Getenv("QUASAR_LLM_API_KEY"),
		API: cfg.LLMAPI, ReasoningEffort: cfg.LLMReasoningEffort,
		MaxOutputTokens: cfg.LLMMaxOutputTokens, HostedTools: cfg.LLMHostedTools,
		ChatParameters: cfg.LLMChatParameters,
	}
	engine := &bridge.Engine{LLM: lc, Dialogue: dialogue.New(cfg.DialogueContext), SystemPrompt: cfg.LLMSystemPrompt}
	var musicQueue *assistant.MusicQueue
	var hc *home.Controller
	if endpoint := os.Getenv("QUASAR_HOME_MCP_URL"); endpoint != "" {
		hc = &home.Controller{Tools: &home.MCP{URL: endpoint}, Planner: lc, SystemPrompt: cfg.LLMSystemPrompt}
	}
	var assist *home.Assist
	if cfg.HomeAssistant.URL != "" {
		assist = &home.Assist{Config: cfg.HomeAssistant, Token: os.Getenv("QUASAR_HOME_ASSISTANT_TOKEN")}
	}
	if assist != nil || hc != nil {
		gateway := &home.Gateway{Assist: assist}
		if hc != nil {
			gateway.Devices = hc
		}
		engine.Home = gateway
	}
	if cfg.AssistantTools.Enabled {
		source := func() journal.Config {
			c := logs.Config()
			if c.Path == "" {
				return cfg.ConversationLog
			}
			return c
		}
		reader := assistant.NewWebReader(cfg.AssistantTools)
		if cfg.AssistantTools.PlaySound && cfg.AssistantTools.Web {
			if base := musicBaseURL(cfg); base != "" {
				var err error
				musicQueue, err = assistant.NewMusicQueue(base, reader)
				if err != nil {
					slog.Error("music queue configuration rejected", "error", err)
					os.Exit(1)
				}
			} else {
				slog.Warn("music playback needs assistant_tools.music_base_url reachable by the speaker")
			}
		}
		var musicPlayback *assistant.MusicPlayback
		if musicQueue != nil {
			musicPlayback = assistant.NewMusicPlayback(musicQueue)
			engine.Playback = musicPlayback
		}
		tools := &assistant.Tools{Config: cfg.AssistantTools, JournalConfig: source, WebReader: reader, Playback: musicPlayback}
		engine.Music = tools
		engine.Assistant = &assistant.Agent{Client: lc, Tools: tools, MaxRounds: cfg.AssistantTools.MaxRounds, MaxCalls: cfg.AssistantTools.MaxCalls}
	}
	if err := engine.SetRouting(cfg.Routing); err != nil {
		slog.Error("routing rejected", "error", err)
		os.Exit(1)
	}
	if *query != "" || *check || *route != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
		defer cancel()
		if *route != "" {
			data, _ := json.Marshal(engine.Decide(ctx, *route))
			fmt.Println(string(data))
			return
		}
		if *check {
			_, err := lc.Complete(ctx, []llm.Message{{Role: "user", Content: "Ответь одним словом: связь"}}, false)
			if err != nil {
				slog.Error("LLM check failed")
				os.Exit(1)
			}
			if hc == nil && assist == nil {
				fmt.Println("LLM: ok; Home Assistant: disabled")
				return
			}
			var parts []string
			if assist != nil {
				if err := assist.Ping(ctx); err != nil {
					slog.Error("Home Assistant check failed", "error", err)
					os.Exit(1)
				}
				parts = append(parts, "conversation ok")
			}
			if hc != nil {
				entities, err := hc.Entities(ctx)
				if err != nil {
					slog.Error("Home Assistant check failed", "error", err)
					os.Exit(1)
				}
				parts = append(parts, fmt.Sprintf("%d entities", len(entities)))
			}
			fmt.Printf("LLM: ok; Home Assistant: %s\n", strings.Join(parts, "; "))
			return
		}
		r, err := engine.Resolve(ctx, *query)
		if err != nil {
			slog.Error("query failed", "route", r.Route, "error", err)
			os.Exit(1)
		}
		data, _ := json.Marshal(r)
		fmt.Println(string(data))
		return
	}
	if err := logs.Configure(cfg.ConversationLog); err != nil {
		slog.Error("conversation log rejected", "error", err)
		os.Exit(1)
	}
	defer logs.Close()
	proxy.Journal = logs
	if cfg.Mode == "hybrid" {
		if hc != nil && cfg.HomeWarmArea != "" {
			warmCtx, warmCancel := context.WithCancel(context.Background())
			defer warmCancel()
			go func() {
				for {
					ctx, cancel := context.WithTimeout(warmCtx, 90*time.Second)
					err := hc.Warm(ctx, cfg.HomeWarmArea)
					cancel()
					slog.Info("home_warm", "ready", err == nil)
					select {
					case <-warmCtx.Done():
						return
					case <-time.After(time.Minute):
					}
				}
			}()
		}

		proxy.NewSession = func(ctx context.Context, up, down bridge.Sender) bridge.Session {
			n := bridge.NewNative(ctx, up, down, engine)
			n.GateText = false
			n.ShouldListen = cfg.ListenAfterReply
			n.SpotterWait = time.Duration(cfg.SpotterWaitMS) * time.Millisecond
			if cfg.LocalProgress.Enabled {
				n.ProgressAfter = time.Duration(cfg.LocalProgress.AfterMS) * time.Millisecond
				n.ProgressInterval = time.Duration(cfg.LocalProgress.IntervalMS) * time.Millisecond
			}

			return n
		}
	}
	mux := http.NewServeMux()
	if musicQueue != nil {
		mux.Handle("/music-stream/", musicQueue)
	}
	mcpConfigured := 0
	if cfg.Mode == "hybrid" && cfg.AssistantTools.Enabled {
		for _, server := range cfg.AssistantTools.MCPServers {
			if !server.Disabled {
				mcpConfigured++
			}
		}
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		state := engine.RoutingStatus()
		data, _ := json.Marshal(map[string]any{"status": "ok", "mode": cfg.Mode, "client_frames": clientCount.Load(), "server_frames": serverCount.Load(), "routing_revision": state.Revision, "routing_rules": state.ActiveRules, "conversation_log_enabled": logs.Enabled(), "conversation_log_failures": logs.Failures(), "assistant_tools_enabled": cfg.AssistantTools.Enabled, "dialogue_context_enabled": cfg.DialogueContext.Enabled && cfg.Mode == "hybrid", "home_assistant_enabled": (hc != nil || assist != nil) && cfg.Mode == "hybrid", "home_assistant_conversation": assist != nil && cfg.Mode == "hybrid", "mcp_servers_configured": mcpConfigured, "music_playback_enabled": musicQueue != nil})
		_, _ = w.Write(data)
	})
	mux.Handle("/uni.ws", proxy)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(cfg.AllowedIPs) > 0 {
			ip, _, _ := net.SplitHostPort(r.RemoteAddr)
			allowed := false
			for _, candidate := range cfg.AllowedIPs {
				if ip == candidate {
					allowed = true
					break
				}
			}
			if !allowed {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
	server := &http.Server{Addr: cfg.Listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reloads := make(chan os.Signal, 1)
	signal.Notify(reloads, syscall.SIGHUP)
	defer signal.Stop(reloads)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-reloads:
				if err := reloadRouting(*path, cfg, engine, logs); err != nil {
					slog.Warn("routing_reload_rejected", "error", err)
					continue
				}
				state := engine.RoutingStatus()
				slog.Info("routing_reloaded", "revision", state.Revision, "active_rules", state.ActiveRules)
			}
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	slog.Info("listening", "address", cfg.Listen, "mode", cfg.Mode, "routing_revision", engine.RoutingStatus().Revision, "system_prompt_chars", utf8.RuneCountInString(cfg.LLMSystemPrompt), "home_assistant_conversation", assist != nil)
	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		err = server.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	} else {
		err = server.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("listener failed")
		os.Exit(1)
	}
}
