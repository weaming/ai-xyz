use anyhow::{Context, Result};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use reqwest::{Client, Proxy};
use rmcp::{
    ErrorData as McpError,
    model::{CallToolResponse, CallToolResult, JsonObject, Tool},
};
use serde_json::{Value, json};
use std::{
    borrow::Cow,
    fs,
    path::PathBuf,
    sync::{Arc, OnceLock},
    time::{Duration, SystemTime, UNIX_EPOCH},
};

pub const NAME: &str = "web_search";
const OPEN_NAME: &str = "web_open";
const FIND_NAME: &str = "web_find";
const CLICK_NAME: &str = "web_click";
const DEFAULT_ENDPOINT: &str = "https://chatgpt.com/backend-api/codex/alpha/search";
const CLIENT_ID: &str = "app_EMoamEEZ73f0CkXaXp7hrann";
const MODEL: &str = "gpt-5.6-luna";
const USER_AGENT: &str = "codex-cli/0.154.0 (macOS 26.4.1; arm64) ghostty/1.3.1";
static SESSION_ID: OnceLock<String> = OnceLock::new();
fn session_id() -> &'static str {
    SESSION_ID.get_or_init(|| format!("mcp-{}", uuid::Uuid::new_v4()))
}

struct Auth {
    path: PathBuf,
    access: String,
    refresh: String,
    account: Option<String>,
}
fn auth_file_path() -> PathBuf {
    std::env::var_os("CODEX_AUTH_FILE")
        .map(PathBuf::from)
        .unwrap_or_else(|| {
            dirs::home_dir()
                .unwrap_or_default()
                .join(".codex/auth.json")
        })
}

fn auth_file_exists() -> bool {
    auth_file_path().is_file()
}

fn auth() -> Result<Auth> {
    let path = auth_file_path();
    let v: Value = serde_json::from_str(
        &fs::read_to_string(&path).with_context(|| format!("读取凭证失败: {}", path.display()))?,
    )?;
    let t = v.get("tokens").context("auth.json 缺少 tokens")?;
    Ok(Auth {
        path,
        access: t
            .get("access_token")
            .and_then(Value::as_str)
            .context("缺少 access_token")?
            .into(),
        refresh: t
            .get("refresh_token")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .into(),
        account: t.get("account_id").and_then(Value::as_str).map(Into::into),
    })
}
fn jwt_exp(token: &str) -> Option<SystemTime> {
    let p = token.split('.').nth(1)?;
    let v: Value = serde_json::from_slice(&URL_SAFE_NO_PAD.decode(p).ok()?).ok()?;
    Some(UNIX_EPOCH + Duration::from_secs(v.get("exp")?.as_u64()?))
}
async fn refresh(client: &Client, a: &mut Auth) -> Result<()> {
    if a.refresh.is_empty() {
        anyhow::bail!("access_token 已过期且没有 refresh_token，请重新 codex login");
    }
    let v: Value = client
        .post("https://auth.openai.com/oauth/token")
        .json(
            &json!({"client_id":CLIENT_ID,"grant_type":"refresh_token","refresh_token":a.refresh}),
        )
        .send()
        .await?
        .error_for_status()?
        .json()
        .await?;
    a.access = v
        .get("access_token")
        .and_then(Value::as_str)
        .context("刷新响应缺少 access_token")?
        .into();
    if let Some(r) = v.get("refresh_token").and_then(Value::as_str) {
        a.refresh = r.into();
    }
    let mut raw: Value = serde_json::from_str(&fs::read_to_string(&a.path)?)?;
    raw["tokens"]["access_token"] = json!(a.access);
    raw["tokens"]["refresh_token"] = json!(a.refresh);
    fs::write(&a.path, serde_json::to_vec_pretty(&raw)?)?;
    Ok(())
}
fn client() -> Result<Client> {
    let mut b = Client::builder();
    if let Some(p) = std::env::var_os("CODEX_HTTP_PROXY")
        .or_else(|| std::env::var_os("HTTPS_PROXY"))
        .or_else(|| std::env::var_os("HTTP_PROXY"))
        .or_else(|| Some("http://127.0.0.1:7890".into()))
    {
        b = b.proxy(Proxy::all(p.to_string_lossy().as_ref())?);
    }
    Ok(b.build()?)
}
fn schema(value: Value) -> rmcp::model::JsonObject {
    serde_json::from_value(value).expect("valid tool schema")
}

pub fn tools() -> Vec<Tool> {
    if !auth_file_exists() {
        return Vec::new();
    }
    vec![
        Tool::new(
            Cow::Borrowed(NAME),
            Cow::Borrowed("使用 ChatGPT/Codex 搜索端点联网搜索。"),
            Arc::new(schema(
                json!({"type":"object","properties":{"query":{"type":"string"},"recency":{"type":"integer"},"domains":{"type":"array","items":{"type":"string"}},"response_length":{"type":"string","enum":["short","medium","long"]}},"required":["query"],"additionalProperties":false}),
            )),
        ),
        Tool::new(
            Cow::Borrowed(OPEN_NAME),
            Cow::Borrowed("打开搜索结果 ref_id 或 URL。"),
            Arc::new(schema(
                json!({"type":"object","properties":{"ref_id":{"type":"string"},"lineno":{"type":"integer"}},"required":["ref_id"],"additionalProperties":false}),
            )),
        ),
        Tool::new(
            Cow::Borrowed(FIND_NAME),
            Cow::Borrowed("在页面中查找文本。"),
            Arc::new(schema(
                json!({"type":"object","properties":{"ref_id":{"type":"string"},"pattern":{"type":"string"}},"required":["ref_id","pattern"],"additionalProperties":false}),
            )),
        ),
        Tool::new(
            Cow::Borrowed(CLICK_NAME),
            Cow::Borrowed("点击页面中的编号链接。"),
            Arc::new(schema(
                json!({"type":"object","properties":{"ref_id":{"type":"string"},"id":{"type":"integer"}},"required":["ref_id","id"],"additionalProperties":false}),
            )),
        ),
    ]
}
pub fn tool() -> Tool {
    tools().remove(0)
}

pub async fn call_named(
    name: &str,
    arguments: Option<JsonObject>,
) -> Result<CallToolResponse, McpError> {
    let args: Value =
        serde_json::to_value(arguments.ok_or_else(|| McpError::invalid_params("缺少参数", None))?)
            .unwrap();
    let commands = match name {
        NAME => {
            let q = args
                .get("query")
                .and_then(Value::as_str)
                .unwrap_or("")
                .trim();
            if q.is_empty() {
                return Err(McpError::invalid_params("query 不能为空", None));
            }
            let mut sq = json!({"q":q});
            if let Some(v) = args.get("recency") {
                sq["recency"] = v.clone();
            }
            if let Some(v) = args.get("domains") {
                sq["domains"] = v.clone();
            }
            let mut c = json!({"search_query":[sq]});
            if let Some(v) = args.get("response_length") {
                c["response_length"] = v.clone();
            }
            c
        }
        OPEN_NAME => {
            json!({"open":[{"ref_id":args.get("ref_id").and_then(Value::as_str).unwrap_or(""),"lineno":args.get("lineno").cloned().unwrap_or(Value::Null)}]})
        }
        FIND_NAME => {
            json!({"find":[{"ref_id":args.get("ref_id").and_then(Value::as_str).unwrap_or(""),"pattern":args.get("pattern").and_then(Value::as_str).unwrap_or("")}]})
        }
        CLICK_NAME => {
            json!({"click":[{"ref_id":args.get("ref_id").and_then(Value::as_str).unwrap_or(""),"id":args.get("id").cloned().unwrap_or(json!(0))}]})
        }
        _ => {
            return Err(McpError::invalid_params(
                format!("unknown tool: {name}"),
                None,
            ));
        }
    };
    call_codex(commands).await
}

async fn call_codex(commands: Value) -> Result<CallToolResponse, McpError> {
    let client = client().map_err(|e| McpError::internal_error(e.to_string(), None))?;
    let mut a = auth().map_err(|e| McpError::internal_error(e.to_string(), None))?;
    if jwt_exp(&a.access).is_some_and(|e| e <= SystemTime::now() + Duration::from_secs(300)) {
        refresh(&client, &mut a)
            .await
            .map_err(|e| McpError::internal_error(e.to_string(), None))?;
    }
    let body = json!({"id":session_id(),"model": MODEL,"input":"","commands":commands,"max_output_tokens":2000});
    let endpoint =
        std::env::var("CODEX_SEARCH_ENDPOINT").unwrap_or_else(|_| DEFAULT_ENDPOINT.into());
    let mut req = client
        .post(endpoint)
        .bearer_auth(&a.access)
        .header("User-Agent", USER_AGENT)
        .json(&body);
    if let Some(id) = a.account {
        req = req.header("ChatGPT-Account-ID", id);
    }
    let response = req
        .send()
        .await
        .map_err(|e| McpError::internal_error(e.to_string(), None))?;
    let status = response.status();
    let text = response
        .text()
        .await
        .map_err(|e| McpError::internal_error(e.to_string(), None))?;
    if !status.is_success() {
        return Err(McpError::internal_error(
            format!("Codex search HTTP {status}: {text}"),
            None,
        ));
    }
    let v: Value =
        serde_json::from_str(&text).map_err(|e| McpError::internal_error(e.to_string(), None))?;
    let out = v.get("output").and_then(Value::as_str).unwrap_or(&text);
    Ok(CallToolResult::success(vec![rmcp::model::ContentBlock::text(out)]).into())
}
pub async fn call(arguments: Option<JsonObject>) -> Result<CallToolResponse, McpError> {
    call_named(NAME, arguments).await
}
