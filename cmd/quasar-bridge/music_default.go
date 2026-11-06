package main

import (
	"context"
	"encoding/json/jsontext"
	"os"
	"quasarbridge/internal/sound"
)

type liveMusicDefault interface {
	SetDefault(bool)
	IsDefault() bool
}

func (c *configurationControl) MusicDefault(ctx context.Context, target string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	provider, available := c.engine.SoundCloud.(liveMusicDefault)
	if target == "status" {
		if available && provider.IsDefault() {
			return "Музыкальный сервис по умолчанию — SoundCloud.", nil
		}
		return "Музыкальный сервис по умолчанию — Яндекс Музыка.", nil
	}
	if target != "soundcloud" && target != "yandex" {
		return "Можно выбрать SoundCloud или Яндекс Музыку по умолчанию.", nil
	}
	sequence := sound.RequestSequence(ctx)
	if sequence != 0 && sequence < c.musicDefaultSequence {
		return "", context.Canceled
	}
	if target == "soundcloud" && !available {
		return "SoundCloud сейчас отключён или недоступен. Музыкальный сервис по умолчанию не изменён.", nil
	}
	value := target == "soundcloud"
	saved, readErr := os.ReadFile(c.path)
	if readErr != nil {
		return "Не удалось прочитать настройку музыкального сервиса. Настройка не изменена.", nil
	}
	next, parseErr := parseConfig(saved, false)
	if parseErr != nil {
		return "Не удалось прочитать настройку музыкального сервиса. Настройка не изменена.", nil
	}
	if value && !next.SoundCloud.Enabled {
		return "SoundCloud отключён в сохранённых настройках. Музыкальный сервис по умолчанию не изменён.", nil
	}
	raw := jsontext.Value("false")
	if value {
		raw = jsontext.Value("true")
	}
	if _, err := c.setLocked(ctx, map[string]jsontext.Value{"soundcloud.default": raw}, saved); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "Не удалось сохранить музыкальный сервис по умолчанию. Настройка не изменена.", nil
	}
	if available {
		provider.SetDefault(value)
	}
	c.active.SoundCloud.Default = value
	if sequence != 0 {
		c.musicDefaultSequence = sequence
	}
	if value {
		return "Теперь музыкальный сервис по умолчанию — SoundCloud. Настройка сохранена.", nil
	}
	return "Теперь музыкальный сервис по умолчанию — Яндекс Музыка. Настройка сохранена.", nil
}
