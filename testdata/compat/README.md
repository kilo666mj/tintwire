# Compatibility fixtures

These fixtures are synthetic, redacted examples for the three concrete MVP
integration contracts. They contain no production identifiers or credentials.

- `release-list-native.json` exercises every native rich-card component used by
  a release-summary migration.
- `approval-service-post.json` captures the bounded Mattermost attachment-action
  shape. Its callback context is deliberately redacted; end-to-end encryption
  and callback behavior are covered by `TestMattermostBotCompatibilityBridge`.
- `slash-command-import.json` documents the strict administrative import shape.
  Dispatch, actor/channel authority, immediate and delayed responses, and
  idempotency are covered by `TestSlashCommandCompatibility`.

The interactive rendering reference remains
`docs/mockups/release-list-rich-notification.html`.
