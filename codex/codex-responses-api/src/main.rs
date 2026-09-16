use std::{fs, net::IpAddr, path::PathBuf, str::FromStr};

use anyhow::{Context, Result, bail};
use clap::Parser;
use codex_responses_api::{AuthConfig, ProxyConfig, run};

#[derive(Debug, Parser)]
#[command(
    name = "codex-responses-api",
    about = "将本机 Codex ChatGPT 登录代理为 Responses API"
)]
struct Args {
    #[arg(long, value_name = "FILE")]
    auth_file: Option<PathBuf>,

    #[arg(long, default_value = "127.0.0.1")]
    bind: String,

    #[arg(long, default_value_t = 0)]
    port: u16,

    #[arg(
        long,
        default_value = "https://chatgpt.com/backend-api/codex/responses"
    )]
    upstream_url: String,

    #[arg(long, value_name = "URL")]
    proxy_url: Option<String>,

    #[arg(long, conflicts_with = "auth_token_file", value_name = "TOKEN")]
    auth_token: Option<String>,

    #[arg(long, conflicts_with = "auth_token", value_name = "FILE")]
    auth_token_file: Option<PathBuf>,

    #[arg(long, value_name = "FILE")]
    server_info: Option<PathBuf>,
}

fn main() -> Result<()> {
    let args = Args::parse();
    let bind = IpAddr::from_str(&args.bind).context("--bind 必须是有效的 IP 地址")?;
    let auth_file = resolve_auth_file(args.auth_file)?;
    let client = build_client(args.proxy_url.as_deref())?;
    let auth = AuthConfig {
        auth_file,
        client: client.clone(),
    };
    let inbound_token = read_inbound_token(args.auth_token, args.auth_token_file)?;

    run(ProxyConfig {
        bind,
        port: args.port,
        upstream_url: args.upstream_url,
        client,
        auth,
        inbound_token,
        server_info: args.server_info,
    })
}

fn resolve_auth_file(explicit: Option<PathBuf>) -> Result<PathBuf> {
    if let Some(path) = explicit {
        return Ok(path);
    }

    if let Some(codex_home) = std::env::var_os("CODEX_HOME") {
        return Ok(PathBuf::from(codex_home).join("auth.json"));
    }

    dirs::home_dir()
        .map(|home| home.join(".codex/auth.json"))
        .ok_or_else(|| anyhow::anyhow!("无法确定默认认证文件，请使用 --auth-file"))
}

fn read_inbound_token(
    token: Option<String>,
    token_file: Option<PathBuf>,
) -> Result<Option<String>> {
    let Some(token_file) = token_file else {
        return Ok(token.filter(|value| !value.is_empty()));
    };

    let token = fs::read_to_string(&token_file)
        .with_context(|| format!("读取代理鉴权文件失败: {}", token_file.display()))?;
    let token = token.lines().next().unwrap_or_default().trim();
    if token.is_empty() {
        bail!("代理鉴权文件为空: {}", token_file.display());
    }
    Ok(Some(token.to_owned()))
}

fn build_client(proxy_url: Option<&str>) -> Result<reqwest::blocking::Client> {
    let mut builder = reqwest::blocking::Client::builder().timeout(None);
    if let Some(proxy_url) = proxy_url {
        let proxy = reqwest::Proxy::all(proxy_url)
            .with_context(|| format!("无效的代理地址: {proxy_url}"))?;
        builder = builder.proxy(proxy);
    }
    builder.build().context("创建 HTTP 客户端失败")
}
