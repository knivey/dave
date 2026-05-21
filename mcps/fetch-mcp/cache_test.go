package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestMarkdownCacheSetGet(t *testing.T) {
	cache := NewMarkdownCache(5 * time.Minute)
	cache.Set("https://example.com", &CachedMarkdown{
		Markdown: "# Hello\nWorld",
		Title:    "Example",
	})

	item := cache.Get("https://example.com")
	assert.NotNil(t, item)
	assert.Equal(t, "# Hello\nWorld", item.Markdown)
	assert.Equal(t, "Example", item.Title)
	assert.Equal(t, "https://example.com", item.URL)
}

func TestMarkdownCacheExpired(t *testing.T) {
	cache := NewMarkdownCache(1 * time.Millisecond)
	cache.Set("https://example.com", &CachedMarkdown{
		Markdown: "content",
		Title:    "Title",
	})

	time.Sleep(5 * time.Millisecond)
	item := cache.Get("https://example.com")
	assert.Nil(t, item)
	assert.Equal(t, 0, cache.Len())
}

func TestMarkdownCacheMiss(t *testing.T) {
	cache := NewMarkdownCache(5 * time.Minute)
	item := cache.Get("https://nonexistent.com")
	assert.Nil(t, item)
}

func TestMarkdownCacheDelete(t *testing.T) {
	cache := NewMarkdownCache(5 * time.Minute)
	cache.Set("https://example.com", &CachedMarkdown{Markdown: "content"})
	assert.Equal(t, 1, cache.Len())

	cache.Delete("https://example.com")
	assert.Nil(t, cache.Get("https://example.com"))
	assert.Equal(t, 0, cache.Len())
}

func TestMarkdownCacheOverwrite(t *testing.T) {
	cache := NewMarkdownCache(5 * time.Minute)
	cache.Set("https://example.com", &CachedMarkdown{Markdown: "old"})
	cache.Set("https://example.com", &CachedMarkdown{Markdown: "new"})

	item := cache.Get("https://example.com")
	assert.NotNil(t, item)
	assert.Equal(t, "new", item.Markdown)
	assert.Equal(t, 1, cache.Len())
}
