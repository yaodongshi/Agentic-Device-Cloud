// Package clusterbus implements the cross-node message abstraction
// (design/31 I20, ADR-02): a frozen MessageBus interface so the V1.0
// Valkey Pub/Sub transport can be replaced by NATS JetStream in phase 2
// without touching business code. The bus carries request-reply calls
// over adc:req:{node} / adc:resp:{node} channels with HMAC-SHA256 node
// signatures (SEC-06: recipients verify the sender before executing a
// device command).
package clusterbus

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// encodePayload/base64-decodePayload translate between raw bytes and the
// wire form of the envelope.
func encodePayload(payload []byte) string {
	return base64.StdEncoding.EncodeToString(payload)
}

func decodePayload(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(s)
}

// Channel builders shared by the bus and its consumers. The layout is
// pinned in design/31 1.3.7: requests flow to the node owning the device,
// responses flow back to the requesting node.
func RequestChannel(nodeID string) string  { return "adc:req:" + nodeID }
func ResponseChannel(nodeID string) string { return "adc:resp:" + nodeID }

// MessageBus is the frozen cross-node abstraction (design/31 3.2.2).
//
//	Request sends a payload to one topic on one target node and blocks for
//	the response up to timeout. No response means the message is never
//	redelivered: device commands must not be retried automatically
//	(design/31 3.2.5), the caller decides.
//	Publish broadcasts a payload to every subscriber of a topic.
//	Subscribe registers fn for a topic and blocks until ctx is done;
//	all subscribers of the topic receive every published payload.
type MessageBus interface {
	Request(ctx context.Context, targetNode, topic string, payload []byte, timeout time.Duration) ([]byte, error)
	Publish(ctx context.Context, topic string, payload []byte) error
	Subscribe(ctx context.Context, topic string, fn func(payload []byte)) error
}

// HandlerFunc answers one cross-node Request. The returned payload is
// routed back to the requester over adc:resp:{fromNode}; a non-nil error
// is carried as the response error.
type HandlerFunc func(ctx context.Context, payload []byte) ([]byte, error)

// Responder is the request-reply extension implemented by ValkeyBus. The
// frozen MessageBus interface only covers transport; the handler registry
// is what turns a topic into a service (route.ClusterRouter registers the
// tool-call handler on it).
type Responder interface {
	RegisterHandler(ctx context.Context, topic string, fn HandlerFunc) error
}

// Sentinels shared with consumers.
var (
	// ErrTimeout marks a Request whose target node never answered within
	// the timeout (design/31 3.2.8: maps to 504, retryable).
	ErrTimeout = errors.New("clusterbus: request timed out")
	// ErrSignature marks a message that failed the node signature check
	// (SEC-06). Bad messages are dropped, never dispatched.
	ErrSignature = errors.New("clusterbus: invalid node signature")
	// ErrStale marks a message outside the signature freshness window
	// (replay guard, SEC-06).
	ErrStale = errors.New("clusterbus: message timestamp outside freshness window")
)

// sigFields is the canonical signing input: the envelope without its
// signature. JSON marshalling of a struct with a fixed field order is
// deterministic in Go, so sign/verify agree byte-for-byte. Payloads are
// base64 on the wire: MessageBus carries arbitrary bytes, and json.RawMessage
// would reject payloads that are not valid JSON.
type sigFields struct {
	RequestID string `json:"request_id"`
	FromNode  string `json:"from_node"`
	Topic     string `json:"topic"`
	TS        int64  `json:"ts"`
	Payload   string `json:"payload,omitempty"` // base64
	Error     string `json:"error,omitempty"`
}

// message is the wire envelope on adc:req:{node} / adc:resp:{node}.
type message struct {
	RequestID string `json:"request_id"`
	FromNode  string `json:"from_node"`
	Topic     string `json:"topic"`
	TS        int64  `json:"ts"`
	Payload   string `json:"payload,omitempty"` // base64
	Error     string `json:"error,omitempty"`
	Sig       string `json:"sig"`
}

func (m *message) toSigFields() sigFields {
	return sigFields{
		RequestID: m.RequestID,
		FromNode:  m.FromNode,
		Topic:     m.Topic,
		TS:        m.TS,
		Payload:   m.Payload,
		Error:     m.Error,
	}
}

// SignMessage computes the hex HMAC-SHA256 node signature over the
// canonical fields (SEC-06). The node key is shared by every node of the
// cluster and injected via ADC_NODE_KEY (SEC-13, never in code).
func SignMessage(nodeKey []byte, m *message) string {
	mac := hmac.New(sha256.New, nodeKey)
	b, err := json.Marshal(m.toSigFields())
	if err != nil {
		panic(fmt.Sprintf("clusterbus: marshal signature fields: %v", err))
	}
	mac.Write(b)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyMessage checks the HMAC node signature and the freshness window
// (replay guard: a captured envelope cannot be replayed after maxSkew).
func VerifyMessage(nodeKey []byte, m *message, now time.Time, maxSkew time.Duration) error {
	if len(nodeKey) == 0 {
		return errors.New("clusterbus: empty node key cannot verify signatures")
	}
	if !hmac.Equal([]byte(m.Sig), []byte(SignMessage(nodeKey, m))) {
		return ErrSignature
	}
	ts := time.Unix(m.TS, 0)
	delta := now.Sub(ts)
	if delta < 0 {
		delta = -delta
	}
	if delta > maxSkew {
		return ErrStale
	}
	return nil
}

// newRequestID returns a UUIDv4 string from crypto/rand only.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
