package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTestProxyFile(t *testing.T, dir string, content string) string {
	t.Helper()
	path := filepath.Join(dir, "proxies.txt")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	return path
}

func TestProxyRotatorEmpty(t *testing.T) {
	r, err := NewProxyRotator("", "")
	require.NoError(t, err)
	assert.Equal(t, 0, r.Len())
	assert.Equal(t, "", r.Next())
}

func TestProxyRotatorRoundRobin(t *testing.T) {
	dir := t.TempDir()
	path := writeTestProxyFile(t, dir, "http://a:8080\nhttp://b:8080\nhttp://c:8080\n")

	r, err := NewProxyRotator(path, dir)
	require.NoError(t, err)
	assert.Equal(t, 3, r.Len())

	assert.Equal(t, "http://a:8080", r.Next())
	assert.Equal(t, "http://b:8080", r.Next())
	assert.Equal(t, "http://c:8080", r.Next())
	assert.Equal(t, "http://a:8080", r.Next())
}

func TestProxyRotatorSkipsBlanksAndComments(t *testing.T) {
	dir := t.TempDir()
	path := writeTestProxyFile(t, dir, "# comment\n\nhttp://a:8080\n  \n# another\nhttp://b:8080\n")

	r, err := NewProxyRotator(path, dir)
	require.NoError(t, err)
	assert.Equal(t, 2, r.Len())
	assert.Equal(t, "http://a:8080", r.Next())
	assert.Equal(t, "http://b:8080", r.Next())
}

func TestProxyRotatorEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := writeTestProxyFile(t, dir, "# only comments\n\n")

	r, err := NewProxyRotator(path, dir)
	require.NoError(t, err)
	assert.Equal(t, 0, r.Len())
	assert.Equal(t, "", r.Next())
}

func TestProxyRotatorFileNotFound(t *testing.T) {
	_, err := NewProxyRotator("/nonexistent/proxies.txt", "")
	assert.Error(t, err)
}

func TestProxyRotatorReload(t *testing.T) {
	dir := t.TempDir()
	path := writeTestProxyFile(t, dir, "http://a:8080\nhttp://b:8080\n")

	r, err := NewProxyRotator(path, dir)
	require.NoError(t, err)
	assert.Equal(t, "http://a:8080", r.Next())

	writeTestProxyFile(t, dir, "http://x:8080\nhttp://y:8080\nhttp://z:8080\n")
	require.NoError(t, r.Reload(path))

	assert.Equal(t, 3, r.Len())
	assert.Equal(t, "http://y:8080", r.Next())
	assert.Equal(t, "http://z:8080", r.Next())
}

func TestProxyRotatorReloadClearsOnEmpty(t *testing.T) {
	dir := t.TempDir()
	path := writeTestProxyFile(t, dir, "http://a:8080\n")

	r, err := NewProxyRotator(path, dir)
	require.NoError(t, err)
	assert.Equal(t, 1, r.Len())

	require.NoError(t, r.Reload(""))
	assert.Equal(t, 0, r.Len())
	assert.Equal(t, "", r.Next())
}

func TestProxyRotatorConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := writeTestProxyFile(t, dir, "http://a:8080\nhttp://b:8080\n")

	r, err := NewProxyRotator(path, dir)
	require.NoError(t, err)

	done := make(chan string, 100)
	for i := 0; i < 100; i++ {
		go func() {
			done <- r.Next()
		}()
	}

	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		counts[<-done]++
	}

	assert.Equal(t, 50, counts["http://a:8080"])
	assert.Equal(t, 50, counts["http://b:8080"])
}

func TestProxyRotatorStatePersist(t *testing.T) {
	dir := t.TempDir()
	path := writeTestProxyFile(t, dir, "http://a:8080\nhttp://b:8080\nhttp://c:8080\n")

	r1, err := NewProxyRotator(path, dir)
	require.NoError(t, err)

	assert.Equal(t, "http://a:8080", r1.Next())
	assert.Equal(t, "http://b:8080", r1.Next())

	r2, err := NewProxyRotator(path, dir)
	require.NoError(t, err)

	assert.Equal(t, "http://c:8080", r2.Next())
	assert.Equal(t, "http://a:8080", r2.Next())
}

func TestProxyRotatorStateNoDir(t *testing.T) {
	dir := t.TempDir()
	path := writeTestProxyFile(t, dir, "http://a:8080\nhttp://b:8080\n")

	r, err := NewProxyRotator(path, "")
	require.NoError(t, err)

	assert.Equal(t, "http://a:8080", r.Next())
	assert.Equal(t, "http://b:8080", r.Next())
}

func TestLoadProxyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxies.txt")
	require.NoError(t, os.WriteFile(path, []byte("socks5://user:pass@host:1080\nhttp://proxy:8080\n"), 0644))

	proxies, err := loadProxyFile(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"socks5://user:pass@host:1080", "http://proxy:8080"}, proxies)
}
