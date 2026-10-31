use anyhow::{Context, Result};
use chrono::{DateTime, SecondsFormat, Utc};
use reqwest::blocking::Client;
use reqwest::header::HeaderMap;
use serde::{Deserialize, Serialize};
use url::Url;

use crate::auth::AuthState;

#[derive(Debug, Deserialize)]
struct UsagePayload {
    #[serde(default)]
    plan_type: Option<String>,
    #[serde(default)]
    rate_limit: Option<RateLimitDetails>,
    #[serde(default)]
    rate_limit_reset_credits: Option<ResetCreditsSummary>,
}

#[derive(Debug, Deserialize)]
struct RateLimitDetails {
    #[serde(default)]
    allowed: Option<bool>,
    #[serde(default)]
    primary_window: Option<RateLimitWindow>,
    #[serde(default)]
    secondary_window: Option<RateLimitWindow>,
}

#[derive(Debug, Deserialize)]
struct RateLimitWindow {
    used_percent: i32,
    limit_window_seconds: i32,
    reset_at: i64,
}

#[derive(Debug, Deserialize, Serialize, Clone)]
pub(crate) struct ResetCreditsSummary {
    available_count: i64,
    #[serde(skip_serializing_if = "Option::is_none")]
    credits: Option<Vec<ResetCredit>>,
}

#[derive(Debug, Deserialize, Serialize, Clone)]
pub(crate) struct ResetCredit {
    id: String,
    reset_type: String,
    status: String,
    granted_at: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    expires_at: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    title: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    description: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct UsageResponse {
    pub fetched_at: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub plan_type: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ordinary_usage_allowed: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub five_hour: Option<QuotaWindow>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub weekly: Option<QuotaWindow>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reset_credits: Option<ResetCreditsSummary>,
}

#[derive(Debug, Serialize, PartialEq)]
pub struct QuotaWindow {
    pub used_percent: i32,
    pub remaining_percent: i32,
    pub window_duration_minutes: i32,
    pub resets_at: i64,
    pub resets_at_utc: Option<String>,
}

pub fn fetch_usage(
    client: &Client,
    auth: &AuthState,
    usage_url: &Url,
    reset_credits_url: &Url,
) -> Result<UsageResponse> {
    let headers = auth.auth_headers()?;
    let payload: UsagePayload =
        get_json(client, usage_url, &headers).context("请求 ChatGPT 用量接口失败")?;
    let reset_credits = fetch_reset_credits(client, reset_credits_url, &headers)
        .or(payload.rate_limit_reset_credits);
    let rate_limit = payload.rate_limit;

    Ok(UsageResponse {
        fetched_at: Utc::now().to_rfc3339_opts(SecondsFormat::Secs, true),
        plan_type: payload.plan_type,
        ordinary_usage_allowed: rate_limit.as_ref().and_then(|limit| limit.allowed),
        five_hour: rate_limit
            .as_ref()
            .and_then(|limit| limit.primary_window.as_ref())
            .map(quota_window),
        weekly: rate_limit
            .as_ref()
            .and_then(|limit| limit.secondary_window.as_ref())
            .map(quota_window),
        reset_credits,
    })
}

fn fetch_reset_credits(
    client: &Client,
    url: &Url,
    headers: &HeaderMap,
) -> Option<ResetCreditsSummary> {
    match get_json(client, url, headers) {
        Ok(details) => Some(details),
        Err(error) => {
            eprintln!("读取 Codex reset 明细失败，将使用用量接口摘要: {error:#}");
            None
        }
    }
}

fn get_json<T>(client: &Client, url: &Url, headers: &HeaderMap) -> Result<T>
where
    T: for<'de> Deserialize<'de>,
{
    let response = client
        .get(url.clone())
        .headers(headers.clone())
        .send()
        .with_context(|| format!("请求 {url} 失败"))?
        .error_for_status()
        .with_context(|| format!("请求 {url} 返回错误状态"))?;
    response
        .json()
        .with_context(|| format!("解析 {url} JSON 响应失败"))
}

fn quota_window(window: &RateLimitWindow) -> QuotaWindow {
    let used_percent = window.used_percent.clamp(0, 100);
    let window_duration_minutes = if window.limit_window_seconds > 0 {
        (window.limit_window_seconds + 59) / 60
    } else {
        0
    };

    QuotaWindow {
        used_percent,
        remaining_percent: 100 - used_percent,
        window_duration_minutes,
        resets_at: window.reset_at,
        resets_at_utc: format_timestamp(window.reset_at),
    }
}

fn format_timestamp(timestamp: i64) -> Option<String> {
    DateTime::<Utc>::from_timestamp(timestamp, 0)
        .map(|value| value.to_rfc3339_opts(SecondsFormat::Secs, true))
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::{QuotaWindow, RateLimitWindow, UsagePayload, quota_window};

    #[test]
    fn quota_window_reports_remaining_percent_and_reset_time() {
        assert_eq!(
            quota_window(&RateLimitWindow {
                used_percent: 15,
                limit_window_seconds: 18_000,
                reset_at: 1_791_763_207,
            }),
            QuotaWindow {
                used_percent: 15,
                remaining_percent: 85,
                window_duration_minutes: 300,
                resets_at: 1_791_763_207,
                resets_at_utc: Some("2026-10-12T00:00:07Z".to_owned()),
            }
        );
    }

    #[test]
    fn usage_payload_accepts_backend_snake_case_fields() {
        let payload: UsagePayload = serde_json::from_value(json!({
            "plan_type": "plus",
            "rate_limit": {
                "allowed": true,
                "primary_window": {
                    "used_percent": 15,
                    "limit_window_seconds": 18000,
                    "reset_at": 1791763207
                },
                "secondary_window": {
                    "used_percent": 3,
                    "limit_window_seconds": 604800,
                    "reset_at": 1792295953
                }
            },
            "rate_limit_reset_credits": {"available_count": 3}
        }))
        .expect("用量响应应可解析");

        assert_eq!(payload.plan_type.as_deref(), Some("plus"));
        assert_eq!(
            payload.rate_limit.as_ref().and_then(|value| value.allowed),
            Some(true)
        );
        assert_eq!(
            payload
                .rate_limit
                .as_ref()
                .and_then(|value| value.primary_window.as_ref())
                .map(quota_window),
            Some(QuotaWindow {
                used_percent: 15,
                remaining_percent: 85,
                window_duration_minutes: 300,
                resets_at: 1791763207,
                resets_at_utc: Some("2026-10-12T00:00:07Z".to_owned()),
            })
        );
    }

    #[test]
    fn invalid_window_duration_is_reported_as_zero() {
        assert_eq!(
            quota_window(&RateLimitWindow {
                used_percent: 120,
                limit_window_seconds: -1,
                reset_at: 0,
            }),
            QuotaWindow {
                used_percent: 100,
                remaining_percent: 0,
                window_duration_minutes: 0,
                resets_at: 0,
                resets_at_utc: Some("1970-01-01T00:00:00Z".to_owned()),
            }
        );
    }
}
