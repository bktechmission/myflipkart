/*
 * Ask Sauby · MyFlipKart.com — shared browser helpers
 * Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
 *
 * API calls (with the X-Sauby CSRF header the server requires), Server-Sent Events
 * parsing, HTML escaping, a tiny markdown renderer, inline SVG icons and bar charts.
 */

export const $ = <T extends HTMLElement = HTMLElement>(sel: string, root: ParentNode = document): T =>
  root.querySelector(sel) as T;

export const esc = (s: unknown): string =>
  String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]!));

export class ApiError extends Error {
  constructor(public status: number, message: string) { super(message); }
}

export async function api<T = any>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch(path, {
    ...init,
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", "X-Sauby": "1", ...(init.headers || {}) },
  });
  if (res.status === 401) { location.href = "/login"; throw new ApiError(401, "sign in required"); }
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(res.status, body.error || res.statusText);
  return body as T;
}

/** POST that returns Server-Sent Events: calls onEvent(name, data) for each event. */
export async function postSSE(path: string, payload: unknown, onEvent: (event: string, data: any) => void): Promise<void> {
  const res = await fetch(path, {
    method: "POST", credentials: "same-origin",
    headers: { "Content-Type": "application/json", "X-Sauby": "1" },
    body: JSON.stringify(payload),
  });
  if (res.status === 401) { location.href = "/login"; return; }
  if (!res.ok || !res.body) {
    const b = await res.json().catch(() => ({}));
    throw new ApiError(res.status, b.error || res.statusText);
  }
  if ((res.headers.get("Content-Type") || "").includes("application/json")) {
    onEvent("live", await res.json()); // a person owns this chat: no stream, just the live state
    return;
  }
  const reader = res.body.getReader();
  const dec = new TextDecoder();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += dec.decode(value, { stream: true });
    let i: number;
    while ((i = buf.indexOf("\n\n")) >= 0) {
      const block = buf.slice(0, i);
      buf = buf.slice(i + 2);
      let ev = "message", data = "";
      for (const line of block.split("\n")) {
        if (line.startsWith("event: ")) ev = line.slice(7);
        else if (line.startsWith("data: ")) data += line.slice(6);
      }
      try { onEvent(ev, JSON.parse(data)); } catch { /* ignore malformed */ }
    }
  }
}

/** Minimal markdown: **bold**, "- " lists, paragraphs; product/order IDs become links/chips. */
export function md(text: string): string {
  let html = "", inList = false;
  for (let line of esc(text).split("\n")) {
    line = line.replace(/\*\*(.+?)\*\*/g, "<b>$1</b>")
      .replace(/\b(P\d{4})\b/g, '<a href="/p/$1" target="_blank" rel="noopener">$1</a>');
    if (/^\s*[-*] /.test(line)) {
      if (!inList) { html += "<ul>"; inList = true; }
      html += "<li>" + line.replace(/^\s*[-*] /, "") + "</li>";
    } else {
      if (inList) { html += "</ul>"; inList = false; }
      if (line.trim()) html += "<p>" + line + "</p>";
    }
  }
  return html + (inList ? "</ul>" : "");
}

export const money = (n: number) => "$" + (Math.round(n * 100) / 100).toFixed(n % 1 ? 2 : 0);
export const pct = (x: number) => (100 * (x || 0)).toFixed(1) + "%";
export const f3 = (x: number | undefined | null) => (x == null || isNaN(x) ? "–" : (+x).toFixed(3));

/** Horizontal or vertical bar chart as inline SVG (no chart library, works offline). */
export function bars(data: { label: string; value: number }[], opts: { horizontal?: boolean; color?: string } = {}): string {
  if (!data.length) return '<p class="muted">No data yet.</p>';
  const max = Math.max(1, ...data.map((d) => d.value));
  const color = opts.color || "var(--accent)";
  if (opts.horizontal) {
    const rowH = 26, w = 520, labelW = 150;
    return `<svg viewBox="0 0 ${w} ${data.length * rowH}" class="chart" role="img">` + data.map((d, i) => {
      const bw = ((w - labelW - 40) * d.value) / max;
      return `<text x="${labelW - 8}" y="${i * rowH + 17}" text-anchor="end" class="ct">${esc(d.label)}</text>
        <rect x="${labelW}" y="${i * rowH + 5}" width="${Math.max(bw, 2)}" height="16" rx="3" fill="${color}"/>
        <text x="${labelW + bw + 6}" y="${i * rowH + 17}" class="ct">${d.value}</text>`;
    }).join("") + "</svg>";
  }
  const w = 560, h = 160, bw = w / data.length;
  return `<svg viewBox="0 0 ${w} ${h + 22}" class="chart" role="img">` + data.map((d, i) => {
    const bh = ((h - 14) * d.value) / max;
    return `<rect x="${i * bw + 2}" y="${h - bh}" width="${bw - 4}" height="${Math.max(bh, d.value ? 2 : 0)}" rx="2" fill="${color}"><title>${esc(d.label)}: ${d.value}</title></rect>` +
      (i % 3 === 0 ? `<text x="${i * bw + bw / 2}" y="${h + 16}" text-anchor="middle" class="ct">${esc(d.label)}</text>` : "");
  }).join("") + "</svg>";
}

/** Heat-map cell background for a 0..1 value. */
export const heat = (v: number) => {
  const a = Math.max(0, Math.min(1, v || 0));
  return `background:color-mix(in srgb, var(--accent) ${Math.round(8 + 72 * a)}%, transparent);${a > 0.6 ? "color:#fff" : ""}`;
};

/** Inline SVG icons (24px grid, 1.8 stroke), drawn for this project. */
const ICONS: Record<string, string> = {
  chat: '<path d="M4 5.5h16v10.5H9l-5 4z"/>',
  orders: '<path d="M3.5 7.5 12 3.5l8.5 4v9l-8.5 4-8.5-4z"/><path d="M3.5 7.5 12 11.5l8.5-4M12 11.5v9"/>',
  shop: '<path d="M5 8.5h14l-1.2 11.5H6.2z"/><path d="M9 8.5V7a3 3 0 0 1 6 0v1.5"/>',
  admin: '<path d="M4 20h16M7 16v-4M12 16V7M17 16v-6"/>',
  sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5.3 5.3l1.4 1.4M17.3 17.3l1.4 1.4M5.3 18.7l1.4-1.4M17.3 6.7l1.4-1.4"/>',
  moon: '<path d="M20 14.5A8 8 0 1 1 9.5 4a6.5 6.5 0 0 0 10.5 10.5z"/>',
  logout: '<path d="M14 4h5v16h-5M10 8l-4 4 4 4M6 12h10"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  camera: '<path d="M4 8h3l1.5-2.5h7L17 8h3v11H4z"/><circle cx="12" cy="13" r="3.5"/>',
  send: '<path d="M4.5 12 20 4.5 15 20l-3-6.5z"/><path d="m12 13.5 8-9"/>',
  headset: '<path d="M4.5 14v-2a7.5 7.5 0 0 1 15 0v2"/><path d="M4.5 14h3v5h-2a1 1 0 0 1-1-1zM19.5 14h-3v5h2a1 1 0 0 0 1-1z"/>',
  truck: '<path d="M3 6.5h11v9.5H3zM14 9.5h4l3 3.5v3h-7"/><circle cx="7" cy="17.5" r="1.8"/><circle cx="17.5" cy="17.5" r="1.8"/>',
  check: '<path d="m5 12.5 4.5 4.5L19 7.5"/>',
  clock: '<circle cx="12" cy="12" r="8.5"/><path d="M12 7.5V12l3 2"/>',
  x: '<path d="M6.5 6.5l11 11M17.5 6.5l-11 11"/>',
  refund: '<path d="M4 5v5h5"/><path d="M4.6 10A8 8 0 1 1 6 16.5"/>',
  up: '<path d="M7 20.5H4.5v-9H7zM7 11.5l4-8a2 2 0 0 1 2 2v4h5.5a1.5 1.5 0 0 1 1.5 1.8l-1.5 7.5a1.5 1.5 0 0 1-1.5 1.2H7"/>',
  down: '<path d="M7 3.5H4.5v9H7zM7 12.5l4 8a2 2 0 0 0 2-2v-4h5.5a1.5 1.5 0 0 0 1.5-1.8l-1.5-7.5a1.5 1.5 0 0 0-1.5-1.2H7"/>',
  back: '<path d="M15 5l-7 7 7 7"/>',
  live: '<circle cx="12" cy="8" r="3.5"/><path d="M5 20a7 7 0 0 1 14 0"/>',
  flask: '<path d="M9.5 3.5h5M10 3.5v6L4.8 18a1.8 1.8 0 0 0 1.6 2.5h11.2a1.8 1.8 0 0 0 1.6-2.5L14 9.5v-6"/><path d="M7.5 14.5h9"/>',
  book: '<path d="M5 4.5h11a2 2 0 0 1 2 2v13H7a2 2 0 0 1-2-2z"/><path d="M5 17.5a2 2 0 0 1 2-2h11"/>',
  bulb: '<path d="M9 18h6M10 21h4M12 3a6 6 0 0 0-3.5 10.9V16h7v-2.1A6 6 0 0 0 12 3z"/>',
  queue: '<path d="M4 6h16M4 12h16M4 18h10"/>',
};

export function icon(name: string, cls = "i"): string {
  return `<svg class="${cls}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICONS[name] || ""}</svg>`;
}

/** Fill every [data-icon] element with its SVG. */
export function paintIcons(root: ParentNode = document): void {
  root.querySelectorAll<HTMLElement>("[data-icon]").forEach((el) => { el.innerHTML = icon(el.dataset.icon!); });
}

/** Wire a theme toggle button (sun in black theme, moon in light). */
export function themeButton(btn: HTMLElement): void {
  const t = (window as any).saubyTheme;
  const paint = () => { btn.innerHTML = icon(t.current() === "black" ? "sun" : "moon"); btn.title = "Switch theme"; };
  paint();
  btn.addEventListener("click", () => { t.toggle(); paint(); });
}

export const statusIcon: Record<string, string> = {
  placed: "clock", shipped: "truck", delivered: "check", cancelled: "x", return_requested: "refund",
  refunded: "refund", refund_pending_approval: "clock", return_received: "refund", replacement_requested: "truck", on_hold: "clock", return_pending_approval: "clock",
};
export const statusLabel = (s: string) => ({ refund_pending_approval: "awaiting approval", return_requested: "return started",
  return_received: "return received", return_pending_approval: "return in review", replacement_requested: "replacement sent", on_hold: "in review" } as any)[s] || s.replace(/_/g, " ");
