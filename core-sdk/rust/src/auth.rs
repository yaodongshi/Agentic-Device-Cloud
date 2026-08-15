//! Device authentication: HMAC-SHA256 signature and nonce generation for the
//! WSS upgrade headers (SEC-03).
//!
//! The signature string format matches the Go reference implementation
//! (`mockdevice` `hmacSign`): `device_id + "\n" + timestamp + "\n" + nonce`,
//! hex-encoded HMAC-SHA256 keyed with the device secret.

use hmac::{Hmac, Mac};
use rand::RngCore;
use sha2::Sha256;

type HmacSha256 = Hmac<Sha256>;

/// Computes the device handshake signature:
/// `hex(HMAC-SHA256(secret, device_id + "\n" + timestamp + "\n" + nonce))`.
///
/// `timestamp` is unix seconds as a decimal string; this is the exact signing
/// string used by `hmacSign` in `ce/cmd/mockdevice/main.go`.
pub fn hmac_sign(secret: &str, device_id: &str, timestamp: &str, nonce: &str) -> String {
    let mut mac =
        HmacSha256::new_from_slice(secret.as_bytes()).expect("HMAC accepts keys of any length");
    mac.update(device_id.as_bytes());
    mac.update(b"\n");
    mac.update(timestamp.as_bytes());
    mac.update(b"\n");
    mac.update(nonce.as_bytes());
    hex::encode(mac.finalize().into_bytes())
}

/// Generates a one-time nonce: 8 cryptographically random bytes as hex
/// (16 hex characters), matching the Go `mustBytes(8)` helper.
pub fn generate_nonce() -> String {
    let mut buf = [0u8; 8];
    rand::rng().fill_bytes(&mut buf);
    hex::encode(buf)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nonce_is_sixteen_hex_chars() {
        let nonce = generate_nonce();
        assert_eq!(nonce.len(), 16);
        assert!(nonce.chars().all(|c| c.is_ascii_hexdigit()));
    }

    #[test]
    fn nonces_differ_across_calls() {
        let a = generate_nonce();
        let b = generate_nonce();
        assert_ne!(a, b);
    }
}
