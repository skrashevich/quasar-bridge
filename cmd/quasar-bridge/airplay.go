package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	airplay "github.com/skrashevich/station-airplay"
	"quasarbridge/internal/assistant"
)

type airPlayConfig struct {
	Disabled        bool     `json:"disabled"`
	Name            string   `json:"name"`
	Interface       string   `json:"interface"`
	ListenAddress   string   `json:"listen_address"`
	DataDir         string   `json:"data_dir"`
	Aplay           string   `json:"aplay"`
	Device          string   `json:"device"`
	OutputDelayMS   *int     `json:"output_delay_ms"`
	InitialVolumeDB *float64 `json:"initial_volume_db"`
}

func (c *airPlayConfig) defaults() {
	if c.DataDir == "" {
		state := os.Getenv("QUASAR_DATA_DIR")
		if state == "" {
			state = "/data/opt/quasar-bridge"
		}
		c.DataDir = filepath.Join(state, "airplay")
	}
}

func (c airPlayConfig) receiverConfig() (airplay.Config, error) {
	cfg := airplay.Config{Name: c.Name, Interface: c.Interface, ListenAddress: c.ListenAddress, DataDir: c.DataDir, Aplay: c.Aplay, Device: c.Device, InitialVolumeDB: c.InitialVolumeDB, Logger: slog.NewLogLogger(slog.Default().Handler(), slog.LevelInfo)}
	if c.OutputDelayMS != nil {
		if *c.OutputDelayMS < 0 || *c.OutputDelayMS > 5000 {
			return cfg, errors.New("airplay.output_delay_ms must be between 0 and 5000")
		}
		cfg.OutputDelay = new(time.Duration(*c.OutputDelayMS) * time.Millisecond)
	}
	if !c.Disabled {
		if _, err := airplay.New(cfg); err != nil {
			return cfg, fmt.Errorf("invalid airplay settings: %w", err)
		}
	}
	return cfg, nil
}

type airPlayReceiver interface {
	Start(context.Context) error
	Stop(context.Context) error
	Control(context.Context, string) error
	SetVolume(context.Context, float64) error
	Status() airplay.Status
}

type airPlayControl struct {
	receiver airPlayReceiver
	app      context.Context
}

func newAirPlayControl(app context.Context, cfg airPlayConfig) (*airPlayControl, error) {
	if cfg.Disabled {
		return nil, nil
	}
	options, err := cfg.receiverConfig()
	if err != nil {
		return nil, err
	}
	receiver, err := airplay.New(options)
	if err != nil {
		return nil, err
	}
	return &airPlayControl{receiver: receiver, app: app}, nil
}

func (c *airPlayControl) Control(ctx context.Context, action string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	switch action {
	case "start":
		// A voice request ends as soon as the answer is delivered; only the bridge
		// application context may own a running receiver.
		if err := c.receiver.Start(c.app); err != nil {
			slog.Warn("airplay_start_failed", "error", err)
			return "", err
		}
		return "AirPlay включён. Выберите колонку в списке AirPlay на вашем устройстве.", nil
	case "stop":
		if err := c.receiver.Stop(ctx); err != nil {
			slog.Warn("airplay_stop_failed", "error", err)
			return "", err
		}
		return "AirPlay выключен.", nil
	case "status":
		state := c.receiver.Status()
		switch state.State {
		case airplay.StateRunning:
			return "AirPlay включён и готов к подключению.", nil
		case airplay.StateStarting:
			return "AirPlay запускается.", nil
		case airplay.StateStopping:
			return "AirPlay выключается.", nil
		default:
			if state.Error != "" {
				return "AirPlay выключен после ошибки. Проверьте журнал Quasar Bridge.", nil
			}
			return "AirPlay выключен.", nil
		}
	default:
		return "", errors.New("unknown AirPlay control")
	}
}

func (c *airPlayControl) Status(ctx context.Context) (assistant.AirPlayStatus, error) {
	if err := ctx.Err(); err != nil {
		return assistant.AirPlayStatus{}, err
	}
	state := c.receiver.Status()
	return assistant.AirPlayStatus{
		Enabled: true, State: string(state.State), Name: state.Name,
		Connected: state.Connected, Playing: state.Playing, VolumeDB: state.VolumeDB,
		Error: state.Error,
	}, nil
}

// airPlayHealth reports receiver readiness, not whether a sender is playing audio.
func airPlayHealth(c *airPlayControl) map[string]any {
	if c == nil {
		return map[string]any{"enabled": false, "state": "stopped"}
	}
	state := c.receiver.Status()
	return map[string]any{"enabled": true, "state": state.State, "name": state.Name, "address": state.Address, "error": state.Error, "connected": state.Connected, "playing": state.Playing, "volume_db": state.VolumeDB}
}

func (c *airPlayControl) Active(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	state := c.receiver.Status()
	return state.State == airplay.StateRunning && state.Connected
}

func (c *airPlayControl) ControlMedia(ctx context.Context, action string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !c.Active(ctx) {
		return "", errors.New("AirPlay has no connected sender")
	}
	var err error
	if value, ok := strings.CutPrefix(action, "volume:"); ok {
		percent, parseErr := strconv.Atoi(value)
		if parseErr != nil || percent < 0 || percent > 100 {
			return "", errors.New("invalid AirPlay volume")
		}
		db := -144.0
		if percent > 0 {
			db = 30*float64(percent)/100 - 30
		}
		err = c.receiver.SetVolume(ctx, db)
	} else {
		switch action {
		case "pause", "resume", "stop", "next", "previous", "restart", "volume_up", "volume_down":
			err = c.receiver.Control(ctx, action)
		default:
			return "", errors.New("unknown AirPlay media control")
		}
	}
	if err != nil {
		slog.Warn("airplay_media_failed", "action", action, "error", err)
		return "", err
	}
	switch action {
	case "pause":
		return "Пауза.", nil
	case "resume":
		return "Продолжаю.", nil
	case "stop":
		return "Воспроизведение остановлено.", nil
	case "next":
		return "Следующий трек.", nil
	case "previous":
		return "Предыдущий трек.", nil
	case "restart":
		return "Сначала.", nil
	default:
		return "Громкость AirPlay изменена.", nil
	}
}

func shutdownAirPlay(cancel context.CancelFunc, c *airPlayControl) {
	cancel()
	if c == nil {
		return
	}
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if err := c.receiver.Stop(ctx); err != nil {
		slog.Warn("airplay_shutdown_failed", "error", err)
	}
}
