use std::{
    fs,
    io::Write,
    net::{IpAddr, SocketAddr},
    path::{Path, PathBuf},
    sync::Arc,
};

use anyhow::{Context, Result, anyhow};
use axum::{
    Router,
    body::{Body, Bytes},
    extract::{
        State, WebSocketUpgrade,
        ws::{Message as ClientMessage, WebSocket},
    },
    http::{HeaderMap, HeaderName, StatusCode},
    response::{IntoResponse, Response},
    routing::{get, post},
};
use base64::{Engine, engine::general_purpose::STANDARD};
use futures_util::{SinkExt, StreamExt};
use reqwest::{Client as AsyncClient, blocking::Client};
use subtle::ConstantTimeEq;
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::TcpStream,
};
use tokio_tungstenite::{
    MaybeTlsStream, WebSocketStream, connect_async,
    tungstenite::{Message as UpstreamMessage, client::IntoClientRequest},
};
use url::Url;

use crate::{
    auth::{AuthConfig, AuthState},
    usage::fetch_usage,
};

pub struct ProxyConfig {
    pub bind: IpAddr,
    pub port: u16,
    pub upstream_url: String,
    pub usage_url: String,
    pub reset_credits_url: String,
    pub client: Client,
    pub auth: AuthConfig,
    pub inbound_token: Option<String>,
    pub server_info: Option<PathBuf>,
    pub proxy_url: Option<String>,
}

struct ProxyState {
    client: AsyncClient,
    blocking_client: Client,
    upstream_url: Url,
    usage_url: Url,
    reset_credits_url: Url,
    auth: Arc<AuthState>,
    inbound_token: Option<Vec<u8>>,
    proxy_url: Option<String>,
}

pub fn run(config: ProxyConfig) -> Result<()> {
    let blocking_client_guard = config.client.clone();
    let auth_client_guard = config.auth.client.clone();
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .context("创建异步运行时失败")?;
    let result = runtime.block_on(run_async(config));
    drop(runtime);
    drop(blocking_client_guard);
    drop(auth_client_guard);
    result
}

async fn run_async(config: ProxyConfig) -> Result<()> {
    let mut client_builder = AsyncClient::builder();
    if let Some(proxy_url) = config.proxy_url.as_deref() {
        client_builder = client_builder.proxy(reqwest::Proxy::all(proxy_url)?);
    }
    let state = Arc::new(ProxyState {
        client: client_builder.build().context("创建异步 HTTP 客户端失败")?,
        blocking_client: config.client,
        upstream_url: Url::parse(&config.upstream_url).context("解析 --upstream-url 失败")?,
        usage_url: Url::parse(&config.usage_url).context("解析 --usage-url 失败")?,
        reset_credits_url: Url::parse(&config.reset_credits_url)
            .context("解析 --reset-credits-url 失败")?,
        auth: Arc::new(AuthState::load(config.auth)?),
        inbound_token: config.inbound_token.map(String::into_bytes),
        proxy_url: config
            .proxy_url
            .or_else(|| proxy_from_env(&config.upstream_url)),
    });
    let listener = tokio::net::TcpListener::bind(SocketAddr::from((config.bind, config.port)))
        .await
        .context("绑定代理监听地址失败")?;
    let addr = listener.local_addr().context("读取监听地址失败")?;
    if let Some(path) = config.server_info.as_deref() {
        write_server_info(path, addr.port())?;
    }
    eprintln!(
        "codex-proxy listening on http://{addr}/v1/responses and http://{addr}/v1/usage"
    );
    let app = Router::new()
        .route("/v1/responses", post(forward_http).get(forward_websocket))
        .route("/v1/usage", get(forward_usage))
        .with_state(state);
    axum::serve(listener, app)
        .await
        .context("HTTP 服务意外停止")
}

fn is_authorized(headers: &HeaderMap, expected: Option<&[u8]>) -> bool {
    let Some(expected) = expected else {
        return true;
    };
    headers
        .get("authorization")
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.strip_prefix("Bearer "))
        .is_some_and(|token| bool::from(token.as_bytes().ct_eq(expected)))
}

async fn auth_headers(auth: Arc<AuthState>) -> Result<HeaderMap> {
    tokio::task::spawn_blocking(move || auth.auth_headers())
        .await
        .context("认证任务失败")?
}

async fn forward_http(
    State(state): State<Arc<ProxyState>>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    if !is_authorized(&headers, state.inbound_token.as_deref()) {
        return unauthorized_response();
    }
    // ChatGPT 后端只接受 store=false、stream=true，客户端没写或写错时直接 400
    let body = match normalize_request_body(&body) {
        Ok(body) => body,
        Err(error) => return (StatusCode::BAD_REQUEST, error.to_string()).into_response(),
    };
    let auth = match auth_headers(Arc::clone(&state.auth)).await {
        Ok(auth) => auth,
        Err(error) => return error_response(StatusCode::BAD_GATEWAY, error),
    };
    let mut upstream_headers = forwarded_http_headers(&headers);
    upstream_headers.extend(auth);
    let upstream = match state
        .client
        .post(state.upstream_url.clone())
        .headers(upstream_headers)
        .body(body)
        .send()
        .await
    {
        Ok(response) => response,
        Err(error) => return error_response(StatusCode::BAD_GATEWAY, error),
    };
    let mut response = Response::builder().status(upstream.status());
    for (name, value) in upstream.headers() {
        if !matches!(
            name.as_str(),
            "content-length" | "transfer-encoding" | "connection" | "trailer" | "upgrade"
        ) {
            response = response.header(name, value);
        }
    }
    response
        .body(Body::from_stream(upstream.bytes_stream()))
        .unwrap_or_else(|error| error_response(StatusCode::BAD_GATEWAY, error))
}

fn normalize_request_body(body: &[u8]) -> Result<Vec<u8>> {
    let mut payload: serde_json::Value =
        serde_json::from_slice(body).context("Responses 请求体必须是合法 JSON")?;
    let object = payload
        .as_object_mut()
        .context("Responses 请求体必须是 JSON 对象")?;
    object.insert("store".to_owned(), serde_json::Value::Bool(false));
    object.insert("stream".to_owned(), serde_json::Value::Bool(true));
    serde_json::to_vec(&payload).context("序列化 Responses 请求体失败")
}

async fn forward_usage(State(state): State<Arc<ProxyState>>, headers: HeaderMap) -> Response {
    if !is_authorized(&headers, state.inbound_token.as_deref()) {
        return unauthorized_response();
    }
    let result = tokio::task::spawn_blocking(move || {
        fetch_usage(
            &state.blocking_client,
            &state.auth,
            &state.usage_url,
            &state.reset_credits_url,
        )
    })
    .await;
    match result {
        Ok(Ok(usage)) => axum::Json(usage).into_response(),
        Ok(Err(error)) => error_response(StatusCode::BAD_GATEWAY, error),
        Err(error) => error_response(StatusCode::BAD_GATEWAY, error),
    }
}

async fn forward_websocket(
    State(state): State<Arc<ProxyState>>,
    headers: HeaderMap,
    upgrade: WebSocketUpgrade,
) -> Response {
    if !is_authorized(&headers, state.inbound_token.as_deref()) {
        return unauthorized_response();
    }
    let auth = match auth_headers(Arc::clone(&state.auth)).await {
        Ok(auth) => auth,
        Err(error) => return error_response(StatusCode::BAD_GATEWAY, error),
    };
    let mut url = state.upstream_url.clone();
    let scheme = match url.scheme() {
        "https" => "wss",
        "http" => "ws",
        _ => return StatusCode::BAD_GATEWAY.into_response(),
    };
    if url.set_scheme(scheme).is_err() {
        return StatusCode::BAD_GATEWAY.into_response();
    }
    let mut request = match url.as_str().into_client_request() {
        Ok(request) => request,
        Err(error) => return error_response(StatusCode::BAD_GATEWAY, error),
    };
    request
        .headers_mut()
        .extend(forwarded_websocket_headers(&headers));
    request.headers_mut().extend(auth);
    let upstream = match tokio::time::timeout(
        std::time::Duration::from_secs(20),
        connect_upstream(request, state.proxy_url.as_deref()),
    )
    .await
    {
        Ok(Ok((upstream, _))) => {
            log_line(format_args!("ws 已连接上游"));
            upstream
        }
        Ok(Err(error)) => return error_response(StatusCode::BAD_GATEWAY, error),
        Err(error) => return error_response(StatusCode::GATEWAY_TIMEOUT, error),
    };
    upgrade
        .on_upgrade(move |downstream| relay_websocket(downstream, upstream))
        .into_response()
}

type UpstreamSocket = WebSocketStream<MaybeTlsStream<TcpStream>>;

async fn connect_upstream(
    request: tokio_tungstenite::tungstenite::http::Request<()>,
    proxy_url: Option<&str>,
) -> Result<(
    UpstreamSocket,
    tokio_tungstenite::tungstenite::handshake::client::Response,
)> {
    let Some(proxy_url) = proxy_url else {
        return connect_async(request).await.map_err(Into::into);
    };
    let proxy = Url::parse(proxy_url).context("解析 WebSocket 代理地址失败")?;
    if proxy.scheme() != "http" {
        return Err(anyhow!("WebSocket 代理目前只支持 HTTP CONNECT"));
    }
    let host = proxy.host_str().context("WebSocket 代理缺少 host")?;
    let port = proxy
        .port_or_known_default()
        .context("WebSocket 代理缺少 port")?;
    let mut stream = TcpStream::connect((host, port))
        .await
        .context("连接 WebSocket 代理失败")?;
    let uri = request.uri();
    let target_host = uri.host().context("WebSocket 上游缺少 host")?;
    let target_port = uri
        .port_u16()
        .unwrap_or(if uri.scheme_str() == Some("wss") {
            443
        } else {
            80
        });
    let target = format!("{target_host}:{target_port}");
    let mut connect = format!("CONNECT {target} HTTP/1.1\r\nHost: {target}\r\n");
    if !proxy.username().is_empty() || proxy.password().is_some() {
        let credentials = format!(
            "{}:{}",
            proxy.username(),
            proxy.password().unwrap_or_default()
        );
        connect.push_str(&format!(
            "Proxy-Authorization: Basic {}\r\n",
            STANDARD.encode(credentials)
        ));
    }
    connect.push_str("\r\n");
    stream
        .write_all(connect.as_bytes())
        .await
        .context("发送 CONNECT 失败")?;
    let mut response = Vec::new();
    while !response.ends_with(b"\r\n\r\n") {
        if response.len() >= 8192 {
            return Err(anyhow!("WebSocket 代理响应头过长"));
        }
        let mut byte = [0];
        stream
            .read_exact(&mut byte)
            .await
            .context("读取 CONNECT 响应失败")?;
        response.push(byte[0]);
    }
    if !response.starts_with(b"HTTP/1.1 200 ") && !response.starts_with(b"HTTP/1.0 200 ") {
        return Err(anyhow!(
            "WebSocket 代理 CONNECT 失败: {}",
            String::from_utf8_lossy(&response)
                .lines()
                .next()
                .unwrap_or_default()
        ));
    }
    tokio_tungstenite::client_async_tls(request, stream)
        .await
        .context("通过代理连接上游 WebSocket 失败")
}

async fn relay_websocket(mut downstream: WebSocket, mut upstream: UpstreamSocket) {
    let mut heartbeat = tokio::time::interval(std::time::Duration::from_secs(30));
    heartbeat.tick().await;
    // 断链诊断：上游断开时用它判断是「请求还没开始」还是「响应中途被掐」
    let mut upstream_events = 0usize;
    let mut last_event_type = String::from("-");
    let mut last_event_at = std::time::Instant::now();
    let mut has_started = false;
    loop {
        tokio::select! {
            _ = heartbeat.tick() => {
                if let Err(error) = downstream.send(ClientMessage::Ping(Bytes::new())).await {
                    log_line(format_args!("ws 心跳（下游）失败: {error}"));
                    break;
                }
                if let Err(error) = upstream.send(UpstreamMessage::Ping(Vec::new().into())).await {
                    log_line(format_args!("ws 心跳（上游）失败: {error}"));
                    break;
                }
            }
            message = downstream.next() => {
                let message = match message {
                    Some(Ok(message)) => message,
                    Some(Err(error)) => {
                        log_line(format_args!("ws 下游读错误: {error}"));
                        break;
                    }
                    None => {
                        log_line(format_args!("ws 下游连接结束"));
                        break;
                    }
                };
                let is_close = matches!(message, ClientMessage::Close(_));
                if let ClientMessage::Close(frame) = &message {
                    log_line(format_args!("ws 下游关闭: {frame:?}"));
                }
                let mapped = match message {
                    ClientMessage::Text(text) => UpstreamMessage::Text(text.to_string().into()),
                    ClientMessage::Binary(bytes) => UpstreamMessage::Binary(bytes),
                    ClientMessage::Ping(_) => {
                        if downstream.flush().await.is_err() { break; }
                        continue;
                    }
                    ClientMessage::Pong(_) => continue,
                    ClientMessage::Close(_) => UpstreamMessage::Close(None),
                };
                if upstream.send(mapped).await.is_err() || is_close { break; }
            }
            message = upstream.next() => {
                let message = match message {
                    Some(Ok(message)) => message,
                    Some(Err(error)) => {
                        log_line(format_args!(
                            "ws 上游读错误: {error}（已收事件={upstream_events} 最后事件={last_event_type} 已开始响应={has_started} 距上次上游帧={}ms）",
                            last_event_at.elapsed().as_millis()
                        ));
                        break;
                    }
                    None => {
                        log_line(format_args!(
                            "ws 上游连接结束（已收事件={upstream_events} 最后事件={last_event_type} 已开始响应={has_started}）"
                        ));
                        break;
                    }
                };
                let is_close = matches!(message, UpstreamMessage::Close(_));
                if let UpstreamMessage::Close(frame) = &message {
                    log_line(format_args!(
                        "ws 上游关闭: {frame:?}（已收事件={upstream_events} 最后事件={last_event_type}）"
                    ));
                }
                if let UpstreamMessage::Text(text) = &message {
                    if let Ok(value) = serde_json::from_str::<serde_json::Value>(text) {
                        if let Some(event_type) = value.get("type").and_then(|value| value.as_str()) {
                            upstream_events += 1;
                            has_started |= event_type.starts_with("response.");
                            last_event_type = event_type.to_owned();
                            last_event_at = std::time::Instant::now();
                            if event_type == "error" {
                                // 上游用 error 帧说明原因，随后可能直接 RST，必须留证
                                let detail: String = text.chars().take(1000).collect();
                                log_line(format_args!("ws 上游错误帧: {detail}"));
                            }
                        }
                    }
                }
                let mapped = match message {
                    UpstreamMessage::Text(text) => ClientMessage::Text(text.to_string().into()),
                    UpstreamMessage::Binary(bytes) => ClientMessage::Binary(bytes),
                    UpstreamMessage::Ping(_) => {
                        if upstream.flush().await.is_err() { break; }
                        continue;
                    }
                    UpstreamMessage::Pong(_) => continue,
                    UpstreamMessage::Close(_) => ClientMessage::Close(None),
                    UpstreamMessage::Frame(_) => continue,
                };
                if downstream.send(mapped).await.is_err() || is_close { break; }
            }
        }
    }
}

fn forwarded_http_headers(headers: &HeaderMap) -> HeaderMap {
    let mut forwarded = HeaderMap::new();
    for (name, value) in headers {
        if matches!(
            name.as_str(),
            "authorization"
                | "host"
                | "connection"
                | "content-length"
                | "transfer-encoding"
                | "te"
                | "trailer"
                | "upgrade"
                | "proxy-authorization"
                | "proxy-connection"
        ) {
            continue;
        }
        forwarded.append(name, value.clone());
    }
    forwarded
}

fn forwarded_websocket_headers(headers: &HeaderMap) -> HeaderMap {
    let mut forwarded = HeaderMap::new();
    for name in [
        "accept",
        "user-agent",
        "openai-beta",
        "x-openai-client-user-agent",
    ] {
        if let Some(value) = headers.get(name)
            && let Ok(name) = HeaderName::from_bytes(name.as_bytes())
        {
            forwarded.insert(name, value.clone());
        }
    }
    forwarded
}

fn proxy_from_env(upstream_url: &str) -> Option<String> {
    let upstream = Url::parse(upstream_url).ok()?;
    let host = upstream.host_str()?;
    let no_proxy = std::env::var("NO_PROXY")
        .or_else(|_| std::env::var("no_proxy"))
        .unwrap_or_default();
    if no_proxy.split(',').map(str::trim).any(|entry| {
        entry == "*"
            || entry.eq_ignore_ascii_case(host)
            || entry.strip_prefix('.').is_some_and(|suffix| {
                host.eq_ignore_ascii_case(suffix)
                    || host
                        .to_ascii_lowercase()
                        .ends_with(&format!(".{}", suffix.to_ascii_lowercase()))
            })
    }) {
        return None;
    }
    let names = if upstream.scheme() == "https" {
        ["HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"]
    } else {
        ["HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"]
    };
    names
        .into_iter()
        .find_map(|name| std::env::var(name).ok().filter(|value| !value.is_empty()))
}

fn error_response(status: StatusCode, error: impl std::fmt::Display) -> Response {
    log_line(format_args!("代理请求失败: {error}"));
    status.into_response()
}

/// 带本地时区的行日志：与 xbot / relay 的日志对时间线时用。
fn log_line(message: impl std::fmt::Display) {
    eprintln!(
        "[{}] {message}",
        chrono::Local::now().format("%Y-%m-%d %H:%M:%S")
    );
}

fn unauthorized_response() -> Response {
    (StatusCode::UNAUTHORIZED, [("www-authenticate", "Bearer")]).into_response()
}

fn write_server_info(path: &Path, port: u16) -> Result<()> {
    if let Some(parent) = path.parent()
        && !parent.as_os_str().is_empty()
    {
        fs::create_dir_all(parent)?;
    }
    let mut file = fs::File::create(path)?;
    writeln!(
        file,
        "{}",
        serde_json::json!({"port": port, "pid": std::process::id()})
    )?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use base64::engine::general_purpose::URL_SAFE_NO_PAD;
    use serde_json::json;
    use tokio::time::{Duration, timeout};
    use tokio_tungstenite::accept_async;

    fn test_auth_file(temp_dir: &tempfile::TempDir) -> PathBuf {
        let auth_file = temp_dir.path().join("auth.json");
        let id_token = format!("header.{}.signature", URL_SAFE_NO_PAD.encode("{}"));
        fs::write(
            &auth_file,
            json!({"tokens": {
                "id_token": id_token, "access_token": "access-test",
                "refresh_token": "refresh-test", "account_id": "acct_test"
            }})
            .to_string(),
        )
        .expect("写入测试认证文件");
        auth_file
    }

    fn test_state(
        upstream_url: Url,
        auth_file: PathBuf,
        blocking_client: Client,
    ) -> Arc<ProxyState> {
        Arc::new(ProxyState {
            client: AsyncClient::new(),
            blocking_client: blocking_client.clone(),
            upstream_url: upstream_url.clone(),
            usage_url: upstream_url.clone(),
            reset_credits_url: upstream_url,
            auth: Arc::new(
                AuthState::load(AuthConfig {
                    auth_file,
                    client: blocking_client,
                })
                .expect("读取认证"),
            ),
            inbound_token: None,
            proxy_url: None,
        })
    }

    #[test]
    fn websocket_relays_messages_in_both_directions() {
        let blocking_client = Client::new();
        let runtime = tokio::runtime::Builder::new_multi_thread()
            .enable_all()
            .build()
            .expect("创建运行时");
        runtime.block_on(async {
            let upstream_listener = tokio::net::TcpListener::bind("127.0.0.1:0")
                .await
                .expect("绑定模拟上游");
            let upstream_addr = upstream_listener.local_addr().expect("读取上游地址");
            let upstream_task = tokio::spawn(async move {
                let (stream, _) = upstream_listener.accept().await.expect("接受上游连接");
                let mut socket = accept_async(stream).await.expect("上游握手");
                let created = socket
                    .next()
                    .await
                    .expect("读取创建消息")
                    .expect("创建消息有效");
                assert_eq!(
                    created.to_text().expect("文本消息"),
                    r#"{"type":"response.create"}"#
                );
                socket
                    .send(UpstreamMessage::Text(
                        r#"{"type":"response.created"}"#.into(),
                    ))
                    .await
                    .expect("发送创建事件");
                let interrupt = socket
                    .next()
                    .await
                    .expect("读取中断消息")
                    .expect("中断消息有效");
                assert_eq!(
                    interrupt.to_text().expect("文本消息"),
                    r#"{"type":"response.interrupt"}"#
                );
                socket
                    .send(UpstreamMessage::Text(
                        r#"{"type":"response.completed"}"#.into(),
                    ))
                    .await
                    .expect("发送完成事件");
                let next_turn = socket
                    .next()
                    .await
                    .expect("读取下一轮消息")
                    .expect("下一轮消息有效");
                assert_eq!(
                    next_turn.to_text().expect("文本消息"),
                    r#"{"type":"response.create","input":[]}"#
                );
                socket
                    .send(UpstreamMessage::Text(
                        r#"{"type":"response.completed","turn":2}"#.into(),
                    ))
                    .await
                    .expect("发送下一轮完成事件");
            });
            let temp_dir = tempfile::tempdir().expect("创建临时目录");
            let auth_file = test_auth_file(&temp_dir);
            let url = Url::parse(&format!("http://{upstream_addr}/responses")).expect("上游 URL");
            let state = test_state(url, auth_file, blocking_client);
            let app = Router::new()
                .route("/v1/responses", get(forward_websocket))
                .with_state(state);
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
                .await
                .expect("绑定代理");
            let addr = listener.local_addr().expect("读取代理地址");
            let server_task = tokio::spawn(async move {
                axum::serve(listener, app).await.expect("运行代理");
            });
            let (mut client, _) = timeout(
                Duration::from_secs(3),
                connect_async(format!("ws://{addr}/v1/responses")),
            )
            .await
            .expect("连接超时")
            .expect("连接代理");
            client
                .send(UpstreamMessage::Text(
                    r#"{"type":"response.create"}"#.into(),
                ))
                .await
                .expect("发送创建消息");
            assert_eq!(
                client
                    .next()
                    .await
                    .expect("读取创建事件")
                    .expect("创建事件有效")
                    .to_text()
                    .expect("文本事件"),
                r#"{"type":"response.created"}"#
            );
            client
                .send(UpstreamMessage::Text(
                    r#"{"type":"response.interrupt"}"#.into(),
                ))
                .await
                .expect("发送中断消息");
            assert_eq!(
                client
                    .next()
                    .await
                    .expect("读取完成事件")
                    .expect("完成事件有效")
                    .to_text()
                    .expect("文本事件"),
                r#"{"type":"response.completed"}"#
            );
            client
                .send(UpstreamMessage::Text(
                    r#"{"type":"response.create","input":[]}"#.into(),
                ))
                .await
                .expect("发送下一轮创建消息");
            assert_eq!(
                client
                    .next()
                    .await
                    .expect("读取下一轮事件")
                    .expect("下一轮事件有效")
                    .to_text()
                    .expect("文本事件"),
                r#"{"type":"response.completed","turn":2}"#
            );
            timeout(Duration::from_secs(3), upstream_task)
                .await
                .expect("上游任务超时")
                .expect("上游任务失败");
            server_task.abort();
        });
    }

    #[test]
    fn http_stream_forwards_sse_without_waiting_for_completion() {
        let blocking_client = Client::new();
        let runtime = tokio::runtime::Builder::new_multi_thread()
            .enable_all()
            .build()
            .expect("创建运行时");
        runtime.block_on(async {
            let upstream_listener = tokio::net::TcpListener::bind("127.0.0.1:0")
                .await
                .expect("绑定模拟上游");
            let upstream_addr = upstream_listener.local_addr().expect("读取上游地址");
            let (continue_sender, continue_receiver) = tokio::sync::oneshot::channel::<()>();
            let upstream_task = tokio::spawn(async move {
                let (mut stream, _) = upstream_listener.accept().await.expect("接受上游连接");
                let mut request = Vec::new();
                let header_end = loop {
                    let mut chunk = [0; 1024];
                    let size = stream.read(&mut chunk).await.expect("读取上游请求");
                    assert!(size > 0, "上游请求提前结束");
                    request.extend_from_slice(&chunk[..size]);
                    if let Some(index) = request.windows(4).position(|bytes| bytes == b"\r\n\r\n") {
                        break index + 4;
                    }
                };
                let headers = String::from_utf8_lossy(&request[..header_end]);
                assert!(headers.contains("POST /responses"));
                let content_length = headers.lines()
                    .find_map(|line| line.to_ascii_lowercase().strip_prefix("content-length: ").and_then(|value| value.trim().parse::<usize>().ok()))
                    .expect("请求包含 Content-Length");
                while request.len() - header_end < content_length {
                    let mut chunk = [0; 1024];
                    let size = stream.read(&mut chunk).await.expect("读取请求体");
                    assert!(size > 0, "请求体提前结束");
                    request.extend_from_slice(&chunk[..size]);
                }
                // 上游只认 store=false、stream=true，代理必须改写后再转发
                assert_eq!(&request[header_end..header_end + content_length], b"{\"input\":[],\"model\":\"test\",\"store\":false,\"stream\":true}");
                stream.write_all(b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n")
                    .await.expect("发送响应头");
                stream.write_all(b"data: {\"type\":\"response.created\"}\n\n")
                    .await.expect("发送首个事件");
                continue_receiver.await.expect("等待首个事件转发");
                stream.write_all(b"data: {\"type\":\"response.completed\"}\n\n")
                    .await.expect("发送完成事件");
            });
            let temp_dir = tempfile::tempdir().expect("创建临时目录");
            let url = Url::parse(&format!("http://{upstream_addr}/responses")).expect("上游 URL");
            let state = test_state(url, test_auth_file(&temp_dir), blocking_client);
            let app = Router::new().route("/v1/responses", post(forward_http)).with_state(state);
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.expect("绑定代理");
            let addr = listener.local_addr().expect("读取代理地址");
            let server_task = tokio::spawn(async move { axum::serve(listener, app).await.expect("运行代理"); });
            let response = AsyncClient::new().post(format!("http://{addr}/v1/responses"))
                .header("content-type", "application/json")
                .body("{\"model\":\"test\",\"input\":[],\"store\":true,\"stream\":true}")
                .send().await.expect("请求代理");
            assert_eq!(response.status(), StatusCode::OK);
            assert_eq!(response.headers()["content-type"], "text/event-stream");
            let mut body = response.bytes_stream();
            let first = timeout(Duration::from_secs(3), body.next())
                .await.expect("首个事件超时").expect("首个事件存在").expect("首个事件有效");
            assert!(String::from_utf8_lossy(&first).contains("response.created"));
            continue_sender.send(()).expect("继续发送事件");
            let second = timeout(Duration::from_secs(3), body.next())
                .await.expect("完成事件超时").expect("完成事件存在").expect("完成事件有效");
            assert!(String::from_utf8_lossy(&second).contains("response.completed"));
            timeout(Duration::from_secs(3), upstream_task).await.expect("上游任务超时").expect("上游任务失败");
            server_task.abort();
        });
    }
}
