install:
	cd $(NAME) && go mod tidy && go fmt ./... && go install -trimpath -buildvcs=false -ldflags '-s -w' .

install-codex-mcp:
	cargo build --release --locked --manifest-path codex/mcp/Cargo.toml --bin codex-mcp
	cp codex/mcp/target/release/codex-mcp $(HOME)/.cargo/bin/codex-mcp
