package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"quasarbridge/internal/chatgpt"
	"quasarbridge/internal/llm"
)

func newLLMClient(cfg config, apiKey string) *llm.Client {
	c := &llm.Client{
		BaseURL: cfg.LLMBaseURL, Model: cfg.LLMModel, APIKey: apiKey,
		API: cfg.LLMAPI, ReasoningEffort: cfg.LLMReasoningEffort,
		MaxOutputTokens: cfg.LLMMaxOutputTokens, HostedTools: cfg.LLMHostedTools,
		ChatParameters: cfg.LLMChatParameters,
	}
	if cfg.LLMAPI == "chatgpt" {
		store := &chatgpt.Store{Path: cfg.LLMChatGPTAuthFile}
		c.AccessToken = store.Token
		c.APIKey = ""
	}
	return c
}

func runChatGPTAuth(ctx context.Context, action, path, address string, out io.Writer) error {
	if path == "" {
		var err error
		path, err = chatgpt.DefaultPath()
		if err != nil {
			return err
		}
	}
	store := &chatgpt.Store{Path: path}
	switch action {
	case "login":
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		if err := store.Login(ctx, address, func(url string) error {
			_, err := fmt.Fprintf(out, "Continue with ChatGPT: open this URL in your browser.\n%s\nWaiting for browser sign-in (10 minutes)…\n", url)
			return err
		}); err != nil {
			return err
		}
		fmt.Fprintln(out, "ChatGPT sign-in completed. Usage and app limits: https://chatgpt.com/settings/usage")
		return printChatGPTStatus(ctx, store, out)
	case "status":
		return printChatGPTStatus(ctx, store, out)
	case "logout":
		if err := store.Logout(ctx); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "Signed out of ChatGPT.")
		return err
	case "models":
		if err := printChatGPTStatus(ctx, store, out); err != nil {
			return err
		}
		return listChatGPTModels(ctx, store.Token, http.DefaultClient, out)
	default:
		return errors.New("chatgpt-auth must be login, status, logout or models")
	}
}

func printChatGPTStatus(ctx context.Context, store *chatgpt.Store, out io.Writer) error {
	status, err := store.Status(ctx)
	if err != nil {
		return err
	}
	if err := json.MarshalWrite(out, status); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out)
	return err
}

func listChatGPTModels(ctx context.Context, token func(context.Context) (string, error), hc *http.Client, out io.Writer) error {
	client := &llm.Client{API: "chatgpt", AccessToken: token, HTTP: hc}
	models, err := client.Models(ctx)
	if err != nil {
		return err
	}
	for _, model := range models {
		if _, err := fmt.Fprintf(out, "%q\t%q\n", model.ID, model.Name); err != nil {
			return err
		}
	}
	return nil
}
