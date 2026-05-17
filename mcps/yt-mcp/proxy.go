package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type ProxyRotator struct {
	mu       sync.Mutex
	proxies  []string
	index    uint64
	stateDir string
}

func NewProxyRotator(proxyPath string, stateDir string) (*ProxyRotator, error) {
	if proxyPath == "" {
		return &ProxyRotator{}, nil
	}

	proxies, err := loadProxyFile(proxyPath)
	if err != nil {
		return nil, err
	}

	if len(proxies) == 0 && logger != nil {
		logger.Warn("proxy file has no entries", "path", proxyPath)
	}

	r := &ProxyRotator{proxies: proxies, stateDir: stateDir}
	r.index = r.loadState()
	return r, nil
}

func (r *ProxyRotator) Next() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.proxies) == 0 {
		return ""
	}

	proxy := r.proxies[r.index%uint64(len(r.proxies))]
	r.index++
	r.saveState()
	return proxy
}

func (r *ProxyRotator) Reload(proxyPath string) error {
	if proxyPath == "" {
		r.mu.Lock()
		r.proxies = nil
		r.mu.Unlock()
		return nil
	}

	proxies, err := loadProxyFile(proxyPath)
	if err != nil {
		return err
	}

	r.mu.Lock()
	r.proxies = proxies
	r.mu.Unlock()
	return nil
}

func (r *ProxyRotator) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.proxies)
}

func (r *ProxyRotator) statePath() string {
	if r.stateDir == "" {
		return ""
	}
	return filepath.Join(r.stateDir, "proxy_state")
}

func (r *ProxyRotator) loadState() uint64 {
	path := r.statePath()
	if path == "" {
		return 0
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}

	idx, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}

	return idx
}

func (r *ProxyRotator) saveState() {
	path := r.statePath()
	if path == "" {
		return
	}

	_ = os.WriteFile(path, []byte(strconv.FormatUint(r.index, 10)), 0644)
}

func loadProxyFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening proxy file %s: %w", path, err)
	}
	defer f.Close()

	var proxies []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		proxies = append(proxies, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading proxy file %s: %w", path, err)
	}

	return proxies, nil
}
