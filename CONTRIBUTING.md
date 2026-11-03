# Contributing to AIGate

Thanks for taking the time to contribute. AIGate is a self-hosted LLM gateway:
correctness, compatibility and a small operational surface matter more than new
features.

## Before you start

- Search existing issues and discussions first.
- For large changes, open an issue with the problem and proposed design before
  writing code. This avoids a PR that cannot be merged.
- Never include real API keys, tokens, customer data or internal endpoints in
  issues, tests or commits.

## Development

Requirements: Go 1.24+.

```bash
make build
make test
make vet
```

For an end-to-end check using Docker and a mock upstream:

```bash
./demo/quickstart.sh
```

## Pull requests

- Keep the change focused; one problem per PR.
- Add or update tests for behavior changes.
- Update `docs/` when the public API or configuration changes.
- Run `gofmt`, `go build ./...`, `go vet ./...` and `go test ./...` before
  opening the PR.
- Describe the problem, the approach and how you verified it. Include a
  reproduction or curl example when relevant.
- API and configuration changes should stay backward compatible within v0.x
  unless the change is explicitly called out in `CHANGELOG.md`.

## Commit messages

Use a short imperative subject, optionally with a Conventional Commits prefix:

```text
fix(proxy): preserve cache_control on Anthropic passthrough
feat(quota): support group-level global overrides
docs: add Dify integration guide
```

## Security

Please do not report vulnerabilities in public issues. See `SECURITY.md`.

## License

AIGate is licensed under Apache-2.0. By contributing, you agree that your
contribution is licensed under the same terms. See `LICENSE` and `NOTICE`.
