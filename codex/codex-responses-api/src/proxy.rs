use std::{
    fs,
    io::{Read, Write},
    net::{IpAddr, SocketAddr, TcpListener},
    path::{Path, PathBuf},
    sync::Arc,
};

use anyhow::{Context, Result, anyhow};
use reqwest::blocking::{Client, Response as UpstreamResponse};
use reqwest::header::{HOST, HeaderMap, HeaderName, HeaderValue};
use subtle::ConstantTimeEq;
use tiny_http::{Header, Method, Request, Response, Server, StatusCode};
use url::Url;

use crate::auth::{AuthConfig, AuthState};

const RESPONSE_PATH: &str = "/v1/responses";

pub struct ProxyConfig {
    pub bind: IpAddr,
    pub port: u16,
    pub upstream_url: String,
    pub client: Client,
    pub auth: AuthConfig,
    pub inbound_token: Option<String>,
    pub server_info: Option<PathBuf>,
}

pub fn run(config: ProxyConfig) -> Result<()> {
    let upstream_url = Url::parse(&config.upstream_url).context("解析 --upstream-url 失败")?;
    let host = upstream_host(&upstream_url)?;
    let host_header = HeaderValue::from_str(&host).context("构造上游 Host 头失败")?;
    let auth = Arc::new(AuthState::load(config.auth)?);
    let inbound_token = config
        .inbound_token
        .map(|token| Arc::new(token.into_bytes()));
    let listener = TcpListener::bind(SocketAddr::from((config.bind, config.port)))
        .with_context(|| format!("绑定 {}:{} 失败", config.bind, config.port))?;
    let bound_addr = listener.local_addr().context("读取监听地址失败")?;
    let server = Server::from_listener(listener, None)
        .map_err(|error| anyhow!("创建 HTTP 服务失败: {error}"))?;

    if let Some(path) = config.server_info.as_deref() {
        write_server_info(path, bound_addr.port())?;
    }
    eprintln!("codex-responses-api listening on http://{bound_addr}{RESPONSE_PATH}");

    let client = Arc::new(config.client);
    let upstream_url = Arc::new(upstream_url);
    let host_header = Arc::new(host_header);

    for request in server.incoming_requests() {
        let client = Arc::clone(&client);
        let upstream_url = Arc::clone(&upstream_url);
        let host_header = Arc::clone(&host_header);
        let auth = Arc::clone(&auth);
        let inbound_token = inbound_token.clone();
        std::thread::spawn(move || {
            if let Err(error) = handle_request(
                client,
                upstream_url,
                host_header,
                auth,
                inbound_token,
                request,
            ) {
                eprintln!("处理请求失败: {error}");
            }
        });
    }

    Err(anyhow!("HTTP 服务意外停止"))
}

fn handle_request(
    client: Arc<Client>,
    upstream_url: Arc<Url>,
    host_header: Arc<HeaderValue>,
    auth: Arc<AuthState>,
    inbound_token: Option<Arc<Vec<u8>>>,
    request: Request,
) -> Result<()> {
    if !is_authorized(
        &request,
        inbound_token.as_ref().map(|token| token.as_slice()),
    ) {
        respond_error(request, StatusCode(401), "代理鉴权失败", true);
        return Ok(());
    }

    if request.method() != &Method::Post || request.url() != RESPONSE_PATH {
        respond_error(request, StatusCode(404), "只支持 POST /v1/responses", false);
        return Ok(());
    }

    let mut body = Vec::new();
    let mut request = request;
    if let Err(error) = request.as_reader().read_to_end(&mut body) {
        respond_error(request, StatusCode(400), "读取请求体失败", false);
        return Err(error.into());
    }
    body = match normalize_request_body(&body) {
        Ok(body) => body,
        Err(error) => {
            respond_error(
                request,
                StatusCode(400),
                "请求体必须是合法 JSON 对象",
                false,
            );
            return Err(error);
        }
    };
    let auth_headers = match auth.auth_headers() {
        Ok(headers) => headers,
        Err(error) => {
            respond_error(request, StatusCode(502), "读取 ChatGPT 登录凭证失败", false);
            return Err(error).context("读取 ChatGPT 登录凭证失败");
        }
    };
    let headers = forward_headers(&request, &auth_headers, &host_header);
    let upstream_response = match client
        .post(upstream_url.as_ref().clone())
        .headers(headers)
        .body(body)
        .send()
    {
        Ok(response) => response,
        Err(error) => {
            respond_error(request, StatusCode(502), "上游请求失败", false);
            return Err(error).context("转发 Responses API 请求失败");
        }
    };

    respond_upstream(request, upstream_response)
}

fn normalize_request_body(body: &[u8]) -> Result<Vec<u8>> {
    let mut payload: serde_json::Value =
        serde_json::from_slice(body).context("解析 Responses API 请求体失败")?;
    let object = payload
        .as_object_mut()
        .context("Responses API 请求体必须是 JSON 对象")?;
    object.insert("store".to_owned(), serde_json::Value::Bool(false));
    object.insert("stream".to_owned(), serde_json::Value::Bool(true));
    serde_json::to_vec(&payload).context("序列化 Responses API 请求体失败")
}

fn is_authorized(request: &Request, expected: Option<&[u8]>) -> bool {
    let Some(expected) = expected else {
        return true;
    };
    let Some(header) = request
        .headers()
        .iter()
        .find(|header| header_name_lower(header) == "authorization")
    else {
        return false;
    };
    let Some(token) = header.value.as_str().strip_prefix("Bearer ") else {
        return false;
    };
    token.as_bytes().ct_eq(expected).into()
}

fn forward_headers(
    request: &Request,
    auth_headers: &HeaderMap,
    host_header: &HeaderValue,
) -> HeaderMap {
    let mut headers = HeaderMap::new();
    for header in request.headers() {
        let name = header.field.as_str();
        let lower_name = name.to_ascii_lowercase();
        if lower_name == "authorization"
            || lower_name == "host"
            || lower_name == "chatgpt-account-id"
            || lower_name == "x-openai-fedramp"
        {
            continue;
        }
        let Ok(name) = HeaderName::from_bytes(name.as_bytes()) else {
            continue;
        };
        let Ok(value) = HeaderValue::from_bytes(header.value.as_bytes()) else {
            continue;
        };
        headers.append(name, value);
    }

    for (name, value) in auth_headers {
        headers.insert(name.clone(), value.clone());
    }
    headers.insert(HOST, host_header.clone());
    headers
}

fn respond_upstream(request: Request, upstream: UpstreamResponse) -> Result<()> {
    let status = upstream.status();
    let content_length = upstream
        .content_length()
        .and_then(|length| usize::try_from(length).ok());
    let mut response_headers = Vec::new();
    for (name, value) in upstream.headers() {
        if matches!(
            name.as_str(),
            "content-length" | "transfer-encoding" | "connection" | "trailer" | "upgrade"
        ) {
            continue;
        }
        if let Ok(header) = Header::from_bytes(name.as_str().as_bytes(), value.as_bytes()) {
            response_headers.push(header);
        }
    }

    let body: Box<dyn Read + Send> = Box::new(upstream);
    let response = Response::new(
        StatusCode(status.as_u16()),
        response_headers,
        body,
        content_length,
        None,
    );
    request.respond(response).map_err(Into::into)
}

fn respond_error(request: Request, status: StatusCode, message: &str, unauthorized: bool) {
    let mut response = Response::from_string(message).with_status_code(status);
    if unauthorized && let Ok(header) = Header::from_bytes(b"WWW-Authenticate", b"Bearer") {
        response = response.with_header(header);
    }
    let _ = request.respond(response);
}

fn header_name_lower(header: &Header) -> String {
    header.field.as_str().to_ascii_lowercase().to_string()
}

fn upstream_host(url: &Url) -> Result<String> {
    let host = url.host_str().context("上游地址必须包含 host")?;
    Ok(match url.port() {
        Some(port) => format!("{host}:{port}"),
        None => host.to_owned(),
    })
}

fn write_server_info(path: &Path, port: u16) -> Result<()> {
    if let Some(parent) = path.parent()
        && !parent.as_os_str().is_empty()
    {
        fs::create_dir_all(parent)?;
    }
    let content = serde_json::json!({
        "port": port,
        "pid": std::process::id(),
    });
    let mut file = fs::File::create(path)?;
    writeln!(file, "{}", serde_json::to_string(&content)?)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::normalize_request_body;
    use serde_json::json;

    #[test]
    fn normalize_request_body_sets_upstream_required_fields() {
        let body = br#"{"model":"gpt-5.6-luna","input":[],"store":true,"stream":false}"#;
        let normalized = normalize_request_body(body).expect("请求体应可解析");
        let payload: serde_json::Value =
            serde_json::from_slice(&normalized).expect("规范化后的请求体应可解析");

        assert_eq!(payload["model"], "gpt-5.6-luna");
        assert_eq!(payload["store"], json!(false));
        assert_eq!(payload["stream"], json!(true));
    }

    #[test]
    fn normalize_request_body_rejects_non_object() {
        let error = normalize_request_body(br#"[]"#).expect_err("数组不应被接受");
        assert!(error.to_string().contains("必须是 JSON 对象"));
    }
}
