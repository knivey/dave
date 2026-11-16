package main

import (
	"crypto/tls"
	"strings"
	"sync"
	"time"

	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
)

// DESIGN NOTE (docs/superpowers/specs/2026-11-16-soju-bouncer-support-design.md):
// SojuAdmin is dave's unbound session against the configured soju bouncer —
// an ordinary girc client bound to no network, so it sees only soju's
// BouncerServ service. dave's TOML is the single source of truth; this
// session is how the projection gets maintained: networks are created and
// deleted, SASL credentials and connect-commands are pushed, orphaned soju
// state is removed (Task 6). Its death never affects network sessions.
//
// Command/reply protocol constraint: BouncerServ replies are plain NOTICEs
// from the service with no per-command correlation, so commands are
// serialized under a mutex and replies are attributed by quiet-period timing.

const bouncerServiceNick = "BouncerServ"

// SojuAdmin is one process-wide instance (global `sojuAdmin`), started iff
// any enabled network has bouncer = true.
//
// Two mutexes, deliberately: `mu` guards the mutable connection state
// (client/connected/sink) in SHORT critical sections only — deliver() runs on
// the NOTICE handler goroutine and must be able to fan a reply into the sink
// WHILE Command() is collecting, so Command never holds `mu` across
// collectReplies (holding it there would block deliver until the quiet timer
// expired and every reply would be lost). `cmdMu` serializes whole Command
// calls — BouncerServ replies carry no per-command correlation, so two
// concurrent commands' replies would be indistinguishable.
type SojuAdmin struct {
	mu        sync.Mutex
	client    *girc.Client
	connected bool
	sink      chan string

	cmdMu sync.Mutex

	Reconnect bool
	quitCh    chan struct{}
	stopOnce  sync.Once
	soju      *SojuConfig
}

func startSojuAdmin(soju *SojuConfig) *SojuAdmin {
	log := newLogger("soju-admin")
	a := &SojuAdmin{
		Reconnect: true,
		quitCh:    make(chan struct{}),
		soju:      soju,
	}
	client := girc.New(girc.Config{
		Server:     soju.Host,
		Port:       soju.DialPort(),
		Nick:       soju.User,
		SSL:        soju.UseTLS(),
		TLSConfig:  &tls.Config{ServerName: soju.Host, InsecureSkipVerify: soju.InsecureSkipVerify},
		AllowFlood: true,
	})
	client.Config.SASL = &girc.SASLPlain{User: soju.User, Pass: soju.Pass}
	a.client = client

	client.Handlers.Add(girc.RPL_WELCOME, func(c *girc.Client, e girc.Event) {
		a.mu.Lock()
		a.connected = true
		a.mu.Unlock()
		log.Info("admin session connected")
	})

	client.Handlers.AddBg(girc.NOTICE, func(c *girc.Client, e girc.Event) {
		if e.Source == nil || !strings.EqualFold(e.Source.Name, bouncerServiceNick) {
			return
		}
		a.deliver(e.Last())
	})

	client.Handlers.Add(girc.DISCONNECTED, func(c *girc.Client, e girc.Event) {
		a.mu.Lock()
		a.connected = false
		a.mu.Unlock()
	})

	wg.Add(1)
	go a.loop(log)
	return a
}

// loop is the reconnect loop: Connect blocks until the session ends; on
// error it waits a fixed delay (soju is local infrastructure — a slow cadence
// is fine and keeps logs quiet) and retries, until Stop flips Reconnect and
// closes quitCh. No lock is held across Connect.
func (a *SojuAdmin) loop(log logxi.Logger) {
	defer wg.Done()
	for {
		if err := a.client.Connect(); err != nil {
			delay := 60 * time.Second
			select {
			case <-time.After(delay):
			case <-a.quitCh:
			}
		}
		a.mu.Lock()
		reconnect := a.Reconnect
		a.mu.Unlock()
		if !reconnect {
			log.Info("admin session finished quitting")
			return
		}
	}
}

// Stop is idempotent (shutdown path + tests call it freely).
func (a *SojuAdmin) Stop() {
	a.stopOnce.Do(func() {
		a.mu.Lock()
		a.Reconnect = false
		client := a.client
		a.mu.Unlock()
		close(a.quitCh)
		if client != nil {
			client.Close()
		}
	})
}

func (a *SojuAdmin) Connected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connected
}

// deliver fans a BouncerServ NOTICE line into the active command's sink, if
// any. Called from the NOTICE handler goroutine.
func (a *SojuAdmin) deliver(line string) {
	a.mu.Lock()
	if a.sink != nil {
		select {
		case a.sink <- line:
		default: // sink full: drop the line, Command's max-wait still bounds the call
		}
	}
	a.mu.Unlock()
}

// Command sends one BouncerServ command and collects its NOTICE replies,
// returning when the service goes quiet or maxWait elapses. Serialized by
// cmdMu (replies carry no per-command correlation). The state mutex is taken
// only for the gate check and sink install/clear — never across
// collectReplies, so deliver() can fan replies in mid-collection. Returns nil
// when the session is down — callers must treat that as "management
// unavailable", never fatal.
func (a *SojuAdmin) Command(line string) []string {
	a.cmdMu.Lock()
	defer a.cmdMu.Unlock()
	a.mu.Lock()
	client := a.client
	if client == nil || !a.connected {
		a.mu.Unlock()
		return nil
	}
	sink := make(chan string, 64)
	a.sink = sink
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.sink = nil
		a.mu.Unlock()
	}()
	// girc v1.1.3's send path returns no error — a write on a dead socket is
	// dropped inside Client.write (nil-conn guard applies even with
	// AllowFlood). The connected gate above is the session-down guard.
	client.Cmd.Message(bouncerServiceNick, line)
	return collectReplies(sink, 750*time.Millisecond, 5*time.Second)
}

// collectReplies drains sink until quietPeriod passes with no new line, or
// maxWait elapses, whichever first. Pure so tests can drive it directly.
func collectReplies(sink <-chan string, quietPeriod, maxWait time.Duration) []string {
	var replies []string
	quiet := time.NewTimer(quietPeriod)
	max := time.NewTimer(maxWait)
	defer quiet.Stop()
	defer max.Stop()
	for {
		select {
		case r := <-sink:
			replies = append(replies, r)
			if !quiet.Stop() {
				select {
				case <-quiet.C:
				default:
				}
			}
			quiet.Reset(quietPeriod)
		case <-quiet.C:
			return replies
		case <-max.C:
			return replies
		}
	}
}

// commandRunner is the seam reconciliation is tested through (Task 6): the
// real admin session or a recording fake.
type commandRunner interface {
	Command(line string) []string
}

// anyEnabledBouncerNetwork reports whether any network in the map is enabled
// AND in bouncer mode — the admin session only exists then.
var anyEnabledBouncerNetwork = func(networks map[string]Network) bool {
	for _, n := range networks {
		if n.IsEnabled() && n.IsBouncer() {
			return true
		}
	}
	return false
}
