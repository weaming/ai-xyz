use std::{
    fs,
    io::{self, Write},
    path::{Path, PathBuf},
    sync::Mutex,
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use anyhow::{Context, Result, bail};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use chrono::Utc;
use reqwest::blocking::Client;
use reqwest::header::{AUTHORIZATION, HeaderMap, HeaderValue};
use serde::{Deserialize, Serialize};
use serde_json::Value;

const CLIENT_ID: &str = "app_EMoamEEZ73f0CkXaXp7hrann";
const DEFAULT_REFRESH_URL: &str = "https://auth.openai.com/oauth/token";
const REFRESH_WINDOW: Duration = Duration::from_secs(300);

pub struct AuthConfig {
    pub auth_file: PathBuf,
    pub client: Client,
}

pub struct AuthState {
    auth_file: PathBuf,
    refresh_url: String,
    client: Client,
    state: Mutex<StoredAuth>,
}

#[derive(Debug, Deserialize, Serialize)]
struct StoredTokens {
    id_token: String,
    access_token: String,
    refresh_token: String,
    #[serde(default)]
    account_id: Option<String>,
}

struct StoredAuth {
    raw: Value,
    tokens: StoredTokens,
    account_id: Option<String>,
    is_fedramp: bool,
}

#[derive(Debug, Deserialize)]
struct IdTokenClaims {
    #[serde(rename = "https://api.openai.com/auth")]
    auth: Option<AuthClaims>,
    exp: Option<u64>,
}

#[derive(Debug, Deserialize)]
struct AuthClaims {
    #[serde(default)]
    chatgpt_account_id: Option<String>,
    #[serde(default)]
    chatgpt_account_is_fedramp: bool,
}

#[derive(Debug, Deserialize)]
struct RefreshResponse {
    id_token: Option<String>,
    access_token: Option<String>,
    refresh_token: Option<String>,
}

impl AuthState {
    pub fn load(config: AuthConfig) -> Result<Self> {
        let content = fs::read_to_string(&config.auth_file)
            .with_context(|| format!("读取 Codex 认证文件失败: {}", config.auth_file.display()))?;
        let raw: Value = serde_json::from_str(&content)
            .with_context(|| format!("认证文件不是有效 JSON: {}", config.auth_file.display()))?;
        if !raw.is_object() {
            bail!("认证文件顶层必须是 JSON 对象");
        }

        let has_api_key = raw
            .get("OPENAI_API_KEY")
            .is_some_and(|value| !value.is_null());
        let is_api_key_mode = raw
            .get("auth_mode")
            .and_then(Value::as_str)
            .is_some_and(|mode| {
                mode.eq_ignore_ascii_case("api_key") || mode.eq_ignore_ascii_case("apikey")
            });
        if has_api_key || is_api_key_mode {
            bail!("认证文件是 API key 模式；此代理只支持 ChatGPT 登录 tokens");
        }

        let tokens_value = raw
            .get("tokens")
            .context("认证文件缺少 tokens，当前只支持 ChatGPT 网页登录")?;
        let tokens: StoredTokens = serde_json::from_value(tokens_value.clone())
            .context("认证文件中的 tokens 字段格式不完整")?;
        let (account_id, is_fedramp) = parse_account_claims(&tokens.id_token)?;
        let account_id = tokens.account_id.clone().or(account_id);
        if account_id.is_none() {
            bail!("认证文件中的 token 缺少 account_id");
        }

        let refresh_url = std::env::var("CODEX_REFRESH_TOKEN_URL_OVERRIDE")
            .unwrap_or_else(|_| DEFAULT_REFRESH_URL.to_owned());

        Ok(Self {
            auth_file: config.auth_file,
            refresh_url,
            client: config.client,
            state: Mutex::new(StoredAuth {
                raw,
                tokens,
                account_id,
                is_fedramp,
            }),
        })
    }

    /// 返回当前登录对应的 Responses API 认证头，必要时同步刷新并保存 token。
    pub fn auth_headers(&self) -> Result<HeaderMap> {
        let mut state = self
            .state
            .lock()
            .map_err(|_| anyhow::anyhow!("认证状态锁已损坏"))?;

        if should_refresh(&state.tokens.access_token) {
            self.refresh(&mut state)?;
        }

        let account_id = state
            .account_id
            .as_deref()
            .context("认证文件中的 token 缺少 account_id")?;
        let mut headers = HeaderMap::new();
        let mut authorization =
            HeaderValue::from_str(&format!("Bearer {}", state.tokens.access_token))
                .context("access_token 包含无效字符")?;
        authorization.set_sensitive(true);
        headers.insert(AUTHORIZATION, authorization);
        headers.insert(
            "ChatGPT-Account-ID",
            HeaderValue::from_str(account_id).context("account_id 包含无效字符")?,
        );
        if state.is_fedramp {
            headers.insert("X-OpenAI-Fedramp", HeaderValue::from_static("true"));
        }
        Ok(headers)
    }

    fn refresh(&self, state: &mut StoredAuth) -> Result<()> {
        let response = self
            .client
            .post(&self.refresh_url)
            .json(&serde_json::json!({
                "client_id": CLIENT_ID,
                "grant_type": "refresh_token",
                "refresh_token": state.tokens.refresh_token,
            }))
            .send()
            .context("请求 ChatGPT token 刷新失败")?;

        let status = response.status();
        if !status.is_success() {
            bail!("ChatGPT token 刷新返回 HTTP {status}");
        }

        let refreshed: RefreshResponse = response.json().context("解析 token 刷新响应失败")?;
        state.tokens.access_token = refreshed
            .access_token
            .context("token 刷新响应缺少 access_token")?;
        if let Some(id_token) = refreshed.id_token {
            state.tokens.id_token = id_token;
        }
        if let Some(refresh_token) = refreshed.refresh_token {
            state.tokens.refresh_token = refresh_token;
        }

        let (account_id, is_fedramp) = parse_account_claims(&state.tokens.id_token)?;
        state.account_id = state.tokens.account_id.clone().or(account_id);
        state.is_fedramp = is_fedramp;
        state.raw["tokens"] = serde_json::to_value(&state.tokens)?;
        state.raw["last_refresh"] = Value::String(Utc::now().to_rfc3339());
        persist_auth_file(&self.auth_file, &state.raw).context("写回刷新后的认证文件失败")
    }
}

fn parse_account_claims(jwt: &str) -> Result<(Option<String>, bool)> {
    let mut parts = jwt.split('.');
    let (Some(header), Some(payload), Some(signature)) = (parts.next(), parts.next(), parts.next())
    else {
        bail!("id_token 不是有效 JWT");
    };
    if header.is_empty() || payload.is_empty() || signature.is_empty() || parts.next().is_some() {
        bail!("id_token 不是有效 JWT");
    }
    let bytes = URL_SAFE_NO_PAD
        .decode(payload)
        .context("id_token payload 不是有效 Base64URL")?;
    let claims: IdTokenClaims =
        serde_json::from_slice(&bytes).context("解析 id_token claims 失败")?;
    let auth = claims.auth.unwrap_or(AuthClaims {
        chatgpt_account_id: None,
        chatgpt_account_is_fedramp: false,
    });
    Ok((auth.chatgpt_account_id, auth.chatgpt_account_is_fedramp))
}

fn should_refresh(access_token: &str) -> bool {
    let Some(exp) = jwt_expiration(access_token) else {
        return false;
    };
    let threshold = SystemTime::now().checked_add(REFRESH_WINDOW);
    threshold.is_some_and(|time| exp <= time)
}

fn jwt_expiration(jwt: &str) -> Option<SystemTime> {
    let mut parts = jwt.split('.');
    let (Some(header), Some(payload), Some(signature)) = (parts.next(), parts.next(), parts.next())
    else {
        return None;
    };
    if header.is_empty() || payload.is_empty() || signature.is_empty() || parts.next().is_some() {
        return None;
    }
    let bytes = URL_SAFE_NO_PAD.decode(payload).ok()?;
    let claims: IdTokenClaims = serde_json::from_slice(&bytes).ok()?;
    Some(UNIX_EPOCH + Duration::from_secs(claims.exp?))
}

fn persist_auth_file(path: &Path, value: &Value) -> io::Result<()> {
    let parent = path.parent().unwrap_or_else(|| Path::new("."));
    fs::create_dir_all(parent)?;
    let file_name = path
        .file_name()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "认证文件缺少文件名"))?
        .to_string_lossy();
    let temp_path = parent.join(format!(".{file_name}.{}.tmp", std::process::id()));
    let content = serde_json::to_vec_pretty(value).map_err(io::Error::other)?;

    let result = (|| {
        let mut file = fs::OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&temp_path)?;
        set_private_permissions(&file)?;
        file.write_all(&content)?;
        file.sync_all()?;
        drop(file);
        fs::rename(&temp_path, path)
    })();

    if result.is_err() {
        let _ = fs::remove_file(&temp_path);
    }
    result
}

fn set_private_permissions(file: &fs::File) -> io::Result<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        file.set_permissions(fs::Permissions::from_mode(0o600))?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use base64::Engine;
    use reqwest::blocking::Client;
    use serde_json::json;

    use super::{AuthConfig, AuthState};

    #[test]
    fn loads_chatgpt_auth_headers_from_auth_file() {
        let temp_dir = tempfile::tempdir().expect("创建临时目录");
        let auth_file = temp_dir.path().join("auth.json");
        let id_token = fake_jwt(json!({
            "https://api.openai.com/auth": {
                "chatgpt_account_id": "acct_test",
                "chatgpt_account_is_fedramp": true
            }
        }));
        let auth = json!({
            "auth_mode": "chatgpt",
            "OPENAI_API_KEY": null,
            "tokens": {
                "id_token": id_token,
                "access_token": "access-test",
                "refresh_token": "refresh-test",
                "account_id": "acct_test"
            }
        });
        std::fs::write(
            &auth_file,
            serde_json::to_vec(&auth).expect("序列化认证文件"),
        )
        .expect("写入认证文件");

        let state = AuthState::load(AuthConfig {
            auth_file,
            client: Client::new(),
        })
        .expect("加载认证文件");
        let headers = state.auth_headers().expect("构造认证头");

        assert_eq!(headers["authorization"], "Bearer access-test");
        assert_eq!(headers["ChatGPT-Account-ID"], "acct_test");
        assert_eq!(headers["X-OpenAI-Fedramp"], "true");
    }

    fn fake_jwt(payload: serde_json::Value) -> String {
        let encode = |value: &[u8]| base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(value);
        format!(
            "{}.{}.sig",
            encode(br#"{"alg":"none","typ":"JWT"}"#),
            encode(
                serde_json::to_string(&payload)
                    .expect("序列化 claims")
                    .as_bytes()
            )
        )
    }
}
