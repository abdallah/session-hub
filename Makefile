VERSION ?= $(shell git describe --always --dirty)

# The server host for `make deploy`, an SSH destination. There is no default:
# run `make deploy DEPLOY_HOST=myserver`.
DEPLOY_HOST ?=

.PHONY: build test lint install clean deploy

build:
	CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$(VERSION)" -o bin/sessionhub ./cmd/sessionhub

test:
	go test ./...

lint:
	test -z "$$(gofmt -l .)"
	go vet ./...

install: build
	mkdir -p $(HOME)/.local/bin
	install -m 0755 bin/sessionhub $(HOME)/.local/bin/sessionhub

clean:
	rm -rf bin

# deploy builds a static linux/amd64 binary, copies it and the unit to
# DEPLOY_HOST, and restarts the server. server.toml must exist there first
# (see README.md). Before it replaces the binary, it backs up the live
# database with sqlite3 .backup, which is safe under WAL, and keeps the three
# newest sessionhub.db.bak-* files. The binary is copied to sessionhub.new and renamed, so a running
# server or watcher keeps its old inode until it restarts. The plugin watcher
# does not restart with the server, so the last step reruns install-plugin,
# which stops the old watcher and starts one on the new binary; it is skipped
# until `sessionhub join` has written the client config. Non-interactive ssh has no
# ~/.local/bin in PATH, where herdr is installed, hence the PATH prefix.
deploy:
	@test -n "$(DEPLOY_HOST)" || { echo 'set DEPLOY_HOST to the server'\''s SSH host, e.g. make deploy DEPLOY_HOST=myserver'; exit 1; }
	GOOS=linux GOARCH=amd64 $(MAKE) build
	ssh $(DEPLOY_HOST) 'mkdir -p ~/.local/bin ~/.config/systemd/user'
	scp bin/sessionhub $(DEPLOY_HOST):.local/bin/sessionhub.new
	scp deploy/sessionhub.service $(DEPLOY_HOST):.config/systemd/user/sessionhub.service
	ssh $(DEPLOY_HOST) 'db=$$HOME/.local/share/sessionhub/sessionhub.db; test ! -f $$db || { sqlite3 $$db ".backup $$db.bak-$$(date +%Y%m%dT%H%M%S)" && ls -1t $$db.bak-* | tail -n +4 | xargs -r rm -f; }'
	ssh $(DEPLOY_HOST) 'chmod 0755 ~/.local/bin/sessionhub.new && mv -f ~/.local/bin/sessionhub.new ~/.local/bin/sessionhub'
	ssh $(DEPLOY_HOST) 'systemctl --user daemon-reload && systemctl --user enable --now sessionhub && systemctl --user restart sessionhub'
	ssh $(DEPLOY_HOST) 'for i in 1 2 3 4 5; do curl -fsS http://127.0.0.1:8787/healthz && exit 0; sleep 1; done; systemctl --user --no-pager status sessionhub; exit 1'
	ssh $(DEPLOY_HOST) 'if [ -f ~/.config/sessionhub/config.toml ]; then PATH=$$HOME/.local/bin:$$PATH ~/.local/bin/sessionhub install-plugin; else echo "no client config yet: skipping install-plugin (run sessionhub join)"; fi'
	ssh $(DEPLOY_HOST) '~/.local/bin/sessionhub version'
