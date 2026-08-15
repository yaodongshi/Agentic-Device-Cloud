// adc_edge.h — ADC edge SDK C core interface contract (E-04, V1.0: interface
// definition + demo-level implementation; full ESP32 port lands in V1.5).
//
// The C core is the minimal runtime for MCU-class devices (ESP32/STM32).
// Wire contract: core-sdk/protocol (Go) — json tags are authoritative.
// License: Apache-2.0.

#ifndef ADC_EDGE_H
#define ADC_EDGE_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define ADC_PROTOCOL_VERSION "1.0"
#define ADC_ERR_OK 0
#define ADC_ERR_AUTH (-1)      // handshake rejected (bad signature/expired/replay)
#define ADC_ERR_CONNECT (-2)   // network / TLS failure
#define ADC_ERR_PROTOCOL (-3)  // version negotiation failed (-32001)
#define ADC_ERR_REGISTER (-4)  // tools/list rejected by server

// adc_tool_fn executes one tool call; args is the JSON arguments object,
// out receives the JSON result text. Return 0 on success.
typedef int (*adc_tool_fn)(const char *args, char *out, size_t out_cap);

typedef struct adc_tool {
    const char *name;
    const char *description;
    const char *input_schema;  // JSON Schema text
    int risk_level;            // 0-3, server DB is authoritative (SEC-09)
    adc_tool_fn fn;
} adc_tool;

typedef struct adc_device adc_device;

// adc_device_init allocates a device handle (secret is the plaintext signing
// key, injected at provisioning time, never persisted in clear by the app).
adc_device *adc_device_init(const char *device_code, const char *secret);

// adc_device_register_tools installs the tool table before connect.
int adc_device_register_tools(adc_device *d, const adc_tool *tools, size_t n);

// adc_device_connect establishes the WSS tunnel (TLS required), performs the
// HMAC handshake and version negotiation, then registers tools/list.
// Blocks until registered or fails. Non-blocking run loop follows.
int adc_device_connect(adc_device *d, const char *tunnel_url);

// adc_device_pump services the connection: answers tools/list/tools/call
// frames, keeps the heartbeat alive, handles kick frames and reconnects with
// exponential backoff (1/2/4/8/16/30/60s + jitter per LLD 3.1.8).
// Returns ADC_ERR_CONNECT only when the device is asked to stop.
int adc_device_pump(adc_device *d, int (*should_stop)(void *), void *ctx);

void adc_device_free(adc_device *d);

#ifdef __cplusplus
}
#endif

#endif // ADC_EDGE_H
