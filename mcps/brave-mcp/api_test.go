package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBraveClientDoRequest(t *testing.T) {
	var receivedParams url.Values
	var receivedAuth string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedParams = r.URL.Query()
		receivedAuth = r.Header.Get("X-Subscription-Token")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"web":{"results":[]}}`))
	}))
	defer ts.Close()

	client := newBraveClient("test-key", ts.URL, 5*time.Second, "US", "en")
	_, err := client.doSearchRequest(context.Background(), "/res/v1/web/search", url.Values{"q": {"golang"}})

	require.NoError(t, err)
	assert.Equal(t, "test-key", receivedAuth)
	assert.Equal(t, "golang", receivedParams.Get("q"))
	assert.Equal(t, "US", receivedParams.Get("country"))
	assert.Equal(t, "en", receivedParams.Get("search_lang"))
}

func TestBraveClientDoRequestNoDefaults(t *testing.T) {
	var receivedParams url.Values

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedParams = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	client := newBraveClient("key", ts.URL, 5*time.Second, "US", "en")
	_, err := client.doRequest(context.Background(), "/res/v1/local/pois", url.Values{"ids": {"abc"}})

	require.NoError(t, err)
	assert.Equal(t, "", receivedParams.Get("country"))
	assert.Equal(t, "", receivedParams.Get("search_lang"))
	assert.Equal(t, "abc", receivedParams.Get("ids"))
}

func TestBraveClientDefaultCountryLang(t *testing.T) {
	var receivedParams url.Values

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedParams = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	client := newBraveClient("key", ts.URL, 5*time.Second, "", "")
	_, err := client.doRequest(context.Background(), "/res/v1/web/search", url.Values{"q": {"test"}})

	require.NoError(t, err)
	assert.Equal(t, "", receivedParams.Get("country"))
	assert.Equal(t, "", receivedParams.Get("search_lang"))
}

func TestBraveClientErrorResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer ts.Close()

	client := newBraveClient("key", ts.URL, 5*time.Second, "US", "en")
	_, err := client.doRequest(context.Background(), "/res/v1/web/search", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "API returned 400")
}

func TestFormatWebResults(t *testing.T) {
	input := `{
		"web": {
			"results": [
				{"url": "https://example.com", "title": "Example", "description": "A test result"}
			]
		},
		"faq": {
			"results": [
				{"question": "What?", "answer": "This.", "title": "FAQ", "url": "https://faq.com"}
			]
		},
		"news": {
			"results": [
				{"url": "https://news.com", "title": "Breaking", "description": "News", "age": "2h", "source": "CNN"}
			]
		}
	}`

	result := formatWebResults(json.RawMessage(input))
	assert.Contains(t, result, "Example")
	assert.Contains(t, result, "https://example.com")
	assert.Contains(t, result, "FAQ: What?")
	assert.Contains(t, result, "News: Breaking")
}

func TestFormatWebResultsEmpty(t *testing.T) {
	result := formatWebResults(json.RawMessage(`{"web":{"results":[]}}`))
	assert.Equal(t, "", result)
}

func TestFormatImageResults(t *testing.T) {
	input := `{
		"results": [
			{"url": "https://img.com/1.jpg", "title": "Cat", "description": "A cat", "source": "cats.com"}
		]
	}`
	result := formatImageResults(json.RawMessage(input))
	assert.Contains(t, result, "Cat")
	assert.Contains(t, result, "https://img.com/1.jpg")
}

func TestFormatNewsResults(t *testing.T) {
	input := `{
		"results": [
			{"url": "https://news.com/1", "title": "Breaking", "description": "Big news", "age": "1h", "source": "BBC"}
		]
	}`
	result := formatNewsResults(json.RawMessage(input))
	assert.Contains(t, result, "Breaking (BBC, 1h)")
}

func TestFormatVideoResults(t *testing.T) {
	input := `{
		"results": [
			{"url": "https://vid.com/1", "title": "Tutorial", "description": "Learn Go", "age": "3d", "video": {"duration": "10:30", "views": 1000, "creator": "Gopher"}}
		]
	}`
	result := formatVideoResults(json.RawMessage(input))
	assert.Contains(t, result, "Tutorial [10:30] by Gopher (3d)")
}

func TestFormatSummarizerResults(t *testing.T) {
	input := `{
		"status": "complete",
		"summary": [
			{"type": "token", "data": "Go is a "},
			{"type": "token", "data": "programming language"},
			{"type": "inline_reference", "data": "https://go.dev"}
		]
	}`
	result := formatSummarizerResults(json.RawMessage(input))
	assert.Contains(t, result, "Go is a programming language")
	assert.Contains(t, result, "(https://go.dev)")
}

func TestFormatLocalResults(t *testing.T) {
	pois := `{
		"results": [
			{
				"id": "abc123",
				"title": "Joe's Pizza",
				"rating": {"ratingValue": 4.5, "reviewCount": 120},
				"postal_address": {"displayAddress": "123 Main St"},
				"phone": "555-1234"
			}
		]
	}`
	descs := `{
		"results": [
			{"id": "abc123", "description": "Best pizza in town"}
		]
	}`

	result := formatLocalResults(json.RawMessage(pois), json.RawMessage(descs))
	assert.Contains(t, result, "Joe's Pizza")
	assert.Contains(t, result, "4.5/5")
	assert.Contains(t, result, "Best pizza in town")
}

func TestFormatLocalResultsNoDescriptions(t *testing.T) {
	pois := `{
		"results": [
			{"id": "x", "title": "Place", "rating": {}, "postal_address": {}}
		]
	}`
	result := formatLocalResults(json.RawMessage(pois), nil)
	assert.Contains(t, result, "Place")
}
