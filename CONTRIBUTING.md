# Contributing

Thanks for your interest. Bug reports, fixes, and docs improvements are
welcome. For a new feature, please open an issue first, so we can agree on
the scope before you write the code.

## Build and test

You need Go 1.25 on Linux or macOS. The test suite is run on Linux; CI only
checks that the macOS build compiles.

```sh
make build    # bin/sessionhub
make test     # go test ./...
make lint     # gofmt and go vet
```

The tests need no network and no running services: they use temp `HOME`
directories, an in-process server, and fake `ssh`, `herdr`, and `claude`
scripts. CI runs `make lint` and `make build` on every pull request; run `make test`
yourself before you push.

[`ARCHITECTURE.md`](ARCHITECTURE.md) explains how the pieces fit.

## What a good change looks like

[`docs/dev/DEFINITION-OF-DONE.md`](docs/dev/DEFINITION-OF-DONE.md) is the
full checklist the project is built against. In short:

- **Fail open.** Nothing in the hooks, the MCP server, or the herdr plugin
  may block or fail Claude Code or herdr. A write that can't reach the server
  goes to the offline queue.
- **Tests cover the whole path,** including the wrong ones: a bad token, an
  unknown session, an unreachable server, a malformed payload.
- **Don't invent external APIs.** Claude Code hook fields and herdr events
  come from captured payloads in `testdata/`, not from memory.
  `testdata/README.md` says how to capture new ones.
- **Docs change with the code.** A new command, config key, path, or
  endpoint is documented in `docs/` in the same pull request.
- **Small commits,** one concern each, with the reason in the message body.

## Reporting security problems

Please don't open a public issue. See [`SECURITY.md`](SECURITY.md).
