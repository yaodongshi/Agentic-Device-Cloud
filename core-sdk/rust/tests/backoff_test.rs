//! Reconnect backoff tests (design/31 3.1.8): the delay ladder is
//! 1/2/4/8/16/30/60 seconds, capped at 60, with +/- jitter applied on top.
//! The ladder is tested as a pure function so no clock injection is needed.

use adc_edge_sdk::client::{backoff_delay, jittered};
use std::time::Duration;

#[test]
fn ladder_matches_lld_sequence() {
    let expected_secs = [1u64, 2, 4, 8, 16, 30, 60];
    for (attempt, secs) in expected_secs.iter().enumerate() {
        assert_eq!(
            backoff_delay(attempt as u32),
            Duration::from_secs(*secs),
            "attempt {attempt} must back off {secs}s"
        );
    }
}

#[test]
fn ladder_caps_at_sixty_seconds() {
    for attempt in 7..=20 {
        assert_eq!(
            backoff_delay(attempt),
            Duration::from_secs(60),
            "attempt {attempt}"
        );
    }
}

#[test]
fn jitter_bounds_plus_minus_twenty_percent() {
    // The client samples a factor uniformly from [-0.2, 0.2]; the jittered
    // delay must stay within +/- 20% of the base.
    let base = Duration::from_secs(16);
    let lower = jittered(base, -0.2);
    let upper = jittered(base, 0.2);
    assert_eq!(lower, Duration::from_secs_f64(12.8));
    assert_eq!(upper, Duration::from_secs_f64(19.2));
    assert_eq!(jittered(base, 0.0), base);
}

#[test]
fn jitter_never_goes_negative() {
    assert_eq!(
        jittered(Duration::from_secs(1), -1.0),
        Duration::from_secs(0)
    );
    assert_eq!(
        jittered(Duration::from_secs(1), -2.0),
        Duration::from_secs(0)
    );
}

#[test]
fn whole_ladder_with_jitter_is_monotonic() {
    // Even with maximum negative jitter the ladder must never shrink
    // (design/31 3.1.8: exponential backoff with jitter).
    let mut previous = Duration::ZERO;
    for attempt in 0..8 {
        let delay = jittered(backoff_delay(attempt), -0.2);
        assert!(
            delay >= previous,
            "attempt {attempt}: {delay:?} < {previous:?}"
        );
        previous = delay;
    }
}
