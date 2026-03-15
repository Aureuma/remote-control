use std::time::Duration;

use anyhow::Result;
use base64::Engine;
use chrono::{DateTime, Utc};
use rand::RngCore;
use sha2::{Digest, Sha256};

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct IssuedToken {
    pub value: String,
    pub expires_at: DateTime<Utc>,
}

pub fn new_token() -> Result<String> {
    let mut buf = [0_u8; 32];
    rand::rng().fill_bytes(&mut buf);
    Ok(base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(buf))
}

pub fn new_token_with_ttl(ttl: Duration) -> Result<IssuedToken> {
    let ttl = if ttl.is_zero() {
        Duration::from_secs(3600)
    } else {
        ttl
    };
    Ok(IssuedToken {
        value: new_token()?,
        expires_at: Utc::now() + chrono::Duration::from_std(ttl)?,
    })
}

pub fn is_expired(expires_at: Option<DateTime<Utc>>, now: Option<DateTime<Utc>>) -> bool {
    let Some(expires_at) = expires_at else {
        return false;
    };
    let now = now.unwrap_or_else(Utc::now);
    now >= expires_at
}

pub fn hash_token(token: &str) -> String {
    let hash = Sha256::digest(token.as_bytes());
    let mut out = String::with_capacity(hash.len() * 2);
    for byte in hash {
        out.push_str(&format!("{byte:02x}"));
    }
    out
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use chrono::TimeZone;

    #[test]
    fn new_token_with_ttl_sets_future_expiry() {
        let issued = crate::auth::new_token_with_ttl(Duration::from_secs(120)).unwrap();
        assert!(!issued.value.is_empty());
        assert!(issued.expires_at > chrono::Utc::now());
    }

    #[test]
    fn is_expired_matches_go_behavior() {
        let now = chrono::Utc.with_ymd_and_hms(2026, 2, 28, 12, 0, 0).unwrap();
        assert!(!crate::auth::is_expired(None, Some(now)));
        assert!(!crate::auth::is_expired(
            Some(now + chrono::Duration::seconds(30)),
            Some(now)
        ));
        assert!(crate::auth::is_expired(Some(now), Some(now)));
        assert!(crate::auth::is_expired(
            Some(now - chrono::Duration::seconds(1)),
            Some(now)
        ));
    }
}
