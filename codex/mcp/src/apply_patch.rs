use std::borrow::Cow;
use std::sync::Arc;

use codex_utils_absolute_path::AbsolutePathBuf;
use codex_utils_path_uri::PathUri;
use rmcp::ErrorData as McpError;
use rmcp::model::CallToolResponse;
use rmcp::model::CallToolResult;
use rmcp::model::JsonObject;
use rmcp::model::Tool;
use serde::Deserialize;
use serde_json::Value;

pub(crate) const NAME: &str = "apply_patch";

const TOOL_DESCRIPTION: &str = r#"Apply one or more Codex patches to local files.

Pass the complete patch text in `patch`. Do not pass a shell command, an argument array, or a JSON wrapper.

Patch format:

*** Begin Patch
*** Add File: path/to/file
+new line
*** Delete File: path/to/file
*** Update File: path/to/file
*** Move to: path/to/new-file
@@ optional function or class context
 context line
-old line
+new line
*** End of File
*** End Patch

Rules:
- The patch must start with `*** Begin Patch` and end with `*** End Patch`.
- A patch may contain multiple file operations.
- Every operation must have an `*** Add File:`, `*** Delete File:`, or `*** Update File:` header.
- Add lines start with `+`; removed lines start with `-`; unchanged context lines start with a space.
- Use about three context lines before and after a change. Add `@@` context when needed to identify the correct location.
- Use `*** Move to:` immediately after an update header to rename a file.
- File paths are resolved relative to `cwd`.
- `cwd` is optional; when omitted, the server process working directory is used.

The operation is applied atomically: if any requested change fails, no change is committed."#;

#[derive(Debug, Deserialize)]
struct Arguments {
    patch: String,
    cwd: Option<String>,
}

pub(crate) fn tool() -> Tool {
    let schema: JsonObject = serde_json::from_value(serde_json::json!({
        "type": "object",
        "properties": {
            "patch": {
                "type": "string",
                "description": "Complete Codex patch text, including `*** Begin Patch` and `*** End Patch` markers."
            },
            "cwd": {
                "type": "string",
                "description": "Optional absolute working directory used to resolve relative patch paths."
            }
        },
        "required": ["patch"],
        "additionalProperties": false
    }))
    .expect("apply_patch MCP schema must be valid");

    Tool::new(
        Cow::Borrowed(NAME),
        Cow::Borrowed(TOOL_DESCRIPTION),
        Arc::new(schema),
    )
}

pub(crate) async fn call(arguments: Option<JsonObject>) -> Result<CallToolResponse, McpError> {
    let arguments = parse_arguments(arguments)?;
    let cwd = resolve_cwd(arguments.cwd.as_deref())?;
    let mut stdout = Vec::new();
    let mut stderr = Vec::new();

    match codex_apply_patch::apply_patch(
        &arguments.patch,
        &cwd,
        &mut stdout,
        &mut stderr,
        &codex_apply_patch::LOCAL_FS,
        None,
    )
    .await
    {
        Ok(_) => Ok(
            CallToolResult::success(vec![rmcp::model::ContentBlock::text(
                String::from_utf8_lossy(&stdout).into_owned(),
            )])
            .into(),
        ),
        Err(error) => {
            let mut message = String::from_utf8_lossy(&stderr).into_owned();
            if !message.is_empty() {
                message.push('\n');
            }
            message.push_str(&error.to_string());
            Ok(CallToolResult::error(vec![rmcp::model::ContentBlock::text(message)]).into())
        }
    }
}

fn parse_arguments(arguments: Option<JsonObject>) -> Result<Arguments, McpError> {
    let arguments = arguments
        .ok_or_else(|| McpError::invalid_params("missing arguments for apply_patch", None))?;
    serde_json::from_value(Value::Object(arguments.into_iter().collect()))
        .map_err(|error| McpError::invalid_params(error.to_string(), None))
}

fn resolve_cwd(cwd: Option<&str>) -> Result<PathUri, McpError> {
    let cwd = match cwd {
        Some(cwd) => AbsolutePathBuf::from_absolute_path(cwd),
        None => AbsolutePathBuf::current_dir(),
    }
    .map_err(|error| McpError::invalid_params(format!("invalid cwd: {error}"), None))?;

    Ok(PathUri::from_abs_path(&cwd))
}
