/*
 * Ask Sauby · MyFlipKart.com — customer chat app
 * Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
 *
 * Views: Chat (conversation with Sauby or a support teammate), Orders, Shop.
 *
 * Chat flow:
 *   1. POST /api/chat streams Server-Sent Events: "status" lines while Sauby works,
 *      then one "answer" with the tool result and a trace.
 *   2. If the conversation was handed to a person, the server answers with {live}
 *      instead, and this page polls GET /api/sessions/{sid}/live for their messages.
 *   3. Feedback cards appear when an issue is resolved, escalated, or a teammate leaves.
 */
import { $, api, esc, md, money, postSSE, ApiError, icon, paintIcons, themeButton, statusIcon, statusLabel } from "./lib.js";

interface Me { cid: string; email: string; name: string; picture?: string; role: string }
interface Line { id: string; name: string; image: string; price: number }
interface Order { id: string; status: string; total: number; placed_on: string; delivered_on?: string; lines: Line[] }
interface Product { id: string; name: string; brand: string; category: string; price: number; rating: number; stock: number; image: string; features: string[] }

let me: Me;
let sid = "";
let busy = false;
let cursor = 0;            // how many conversation messages the page has rendered
let liveTimer = 0;
let products: Product[] = [];

const stream = () => $("#stream");

// ---------------- stay pinned to the newest message ----------------
// Like a chat app: while you're at the bottom, new text, product cards and late-loading
// images keep the view at the bottom. Scroll up to read and it stops following;
// the arrow button (or sending a message) jumps back down.
let pinned = true;
const nearBottom = () => { const s = stream(); return s.scrollHeight - s.scrollTop - s.clientHeight < 80; };
function toBottom(force = false): void {
  if (force) pinned = true;
  if (pinned) { const s = stream(); s.scrollTop = s.scrollHeight; }
  $("#jump").classList.toggle("hide", pinned);
}
function watchScroll(): void {
  const s = stream();
  s.addEventListener("scroll", () => { pinned = nearBottom(); $("#jump").classList.toggle("hide", pinned); }, { passive: true });
  new MutationObserver(() => toBottom()).observe(s, { childList: true, subtree: true, characterData: true });
  s.addEventListener("load", () => toBottom(), true); // images grow the page after they load
  new ResizeObserver(() => toBottom()).observe(s);   // window or composer resize
  $("#jump").addEventListener("click", () => { toBottom(true); s.scrollTo({ top: s.scrollHeight, behavior: "smooth" }); });
}
const first = (n: string) => (n || "there").split(" ")[0];

// ---------------- boot ----------------

async function init(): Promise<void> {
  paintIcons();
  themeButton($("#theme"));
  watchScroll();
  me = await api<Me>("/api/me");
  $("#whoName").textContent = me.name || "Guest";
  $("#avatar").textContent = (me.name || "G")[0];
  if (me.role === "admin") $("#adminLink").classList.remove("hide");
  await newChat();
  loadOrders();
  loadSessions();
  const ask = new URLSearchParams(location.search).get("ask");   // from a product page
  if (ask && /^P\d{4}$/.test(ask)) { history.replaceState(null, "", "/"); send(`tell me about ${ask}`); }
  if (location.hash === "#shop") show("shop");
}

function show(view: string): void {
  document.querySelector(".shell")!.classList.toggle("wide", view === "shop");
  for (const v of ["chat", "orders", "shop"]) $("#v-" + v).classList.toggle("hide", v !== view);
  document.querySelectorAll<HTMLElement>("[data-view]").forEach((b) => b.classList.toggle("on", b.dataset.view === view));
  $("#p-chat").classList.toggle("hide", view === "orders");
  $("#p-orders").classList.toggle("hide", view !== "orders");
  if (view === "shop" && !products.length) loadShop();
  if (view === "orders") loadOrders();
}

// ---------------- conversations ----------------

async function newChat(): Promise<void> {
  stopLive();
  sid = (await api<{ sid: string }>("/api/sessions", { method: "POST" })).sid;
  cursor = 0;
  const guest = me.role === "guest";
  stream().innerHTML = `<div class="hello"><span class="mark" style="width:44px;height:44px;font-size:20px">S</span>
    <h1 style="margin-top:14px">Hi ${esc(first(me.name))}, I'm Sauby</h1>
    <p>${guest ? "Ask about products, prices and store policies. Sign in to manage orders." : "I can find products and help with your orders."}</p>
    <div class="suggest">${(guest
      ? ["Headphones under $100", "Laptop for video games", "How do returns work?"]
      : ["Where is my latest order?", "Headphones for long flights", "I want a refund", "Talk to a person"])
      .map((q) => `<button class="chip" data-q="${esc(q)}">${esc(q)}</button>`).join("")}</div></div>`;
}

async function loadSessions(): Promise<void> {
  const ss = await api<{ sid: string; title: string }[]>("/api/sessions");
  $("#sessions").innerHTML = ss.slice(0, 15).map((s) =>
    `<button class="item${s.sid === sid ? " on" : ""}" data-sid="${esc(s.sid)}">${icon("chat")}<span class="t">${esc(s.title || "New chat")}</span></button>`).join("")
    || `<p class="muted" style="padding:0 6px">Your chats will show here.</p>`;
}

async function openSession(id: string): Promise<void> {
  stopLive();
  const list = await api<any[]>(`/api/sessions/${encodeURIComponent(id)}`);
  sid = id;
  stream().innerHTML = "";
  for (const m of list) renderMessage(m, true);
  cursor = list.length;
  toBottom(true);
  show("chat");
  pollLive(); // pick up a handoff that's still open
}

/** Render one stored conversation message (used for history and live polling). */
function renderMessage(m: any, history: boolean): void {
  const meta = m.meta || {};
  if (m.role === "user") addTurn("user", esc(m.content));
  else if (m.role === "assistant") addTurn("bot", renderAnswer({ answer: m.content, ...meta }, history));
  else if (m.role === "agent") addAgentTurn(m.content, meta.agent_name || "Support", meta.agent_kind, history ? "" : meta.ask_feedback);
  else if (m.role === "system") {
    const d = document.createElement("div");
    d.className = "system";
    d.textContent = m.content;
    stream().appendChild(d);
    if (meta.ask_feedback && !history) addTurn("bot", feedbackCard(meta.ask_feedback));
  }
  toBottom();
}

function addTurn(role: "user" | "bot", html: string): HTMLElement {
  stream().querySelector(".hello")?.remove();
  const d = document.createElement("div");
  d.className = "turn " + role;
  d.innerHTML = role === "bot"
    ? `<span class="mark sm" aria-hidden="true">S</span><div class="bubble"><div class="who">Sauby</div>${html}</div>`
    : `<div class="bubble">${html}</div>`;
  stream().appendChild(d);
  toBottom();
  return d;
}

function addAgentTurn(text: string, name: string, kind: string, askFeedback = ""): void {
  stream().querySelector(".hello")?.remove();
  const d = document.createElement("div");
  d.className = "turn bot";
  // Simulated teammates are always labeled: customers should know when they're talking to software.
  d.innerHTML = `<span class="avatar" aria-hidden="true">${esc(name[0] || "?")}</span><div class="bubble">
    <div class="who">${esc(name)} ${kind === "simulated" ? '<span class="tag" title="Simulated teammate in this demo">Demo</span>' : ""}</div>
    ${md(text)}${askFeedback ? feedbackCard(askFeedback) : ""}</div>`;
  stream().appendChild(d);
  toBottom();
}

// ---------------- Sauby's answers ----------------

function renderAnswer(res: any, history = false): string {
  const r = res.result || {};
  // Template answers list every product; when cards show them, keep only the lead line.
  const cardsShown = (r.products?.length || r.orders?.length) && String(res.trace?.answer_mode || "").startsWith("template");
  let html = md(cardsShown ? (res.answer || "").split("\n")[0] : res.answer || "");
  if (r.orders?.length) { // order history, or the candidates when Sauby asks "which order?"
    html += r.orders.map((o: any) => `<div class="ocard"><img class="thumb" src="/img/${o.items?.[0]}.svg" alt="">
      <div style="flex:1"><b>${esc(o.item_cards?.[0]?.name || o.id)}</b>${o.item_cards?.length > 1 ? ` <span class="muted">+${o.item_cards.length - 1} more</span>` : ""}
      <div class="muted">${o.id} · ${money(o.total)}</div>
      <div class="status ${o.status}">${icon(statusIcon[o.status] || "clock")}${esc(statusLabel(o.status))}</div></div>
      ${r.choices?.length || history ? "" : `<button class="btn ghost sm" data-q="I need help with order ${o.id}">Get help</button>`}</div>`).join("");
  }
  if (r.choices?.length && !history) {
    html += `<div class="actions">${r.choices.map((c: any) => `<button class="btn ghost sm" data-q="${esc(c.say)}">${esc(c.label)}</button>`).join("")}</div>`;
  }
  if (r.products?.length) {
    html += `<div class="cards">` + r.products.slice(0, 6).map((p: any) => `<a class="pcard" href="/p/${p.id}">
      <img src="/img/${p.id}.svg" alt="" loading="lazy"><div class="b"><div class="n">${esc(p.name)}</div>
      <div class="p">${money(p.price)} <span class="muted">★ ${p.rating}</span></div></div></a>`).join("") + `</div>`;
  }
  if (r.order && res.tool !== "get_order") {
    const o = r.order;
    html += `<div class="ocard"><img class="thumb" src="/img/${o.items?.[0]}.svg" alt=""><div style="flex:1"><b>${o.id}</b> · ${money(o.total)}
      <div class="status ${o.status}">${icon(statusIcon[o.status] || "clock")}${esc(statusLabel(o.status))}</div></div></div>`;
  }
  if (res.needs_confirm && !history) {
    html += `<div class="actions"><button class="btn sm" data-say="yes">Yes, go ahead</button><button class="btn ghost sm" data-say="no">No</button></div>`;
  }
  if (res.ask_feedback && !history) html += feedbackCard(res.ask_feedback);
  if (res.trace) html += howCard(res, r);
  return html;
}

/** "How I found this": the trace, kept small and collapsed (for learning). */
function howCard(res: any, r: any): string {
  const t = res.trace, rv = r.retrieval;
  let h = `<div>${esc(res.tool)} via ${esc(t.router)}${t.model ? ` (${esc(t.model)})` : ""} · ${t.latency_ms} ms${r.guardrail ? ` · guardrail: ${esc(r.guardrail)}` : ""}</div>`;
  if (rv) h += `<div class="muted">${esc(rv.mode)} search${rv.category ? `, category ${esc(rv.category)}` : ""}${rv.max_price != null ? `, ≤ $${rv.max_price}` : ""}${rv.min_price != null ? `, ≥ $${rv.min_price}` : ""}${rv.reranked ? ", reranked" : ""}</div>`;
  const rows = (r.products || []).filter((p: any) => p.scores).map((p: any) => [p.id, p.scores])
    .concat((r.chunks || []).map((c: any) => [c.id, c]));
  if (rows.length) {
    h += `<div class="scroll"><table><tr><th>doc</th><th>BM25</th><th>vector</th><th>fused</th><th>rerank</th></tr>` +
      rows.map(([id, s]: any) => `<tr><td>${esc(id)}</td><td>${s.bm25_rank ?? "–"}</td><td>${s.vec_rank ?? "–"}</td><td>${s.rrf_rank ?? "–"}</td><td>${s.rerank_score ?? "–"}</td></tr>`).join("") + `</table></div>`;
  }
  return `<details class="how"><summary>How I found this</summary><div class="howbox">${h}</div></details>`;
}

function feedbackCard(outcome: string): string {
  const q = outcome === "escalated" ? "Your request is with a teammate. Was this helpful so far?"
    : outcome === "handoff_closed" ? "How was your chat with our teammate?"
    : outcome === "escalated_decided" ? "Did this resolve your issue?"
    : "Did this solve your issue?";
  return `<div class="feedback" data-outcome="${esc(outcome)}"><div>${q}</div>
    <div class="actions"><button class="btn ghost sm" data-fb="1">${icon("up")}Yes</button><button class="btn ghost sm" data-fb="0">${icon("down")}No</button></div></div>`;
}

async function sendFeedback(card: HTMLElement, helpful: boolean, comment = ""): Promise<void> {
  const r = await api<any>("/api/feedback", { method: "POST", body: JSON.stringify({ sid, helpful, outcome: card.dataset.outcome, comment }) });
  if (r.live) { // "No" connects a teammate straight away
    card.innerHTML = `<span class="muted">Thanks for telling us. A teammate is joining.</span>`;
    pollLive();
    return;
  }
  card.innerHTML = `<span class="muted">${esc(helpful ? "Thanks for the feedback." : r.message || "Thanks for the feedback.")}</span>`;
}

async function send(text: string): Promise<void> {
  text = text.trim();
  if (!text || busy) return;
  busy = true;
  show("chat");
  addTurn("user", esc(text));
  toBottom(true); // sending always brings you back to the latest message
  $<HTMLTextAreaElement>("#q").value = "";
  const turn = addTurn("bot", `<div class="working"><span class="pulse"></span><span class="now">Reading your message</span></div><div class="steps"></div>`);
  const bubble = turn.querySelector(".bubble") as HTMLElement;
  const steps: string[] = [];
  let lastStatus = "";
  try {
    await postSSE("/api/chat", { sid, message: text }, (ev, data) => {
      if (ev === "status") {
        if (lastStatus) steps.push(lastStatus.replace(/…$/, ""));
        lastStatus = data.text;
        const now = bubble.querySelector(".now"), st = bubble.querySelector(".steps");
        if (now) now.textContent = data.text;
        if (st) st.innerHTML = steps.map((s) => `<span>${esc(s)}</span>`).join("");
      }
      if (ev === "answer") {
        bubble.innerHTML = `<div class="who">Sauby</div>` + renderAnswer(data);
        if (["cancel_order", "return_order", "request_refund", "place_order", "replace_order"].includes(data.tool)) loadOrders();
        if ((data.tool === "handoff" || data.tool === "file_complaint") && data.ok && data.result?.ticket) pollLive();
      }
      if (ev === "live") { turn.remove(); pollLive(); }
      toBottom();
    });
    cursor += 2;
  } catch (e) {
    if (e instanceof ApiError && e.status === 200) { /* handled */ }
    bubble.innerHTML = `<div class="who">Sauby</div><span class="bad">${esc((e as Error).message)}</span>`;
  } finally {
    busy = false;
    loadSessions();
  }
}

// ---------------- live handoff (human or simulated teammate) ----------------

function stopLive(): void { if (liveTimer) clearTimeout(liveTimer); liveTimer = 0; }

async function pollLive(): Promise<void> {
  stopLive();
  try {
    const st = await api<any>(`/api/sessions/${sid}/live?after=${cursor}`);
    for (const m of st.messages) {
      // skip echoes of what this page already rendered itself
      if (m.role === "user" || m.role === "assistant") continue;
      renderMessage(m, false);
    }
    cursor = st.total;
    let typing = stream().querySelector(".typing-line") as HTMLElement | null;
    const label = st.mode === "waiting" ? "Finding a teammate for you" : st.typing ? `${st.typing} is typing` : "";
    if (label) {
      if (!typing) { typing = document.createElement("div"); typing.className = "system typing-line"; stream().appendChild(typing); }
      typing.innerHTML = `<span class="working" style="justify-content:center"><span class="pulse"></span>${esc(label)}</span>`;
      stream().appendChild(typing); // keep it last
    } else typing?.remove();
    toBottom();
    if (st.mode !== "bot") liveTimer = window.setTimeout(pollLive, 1500);
  } catch { liveTimer = window.setTimeout(pollLive, 4000); }
}

/** While a person owns the chat, messages go to them (server returns JSON, not SSE). */
async function sendToLive(text: string): Promise<boolean> {
  const res = await fetch("/api/chat", { method: "POST", credentials: "same-origin",
    headers: { "Content-Type": "application/json", "X-Sauby": "1" }, body: JSON.stringify({ sid, message: text }) });
  if (!(res.headers.get("Content-Type") || "").includes("application/json")) return false;
  const body = await res.json();
  if (!body.live) return false;
  addTurn("user", esc(text));
  cursor = body.live.total;
  pollLive();
  return true;
}

// ---------------- orders + shop ----------------

async function loadOrders(): Promise<void> {
  if (me.role === "guest") {
    $("#orderList").innerHTML = $("#ordersPage").innerHTML = `<p class="muted" style="padding:0 6px">Sign in to see your orders.</p>`;
    return;
  }
  const os = await api<Order[]>("/api/orders");
  const status = (o: Order) => `<span class="status ${o.status}">${icon(statusIcon[o.status] || "clock")}${esc(statusLabel(o.status))}</span>`;
  $("#orderList").innerHTML = os.length ? os.slice(0, 8).map((o) => `<button class="item" data-q="where is my order ${o.id}">
      <img class="thumb" src="${o.lines[0]?.image}" alt=""><span class="t">${esc(o.lines[0]?.name || o.id)}<small>${o.id} · ${money(o.total)}</small></span>${status(o)}</button>`).join("")
    : `<p class="muted" style="padding:0 6px">No orders yet.</p>`;
  $("#ordersPage").innerHTML = os.map((o) => `<div class="orow">
      <div class="imgs">${o.lines.slice(0, 2).map((l) => `<img class="thumb" src="${l.image}" alt="">`).join("")}</div>
      <div><b>${esc(o.lines.map((l) => l.name).join(", "))}</b><div class="muted">${o.id} · placed ${o.placed_on} · ${money(o.total)}</div>${status(o)}</div>
      <button class="btn ghost sm" data-q="I need help with order ${o.id}">Get help</button></div>`).join("")
    || `<p class="muted">No orders yet.</p>`;
}

async function loadShop(): Promise<void> {
  products = await api<Product[]>("/api/products");
  const cats = [...new Set(products.map((p) => p.category))];
  $("#filters").innerHTML = [`<button class="chip on" data-cat="">All</button>`]
    .concat(cats.map((c) => `<button class="chip" data-cat="${esc(c)}">${esc(c[0].toUpperCase() + c.slice(1))}</button>`)).join("");
  renderShop("");
}

function renderShop(cat: string): void {
  document.querySelectorAll<HTMLElement>("[data-cat]").forEach((b) => b.classList.toggle("on", b.dataset.cat === cat));
  $("#shopGrid").innerHTML = products.filter((p) => !cat || p.category === cat).map((p) => `<a class="pcard" href="/p/${p.id}">
    <img src="${p.image}" alt="${esc(p.name)}" loading="lazy"><div class="b"><div class="n">${esc(p.name)}</div>
    <div class="p">${money(p.price)} <span class="muted">★ ${p.rating}</span></div></div></a>`).join("");
}

// ---------------- events ----------------

document.addEventListener("click", async (e) => {
  const t = e.target as HTMLElement;
  const view = t.closest<HTMLElement>("[data-view]");
  if (view) { show(view.dataset.view!); if (innerWidth < 760) $("#panel").classList.toggle("open", view.dataset.view === "chat" && !$("#panel").classList.contains("open")); return; }
  const s = t.closest<HTMLElement>("[data-sid]");
  if (s) { openSession(s.dataset.sid!); $("#panel").classList.remove("open"); return; }
  const cat = t.closest<HTMLElement>("[data-cat]");
  if (cat) { renderShop(cat.dataset.cat!); return; }
  const q = t.closest<HTMLElement>("[data-q]");
  if (q) { submit(q.dataset.q!); $("#panel").classList.remove("open"); return; }
  const say = t.closest<HTMLElement>("[data-say]");
  if (say) { say.parentElement?.remove(); submit(say.dataset.say!); return; }
  const fb = t.closest<HTMLElement>("[data-fb]");
  if (fb) {
    const card = fb.closest<HTMLElement>(".feedback")!;
    sendFeedback(card, fb.dataset.fb === "1");
    return;
  }
});

/** Route a message to the live teammate if one owns the chat, otherwise to Sauby. */
async function submit(text: string): Promise<void> {
  text = text.trim();
  if (!text || busy) return;
  if (liveTimer) {
    busy = true;
    try { if (await sendToLive(text)) { $<HTMLTextAreaElement>("#q").value = ""; return; } } finally { busy = false; }
  }
  send(text);
}

$("#compose").addEventListener("submit", (e) => { e.preventDefault(); submit($<HTMLTextAreaElement>("#q").value); });
$("#q").addEventListener("keydown", (e) => {
  const k = e as KeyboardEvent;
  if (k.key === "Enter" && !k.shiftKey) { e.preventDefault(); submit($<HTMLTextAreaElement>("#q").value); }
});
$("#q").addEventListener("input", (e) => { const el = e.target as HTMLTextAreaElement; el.style.height = "auto"; el.style.height = el.scrollHeight + "px"; });
$("#newchat").addEventListener("click", () => { newChat(); loadSessions(); });
$("#human").addEventListener("click", () => submit("I want to talk to a person"));
init();
