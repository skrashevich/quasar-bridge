package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"maps"
	"os"
	"reflect"
	"slices"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/configvoice"
)

type routingBehavior struct {
	ClassifierEnabled bool   `json:"classifier_enabled"`
	DefaultRoute      string `json:"default_route"`
}

func effectiveRoutingBehavior(cfg bridge.RoutingConfig) routingBehavior {
	return routingBehavior{cfg.ClassifierEnabled(), cfg.DefaultDestination()}
}

func (c *configurationControl) GetRouting(ctx context.Context) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	next, err := readConfig(c.path)
	if err != nil {
		return nil, err
	}
	active, err := bridge.CompileRouting(c.active.Routing)
	if err != nil {
		return nil, err
	}
	saved, err := bridge.CompileRouting(next.Routing)
	if err != nil {
		return nil, err
	}
	return struct {
		Active         bridge.RoutingConfig `json:"active"`
		Saved          bridge.RoutingConfig `json:"saved"`
		ActiveRevision string               `json:"active_revision"`
		SavedRevision  string               `json:"saved_revision"`
		Pending        bool                 `json:"pending"`
		Order          string               `json:"order"`
		ActiveBehavior routingBehavior      `json:"active_behavior"`
		SavedBehavior  routingBehavior      `json:"saved_behavior"`
	}{c.active.Routing, next.Routing, active.Revision(), saved.Revision(), !reflect.DeepEqual(c.active.Routing, next.Routing), "first enabled matching rule wins; Bridge configuration administration has priority over these rules", effectiveRoutingBehavior(c.active.Routing), effectiveRoutingBehavior(next.Routing)}, nil
}

func (c *configurationControl) MutateRouting(ctx context.Context, mutation assistant.RoutingMutation) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.restartPending() {
		return nil, errors.New("restart already scheduled; wait for the new process")
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return nil, errors.New("cannot read configuration file")
	}
	cfg, err := parseConfig(raw, false)
	if err != nil {
		return nil, err
	}
	policy, err := bridge.CompileRouting(cfg.Routing)
	if err != nil {
		return nil, err
	}
	if mutation.ExpectedRevision == "" || mutation.ExpectedRevision != policy.Revision() {
		return nil, errors.New("routing revision changed; read get_quasar_bridge_routing again before retrying")
	}
	if !reflect.DeepEqual(cfg.Routing, c.active.Routing) {
		return nil, errors.New("routing has unapplied saved changes; explicitly apply or discard them before editing an individual rule")
	}
	rules := cfg.Routing.Rules
	name := mutation.Name
	var rule bridge.RoutingRule
	if mutation.Action == "upsert" {
		if json.Unmarshal(mutation.Rule, &rule, json.RejectUnknownMembers(true)) != nil || rule.Name == "" {
			return nil, errors.New("invalid routing rule object")
		}
		name = rule.Name
	}
	index := slices.IndexFunc(rules, func(r bridge.RoutingRule) bool { return r.Name == name })
	if mutation.Action != "upsert" && index < 0 {
		return nil, errors.New("routing rule not found; use its exact name from get_quasar_bridge_routing")
	}
	switch mutation.Action {
	case "upsert":
		if index < 0 {
			rules = slices.Insert(rules, 0, rule)
			index = 0
		} else {
			rules[index] = rule
		}
	case "enable", "disable":
		if mutation.Before != nil {
			return nil, errors.New("before is only valid for upsert or move")
		}
		rules[index].Disabled = mutation.Action == "disable"
	case "delete":
		if mutation.Before != nil {
			return nil, errors.New("before is only valid for upsert or move")
		}
		rules = slices.Delete(rules, index, index+1)
	case "move":
		if mutation.Before == nil {
			return nil, errors.New("move requires before; an empty string means the end")
		}
	default:
		return nil, errors.New("unknown routing rule operation")
	}
	if mutation.Before != nil && *mutation.Before != name {
		// Remove the moved rule before locating its anchor, so changing relative
		// order never depends on the old index or the model's index arithmetic.
		moving := rules[index]
		rules = slices.Delete(rules, index, index+1)
		position := len(rules)
		if *mutation.Before != "" {
			position = slices.IndexFunc(rules, func(r bridge.RoutingRule) bool { return r.Name == *mutation.Before })
			if position < 0 {
				return nil, errors.New("before routing rule not found")
			}
		}
		rules = slices.Insert(rules, position, moving)
	}
	cfg.Routing.Rules = rules
	// The regular configuration writer validates the whole candidate and keeps
	// its existing atomic backup, external-edit detection and permissions.
	encoded, err := json.Marshal(rules)
	if err != nil {
		return nil, errors.New("cannot encode routing rules")
	}
	result, err := c.applyRoutingChangesLocked(ctx, cfg, raw, map[string]jsontext.Value{"routing.rules": encoded})
	if err != nil {
		return nil, err
	}
	return struct {
		configurationResult
		Rule     string `json:"rule"`
		Revision string `json:"routing_revision"`
	}{result, name, c.engine.RoutingStatus().Revision}, nil
}

func (c *configurationControl) applyRoutingChangesLocked(ctx context.Context, cfg config, raw []byte, changes map[string]jsontext.Value) (configurationResult, error) {
	if _, err := c.setLocked(ctx, changes, raw); err != nil {
		return configurationResult{}, err
	}
	if err := c.engine.SetRouting(cfg.Routing); err != nil {
		return configurationResult{}, errors.New("routing saved but not applied; reload routing before claiming success")
	}
	c.active.Routing = cfg.Routing
	result := c.result("applied", cfg)
	result.Changed = slices.Sorted(maps.Keys(changes))
	return result, nil
}

func (c *configurationControl) ConfigureRouting(ctx context.Context, options assistant.RoutingOptions) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.restartPending() {
		return nil, errors.New("restart already scheduled; wait for the new process")
	}
	if options.ClassifierEnabled == nil && options.DefaultRoute == nil {
		return nil, errors.New("at least one routing option is required")
	}
	if options.DefaultRoute != nil && !slices.Contains([]string{"llm", "yandex", "home"}, *options.DefaultRoute) {
		return nil, errors.New("default_route must be llm, yandex or home")
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return nil, errors.New("cannot read configuration file")
	}
	cfg, err := parseConfig(raw, false)
	if err != nil {
		return nil, err
	}
	policy, err := bridge.CompileRouting(cfg.Routing)
	if err != nil {
		return nil, err
	}
	if options.ExpectedRevision == "" || options.ExpectedRevision != policy.Revision() {
		return nil, errors.New("routing revision changed; read get_quasar_bridge_routing again before retrying")
	}
	if !reflect.DeepEqual(cfg.Routing, c.active.Routing) {
		return nil, errors.New("routing has unapplied saved changes; explicitly apply or discard them first")
	}
	// Materialize legacy inference before changing either option, so choosing a
	// new default cannot accidentally toggle classification and vice versa.
	enabled, route := cfg.Routing.ClassifierEnabled(), cfg.Routing.DefaultDestination()
	if options.ClassifierEnabled != nil {
		enabled = *options.ClassifierEnabled
	}
	if options.DefaultRoute != nil {
		route = *options.DefaultRoute
	}
	cfg.Routing.Classifier.Enabled = new(enabled)
	cfg.Routing.DefaultRoute = route
	enabledJSON, _ := json.Marshal(enabled)
	routeJSON, _ := json.Marshal(route)
	result, err := c.applyRoutingChangesLocked(ctx, cfg, raw, map[string]jsontext.Value{"routing.classifier.enabled": enabledJSON, "routing.default_route": routeJSON})
	if err != nil {
		return nil, err
	}
	return struct {
		configurationResult
		Behavior routingBehavior `json:"active_behavior"`
		Revision string          `json:"routing_revision"`
	}{result, effectiveRoutingBehavior(cfg.Routing), c.engine.RoutingStatus().Revision}, nil
}

func (c *configurationControl) TestRouting(ctx context.Context, text string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	next, err := readConfig(c.path)
	if err != nil {
		return nil, err
	}
	active, err := bridge.CompileRouting(c.active.Routing)
	if err != nil {
		return nil, err
	}
	saved, err := bridge.CompileRouting(next.Routing)
	if err != nil {
		return nil, err
	}
	a, b := active.Preview(text), saved.Preview(text)
	if c.engine.Configuration && configvoice.IsRequest(text) {
		a = bridge.RouteDecision{Route: "llm", Source: "bridge_configuration"}
		b = a
	}
	return struct {
		Active           bridge.RouteDecision `json:"active"`
		Saved            bridge.RouteDecision `json:"saved"`
		ClassifierCalled bool                 `json:"classifier_called"`
		Scope            string               `json:"scope"`
	}{a, b, false, "static rules and Bridge administration; special media/dialogue handlers are not simulated"}, nil
}
