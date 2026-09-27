# Web Search plugin

The built-in `core:web_search` plugin exposes the host-owned `web_search` tool
through Exa's Search API (`https://api.exa.ai/search`). The plugin is disabled
until an Exa API key is configured and the user enables it in the unified
plugin center.

## Free tier and billing

Exa currently advertises $20 in initial credits plus $10 in monthly credits on
its Starter plan, with no payment method required. Its pricing page currently
lists Search starting at $7 per 1,000 requests for up to 10 results. The plugin
requests query highlights, so check Exa's current pricing for any content-based
charges. Exa's prices and free credits can change; see
https://exa.ai/pricing. The host does not set up an account or enable a paid
plan. If credits run out, the API may reject requests or charge according to
the user's Exa account and payment settings.

The plugin limits each agent search to 10 results and caps the response body at
2 MiB. Its connection check submits one real search query and consumes the
search request and corresponding content credits. The UI discloses this before
the check.

## Data and permissions

The search query is sent to Exa over HTTPS. Do not include secrets, API keys,
passwords, or private user content in a search query. Results are untrusted
external content and the agent should cite relevant result URLs in its answer.
The tool is classified as `external_read`, so turn permission policy applies
before the request is made.

The API key is encrypted with the host vault and stored in the local
`runtime_settings` database. The UI receives only the configured flag and a
masked key hint. Removing a key requires the plugin to be disabled first.

## Configuration

1. Create an Exa account and API key at https://dashboard.exa.ai.
2. In the plugin center, open **联网搜索**, save the key, then enable the plugin.
3. Optionally use **测试连接**. This performs one billable/free-credit search.

The API settings contract is `GET`, `PUT`, and `DELETE
/api/v1/plugins/web-search/settings`; `POST /api/v1/plugins/web-search/test`
performs the connection check. API key values are accepted only by `PUT` and
are never returned by any route.
