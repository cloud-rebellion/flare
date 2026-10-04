# Flare dashboard browser source-map canary

Use this manual check after deploying Flare's own dashboard. It reports one
fixed, handled `FlareBrowserCanary` error per browser page through the normal
Flare browser sender and its existing rate limit. Event detail is ignored.

1. Confirm the deployed image and exact browser release (`PUBLIC_FLARE_RELEASE`,
   including its full `+g<commit>` suffix), the dashboard's public DSN, and the
   receiving Flare project. Upload that build's private JavaScript maps to the
   same project and release; confirm valid artifact receipts. Keep maps outside
   the public build.
2. Open a fresh dashboard `/login` page on the intended origin:
   `https://flare.example.com` or
   `https://flare.cloudrebellion.tech`. In DevTools Sources, find the fixed text
   `Flare dashboard browser source-map canary v1` in a loaded
   `/_app/immutable/` JavaScript asset. In Network, confirm that asset loaded.
   The bridge needs this exact script URL in `document.scripts` or a
   `performance` resource entry initiated by `script` or `link`. The two UI
   origins have different opaque asset names even for the same path.
3. With the Network panel open, run in that page's Console:

   ```js
   window.dispatchEvent(new Event('flare:browser-canary'));
   ```

   The page stays usable. A second dispatch in the same page sends nothing;
   reload for another attempt. Check for one request to the configured Flare
   `/api/<project-id>/events` endpoint. An exhausted sender budget can consume
   the once-per-page attempt, so use a fresh page after the budget resets.
4. In the receiving Flare project, open **Issues** and find the latest
   `FlareBrowserCanary` event with that fixed message. Confirm its receive time,
   `/login` request URL, and exact release. Grouping may reuse an older issue.
   Confirm a **source-mapped** stack frame resolves to the canary handler in
   `src/lib/flare-client.ts`. The matching artifact name is
   `flare-asset-sha256:<SHA-256 of loaded script origin + pathname>`; the
   browser sends that opaque name instead of the raw script URL.

A queued browser request and a valid map upload receipt do not prove live
delivery or frame resolution. Record the Flare event/ingest receipt and the
resolved frame from the deployed release before calling this accepted. If the
frame is `unknown`, check loaded-script evidence and the exact UI origin. If
it has an opaque name but is unmapped, compare the image, browser release,
artifact release and name, and private map from the same build. Do not put
DSNs, request URLs, or payloads in shared notes.
