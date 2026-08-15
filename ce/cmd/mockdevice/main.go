// Command mockdevice is a minimal device that speaks the ADC wire protocol
// over WSS: it answers tools/list and tools/call (design/33 3.3). Dev only.
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"adc.dev/core-sdk/protocol"

	"github.com/gorilla/websocket"
)

func main() {
	serverURL := flag.String("url", "ws://127.0.0.1:8080/v1/devices/tunnel", "tunnel URL")
	deviceID := flag.String("device", "cnc-demo-01", "device code")
	secret := flag.String("secret", "", "device secret (plain)")
	flag.Parse()
	if *secret == "" {
		log.Fatal("-secret required")
	}

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := hex.EncodeToString(mustBytes(8))
	sig := hmacSign(*secret, *deviceID, ts, nonce)

	headers := http.Header{}
	headers.Set(protocol.HeaderXDeviceID, *deviceID)
	headers.Set(protocol.HeaderXDeviceTimestamp, ts)
	headers.Set(protocol.HeaderXDeviceNonce, nonce)
	headers.Set(protocol.HeaderXDeviceSignature, sig)

	conn, _, err := websocket.DefaultDialer.Dial(*serverURL, headers)
	if err != nil {
		log.Fatalf("connect failed: %v", err)
	}
	defer conn.Close()
	log.Println("tunnel online")

	for {
		var req protocol.JSONRPCRequest
		if err := conn.ReadJSON(&req); err != nil {
			log.Printf("read err: %v", err)
			return
		}
		switch req.Method {
		case protocol.MethodToolsList:
			b, _ := json.Marshal(protocol.ToolsListResult{Tools: []protocol.MCPTool{{
				Name:        "get_spindle_status",
				Description: "Read CNC spindle RPM (read-only)",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}, {
				Name:        "set_spindle_speed",
				Description: "Set CNC spindle RPM (high risk)",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"rpm":{"type":"number"}},"required":["rpm"]}`),
			}}})
			_ = conn.WriteJSON(protocol.JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: b})
		case protocol.MethodToolsCall:
			var params protocol.ToolCallParams
			raw, _ := json.Marshal(req.Params)
			_ = json.Unmarshal(raw, &params)
			text := fmt.Sprintf("ok: %s %v", params.Name, params.Arguments)
			b, _ := json.Marshal(protocol.ToolCallResult{Content: []protocol.ToolContent{{Type: "text", Text: text}}})
			_ = conn.WriteJSON(protocol.JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: b})
		}
	}
}

func hmacSign(secret, deviceID, ts, nonce string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	io.WriteString(mac, deviceID+"\n"+ts+"\n"+nonce)
	return hex.EncodeToString(mac.Sum(nil))
}

func mustBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("rand: %v", err)
	}
	return b
}
