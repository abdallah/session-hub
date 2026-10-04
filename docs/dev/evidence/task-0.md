# Task 0 evidence

Toolchain: `go version go1.25.6 linux/amd64`. sqlite is pinned to v1.50.0 because v1.60.1 requires Go 1.26.

## `make build`

```
$ make build
CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=1b47678" -o bin/sessionhub ./cmd/sessionhub
[exit 0]
```

## `make test`

```
$ make test
go test ./...
ok  	session-hub/cmd/sessionhub	0.004s
?   	session-hub/internal/api	[no test files]
?   	session-hub/internal/cli	[no test files]
?   	session-hub/internal/hooks	[no test files]
?   	session-hub/internal/join	[no test files]
?   	session-hub/internal/mcp	[no test files]
ok  	session-hub/internal/paths	0.004s
?   	session-hub/internal/plugin	[no test files]
?   	session-hub/internal/resume	[no test files]
?   	session-hub/internal/server	[no test files]
[exit 0]
```

## `make lint`

```
$ make lint
test -z "$(gofmt -l .)"
go vet ./...
[exit 0]
```

## `./bin/sessionhub help`

```
$ ./bin/sessionhub help
usage: sessionhub <command> [args]

commands:
  server                        run the sessionhub server
  machine add|rm|ls             manage machines (server side)
  plugin startup|event|watch    herdr plugin entry points
  plugin open-picker            open the session picker
  install-plugin                install the herdr plugin
  uninstall-plugin              remove the herdr plugin
  hook <event>                  Claude Code hook handler (always exits 0)
  install-hooks                 add sessionhub hooks to Claude Code settings
  uninstall-hooks               remove sessionhub hooks
  mcp                           run the MCP server
  install-mcp                   register the MCP server with Claude Code
  uninstall-mcp                 remove the MCP registration
  ls                            list sessions
  show <id>                     show one session
  status                        show sessionhub connection status
  resume <id>                   resume a session on this machine
  join                          enroll this machine
  version                       print the version
  help                          print this help
[exit 0]
```

## `./bin/sessionhub ls`

```
$ ./bin/sessionhub ls
ls: not implemented yet
[exit 1]
```

## `./bin/sessionhub hook stop </dev/null`

```
$ ./bin/sessionhub hook stop </dev/null
hook: not implemented yet
[exit 0]
```

## `./bin/sessionhub version`

```
$ ./bin/sessionhub version
sessionhub 1b47678
[exit 0]
```
