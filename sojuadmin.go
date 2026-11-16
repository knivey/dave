package main

import (
	"crypto/tls"
	"fmt"
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
		// Reconciliation re-runs on every (re)connect; it is idempotent.
		go reconcileSojuNetworks(a, log)
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

// sojuNetworkPlan is one TOML bouncer network resolved against the soju
// projection. Addr is the recorded upstream address for network create:
// the first server's host, with :port appended when it is not the TLS
// default (soju connects upstream with TLS by default).
type sojuNetworkPlan struct {
	Name            string
	Addr            string
	Nick            string
	RealName        string
	SASL            *SASLConfig
	ConnectCommands []string
}

// buildSojuPlans snapshots the enabled bouncer networks (plan set) and every
// TOML network name (the never-delete set, regardless of mode or enabled).
func buildSojuPlans(networks map[string]Network) ([]sojuNetworkPlan, map[string]bool) {
	var plans []sojuNetworkPlan
	tomlNames := make(map[string]bool, len(networks))
	for name, n := range networks {
		tomlNames[name] = true
		if !n.IsEnabled() || !n.IsBouncer() {
			continue
		}
		p := sojuNetworkPlan{
			Name:            name,
			Nick:            n.Nick,
			RealName:        n.RealName,
			SASL:            n.SASL,
			ConnectCommands: n.ConnectCommands,
		}
		if len(n.Servers) > 0 {
			s := n.Servers[0]
			if s.GetPort() != 6697 {
				p.Addr = fmt.Sprintf("%s:%d", s.Host, s.GetPort())
			} else {
				p.Addr = s.Host
			}
		}
		plans = append(plans, p)
	}
	return plans, tomlNames
}

// parseNetworkStatus extracts network names from BouncerServ `network status`
// NOTICE lines. soju's service.go prints "<name> [<status>, ...][: details]"
// and "<name> (<addr>) [<status>, ...]" — the name is everything before the
// first " [", minus a trailing " (<addr>)". Anything unparseable is skipped;
// this is the ONLY BouncerServ output dave parses (spec constraint), so if a
// soju upgrade changes the shape, this function is the one file to fix.
func parseNetworkStatus(lines []string) map[string]bool {
	names := map[string]bool{}
	for _, line := range lines {
		idx := strings.Index(line, " [")
		if idx <= 0 {
			continue
		}
		name := line[:idx]
		if i := strings.Index(name, " ("); i > 0 {
			name = name[:i]
		}
		name = strings.TrimSpace(name)
		if name != "" {
			names[name] = true
		}
	}
	return names
}

// reconcileNetworks aligns soju's network set with dave's TOML (the spec's
// network-lifecycle steps): create missing bouncer networks (pushing their
// SASL + connect-commands, since a fresh soju network has none), leave
// existing ones untouched unless force, and delete orphans — soju networks
// whose name matches no TOML network in any mode. Names present in TOML but
// direct-mode/disabled are WARNed and deliberately preserved: a temporary
// bouncer = false flip must never destroy stored credentials.
func reconcileNetworks(run commandRunner, existing map[string]bool, plans []sojuNetworkPlan, tomlNames map[string]bool, force bool, log logxi.Logger) {
	inPlans := make(map[string]bool, len(plans))
	for _, p := range plans {
		inPlans[p.Name] = true
		if existing[p.Name] {
			if force {
				pushSojuCreds(run, p, log)
			}
			continue
		}
		log.Info("soju: creating network", "network", p.Name, "addr", p.Addr)
		run.Command(fmt.Sprintf("network create -addr %s -name %s -nick %q -realname %q", p.Addr, p.Name, p.Nick, p.RealName))
		pushSojuCreds(run, p, log)
	}
	for name := range existing {
		switch {
		case tomlNames[name] && !inPlans[name]:
			log.Warn("soju: network present in TOML but not bouncer mode (or disabled) — leaving soju state as-is", "network", name)
		case tomlNames[name]:
			// bouncer-mode plan, already handled above
		default:
			log.Warn("soju: deleting orphaned network (absent from config.toml)", "network", name)
			run.Command("network delete " + name)
		}
	}
}

// pushSojuCreds writes a plan's upstream credentials into soju. Only ever
// called for freshly created networks or under force_reconcile — network
// update forces an upstream reconnect, so unconditional pushes would make
// every dave boot visible to the IRC networks.
func pushSojuCreds(run commandRunner, p sojuNetworkPlan, log logxi.Logger) {
	if p.SASL != nil && p.SASL.User != "" {
		log.Info("soju: pushing SASL credentials", "network", p.Name)
		run.Command(fmt.Sprintf("sasl set-plain -network %s %q %q", p.Name, p.SASL.User, p.SASL.Pass))
	}
	for _, cmd := range p.ConnectCommands {
		log.Info("soju: pushing connect-command", "network", p.Name)
		run.Command(fmt.Sprintf("network update -connect-command %q", cmd))
	}
}

// reconcileSojuNetworks is the real wiring: snapshot config, query soju's
// network list, reconcile. Runs on the admin session's RPL_WELCOME.
func reconcileSojuNetworks(a *SojuAdmin, log logxi.Logger) {
	var soju *SojuConfig
	var networks map[string]Network
	readConfig(func() {
		soju = config.Soju
		networks = make(map[string]Network, len(config.Networks))
		for name, n := range config.Networks {
			networks[name] = n
		}
	})
	if soju == nil {
		return
	}
	plans, tomlNames := buildSojuPlans(networks)
	existing := parseNetworkStatus(a.Command("network status"))
	reconcileNetworks(a, existing, plans, tomlNames, soju.ForceReconcile, log)
}

// channelDiffGrace is how long after RPL_WELCOME a bouncer network waits
// before diffing girc's channel state against the TOML channel list. soju
// sends its stored-channel join burst right after registration; the diff
// must observe the settled state. Re-armed naturally per reconnect (each
// WELCOME schedules one diff; repeats are idempotent).
const channelDiffGrace = 15 * time.Second

// extraChannels returns channels girc shows joined that dave's TOML does not
// configure — the soju-side leftovers a channel removal leaves behind.
// Normalized compare, same rule as Network.GetChannelConfig (config keys stay
// exactly as typed; normalization happens at lookup time).
func extraChannels(joined []string, channels map[string]ChannelConfig, casemapping string) []string {
	cm := casemapping
	if cm == "" {
		cm = "rfc1459"
	}
	var out []string
	for _, ch := range joined {
		norm := normalizeIRC(ch, cm)
		found := false
		for k := range channels {
			if normalizeIRC(k, cm) == norm {
				found = true
				break
			}
		}
		if !found {
			out = append(out, ch)
		}
	}
	return out
}
