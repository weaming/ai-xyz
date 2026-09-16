install-go:
	cd $(NAME) && go mod tidy && go fmt ./... && go install -trimpath -buildvcs=false -ldflags '-s -w' .

install-codex-mcp:
	cargo install --locked --path codex/codex-mcp --force

install-codex-responses-api:
	cargo install --locked --path codex/codex-responses-api --force
