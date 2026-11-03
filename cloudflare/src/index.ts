/*
 * Ask Sauby · MyFlipKart.com — Cloudflare Worker in front of the Go container
 * Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
 *
 * Request path:  browser → Cloudflare edge (TLS, WAF, rate limits) → this Worker → container :8080
 *
 * Optional secrets (wrangler secret put …):
 *   WORKERS_AI_TOKEN   Cloudflare API token with "Workers AI" permission → enables the LLM path
 *   CF_ACCOUNT_ID      your account id (used to build the Workers AI URL)
 *   WORKERS_AI_MODEL   override the model (default below)
 * Without them the demo runs the rule router: free, deterministic, no LLM spend.
 */
import { Container, getContainer } from "@cloudflare/containers";

interface Env {
  SAUBY: DurableObjectNamespace<Sauby>;
  WORKERS_AI_TOKEN?: string;
  CF_ACCOUNT_ID?: string;
  WORKERS_AI_MODEL?: string;
}

const DEFAULT_MODEL = "@cf/meta/llama-3.3-70b-instruct-fp8-fast";

export class Sauby extends Container<Env> {
  defaultPort = 8080;
  sleepAfter = "30m"; // idle demo sleeps; next visit starts fresh (ephemeral disk = automatic reset)
  enableInternet = true; // needed only to reach Workers AI

  constructor(ctx: ConstructorParameters<typeof Container<Env>>[0], env: Env) {
    super(ctx, env);
    const vars: Record<string, string> = { SIM_AGENTS: "1", SIM_JOIN_AFTER_SEC: "6" };
    if (env.WORKERS_AI_TOKEN && env.CF_ACCOUNT_ID) {
      // Workers AI speaks the OpenAI-compatible API the gateway already uses.
      vars.INTERNAL_LLM_URL = `https://api.cloudflare.com/client/v4/accounts/${env.CF_ACCOUNT_ID}/ai/v1`;
      vars.INTERNAL_LLM_MODEL = env.WORKERS_AI_MODEL || DEFAULT_MODEL;
      vars.INTERNAL_LLM_API_KEY = env.WORKERS_AI_TOKEN;
      vars.ROUTER = "auto";
      vars.DAILY_TOKEN_QUOTA = "100000"; // per demo account per day: caps spend
    } else {
      vars.ROUTER = "rules";
    }
    this.envVars = vars;
  }
}

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    // Tell the app the public scheme so cookies are Secure and OIDC URLs use https.
    const url = new URL(request.url);
    const headers = new Headers(request.headers);
    headers.set("X-Forwarded-Proto", url.protocol.replace(":", ""));
    const forwarded = new Request(request, { headers });
    return getContainer(env.SAUBY, "demo").fetch(forwarded);
  },
};
