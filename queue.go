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
	ID             int64
	Network        string
	Channel        string
	Nick           string
	Service        string
	Description    string
	Enqueued       time.Time
	Execute        func(ctx context.Context, output chan<- string)
	outputCh       chan string
	ctx            context.Context
	cancel         context.CancelFunc
	deliveryWaited bool
}

type ChannelQueue struct {
	pending []*QueueItem
	running map[int64]*QueueItem
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
	mu      sync.Mutex
	cond    *sync.Cond
	queue   []*QueueItem
	current *QueueItem
}

func newDeliverySlot() *deliverySlot {
	ds := &deliverySlot{}
	ds.cond = sync.NewCond(&ds.mu)
	return ds
}

func (ds *deliverySlot) enqueue(item *QueueItem) {
	ds.mu.Lock()
	ds.queue = append(ds.queue, item)
	ds.cond.Broadcast()
	ds.mu.Unlock()
}

func (ds *deliverySlot) waitTurn(item *QueueItem) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	for ds.current != nil || (len(ds.queue) > 0 && ds.queue[0] != item) {
		if len(ds.queue) == 0 {
			return
		}
		item.deliveryWaited = true
		ds.cond.Wait()
	}
	if len(ds.queue) > 0 {
		ds.queue = ds.queue[1:]
	}
	ds.current = item
}

func (ds *deliverySlot) done() {
	ds.mu.Lock()
	ds.current = nil
	ds.cond.Broadcast()
	ds.mu.Unlock()
}

type QueueManager struct {
	mu           sync.Mutex
	channels     map[string]*ChannelQueue
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
		channels:     make(map[string]*ChannelQueue),
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
	for _, cq := range qm.channels {
		for _, item := range cq.running {
			if item.cancel != nil {
				item.cancel()
			}
		}
		for _, item := range cq.pending {
			if item.cancel != nil {
				item.cancel()
			}
		}
		cq.pending = nil
	}
}

func (qm *QueueManager) notify() {
	select {
	case qm.changed <- struct{}{}:
	default:
	}
}

func channelKey(network, channel string) string {
	return network + channel
}

func (qm *QueueManager) getOrCreateChannelQueue(key string) *ChannelQueue {
	cq, ok := qm.channels[key]
	if !ok {
		cq = &ChannelQueue{running: make(map[int64]*QueueItem)}
		qm.channels[key] = cq
	}
	return cq
}

func (qm *QueueManager) Enqueue(network, channel, nick, service, desc string, fn func(ctx context.Context, output chan<- string)) int {
	return qm.EnqueueAt(network, channel, nick, service, desc, time.Now(), fn)
}

func (qm *QueueManager) EnqueueAt(network, channel, nick, service, desc string, enqueuedAt time.Time, fn func(ctx context.Context, output chan<- string)) int {
	if !qm.running.Load() {
		return -1
	}

	qm.mu.Lock()
	defer qm.mu.Unlock()

	key := channelKey(network, channel)
	cq := qm.getOrCreateChannelQueue(key)

	position := len(cq.pending) + len(cq.running)
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
		Enqueued:    enqueuedAt,
		Execute:     fn,
		outputCh:    make(chan string, 200),
		ctx:         ctx,
		cancel:      cancel,
	}

	ds := qm.getDeliverySlot(network, channel)
	ds.enqueue(item)

	slot := qm.execSlots[service]
	if slot != nil && !slot.tryAcquire() {
		cq.pending = append(cq.pending, item)
		qm.notify()
		return position
	}

	cq.running[item.ID] = item
	go qm.runJob(item)
	return 0
}

func (qm *QueueManager) StopCurrent(network, channel string) bool {
	qm.mu.Lock()
	key := channelKey(network, channel)
	cq, ok := qm.channels[key]
	if !ok {
		qm.mu.Unlock()
		return false
	}

	ds := qm.getDeliverySlot(network, channel)
	ds.mu.Lock()

	if ds.current != nil {
		item := ds.current
		ds.mu.Unlock()
		qm.mu.Unlock()
		if item.cancel != nil {
			item.cancel()
		}
		return true
	}

	for _, qi := range ds.queue {
		if _, running := cq.running[qi.ID]; running {
			ds.mu.Unlock()
			qm.mu.Unlock()
			if qi.cancel != nil {
				qi.cancel()
			}
			return true
		}
	}

	ds.mu.Unlock()
	qm.mu.Unlock()
	return false
}

func (qm *QueueManager) CancelPending(network, channel, nick string) bool {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	key := channelKey(network, channel)
	cq, ok := qm.channels[key]
	if !ok {
		return false
	}

	removed := false
	newPending := make([]*QueueItem, 0, len(cq.pending))
	for _, item := range cq.pending {
		if item.Nick == nick {
			if item.cancel != nil {
				item.cancel()
			}
			removed = true
			ds := qm.getDeliverySlot(network, channel)
			ds.remove(item)
		} else {
			newPending = append(newPending, item)
		}
	}
	cq.pending = newPending
	return removed
}

func (qm *QueueManager) IsRunning(network, channel, nick string) bool {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	key := channelKey(network, channel)
	cq, ok := qm.channels[key]
	if !ok {
		return false
	}
	for _, item := range cq.running {
		if item.Nick == nick {
			return true
		}
	}
	return false
}

func (qm *QueueManager) QueueStatus(network, channel, nick string) (current *QueueItem, pending []*QueueItem) {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	key := channelKey(network, channel)
	cq, ok := qm.channels[key]
	if !ok {
		return nil, nil
	}

	for _, item := range cq.running {
		if item.Nick != nick {
			continue
		}
		if current == nil {
			current = item
		} else {
			pending = append(pending, item)
		}
	}
	for _, item := range cq.pending {
		if item.Nick == nick {
			pending = append(pending, item)
		}
	}
	return current, pending
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

	for _, cq := range qm.channels {
		var remaining []*QueueItem
		for _, item := range cq.pending {
			slot := qm.execSlots[item.Service]
			if slot != nil && !slot.tryAcquire() {
				remaining = append(remaining, item)
				continue
			}
			cq.running[item.ID] = item
			go qm.runJob(item)
		}
		cq.pending = remaining
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
		qm.executionComplete(item)
	}()

	for !botReadyFn(item.Network, item.Channel) {
		if item.ctx.Err() != nil {
			for range item.outputCh {
			}
			ds := qm.getDeliverySlot(item.Network, item.Channel)
			ds.remove(item)
			return
		}
		time.Sleep(1 * time.Second)
	}

	bot := getBotFn(item.Network)
	if bot == nil || bot.Client == nil {
		for range item.outputCh {
		}
		ds := qm.getDeliverySlot(item.Network, item.Channel)
		ds.remove(item)
		return
	}

	ds := qm.getDeliverySlot(item.Network, item.Channel)
	ds.waitTurn(item)

	defer ds.done()

	if item.ctx.Err() != nil {
		for range item.outputCh {
		}
		return
	}

	waitTime := time.Since(item.Enqueued)
	if item.deliveryWaited || waitTime > time.Second {
		msg := qm.formatStartedMsg(item.Nick, waitTime)
		bot.Client.Cmd.Message(item.Channel, msg)
	}

	throttle := bot.Network.Throttle
	for line := range item.outputCh {
		if item.ctx.Err() != nil {
			for range item.outputCh {
			}
			break
		}
		bot.Client.Cmd.Message(item.Channel, "\x02\x02"+line)
		time.Sleep(time.Millisecond * time.Duration(throttle))
	}
}

func (qm *QueueManager) executionComplete(item *QueueItem) {
	qm.mu.Lock()
	key := channelKey(item.Network, item.Channel)
	cq, ok := qm.channels[key]
	if ok {
		delete(cq.running, item.ID)
	}
	qm.mu.Unlock()
}

func (qm *QueueManager) getDeliverySlot(network, channel string) *deliverySlot {
	key := network + channel
	qm.deliveryMu.Lock()
	defer qm.deliveryMu.Unlock()
	ds, ok := qm.deliverySlot[key]
	if !ok {
		ds = newDeliverySlot()
		qm.deliverySlot[key] = ds
	}
	return ds
}

func (ds *deliverySlot) remove(item *QueueItem) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	for i, q := range ds.queue {
		if q.ID == item.ID {
			ds.queue = append(ds.queue[:i], ds.queue[i+1:]...)
			ds.cond.Broadcast()
			return
		}
	}
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
