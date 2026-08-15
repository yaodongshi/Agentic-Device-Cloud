//! Auth tests: the HMAC signature must match the Go reference implementation
//! byte for byte. Known vectors below were generated with the exact signing
//! string of `hmacSign` in `ce/cmd/mockdevice/main.go`
//! (`device_id + "\n" + timestamp + "\n" + nonce`), computed independently
//! with the Python `hmac`/`hashlib` standard library.

use adc_edge_sdk::auth::{generate_nonce, hmac_sign};

#[test]
fn signature_matches_go_hmac_sign_known_vector() {
    // secret = "sdk-test-secret", device_id = "cnc-demo-01",
    // ts = "1755117000", nonce = "0123456789abcdef"
    let signature = hmac_sign(
        "sdk-test-secret",
        "cnc-demo-01",
        "1755117000",
        "0123456789abcdef",
    );
    assert_eq!(
        signature,
        "42beaa3240aa80f4c6bcaf915bd25842b910b0654549d8569d973bf9c0fe85be"
    );
}

#[test]
fn signature_matches_go_hmac_sign_second_vector() {
    // secret = "another-secret", device_id = "dev-2",
    // ts = "1700000000", nonce = "fedcba9876543210"
    let signature = hmac_sign("another-secret", "dev-2", "1700000000", "fedcba9876543210");
    assert_eq!(
        signature,
        "3ddc72d18579950f7e920a00d23253efd9c6a582f7ad68e0cfe644cc965873d4"
    );
}

#[test]
fn signature_is_sensitive_to_every_input() {
    let base = hmac_sign("secret", "device", "1700000000", "0123456789abcdef");
    assert_ne!(
        base,
        hmac_sign("SECRET", "device", "1700000000", "0123456789abcdef")
    );
    assert_ne!(
        base,
        hmac_sign("secret", "DEVICE", "1700000000", "0123456789abcdef")
    );
    assert_ne!(
        base,
        hmac_sign("secret", "device", "1700000001", "0123456789abcdef")
    );
    assert_ne!(
        base,
        hmac_sign("secret", "device", "1700000000", "0123456789abcdee")
    );
}

#[test]
fn signature_is_64_hex_chars() {
    let signature = hmac_sign("s", "d", "1", "2");
    assert_eq!(signature.len(), 64);
    assert!(signature.chars().all(|c| c.is_ascii_hexdigit()));
}

#[test]
fn nonce_matches_go_must_bytes_8_hex_format() {
    // Go mockdevice: hex.EncodeToString(mustBytes(8)) -> 16 lowercase hex chars.
    let nonce = generate_nonce();
    assert_eq!(nonce.len(), 16);
    assert!(nonce
        .chars()
        .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase()));
    assert_eq!(nonce.to_lowercase(), nonce);
}
