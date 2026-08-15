// Callback signature helpers (SEC-01/SEC-13, design/33 1.4). The approval
// card URL carries sig = hex(HMAC-SHA256(callback_secret,
// ticket_id + "." + decision + "." + expire)), where expire is the ticket
// expiry as Unix seconds. callback_secret is the ticket-level one-shot key
// generated at create time (SEC-13); it never reaches PostgreSQL, which
// persists only its SHA-256 (SecretHash).

package approval

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

// SignCallback builds the card URL signature for a decision (design/33 1.4):
// HMAC-SHA256 over ticket_id + "." + decision + "." + expire(Unix seconds),
// hex encoded. The same inputs must reproduce the same signature, which is
// what makes the later verification deterministic.
func SignCallback(secret, ticketID, decision string, expire time.Time) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(ticketID))
	_, _ = mac.Write([]byte{'.'})
	_, _ = mac.Write([]byte(decision))
	_, _ = mac.Write([]byte{'.'})
	_, _ = mac.Write([]byte(strconv.FormatInt(expire.Unix(), 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyCallbackSignature reports whether sig is the exact signature of the
// given inputs under secret. Comparison is constant-time (hmac.Equal) to
// avoid timing side channels, and decisions other than approve/reject are
// rejected outright (design/33 3.4.1: error code 10001).
func VerifyCallbackSignature(secret, ticketID, decision, sig string, expire time.Time) bool {
	if decision != DecisionApprove && decision != DecisionReject {
		return false
	}
	expected := SignCallback(secret, ticketID, decision, expire)
	return hmac.Equal([]byte(expected), []byte(sig))
}

// CallbackWindowValid reports whether now falls inside the validity window
// the signature commits to. The expire component is the ticket expiry as
// Unix seconds, so a callback is only acceptable while now < expire. This is
// the time-window half of SEC-01/SEC-13: an expired ticket rejects approval
// here, before the state machine is even consulted (SEC-11/HITL-009).
func CallbackWindowValid(expire, now time.Time) bool {
	return now.Before(expire)
}
