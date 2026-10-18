# Connect an AI client to Flare

Flare has a remote HTTP MCP endpoint at **`https://<your-flare-host>/api/mcp`**.
Use HTTPS outside a local development environment. No local server or stdio
bridge is needed.

## Create a credential

1. Sign in and open **Settings → Connect your AI to Flare**.
2. A workspace administrator creates a separate **MCP token** for each client.
   Choose **Read-only** to investigate. **Read and triage** also permits AI
   triage and issue status updates; it does not grant workspace administration.
3. Choose 7, 30, or 90 days. The server defaults to 30 days and refuses an MCP
   lifetime above 90 days. Save the token when it is shown once.
4. Use **Test token** to check authenticated HTTP initialization and tool
   discovery without relying on your browser session.

MCP tokens belong to their workspace. They are stored as hashes and are valid
only at `/api/mcp`, not the REST management API or telemetry ingest routes.
Administrators can revoke them from the same page. Existing general API keys
retain their previous API and MCP compatibility; create a dedicated MCP token
for a new client.

These are workspace service credentials, not an OAuth delegation of the
creator's current login. Revocation and expiry bound their lifetime. Browser
OAuth consent is not implemented by this connection flow.

## Codex

Put the token in the **`FLARE_MCP_TOKEN`** environment variable using your secret
manager. For a temporary terminal session, a hidden prompt avoids placing the
value in shell history. In Bash or Zsh, run these lines, paste the token when
`read` is waiting, and press Enter:

```sh
read -r -s FLARE_MCP_TOKEN
export FLARE_MCP_TOKEN
```

Register the server, using your deployment's HTTPS hostname:

```sh
codex mcp add flare --url https://flare.cloudrebellion.tech/api/mcp --bearer-token-env-var FLARE_MCP_TOKEN
```

The equivalent configuration is:

```toml
[mcp_servers.flare]
url = "https://flare.cloudrebellion.tech/api/mcp"
bearer_token_env_var = "FLARE_MCP_TOKEN"
startup_timeout_sec = 20
tool_timeout_sec = 60
```

Only the environment variable's **name** belongs in the configuration. Do not
replace it with the token value. Launch Codex from the environment containing
the variable. An already running desktop app or IDE extension does not acquire
variables exported in a different terminal; reconnect or relaunch it from the
correct environment when convenient. Preserve existing Mesh and Stage entries
and their saved authorizations.

Check the new connection's tool list, then call `get_capabilities` and `overview`.
`get_capabilities` reports the resolved workspace and role without disclosing
credentials. A successful setup-page test does not prove that a separate client
has loaded its environment or connected.

Codex's environment-backed bearer configuration is documented in the
[official MCP documentation](https://developers.openai.com/codex/mcp/).

## Other clients

Choose **Streamable HTTP**, enter the endpoint, and configure
`Authorization: Bearer <token>` using that client's secret storage or supported
environment substitution. Environment syntax differs between clients. Do not
paste a token into a shared JSON file, a command recorded in shell history,
browser code, or chat.

## Connection failures

| Result | Check |
| --- | --- |
| 401 | The variable is present in the client process, the token is valid and unexpired, and it has not been revoked. An explicit invalid bearer never falls back to a dashboard cookie. |
| MCP tool permission error | Write refusals return a tool error inside the JSON-RPC response (HTTP 200). Use a read and triage token only when those actions are intended. Read-only tokens cannot run write tools. |
| 403 on HTTP origin | A browser must use the configured Flare origin. An additional trusted HTTPS alias must be explicitly listed in `FLARE_MCP_ALLOWED_ORIGINS`; do not use a wildcard. Non-browser clients normally omit Origin. |
| 429 | Respect `Retry-After`; the tenant MCP rate limit is shared. |
| No tools in an existing chat | Confirm configuration and process environment, then open a fresh connection. Do not revoke other services' credentials or interrupt unrelated chats. |

The MCP endpoint applies existing tenant checks, role gates, response scrubbing,
request limits, and usage accounting. This connection does not authorize an AI
to merge code, deploy applications, or read another workspace.
