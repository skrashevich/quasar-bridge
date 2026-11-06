package main

import (
	"errors"
	"reflect"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/journal"
)

func reloadRouting(path string, initial config, engine *bridge.Engine, logs ...*journal.Writer) error {
	next, err := readConfig(path)
	if err != nil {
		return err
	}
	return applyReload(next, initial, engine, logs...)
}

func applyReload(next, initial config, engine *bridge.Engine, logs ...*journal.Writer) error {
	// Listener, credentials, backend clients and warm-up jobs remain bound to the
	// original deployment. Do not silently pretend those settings were reloaded.
	a, b := initial, next
	a.Routing = bridge.RoutingConfig{}
	b.Routing = bridge.RoutingConfig{}
	if len(logs) > 0 {
		a.ConversationLog = journal.Config{}
		b.ConversationLog = journal.Config{}
	}
	if !reflect.DeepEqual(a, b) {
		return errors.New("non-routing settings changed; restart required")
	}
	if len(logs) > 0 {
		if err := logs[0].Configure(next.ConversationLog); err != nil {
			return err
		}
	}
	return engine.SetRouting(next.Routing)
}
