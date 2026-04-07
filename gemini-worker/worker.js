/**
 * Cloudflare Worker — Gemini API proxy
 *
 * Forwards requests to generativelanguage.googleapis.com so the bot
 * can reach Gemini from geo-restricted locations.
 *
 * Environment variables (set in Cloudflare dashboard → Worker → Settings → Variables):
 *   WORKER_SECRET  — any random string; bot must send it in X-Worker-Secret header.
 *                    If not set, the worker accepts all requests (not recommended).
 *
 * Deploy:
 *   1. Go to https://dash.cloudflare.com → Workers & Pages → Create Worker
 *   2. Paste this file, click Deploy
 *   3. Copy the worker URL (e.g. https://gemini-proxy.yourname.workers.dev)
 *   4. Set WORKER_SECRET in Worker settings
 *   5. Put the same values in your bot's .env
 */

export default {
  async fetch(request, env) {
    // Auth check
    if (env.WORKER_SECRET) {
      const incoming = request.headers.get('X-Worker-Secret');
      if (incoming !== env.WORKER_SECRET) {
        return new Response('Unauthorized', { status: 401 });
      }
    }

    // Rewrite URL: worker domain → googleapis.com
    const url = new URL(request.url);
    const target = 'https://generativelanguage.googleapis.com' + url.pathname + url.search;

    // Forward without the auth header (don't leak it to Google)
    const headers = new Headers(request.headers);
    headers.delete('X-Worker-Secret');

    const response = await fetch(target, {
      method: request.method,
      headers,
      body: request.body,
    });

    return new Response(response.body, {
      status: response.status,
      headers: response.headers,
    });
  },
};
