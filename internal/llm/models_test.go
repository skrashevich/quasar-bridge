package llm

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestModelsUsesConfiguredProviderCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/models" || r.Header.Get("Authorization") != "Bearer provider-fixture" {
			t.Errorf("model catalog request contract was not respected")
		}
		io.WriteString(w, `{"data":[{"id":"provider/first","name":"First"},{"id":"second","display_name":"Second"},{"id":"provider/first","name":"Duplicate"},{"id":"third"}]}`)
	}))
	defer server.Close()
	c := Client{BaseURL: server.URL + "/api/v1///", APIKey: "provider-fixture"}
	got, err := c.Models(t.Context())
	want := []Model{{ID: "provider/first", Name: "First"}, {ID: "second", Name: "Second"}, {ID: "third", Name: "third"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("models=%+v error=%v", got, err)
	}
}

func TestChatGPTModelsUseSubscriptionAndVisibleAccountCatalog(t *testing.T) {
	credentials := 0
	c := Client{
		API: "chatgpt", APIKey: "must-not-use-this",
		AccessToken: func(context.Context) (string, error) { credentials++; return "subscription-fixture", nil },
		HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodGet || r.URL.String() != "https://api.openai.com/v1/models" || r.Header.Get("Authorization") != "Bearer subscription-fixture" {
				t.Errorf("ChatGPT account model catalog contract was not respected")
			}
			body := `{"models":[{"slug":"model-b","display_name":"Model B","visibility":"list"},{"slug":"hidden","display_name":"Hidden","visibility":"hidden"},{"slug":"model-a","display_name":"Model A","visibility":"list"},{"slug":"model-b","visibility":"list"}]}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
	}
	for range 2 {
		got, err := c.Models(t.Context())
		want := []Model{{ID: "model-b", Name: "Model B"}, {ID: "model-a", Name: "Model A"}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("models=%+v error=%v", got, err)
		}
	}
	if credentials != 2 {
		t.Fatalf("credential retrievals=%d", credentials)
	}
}

func TestModelsRejectMalformedAndEmptyCatalogs(t *testing.T) {
	for _, body := range []string{`{`, `null`, `{}`, `{"data":[]}`, `{"data":[null]}`, `{"data":[{}]}`, `{"data":[{"id":null}]}`, `{"data":[{"id":123}]}`, `{"data":[{"id":"valid"}]}secret`, `{"error":{"message":"secret"}}`} {
		t.Run(body, func(t *testing.T) {
			c := modelCatalogClient(body)
			if got, err := c.Models(t.Context()); err == nil || got != nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("models=%v error=%v", got, err)
			}
		})
	}
	c := modelCatalogClient(`{"models":[{"slug":"hidden","visibility":"hidden"}]}`)
	c.API = "chatgpt"
	c.AccessToken = func(context.Context) (string, error) { return "fixture", nil }
	c.BaseURL = ""
	if _, err := c.Models(t.Context()); err == nil || !strings.Contains(err.Error(), "no available models") {
		t.Fatalf("empty ChatGPT catalog error=%v", err)
	}
}

func TestModelsRejectUnsafeIDsWithoutPartialCatalog(t *testing.T) {
	for _, id := range []string{"", "line\nfeed", "tab\there", "space here", "\u00a0", "\u2003", "control\x00", "delete\x7f", "bidi\u202e", strings.Repeat("x", 513)} {
		t.Run(id, func(t *testing.T) {
			encoded, err := json.Marshal(id)
			if err != nil {
				t.Fatal(err)
			}
			c := modelCatalogClient(`{"data":[{"id":"valid"},{"id":` + string(encoded) + `}]}`)
			if models, err := c.Models(t.Context()); err == nil || models != nil {
				t.Fatalf("unsafe catalog accepted: %v error=%v", models, err)
			}
		})
	}
	c := modelCatalogClient("{\"data\":[{\"id\":\"invalid-\xff\"}]}")
	if _, err := c.Models(t.Context()); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestModelsBoundCatalogSize(t *testing.T) {
	c := modelCatalogClient(`{"data":[{"id":"valid","name":"` + strings.Repeat("x", 2<<20) + `"}]}`)
	if got, err := c.Models(t.Context()); err == nil || got != nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("models=%v error=%v", got, err)
	}
}

func TestModelsDoNotFollowRedirectsOrLeakErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusTemporaryRedirect, "HTTP 307"},
		{http.StatusUnauthorized, "HTTP 401"},
		{http.StatusTooManyRequests, "HTTP 429"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			requests := 0
			c := Client{BaseURL: "https://provider.example.invalid/v1", APIKey: "secret", HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": []string{"https://provider.example.invalid/other"}}, Body: io.NopCloser(strings.NewReader("secret upstream data"))}, nil
			})}}
			if _, err := c.Models(t.Context()); err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret") || requests != 1 {
				t.Fatalf("requests=%d error=%v", requests, err)
			}
		})
	}
	c := Client{BaseURL: "https://provider.example.invalid/secret", HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("secret transport") })}}
	if _, err := c.Models(t.Context()); err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "provider.example") {
		t.Fatalf("transport error=%v", err)
	}
}

func TestModelsPreserveCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	c := Client{BaseURL: server.URL + "/v1"}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.Models(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error=%v", err)
	}
}

func TestChatGPTModelsRequireSubscriptionCredentials(t *testing.T) {
	for _, token := range []func(context.Context) (string, error){nil, func(context.Context) (string, error) { return "", nil }, func(context.Context) (string, error) { return "", errors.New("secret credential") }} {
		c := Client{API: "chatgpt", APIKey: "must-not-use-api-key", AccessToken: token, HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			t.Error("request made without subscription credentials")
			return nil, errors.New("unexpected")
		})}}
		if _, err := c.Models(t.Context()); err == nil || !strings.Contains(err.Error(), "ChatGPT sign-in") || strings.Contains(err.Error(), "secret") {
			t.Fatalf("credential error=%v", err)
		}
	}
}

func TestModelsRejectInvalidEndpointBeforeAuthentication(t *testing.T) {
	for _, endpoint := range []string{"", "ftp://example.invalid/v1", "https://user:secret@example.invalid/v1", "https://example.invalid/v1?secret=value", "https://example.invalid/v1#secret"} {
		c := Client{BaseURL: endpoint}
		if _, err := c.Models(t.Context()); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid endpoint error=%v", err)
		}
	}
	c := Client{API: "chatgpt", BaseURL: "https://example.invalid/v1", AccessToken: func(context.Context) (string, error) {
		t.Error("token requested for nonofficial endpoint")
		return "fixture", nil
	}}
	if _, err := c.Models(t.Context()); err == nil {
		t.Fatal("nonofficial ChatGPT endpoint accepted")
	}
}

func modelCatalogClient(body string) *Client {
	return &Client{BaseURL: "https://provider.example.invalid/v1", HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
}
