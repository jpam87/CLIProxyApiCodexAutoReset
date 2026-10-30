# Codex Auto Reset for CLIProxyAPI

Automatically redeem a Codex manual rate-limit reset credit **10 minutes before it expires**. This native CLIProxyAPI plugin reads the available credits, chooses the one with the earliest future expiration, and redeems that specific `credit_id`.

Multiple credits are supported. The plugin skips expired credits, credits without an expiration, unsupported reset types, and missing credit IDs. It stores a persistent request journal so retries use the same credit ID and idempotency key, then clears the initiating credential's routing cooldown after a confirmed reset.

## Requirements

- **Linux**, with a CLIProxyAPI build supporting native dynamic-library plugins. Tested against CLIProxyAPI `8.0.38-homelab-metadata-fix`, using C ABI 1 and RPC schema 6. Other builds need compatible host callbacks.
- **Go 1.24 or newer** and a C compiler such as GCC, with CGO enabled. Go 1.27.3 was used for validation. Build for the same OS and architecture as the proxy host.
- An enabled, file-backed **Codex OAuth account** in CLIProxyAPI. Runtime-only credentials cannot be loaded through the required `host.auth.get` callback.
- A persistent directory writable by the user running CLIProxyAPI.

On Debian or Ubuntu, install the compiler and build tools if needed:

```bash
sudo apt-get update
sudo apt-get install -y build-essential git make
```

Install a supported [Go toolchain](https://go.dev/doc/install), then check:

```bash
go version
gcc --version
```

## Build

```bash
git clone https://github.com/jpam87/CLIProxyApiCodexAutoReset.git
cd CLIProxyApiCodexAutoReset
CGO_ENABLED=1 make test build
```

The native library is created at `dist/codex-autoreset.so`. No separate Python worker or CrewAI runtime is required to run the plugin.

## Install

These examples assume CLIProxyAPI runs as your current user and is installed at `$HOME/cliproxyapi`. Adjust the paths and service name for your installation.

1. Stop CLIProxyAPI before replacing an existing plugin library. For a user systemd service:

   ```bash
   systemctl --user stop cliproxyapi.service
   ```

2. Copy the library and create the persistent state directory:

   ```bash
   CLIPROXY_DIR="$HOME/cliproxyapi"
   AUTO_RESET_STATE_DIR="$HOME/.local/state/codex-autoreset"

   mkdir -p "$CLIPROXY_DIR/plugins"
   install -m 0644 dist/codex-autoreset.so "$CLIPROXY_DIR/plugins/codex-autoreset.so"
   install -d -m 0700 "$AUTO_RESET_STATE_DIR"
   cp -p "$CLIPROXY_DIR/config.yaml" "$CLIPROXY_DIR/config.yaml.before-autoreset"
   ```

   For a system service, create these directories with ownership assigned to its service user.

3. Merge the following block into your existing CLIProxyAPI `config.yaml`. Replace both absolute paths with your actual paths. If a `plugins:` section already exists, merge into it instead of adding a duplicate section. Preserve other plugin configurations.

   ```yaml
   plugins:
     enabled: true
     dir: /absolute/path/to/cliproxyapi/plugins
     configs:
       codex-autoreset:
         enabled: true
         dry_run: true
         minutes_before_expiry: 10
         poll_seconds: 15
         request_timeout_seconds: 30
         state_file: /absolute/path/to/codex-autoreset/state.json
         accounts: []
   ```

   Use the state directory created in step 2. `state_file` must be absolute; this plugin does not expand `$HOME` or `~` in that value. [config.example.yaml](config.example.yaml) contains the equivalent configuration for automatic mode (`dry_run: false`).

4. Start CLIProxyAPI:

   ```bash
   systemctl --user start cliproxyapi.service
   ```

   Without systemd, restart your existing CLIProxyAPI process using its normal command, for example:

   ```bash
   /absolute/path/to/cli-proxy-api --config /absolute/path/to/config.yaml
   ```

5. Check the status endpoint below. With `dry_run: true`, the plugin identifies due credits without consuming them. Once the configuration is correct, change it to **`dry_run: false`** and restart CLIProxyAPI to enable automatic redemption.

## Check status

The plugin exposes a JSON resource at:

```text
/v0/resource/plugins/codex-autoreset/status
```

Use the address and authentication configured for your CLIProxyAPI instance. For example, after setting `CLIPROXY_URL` and `CLIPROXY_MANAGEMENT_KEY` in your environment:

```bash
curl --fail --silent --show-error \
  -H "Authorization: Bearer $CLIPROXY_MANAGEMENT_KEY" \
  "$CLIPROXY_URL/v0/resource/plugins/codex-autoreset/status"
```

Do not put real keys in source files or commit them. The response contains no access tokens:

```json
{
  "enabled": true,
  "dry_run": false,
  "minutes_before_expiry": 10,
  "version": "0.2.0",
  "accounts": {
    "example-auth-index": "waiting"
  }
}
```

Common account states:

| State | Meaning |
| --- | --- |
| `waiting` | The next eligible credit is outside the expiry margin. |
| `dry_run_due` | A credit is due, but simulation mode prevents redemption. |
| `no_available_credit` | No available Codex reset credits were listed. |
| `no_expiring_credit` | Available credits have no expiration to act on. |
| `redeemed` | The reset was confirmed and its routing cooldown was cleared. |
| `credential_unavailable` | The plugin could not read the required account credentials. |
| `credit_lookup_failed` | The credit lookup failed; the next poll will retry. |
| `consume_unconfirmed_retry_same_id` | The result is uncertain; retries preserve both IDs. |
| `redeemed_cooldown_pending` | Redemption succeeded; cooldown cleanup still needs to succeed. |
| `state_write_failed` | The journal could not be persisted; consumption is blocked. |
| `unconfirmed_expired_requires_review` | An uncertain attempt passed its original deadline; automatic consumption is blocked for that account. |
| `legacy_pending_requires_review` | A pre-0.2.0 attempt used a different payload and requires manual reconciliation. |

The status is a snapshot of the latest poll, not an audit log. Consult the journal and CLIProxyAPI logs when investigating an uncertain attempt.

## Configuration

All options below belong under `plugins.configs.codex-autoreset`:

| Option | Default | Description |
| --- | --- | --- |
| `enabled` | `false` | Starts or stops the plugin worker. Global `plugins.enabled` must also be true. |
| `dry_run` | `true` | Identifies due credits without sending consume requests. |
| `minutes_before_expiry` | `10` | Expiry margin, from 1 to 60 minutes. |
| `poll_seconds` | `15` | Poll interval, from 1 to 60 seconds. |
| `request_timeout_seconds` | `30` | HTTP timeout, from 1 to 120 seconds. |
| `state_file` | `$HOME/.local/state/codex-autoreset/state.json` | Default uses the service user's home directory. An explicit value must be absolute. |
| `accounts` | `[]` | Allowed CLIProxyAPI `auth_index` values; empty selects every enabled Codex credential. |

Disabled credentials are skipped. Aliases of the same upstream account are deduplicated for consumption. Only the credential that initiated a redemption is targeted for cooldown recovery; a historical reset never clears a newly added alias's cooldown.

## How credit selection works

Each poll reads `/backend-api/wham/rate-limit-reset-credits`. The plugin selects an available `codex_rate_limits` credit with a nonempty ID and the earliest future expiration. Equal expiration times are ordered by credit ID. On the first poll inside the margin, it persists a request UUID and sends:

```json
{
  "redeem_request_id": "persistent-request-uuid",
  "credit_id": "selected-credit-id"
}
```

This targets a specific credit even when others are available. The official [Codex backend client](https://github.com/openai/codex/blob/f752c97f7d0be0cbef5cece327f52a16ad6fb1c6/codex-rs/backend-client/src/client/rate_limit_resets.rs) supports this field, and its [selected-credit forwarding test](https://github.com/openai/codex/blob/f752c97f7d0be0cbef5cece327f52a16ad6fb1c6/codex-rs/app-server/tests/suite/v2/rate_limit_reset_credits.rs) verifies the payload. The plugin does not rely on the server's default credit order and never falls back to a request without `credit_id`.

Only `reset` or `already_redeemed` confirms consumption. After a transport failure, retries use the original request UUID and credit ID while the credit is still valid. An expired unresolved attempt blocks further automatic consumption for the account. Pre-0.2.0 unconfirmed attempts also require review because adding `credit_id` would change their original payload.

One credit is attempted per upstream account per poll. With the default interval, a redemption normally starts between 10 minutes and 9 minutes 45 seconds before expiry; slow requests, multiple accounts, or service downtime can delay it. The plugin cannot redeem an expired credit or force a server to accept a reset when it reports nothing to reset.

## Persistent state and upgrades

The journal is written atomically, fsynced before consumption, restricted to mode `0600`, and protected by an exclusive file lock. It contains account/credit IDs, request UUIDs, deadlines, and confirmation flags. It never stores access tokens.

**Keep the journal across restarts and upgrades.** Deleting it while an attempt is unresolved loses the retry identity. Use one active plugin per upstream account; a file lock only coordinates instances sharing the same state file.

To upgrade, stop the service, build the new library, replace the `.so`, and restart with the existing `state_file`. To disable automatic consumption, set `dry_run: true`; to stop the worker, set its `enabled` to `false`. Restart CLIProxyAPI after either change. The state file can remain in place.

Credentials are read in memory through `host.auth.get`. Requests use fixed `chatgpt.com` URLs through host HTTP callbacks. This integrates with the Codex backend contract rather than a separately versioned public API, so upstream changes may require a plugin update.

## Development and verification

```bash
go test -race -count=1 ./...
go vet ./...
make build
```

Tests use fake host callbacks without account network calls. They cover expiry boundaries, multiple-credit selection, payload IDs, restart recovery, changed inventories, state-write failures, account identity, and nonblocking status reads.

An optional integration check loads the built plugin in a temporary instance of your installed CLIProxyAPI binary, with an empty authentication directory and simulation enabled:

```bash
python3 scripts/smoke_host.py --host /absolute/path/to/cli-proxy-api
```

Python 3 is required only for this check. It never redeems real credits, although CLIProxyAPI may perform its normal public version checks. See [VALIDATION.md](VALIDATION.md) for recorded validation and protocol evidence. Live account redemption has not been exercised in the recorded tests.

The C ABI declarations were adapted from CLIProxyAPI's MIT-licensed examples. The upstream license is retained in [LICENSE.upstream](LICENSE.upstream).
