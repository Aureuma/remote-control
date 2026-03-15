use chrono::{DateTime, Utc};

pub fn token_expired(expires_at: Option<DateTime<Utc>>, now: Option<DateTime<Utc>>) -> bool {
    let Some(expires_at) = expires_at else {
        return false;
    };
    let now = now.unwrap_or_else(Utc::now);
    now >= expires_at
}

pub fn is_origin_allowed(origin: &str, host: &str) -> bool {
    let origin = origin.trim();
    if origin.is_empty() {
        return true;
    }
    let Ok(url) = url::Url::parse(origin) else {
        return false;
    };
    let Some(origin_host) = url
        .host_str()
        .map(|value| value.trim().to_ascii_lowercase())
    else {
        return false;
    };
    let request_host = parse_hostname(host).to_ascii_lowercase();
    if !request_host.is_empty() && origin_host == request_host {
        return true;
    }
    matches!(origin_host.as_str(), "localhost" | "127.0.0.1" | "::1")
}

pub fn parse_hostname(host: &str) -> String {
    let host = host.trim();
    if host.is_empty() {
        return String::new();
    }
    url::Url::parse(&format!("http://{host}"))
        .ok()
        .and_then(|url| url.host_str().map(|value| value.to_string()))
        .unwrap_or_else(|| host.to_string())
}

#[cfg(test)]
mod tests {
    use chrono::TimeZone;

    use crate::websocket_support::{is_origin_allowed, parse_hostname, token_expired};

    #[test]
    fn token_expired_matches_go_behavior() {
        let now = chrono::Utc.with_ymd_and_hms(2026, 2, 28, 18, 0, 0).unwrap();
        assert!(!token_expired(None, Some(now)));
        assert!(!token_expired(
            Some(now + chrono::Duration::seconds(30)),
            Some(now)
        ));
        assert!(token_expired(Some(now), Some(now)));
        assert!(token_expired(
            Some(now - chrono::Duration::seconds(1)),
            Some(now)
        ));
    }

    #[test]
    fn is_origin_allowed_matches_go_behavior() {
        let cases = [
            ("", "127.0.0.1:8080", true),
            (
                "https://abc123.trycloudflare.com",
                "abc123.trycloudflare.com",
                true,
            ),
            ("https://127.0.0.1:8080", "127.0.0.1:8080", true),
            ("http://localhost:3000", "other.example.com", true),
            (
                "https://evil.example.com",
                "abc123.trycloudflare.com",
                false,
            ),
            ("://bad", "abc123.trycloudflare.com", false),
        ];
        for (origin, host, want) in cases {
            assert_eq!(is_origin_allowed(origin, host), want);
        }
    }

    #[test]
    fn parse_hostname_matches_go_behavior() {
        assert_eq!(
            parse_hostname("abc123.trycloudflare.com:443"),
            "abc123.trycloudflare.com"
        );
        assert_eq!(parse_hostname("localhost"), "localhost");
    }
}
