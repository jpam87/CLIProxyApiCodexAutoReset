# Validation and execution record — 2026-10-30

## Scope

Native CLIProxyAPI plugin that automatically consumes an available Codex manual reset credit within ten minutes of its expiry. Linux amd64 artifact, tested against the installed `8.0.38-homelab-metadata-fix` binary in a temporary instance with an empty credential directory. Production configuration was not changed and no real reset was consumed.

## Architecture and source evidence

The native library registers ABI 1 / RPC schema 6 and starts one background worker after `plugin.register`. Lifecycle reconfiguration stops and joins the existing worker before replacement. `plugin.quiesce` and native shutdown stop the worker. Credentials are obtained through host callbacks, used only in memory, and excluded from status and journal output.

State is written atomically and fsynced before every consume attempt. The same UUID is reused after failures. Pending requests are bound to the upstream account and original credit expiry; recovery replays both the selected `credit_id` and the original request UUID even if inventory changes. Once an unconfirmed attempt has expired, the account stays blocked with a review-required status. Preserve its journal for investigation; deleting it destroys the retry guarantee.

The consume endpoint is the one already used by the installed management UI. Unlike that UI, this release always sends an explicit `credit_id`. The available credit with the earliest future expiry is selected locally. Multiple credits are supported; expired, non-expiring, wrong-type and missing-ID credits are excluded. No automatic server ordering assumption is needed, and there is no untargeted fallback. Operate one active plugin per account; the file lock prevents local instances from sharing a journal concurrently.

Pinned selected-credit evidence (official Codex commit `f752c97f7d0be0cbef5cece327f52a16ad6fb1c6`):

- https://github.com/openai/codex/blob/f752c97f7d0be0cbef5cece327f52a16ad6fb1c6/codex-rs/backend-client/src/client/rate_limit_resets.rs — optional `credit_id` request field and by-ID method, for both API path styles.
- https://github.com/openai/codex/blob/f752c97f7d0be0cbef5cece327f52a16ad6fb1c6/codex-rs/backend-client/src/client/rate_limit_resets_tests.rs — exact serialized by-ID request body.
- https://github.com/openai/codex/blob/f752c97f7d0be0cbef5cece327f52a16ad6fb1c6/codex-rs/app-server/tests/suite/v2/rate_limit_reset_credits.rs — `consume_account_rate_limit_reset_credit_forwards_selected_credit_id`.
- https://github.com/openai/codex/blob/f752c97f7d0be0cbef5cece327f52a16ad6fb1c6/codex-rs/tui/src/chatwidget/reset_credits.rs — credit options sorted by expiry, carrying selected IDs.

This establishes the supported client contract, not a completed live redemption test. Default backend ordering when `credit_id` is omitted remains unverified and is not used. Older unconfirmed journals without the `explicit_credit` flag require review; their original payload contract is not silently changed.

Pinned ABI evidence:

- https://github.com/router-for-me/CLIProxyAPI/blob/v8.0.38/sdk/pluginabi/types.go
- https://github.com/router-for-me/CLIProxyAPI/blob/v8.0.38/internal/pluginhost/host_callbacks.go
- https://github.com/router-for-me/CLIProxyAPI/blob/v8.0.38/internal/pluginhost/host_callbacks_unix.go
- https://github.com/router-for-me/CLIProxyAPI/blob/v8.0.38/examples/plugin/host-callback-auth-files/go/main.go

The C ABI declarations were adapted from the upstream MIT-licensed example; its license is preserved in `LICENSE.upstream`. Plugin metadata points to this repository.

## Checks

```bash
go test -race -count=1 ./...
go vet ./...
go build -buildmode=c-shared -o dist/codex-autoreset.so .
python3 scripts/smoke_host.py --host /absolute/path/to/cli-proxy-api
```

Offline tests inject a fake host; no provider or account network calls occur in them. They exercise expiry boundaries, dry run, malformed and multiple credits, persistent request identity after a lost response and restart, changed inventories, cooldown identity, write failures and invalid journals. The host smoke test uses the real installed CLIProxyAPI binary with an empty auth directory and dry run; the host itself may perform its usual public version checks. It validates loading and status, not an actual upstream credit redemption.

## 0.2.0 investigation and update

The previous one-credit restriction was based on an incomplete inference from the proxy UI. Public official client code confirms explicit selection. Version 0.2.0 sends the selected credit ID, preserves it across retries, and blocks unconfirmed legacy attempts rather than changing their original request payload.

New offline regression cases cover unsorted multiple credits, expired/non-expiring exclusions, status/type filters, deterministic ties, all-future inventories, stable selected IDs after lost responses and inventory changes, HTTP 400 without untargeted fallback, and the legacy-journal guard.
