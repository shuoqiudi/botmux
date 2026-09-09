# Contributing to Telegram Gateway (it_telegram)

Telegram Gateway is maintained here as an independent application based on BotMux. See [repository identity and verification](docs/repository-rename.md).

## Getting Started

1. Fork the repository
2. Clone your fork: `git clone https://github.com/<YOUR_USERNAME>/it_telegram.git`
3. Create a feature branch: `git checkout -b my-feature`
4. Make your changes
5. Build and test: `go build -o botmux . && go test -p 1 -parallel 1 ./...`
6. Commit your changes with a descriptive message
7. Push to your fork and open a Pull Request

## Development Setup

```bash
# Build (no CGO required)
go build -o botmux .

# Run in demo mode for development
./botmux -demo

# Run tests
go test -p 1 -parallel 1 ./...
```

## Project Structure

This is a monolithic Go application — all source files are in `package main`. See the Architecture section in [README.md](README.md#architecture) for a detailed breakdown.

## Guidelines

- **Keep it simple** — BotMux is a single-binary app. Avoid adding unnecessary dependencies.
- **No CGO** — all dependencies must be pure Go to maintain easy cross-compilation.
- **Test your changes** — external-behavior and end-to-end suites live in
  `tests/`. Narrow white-box unit and Redis contract tests that need access to
  unexported package seams are colocated as `*_test.go` beside their package;
  do not export production internals solely to move those tests.
- **Update documentation** — if your change affects usage, update README.md and/or the Mintlify docs.
- **Frontend is vanilla JS** — the SPA in `templates/index.html` uses no frameworks. Keep it that way.
- **i18n** — if you add user-facing strings, add both English and Russian translations to the `i18n` object.

## Reporting Issues

- Use [GitHub Issues](https://github.com/shuoqiudi/it_telegram/issues) for bug reports and feature requests.
- Include steps to reproduce, expected vs actual behavior, and your environment (OS, Go version, Docker).

## Code Style

- Follow standard Go conventions (`gofmt`).
- Keep commits focused — one logical change per commit.
- Write clear commit messages describing *why*, not just *what*.

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).

## Gateway delivery checks

Use `ee/telegram_gateway/test.sh` after committing a candidate. It builds from
this checkout, runs packaging and process checks with synthetic credentials,
then runs the Go suite serially with isolated Redis/AOF. `smoke.sh` retains the
four original public-interface stages. Python 3 (standard library), Git, Docker,
Compose, curl and flock are required; a host Go installation is optional.
See [migration provenance and validation](docs/gateway-packaging-validation.md).
