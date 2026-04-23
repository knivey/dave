package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	logxi "github.com/mgutz/logxi/v1"
)

var loggerQM = logxi.New("queue")

type QueueItem struct {
	ID          int64
	Network     string
	Channel     string
	Nick        string
	Service     string
	Description string
	Enqueued    time.Time
	Execute     func(ctx context.Context, output chan<- string)
	outputCh    chan string
	ctx         context.Context
	cancel      context.CancelFunc
}

type UserQueue struct {
	current *QueueItem
	pending []*QueueItem
}

type execSlot struct {
	max       int
	semaphore chan struct{}
}

func (es *execSlot) tryAcquire() bool {
	if es.semaphore == nil {
		return true
	}
	select {
	case es.semaphore <- struct{}{}:
		return true
	default:
		return false
	}
}

func (es *execSlot) release() {
	if es.semaphore == nil {
		return
	}
	<-es.semaphore
}

type deliverySlot struct {
	mu sync.Mutex
}

type QueueManager struct {
	mu           sync.Mutex
	users        map[string]*UserQueue
	execSlots    map[string]*execSlot
	deliveryMu   sync.Mutex
	deliverySlot map[string]*deliverySlot
	changed      chan struct{}
	idCounter    atomic.Int64
	maxDepth     int
	queueMsgs    []string
	startedMsg   string
	running      atomic.Bool
}

var queueMgr *QueueManager

func NewQueueManager(queueMsgs []string, startedMsg string, maxDepth int) *QueueManager {
	if len(queueMsgs) == 0 {
		queueMsgs = []string{"queued (position {position})"}
	}
	if startedMsg == "" {
		startedMsg = "\x0306\u25b6 {nick}: Processing your request (waited {wait})...\x0f"
	}
	if maxDepth <= 0 {
		maxDepth = 5
	}
	return &QueueManager{
		users:        make(map[string]*UserQueue),
		execSlots:    make(map[string]*execSlot),
		deliverySlot: make(map[string]*deliverySlot),
		changed:      make(chan struct{}, 1),
		maxDepth:     maxDepth,
		queueMsgs:    queueMsgs,
		startedMsg:   startedMsg,
	}
}

func (qm *QueueManager) Start() {
	qm.running.Store(true)
	go qm.scheduler()
	loggerQM.Info("Queue manager started")
}

func (qm *QueueManager) Stop() {
	qm.running.Store(false)
	qm.notify()
	qm.mu.Lock()
	defer qm.mu.Unlock()
	for _, uq := range qm.users {
		if uq.current != nil && uq.current.cancel != nil {
			uq.current.cancel()
		}
		for _, item := range uq.pending {
			if item.cancel != nil {
				item.cancel()
			}
		}
		uq.pending = nil
	}
}

func (qm *QueueManager) notify() {
	select {
	case qm.changed <- struct{}{}:
	default:
	}
}

func itemKey(network, channel, nick string) string {
	return network + channel + nick
}

func (qm *QueueManager) getOrCreateUserQueue(key string) *UserQueue {
	uq, ok := qm.users[key]
	if !ok {
		uq = &UserQueue{}
		qm.users[key] = uq
	}
	return uq
}

func (qm *QueueManager) Enqueue(network, channel, nick, service, desc string, fn func(ctx context.Context, output chan<- string)) int {
	if !qm.running.Load() {
		return -1
	}

	qm.mu.Lock()
	defer qm.mu.Unlock()

	key := itemKey(network, channel, nick)
	uq := qm.getOrCreateUserQueue(key)

	position := len(uq.pending)
	if uq.current != nil {
		position++
	}

	if position >= qm.maxDepth {
		return -1
	}

	ctx, cancel := context.WithCancel(context.Background())

	item := &QueueItem{
		ID:          qm.idCounter.Add(1),
		Network:     network,
		Channel:     channel,
		Nick:        nick,
		Service:     service,
		Description: desc,
		Enqueued:    time.Now(),
		Execute:     fn,
		outputCh:    make(chan string, 200),
		ctx:         ctx,
		cancel:      cancel,
	}

	if uq.current == nil {
		slot := qm.execSlots[service]
		if slot != nil && !slot.tryAcquire() {
			uq.pending = append(uq.pending, item)
			qm.notify()
			return len(uq.pending)
		}
		uq.current = item
		go qm.runJob(item)
		return 0
	}

	uq.pending = append(uq.pending, item)
	qm.notify()
	return position
}

func (qm *QueueManager) StopCurrent(network, channel, nick string) bool {
	qm.mu.Lock()
	key := itemKey(network, channel, nick)
	uq, ok := qm.users[key]
	if !ok || uq.current == nil {
		qm.mu.Unlock()
		return false
	}
	item := uq.current
	qm.mu.Unlock()

	if item.cancel != nil {
		item.cancel()
	}
	return true
}

func (qm *QueueManager) IsRunning(network, channel, nick string) bool {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	key := itemKey(network, channel, nick)
	uq, ok := qm.users[key]
	if !ok {
		return false
	}
	return uq.current != nil
}

func (qm *QueueManager) QueueStatus(network, channel, nick string) (current *QueueItem, pending []*QueueItem) {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	key := itemKey(network, channel, nick)
	uq, ok := qm.users[key]
	if !ok {
		return nil, nil
	}
	return uq.current, uq.pending
}

func (qm *QueueManager) UpdateServiceLimits(services map[string]Service) {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	newSlots := make(map[string]*execSlot)
	for name, svc := range services {
		parallel := svc.Parallel
		if parallel < 0 {
			parallel = 1
		}
		var sem chan struct{}
		if parallel > 0 {
			sem = make(chan struct{}, parallel)
		}
		if existing, ok := qm.execSlots[name]; ok && existing.max == parallel {
			newSlots[name] = existing
			continue
		}
		newSlots[name] = &execSlot{
			max:       parallel,
			semaphore: sem,
		}
	}
	qm.execSlots = newSlots
	qm.notify()
}

func (qm *QueueManager) scheduler() {
	for range qm.changed {
		if !qm.running.Load() {
			return
		}
		qm.schedule()
	}
}

func (qm *QueueManager) schedule() {
	if !qm.running.Load() {
		return
	}

	qm.mu.Lock()
	defer qm.mu.Unlock()

	for _, uq := range qm.users {
		if uq.current != nil || len(uq.pending) == 0 {
			continue
		}
		item := uq.pending[0]
		slot := qm.execSlots[item.Service]
		if slot != nil && !slot.tryAcquire() {
			continue
		}
		uq.pending = uq.pending[1:]
		uq.current = item
		go qm.runJob(item)
	}
}

func (qm *QueueManager) runJob(item *QueueItem) {
	qm.mu.Lock()
	slotRef := qm.execSlots[item.Service]
	qm.mu.Unlock()

	var releaseOnce sync.Once
	releaseSlot := func() {
		releaseOnce.Do(func() {
			if slotRef != nil {
				slotRef.release()
			}
			qm.notify()
		})
	}

	defer func() {
		releaseSlot()
		qm.completeItem(item)
	}()

	execDone := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				loggerQM.Error("execute panicked", "id", item.ID, "error", r)
				select {
				case item.outputCh <- errorMsg(fmt.Sprintf("internal error: %v", r)):
				case <-item.ctx.Done():
				}
			}
			close(item.outputCh)
			close(execDone)
		}()
		item.Execute(item.ctx, item.outputCh)
	}()

	go func() {
		<-execDone
		releaseSlot()
	}()

	bot := getBotFn(item.Network)
	if bot == nil || bot.Client == nil {
		for range item.outputCh {
		}
		return
	}

	ds := qm.getDeliverySlot(item.Network, item.Channel)
	ds.mu.Lock()
	defer ds.mu.Unlock()

	if item.ctx.Err() != nil {
		return
	}

	waitTime := time.Since(item.Enqueued)
	if waitTime > time.Second {
		msg := qm.formatStartedMsg(item.Nick, waitTime)
		bot.Client.Cmd.Message(item.Channel, msg)
	}

	throttle := bot.Network.Throttle
	for line := range item.outputCh {
		if item.ctx.Err() != nil {
			break
		}
		bot.Client.Cmd.Message(item.Channel, "\x02\x02"+line)
		time.Sleep(time.Millisecond * time.Duration(throttle))
	}
}

func (qm *QueueManager) completeItem(item *QueueItem) {
	qm.mu.Lock()
	key := itemKey(item.Network, item.Channel, item.Nick)
	uq, ok := qm.users[key]
	if ok && uq.current == item {
		uq.current = nil
	}
	qm.mu.Unlock()
	qm.notify()
}

func (qm *QueueManager) getDeliverySlot(network, channel string) *deliverySlot {
	key := network + channel
	qm.deliveryMu.Lock()
	defer qm.deliveryMu.Unlock()
	ds, ok := qm.deliverySlot[key]
	if !ok {
		ds = &deliverySlot{}
		qm.deliverySlot[key] = ds
	}
	return ds
}

func (qm *QueueManager) formatStartedMsg(nick string, waitTime time.Duration) string {
	s := qm.startedMsg
	s = strings.ReplaceAll(s, "{nick}", nick)
	s = strings.ReplaceAll(s, "{wait}", waitTime.Round(time.Second).String())
	s = strings.ReplaceAll(s, "{position}", "0")
	s = strings.ReplaceAll(s, "{eta}", "0s")
	return s
}

func (c *Config) QueueMsg(position int, eta time.Duration) string {
	if len(c.QueueMsgs) == 0 {
		return fmt.Sprintf("queued (position %d)", position)
	}
	s := c.QueueMsgs[globalRand.Intn(len(c.QueueMsgs))]
	s = strings.ReplaceAll(s, "{position}", fmt.Sprintf("%d", position))
	s = strings.ReplaceAll(s, "{eta}", eta.Round(time.Second).String())
	return s
}
