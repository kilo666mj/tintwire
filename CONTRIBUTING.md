# Contributing

Issues and pull requests are welcome. Please keep changes focused and include
tests for behavior changes.

Before submitting a pull request, run:

```sh
go test ./...
go test -race ./...
go vet ./...
test -z "$(gofmt -l .)"
node --test internal/server/webtest/*.js
cargo check --manifest-path desktop/src-tauri/Cargo.toml --locked
cargo fmt --manifest-path desktop/src-tauri/Cargo.toml --all -- --check
```

PostgreSQL integration tests additionally use `TINTWIRE_TEST_POSTGRES_DSN`;
the CI workflow shows the two isolated databases expected by those tests.

Never commit real webhook tokens, passwords, signing keys, push endpoints,
notification payloads, internal hostnames, deployment inventories, or database
files. Use `example.com`, `example.invalid`, and synthetic data in tests and
documentation.
