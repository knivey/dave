package main

import (
	"sync"
	"time"
)

type CachedMarkdown struct {
	Markdown string
	Title    string
	URL      string
	CachedAt time.Time
}

type MarkdownCache struct {
	mu    sync.RWMutex
	items map[string]*CachedMarkdown
	ttl   time.Duration
}

func NewMarkdownCache(ttl time.Duration) *MarkdownCache {
	return &MarkdownCache{
		items: make(map[string]*CachedMarkdown),
		ttl:   ttl,
	}
}

func (c *MarkdownCache) Get(url string) *CachedMarkdown {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[url]
	if !ok {
		return nil
	}
	if time.Since(item.CachedAt) > c.ttl {
		delete(c.items, url)
		return nil
	}
	return item
}

func (c *MarkdownCache) Set(url string, item *CachedMarkdown) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item.URL = url
	item.CachedAt = time.Now()
	c.items[url] = item
}

func (c *MarkdownCache) Delete(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, url)
}

func (c *MarkdownCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
