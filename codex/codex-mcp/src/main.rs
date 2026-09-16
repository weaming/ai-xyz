mod apply_patch;

use std::borrow::Cow;
use std::sync::Arc;

use anyhow::{Result, bail};
use rmcp::ErrorData as McpError;
use rmcp::ServiceExt;
use rmcp::handler::server::ServerHandler;
use rmcp::model::CallToolRequestParams;
use rmcp::model::CallToolResponse;
use rmcp::model::ListToolsResult;
use rmcp::model::PaginatedRequestParams;
use rmcp::model::ProtocolVersion;
use rmcp::model::ServerCapabilities;
use rmcp::model::ServerInfo;

#[derive(Clone, Default)]
struct McpServer;

impl ServerHandler for McpServer {
    fn get_info(&self) -> ServerInfo {
        ServerInfo::new(ServerCapabilities::builder().enable_tools().build())
            .with_protocol_version(ProtocolVersion::V_2025_11_25)
            .with_instructions("Use apply_patch for file edits.")
    }

    fn supported_protocol_versions(&self) -> Cow<'static, [ProtocolVersion]> {
        Cow::Borrowed(&[ProtocolVersion::V_2025_11_25, ProtocolVersion::V_2026_07_28])
    }

    fn list_tools(
        &self,
        _request: Option<PaginatedRequestParams>,
        _context: rmcp::service::RequestContext<rmcp::service::RoleServer>,
    ) -> impl std::future::Future<Output = Result<ListToolsResult, McpError>> + Send + '_ {
        async { Ok(ListToolsResult::with_all_items(vec![apply_patch::tool()])) }
    }

    async fn call_tool(
        &self,
        request: CallToolRequestParams,
        _context: rmcp::service::RequestContext<rmcp::service::RoleServer>,
    ) -> Result<CallToolResponse, McpError> {
        let name = request.name;
        let arguments = request.arguments;

        match name.as_ref() {
            apply_patch::NAME => apply_patch::call(arguments).await,
            _ => Err(McpError::invalid_params(
                format!("unknown tool: {name}"),
                None,
            )),
        }
    }
}

async fn run_stdio() -> Result<()> {
    let service = McpServer
        .serve((tokio::io::stdin(), tokio::io::stdout()))
        .await?;
    service.waiting().await?;
    Ok(())
}

async fn run_http(port: u16) -> Result<()> {
    let service = rmcp::transport::streamable_http_server::StreamableHttpService::new(
        || Ok(McpServer),
        Arc::new(
            rmcp::transport::streamable_http_server::session::local::LocalSessionManager::default(),
        ),
        rmcp::transport::streamable_http_server::StreamableHttpServerConfig::default(),
    );
    let router = axum::Router::new().nest_service("/mcp", service);
    let listener = tokio::net::TcpListener::bind(("127.0.0.1", port)).await?;

    eprintln!("codex-mcp listening on http://127.0.0.1:{port}/mcp");
    axum::serve(listener, router).await?;
    Ok(())
}

struct CommandLineArgs {
    port: Option<u16>,
    show_help: bool,
}

fn parse_args() -> Result<CommandLineArgs> {
    let mut args = std::env::args().skip(1);
    let mut port = None;

    while let Some(arg) = args.next() {
        match arg.as_str() {
            "-p" | "--port" => {
                let value = args.next().ok_or_else(|| anyhow::anyhow!("缺少端口值"))?;
                port = Some(parse_port_value(&value)?);
            }
            "--help" | "-h" => {
                return Ok(CommandLineArgs {
                    port,
                    show_help: true,
                });
            }
            _ => bail!("未知参数: {arg}，使用 -h 查看帮助"),
        }
    }

    Ok(CommandLineArgs {
        port,
        show_help: false,
    })
}

fn parse_port_value(value: &str) -> Result<u16> {
    let port = value
        .parse::<u16>()
        .map_err(|error| anyhow::anyhow!("无效端口 {value:?}: {error}"))?;
    if port == 0 {
        bail!("端口必须在 1 到 65535 之间");
    }
    Ok(port)
}

#[tokio::main(flavor = "current_thread")]
async fn main() -> Result<()> {
    let args = parse_args()?;
    if args.show_help {
        println!(
            "用法: codex-mcp [-p|--port PORT]\n\n默认使用 stdio;指定端口后使用 Streamable HTTP: /mcp"
        );
        return Ok(());
    }

    match args.port {
        Some(port) => run_http(port).await,
        None => run_stdio().await,
    }
}
