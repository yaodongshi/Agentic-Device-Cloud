package clusterbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Backend is the Pub/Sub transport behind ValkeyBus: Valkey today, NATS
// JetStream in phase 2 (ADR-02). Subscribe registers fn and returns
// immediately; the backend keeps delivering until ctx is done. The
// in-memory MemBackend implements the same contract for tests and
// degraded single-process deployments.
type Backend interface {
	Publish(ctx context.Context, channel string, payload []byte) error
	Subscribe(ctx context.Context, channel string, fn func(payload []byte)) error
}

// DefaultMaxSkew bounds message freshness for the replay guard (SEC-06).
const DefaultMaxSkew = 5 * time.Minute

// BusConfig wires a ValkeyBus. NodeKey is the shared cluster node signing
// key (ADC_NODE_KEY, hex encoded at the cmd boundary, SEC-13); a non-nil
// key enables signature verification, which is mandatory outside tests.
type BusConfig struct {
	NodeID         string
	NodeKey        []byte
	Backend        Backend
	MaxSkew        time.Duration // signature freshness window; 0 = DefaultMaxSkew
	Logger         *slog.Logger
	InboundTimeout time.Duration // cap for handler execution; 0 = 60s
}

// ValkeyBus implements MessageBus and Responder over Pub/Sub channels.
//
// Request correlation uses a pending map keyed by requestID (design/31
// 3.2.8): the requester registers a waiter before publishing, and the
// response handler routes adc:resp:{self} messages back to it. Timeouts
// and ctx cancellation both remove the entry, so a silent node cannot
// leak waiters. Incoming requests on adc:req:{self} are signature-checked
// (SEC-06), then dispatched to the topic's registered handler, or to
// broadcast subscribers; a topic without a handler answers an error so
// the requester fails fast instead of waiting out the timeout.
//
// Pub/Sub is fire-and-forget: a request can race the responder's
// subscription and time out; phase 2 persistence (NATS JetStream) removes
// the race, business code stays unchanged (ADR-02).
type ValkeyBus struct {
	cfg      BusConfig
	respChan string
	reqChan  string
	log      *slog.Logger
	now      func() time.Time

	mu       sync.Mutex
	pending  map[string]*pendingEntry // requestID -> waiter
	handlers map[string]HandlerFunc   // topic -> request-reply handler
	subs     map[string][]*subscriber // topic -> broadcast subscribers
	topics   map[string]bool          // topics with a backend subscription

	once       sync.Once
	loopCtx    context.Context
	loopCancel context.CancelFunc
}

// pendingEntry pairs the waiter channel with the node the request went
// to, so responses are only accepted from the node we asked.
type pendingEntry struct {
	ch   chan *message
	node string
}

// NewBus builds the production bus over a go-redis/v9 client.
func NewBus(nodeID string, nodeKey []byte, rdb *redis.Client) *ValkeyBus {
	return NewValkeyBus(BusConfig{NodeID: nodeID, NodeKey: nodeKey, Backend: redisBackend{rdb: rdb}})
}

// NewValkeyBus builds a bus over an arbitrary Backend (tests, or a future
// NATS adapter, ADR-02).
func NewValkeyBus(cfg BusConfig) *ValkeyBus {
	if cfg.MaxSkew <= 0 {
		cfg.MaxSkew = DefaultMaxSkew
	}
	if cfg.InboundTimeout <= 0 {
		cfg.InboundTimeout = 60 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &ValkeyBus{
		cfg:      cfg,
		respChan: ResponseChannel(cfg.NodeID),
		reqChan:  RequestChannel(cfg.NodeID),
		log:      cfg.Logger,
		now:      time.Now,
		pending:  make(map[string]*pendingEntry),
		handlers: make(map[string]HandlerFunc),
		subs:     make(map[string][]*subscriber),
		topics:   make(map[string]bool),
	}
}

// Close stops the internal loops; pending Request waiters observe
// ctx cancellation (their ctx outlives the loops in normal wiring).
func (b *ValkeyBus) Close() {
	if b.loopCancel != nil {
		b.loopCancel()
	}
}

// ensureLoops registers the two internal subscriptions synchronously: once
// it returns, this node receives requests and responses, so callers can
// rely on readiness without goroutine-scheduling races.
func (b *ValkeyBus) ensureLoops() {
	b.once.Do(func() {
		b.loopCtx, b.loopCancel = context.WithCancel(context.Background())
		if err := b.cfg.Backend.Subscribe(b.loopCtx, b.reqChan, b.handleInbound); err != nil {
			b.log.Warn("clusterbus: subscribe requests failed", "channel", b.reqChan, "err", err)
		}
		if err := b.cfg.Backend.Subscribe(b.loopCtx, b.respChan, b.handleResponse); err != nil {
			b.log.Warn("clusterbus: subscribe responses failed", "channel", b.respChan, "err", err)
		}
	})
}

// Request sends payload to targetNode's topic and waits up to timeout for
// the signed response (SEC-06: the responder's envelope must verify and
// carry the requested node's id).
func (b *ValkeyBus) Request(ctx context.Context, targetNode, topic string, payload []byte, timeout time.Duration) ([]byte, error) {
	b.ensureLoops()
	if targetNode == "" {
		return nil, errors.New("clusterbus: empty target node")
	}
	env := &message{
		RequestID: newRequestID(),
		FromNode:  b.cfg.NodeID,
		Topic:     topic,
		TS:        b.now().Unix(),
		Payload:   encodePayload(payload),
	}
	env.Sig = SignMessage(b.cfg.NodeKey, env)
	body, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("clusterbus: marshal request: %w", err)
	}

	entry := &pendingEntry{ch: make(chan *message, 1), node: targetNode}
	b.mu.Lock()
	b.pending[env.RequestID] = entry
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, env.RequestID)
		b.mu.Unlock()
	}()

	if err := b.cfg.Backend.Publish(ctx, RequestChannel(targetNode), body); err != nil {
		return nil, fmt.Errorf("clusterbus: publish request: %w", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, ErrTimeout
	case resp := <-entry.ch:
		if resp.Error != "" {
			return nil, errors.New(resp.Error)
		}
		out, err := decodePayload(resp.Payload)
		if err != nil {
			return nil, fmt.Errorf("clusterbus: decode response payload: %w", err)
		}
		return out, nil
	}
}

// Publish broadcasts payload to every subscriber of topic.
func (b *ValkeyBus) Publish(ctx context.Context, topic string, payload []byte) error {
	if topic == "" {
		return errors.New("clusterbus: empty publish topic")
	}
	if err := b.cfg.Backend.Publish(ctx, topic, payload); err != nil {
		return fmt.Errorf("clusterbus: publish %s: %w", topic, err)
	}
	return nil
}

// subscriber is a pointer-identity wrapper around a broadcast callback:
// Go funcs are not comparable, so removal from the subscriber set goes by
// the wrapper pointer.
type subscriber struct {
	fn func(payload []byte)
}

// Subscribe registers fn for topic and blocks until ctx is done, then
// removes it. One backend subscription is shared by all subscribers of a
// topic; every published payload reaches every subscriber.
func (b *ValkeyBus) Subscribe(ctx context.Context, topic string, fn func(payload []byte)) error {
	if fn == nil {
		return errors.New("clusterbus: nil subscribe handler")
	}
	sub := &subscriber{fn: fn}
	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], sub)
	started := b.topics[topic]
	b.topics[topic] = true
	b.mu.Unlock()
	if !started {
		if err := b.cfg.Backend.Subscribe(ctx, topic, b.dispatch(topic)); err != nil {
			return fmt.Errorf("clusterbus: subscribe %s: %w", topic, err)
		}
	}
	<-ctx.Done()
	b.mu.Lock()
	subs := b.subs[topic]
	for i, s := range subs {
		if s == sub {
			b.subs[topic] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	b.mu.Unlock()
	return nil
}

// SubscriberCount returns the number of registered broadcast subscribers
// for a topic (test helper: Publish must not race registration).
func (b *ValkeyBus) SubscriberCount(topic string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs[topic])
}

// RegisterHandler registers the request-reply handler for a topic and
// blocks until ctx is done, then removes it. It implements Responder.
func (b *ValkeyBus) RegisterHandler(ctx context.Context, topic string, fn HandlerFunc) error {
	if fn == nil {
		return errors.New("clusterbus: nil request handler")
	}
	b.ensureLoops()
	b.mu.Lock()
	b.handlers[topic] = fn
	b.mu.Unlock()
	<-ctx.Done()
	b.mu.Lock()
	delete(b.handlers, topic)
	b.mu.Unlock()
	return nil
}

// dispatch returns the per-topic broadcast callback reading the current
// subscriber set on every message.
func (b *ValkeyBus) dispatch(topic string) func(payload []byte) {
	return func(payload []byte) {
		b.mu.Lock()
		subs := append([]*subscriber(nil), b.subs[topic]...)
		b.mu.Unlock()
		for _, s := range subs {
			s.fn(payload)
		}
	}
}

// handleResponse routes an adc:resp:{self} message to the pending waiter
// of its requestID. Messages with an invalid signature or from an
// unexpected node are dropped silently (SEC-06).
func (b *ValkeyBus) handleResponse(payload []byte) {
	var env message
	if err := json.Unmarshal(payload, &env); err != nil {
		b.log.Warn("clusterbus: drop malformed response", "err", err)
		return
	}
	if err := VerifyMessage(b.cfg.NodeKey, &env, b.now(), b.cfg.MaxSkew); err != nil {
		b.log.Warn("clusterbus: drop response with bad signature", "from", env.FromNode, "err", err)
		return
	}
	b.mu.Lock()
	entry := b.pending[env.RequestID]
	b.mu.Unlock()
	if entry == nil || entry.node != env.FromNode {
		return // no waiter here, or answered by someone else
	}
	select {
	case entry.ch <- &env:
	default:
	}
}

// handleInbound verifies an adc:req:{self} request and dispatches it:
// the registered handler answers over adc:resp:{fromNode}, broadcast
// subscribers receive fire-and-forget payloads (the requester times out),
// and an unhandled topic answers an error so the requester fails fast.
func (b *ValkeyBus) handleInbound(payload []byte) {
	var env message
	if err := json.Unmarshal(payload, &env); err != nil {
		b.log.Warn("clusterbus: drop malformed request", "err", err)
		return
	}
	if err := VerifyMessage(b.cfg.NodeKey, &env, b.now(), b.cfg.MaxSkew); err != nil {
		b.log.Warn("clusterbus: drop request with bad signature", "from", env.FromNode, "err", err)
		return
	}
	b.mu.Lock()
	handler := b.handlers[env.Topic]
	subs := append([]*subscriber(nil), b.subs[env.Topic]...)
	b.mu.Unlock()
	payload, err := decodePayload(env.Payload)
	if err != nil {
		b.log.Warn("clusterbus: drop request with bad payload encoding", "from", env.FromNode, "err", err)
		return
	}
	switch {
	case handler != nil:
		go b.runHandler(env, handler, payload)
	case len(subs) > 0:
		for _, s := range subs {
			s.fn(payload)
		}
	default:
		b.replyError(env, "no handler registered for topic "+env.Topic)
	}
}

// runHandler executes the topic handler under a bounded inbound timeout
// and always answers the requester, success or failure.
func (b *ValkeyBus) runHandler(env message, handler HandlerFunc, payload []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), b.cfg.InboundTimeout)
	defer cancel()
	out, err := handler(ctx, payload)
	reply := message{
		RequestID: env.RequestID,
		FromNode:  b.cfg.NodeID,
		Topic:     env.Topic,
		TS:        b.now().Unix(),
	}
	if err != nil {
		reply.Error = err.Error()
	} else {
		reply.Payload = encodePayload(out)
	}
	reply.Sig = SignMessage(b.cfg.NodeKey, &reply)
	body, merr := json.Marshal(&reply)
	if merr != nil {
		b.log.Warn("clusterbus: marshal response failed", "request_id", env.RequestID, "err", merr)
		return
	}
	actx, acancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer acancel()
	if perr := b.cfg.Backend.Publish(actx, ResponseChannel(env.FromNode), body); perr != nil {
		b.log.Warn("clusterbus: publish response failed", "request_id", env.RequestID, "err", perr)
	}
}

// replyError answers a request with an error payload (fail fast instead of
// letting the requester time out).
func (b *ValkeyBus) replyError(env message, msg string) {
	reply := message{
		RequestID: env.RequestID,
		FromNode:  b.cfg.NodeID,
		Topic:     env.Topic,
		TS:        b.now().Unix(),
		Error:     msg,
	}
	reply.Sig = SignMessage(b.cfg.NodeKey, &reply)
	body, err := json.Marshal(&reply)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = b.cfg.Backend.Publish(ctx, ResponseChannel(env.FromNode), body)
}

// redisBackend adapts go-redis/v9 to Backend. Subscribe spawns a delivery
// goroutine and returns immediately (the Backend contract): the pubsub
// close on ctx done tears the channel down.
type redisBackend struct {
	rdb *redis.Client
}

func (r redisBackend) Publish(ctx context.Context, channel string, payload []byte) error {
	return r.rdb.Publish(ctx, channel, string(payload)).Err()
}

func (r redisBackend) Subscribe(ctx context.Context, channel string, fn func(payload []byte)) error {
	pubsub := r.rdb.Subscribe(ctx, channel)
	ch := pubsub.Channel()
	go func() {
		defer pubsub.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				fn([]byte(msg.Payload))
			}
		}
	}()
	return nil
}
