/*
 * Ask Sauby · MyFlipKart.com — "How it works" animated walkthrough (GitHub Pages)
 * Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
 *
 * A scripted replay of real request paths through the system. Each step lights up
 * the components involved, sends a packet along the connection, updates the chat on
 * the left, and explains what happened (with the payload you'd see in the trace).
 * No backend needed: it's an educational animation of what the app does.
 */

type NodeId = "customer" | "edge" | "idp" | "api" | "orch" | "gateway" | "llm" | "bm25" | "vector" | "rrf" | "rerank"
  | "tools" | "pg" | "nosql" | "prom" | "logs" | "evals" | "human";

interface ChatLine { who: "user" | "sauby" | "agent" | "system"; text: string; typing?: boolean; cards?: string[] }
interface Step { edges: [NodeId, NodeId][]; caption: string; detail?: string; chat?: ChatLine; status?: string }

// Layout on a 1000 x 600 board (centers). Grouped in rows: edge, server, tools, data.
const NODES: Record<NodeId, { x: number; y: number; label: string; sub: string; kind: string }> = {
  customer: { x: 90, y: 70, label: "Customer", sub: "web app", kind: "edge" },
  edge: { x: 260, y: 70, label: "Edge", sub: "TLS · WAF", kind: "edge" },
  idp: { x: 430, y: 70, label: "Identity", sub: "Google OIDC", kind: "edge" },
  human: { x: 900, y: 70, label: "Human agent", sub: "console", kind: "human" },
  api: { x: 90, y: 210, label: "API", sub: "auth · limits", kind: "core" },
  orch: { x: 300, y: 210, label: "Orchestrator", sub: "route · tools", kind: "core" },
  gateway: { x: 520, y: 210, label: "LLM gateway", sub: "route · failover", kind: "llm" },
  llm: { x: 720, y: 210, label: "Open-source LLM", sub: "tool calling", kind: "llm" },
  bm25: { x: 90, y: 350, label: "BM25", sub: "keywords", kind: "rag" },
  vector: { x: 260, y: 350, label: "Vector index", sub: "meaning", kind: "rag" },
  rrf: { x: 430, y: 350, label: "RRF fusion", sub: "merge ranks", kind: "rag" },
  rerank: { x: 600, y: 350, label: "Reranker", sub: "20 → 5", kind: "rag" },
  tools: { x: 800, y: 350, label: "Order tools", sub: "guardrails", kind: "human" },
  pg: { x: 110, y: 500, label: "PostgreSQL", sub: "orders · products", kind: "data" },
  nosql: { x: 310, y: 500, label: "NoSQL", sub: "chats by cid+sid", kind: "data" },
  prom: { x: 510, y: 500, label: "Prometheus", sub: "metrics", kind: "data" },
  logs: { x: 700, y: 500, label: "Logs", sub: "Loki / S3", kind: "data" },
  evals: { x: 890, y: 500, label: "Evals", sub: "release gates", kind: "loop" },
};

const SCENARIOS: Record<string, { title: string; steps: Step[] }> = {
  search: {
    title: "Find a product",
    steps: [
      { edges: [], chat: { who: "user", text: "Headphones for long flights", typing: true }, caption: "The customer asks a question.", detail: "Typed in the TypeScript chat app." },
      { edges: [["customer", "edge"]], caption: "TLS ends at the edge; the WAF and rate limits screen the request.", detail: "Bots and floods stop here, before any LLM spend." },
      { edges: [["edge", "api"]], status: "Reading your message", caption: "The API checks the signed session from Google sign-in.", detail: "Session → customer id (cid). Every later step is scoped to this cid." },
      { edges: [["api", "nosql"]], caption: "The message is saved before anything else.", detail: "Key: cid + sid (session). Durable: a reload or a handoff sees the full chat." },
      { edges: [["api", "orch"], ["orch", "gateway"], ["gateway", "llm"]], caption: "The orchestrator asks the LLM which tool fits.", detail: "Simple turns go to the internal open-source model; complex ones may go to an external one." },
      { edges: [["llm", "gateway"], ["gateway", "orch"]], caption: "The LLM answers with a tool call, not prose.", detail: 'search_products { "query": "headphones for long flights" }' },
      { edges: [["orch", "bm25"], ["orch", "vector"]], status: "Searching the catalog", caption: "Two searches run together.", detail: "BM25 matches exact words and model names. Vectors match meaning: “long flights” ≈ “noise cancellation”." },
      { edges: [["bm25", "rrf"], ["vector", "rrf"]], caption: "Reciprocal Rank Fusion merges the two ranked lists.", detail: "score = 1/(60 + rank_bm25) + 1/(60 + rank_vector). Ranks, not raw scores, so they combine fairly." },
      { edges: [["rrf", "rerank"]], caption: "The reranker re-scores the top 20 and keeps 5.", detail: "Here: name match. In production: a cross-encoder. The evals decide which signals stay." },
      { edges: [["rerank", "orch"], ["orch", "gateway"], ["gateway", "llm"]], status: "Writing the answer", caption: "Results go back to the LLM to write a short, grounded answer.", detail: "The prompt says: use only these products, keep IDs and prices exact." },
      { edges: [["llm", "gateway"], ["gateway", "orch"], ["orch", "api"], ["api", "customer"]], chat: { who: "sauby", text: "For long flights, these block engine noise best:", cards: ["Quietude Zen 7 · $279", "Sonix WH-900 · $349"] }, caption: "The answer streams back to the chat.", detail: "Status lines arrived first, so the customer never stared at a blank screen." },
      { edges: [["orch", "prom"], ["orch", "logs"]], caption: "Every turn emits metrics and one JSON log line.", detail: "Latency by path vs SLO (FAQ p95 < 1 s), tool, tokens, results count, flags." },
      { edges: [["logs", "evals"]], caption: "Logs feed the eval loop.", detail: "Zero-result searches and thumbs-down chats become new golden test cases." },
    ],
  },
  refund: {
    title: "Refund over $30",
    steps: [
      { edges: [], chat: { who: "user", text: "I want a refund for O5008", typing: true }, caption: "A refund request on a $159 order.", detail: "Money path: strong consistency, human approval above $30." },
      { edges: [["customer", "edge"], ["edge", "api"], ["api", "orch"]], status: "Checking refund eligibility", caption: "Authenticated and routed like any turn.", detail: "cid comes from the session, never from the message or the model." },
      { edges: [["orch", "gateway"], ["gateway", "llm"], ["llm", "gateway"], ["gateway", "orch"]], caption: "The LLM picks the tool and extracts the order id.", detail: 'request_refund { "order_id": "O5008" }' },
      { edges: [["orch", "tools"], ["tools", "pg"]], caption: "Dry run: is it this customer's order, delivered, inside 30 days?", detail: "Another customer's order gets “not found” — its existence is never confirmed." },
      { edges: [["pg", "tools"], ["tools", "orch"], ["orch", "customer"]], chat: { who: "sauby", text: "Just to confirm: I'll send your $159 refund for O5008 to a teammate for approval (over $30). Reply yes to go ahead." }, caption: "Nothing changes until the customer says yes.", detail: "Two-phase: check first, commit only on confirmation." },
      { edges: [["customer", "api"], ["api", "orch"]], chat: { who: "user", text: "Yes" }, caption: "The customer confirms.", detail: "" },
      { edges: [["orch", "tools"], ["tools", "pg"]], status: "Sending to a teammate", caption: "One transaction: approval + ticket + order status.", detail: "Eligibility is re-checked under the lock. One refund per order, ever (idempotent)." },
      { edges: [["pg", "human"]], caption: "A person reviews it in the approvals queue.", detail: "Big refunds and unusual refund counts always get human eyes." },
      { edges: [["human", "pg"], ["pg", "nosql"], ["nosql", "customer"]], chat: { who: "agent", text: "Update from Priya: your $159 refund for O5008 was approved. It reaches your card in 5-7 business days." }, caption: "Approved: the money moves once, and the customer hears about it in the same chat.", detail: "" },
      { edges: [["orch", "prom"], ["prom", "evals"]], chat: { who: "system", text: "Did this solve your issue?  👍  👎" }, caption: "Feedback and metrics close the loop.", detail: "Escalation rate, resolution rate, CSAT, and the release gate “escalation_ok = 100%”." },
    ],
  },
  handoff: {
    title: "Talk to a person",
    steps: [
      { edges: [], chat: { who: "user", text: "Can I talk to a person?", typing: true }, caption: "The customer asks for a human.", detail: "" },
      { edges: [["customer", "edge"], ["edge", "api"], ["api", "orch"], ["orch", "gateway"], ["gateway", "llm"]], status: "Connecting you with a teammate", caption: "The LLM routes to the handoff tool.", detail: 'handoff { "summary": "wants a person" }' },
      { edges: [["orch", "tools"], ["tools", "pg"]], chat: { who: "sauby", text: "I'm connecting you with a teammate now. They'll see our conversation." }, caption: "A live ticket is opened with cid, sid and a summary.", detail: "The chat switches to “waiting”: Sauby stops answering." },
      { edges: [["pg", "human"], ["nosql", "human"]], chat: { who: "system", text: "Finding a teammate for you…" }, caption: "The agent console shows the queue with full context.", detail: "Who, which session, recent orders, pending approvals, the whole transcript." },
      { edges: [["human", "nosql"], ["nosql", "customer"]], chat: { who: "system", text: "Priya joined the chat" }, caption: "The agent joins the same chat window.", detail: "Their messages are written to the same cid + sid conversation the customer sees." },
      { edges: [["human", "nosql"], ["nosql", "customer"]], chat: { who: "agent", text: "Hi Aarav, I'm Priya. I've read your chat — how can I help?", typing: true }, caption: "The customer sees the agent typing, then the reply.", detail: "No repeating the story: context came with the handoff." },
      { edges: [["customer", "api"], ["api", "nosql"], ["nosql", "human"]], chat: { who: "user", text: "All sorted, thanks!" }, caption: "Customer replies go to the person, not the bot.", detail: "" },
      { edges: [["human", "orch"], ["orch", "customer"]], chat: { who: "system", text: "Priya left the chat. Sauby is back.  How was your chat?  👍  👎" }, caption: "The agent closes; Sauby takes over and asks for feedback.", detail: "" },
      { edges: [["orch", "prom"], ["orch", "logs"], ["logs", "evals"]], caption: "Handoffs are measured and mined.", detail: "Handoff rate, wait time to join, recontact within 72 h. The question becomes a golden case so the next agent can answer it." },
    ],
  },
};

const METRICS = [
  { group: "Retrieval", items: ["precision@5", "recall@5", "MRR", "nDCG@10", "by query type"] },
  { group: "Routing", items: ["confusion matrix", "per-tool F1", "argument accuracy"] },
  { group: "Outcomes", items: ["resolution", "deflection", "handoff", "escalation", "recontact 72 h", "CSAT"] },
  { group: "Safety", items: ["no cross-customer leaks", "confirm before commit", "refunds > $30 escalated", "grounded answers", "exactly-once refunds"] },
  { group: "Ops", items: ["p95 / p99 by path", "time to first status", "availability 99.99%", "tokens and cost"] },
];

// ---------------- rendering ----------------

const $ = (s: string) => document.querySelector(s) as HTMLElement;
const esc = (s: string) => s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]!));
const reduced = matchMedia("(prefers-reduced-motion: reduce)").matches;
const W = 150, H = 54;

/** Segment from the border of node a's box to the border of node b's box. */
function ends(a: NodeId, b: NodeId): [number, number, number, number] {
  const p = NODES[a], q = NODES[b];
  const clip = (cx: number, cy: number, dx: number, dy: number) => {
    const tx = dx ? (W / 2 + 4) / Math.abs(dx) : Infinity, ty = dy ? (H / 2 + 4) / Math.abs(dy) : Infinity;
    const t = Math.min(tx, ty);
    return [cx + dx * t, cy + dy * t];
  };
  const dx = q.x - p.x, dy = q.y - p.y;
  const [x1, y1] = clip(p.x, p.y, dx, dy), [x2, y2] = clip(q.x, q.y, -dx, -dy);
  return [x1, y1, x2, y2];
}

function drawBoard(): void {
  const svg = $("#board");
  // markers for arrowheads; edges are drawn per step (only the active connections)
  let html = `<defs><marker id="ah" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
    <path d="M0 0L10 5L0 10z" class="ahead"/></marker></defs><g id="edges"></g>`;
  html += Object.entries(NODES).map(([id, n]) => `<g class="node k-${n.kind}" id="n-${id}" transform="translate(${n.x - W / 2} ${n.y - H / 2})">
    <rect width="${W}" height="${H}" rx="12"/><text x="${W / 2}" y="23" class="nl">${esc(n.label)}</text><text x="${W / 2}" y="40" class="ns">${esc(n.sub)}</text></g>`).join("");
  html += `<g id="packets"></g>`;
  svg.innerHTML = html;
}

let scenario = "search", idx = -1, playing = true, timer = 0;

function setScenario(id: string): void {
  scenario = id;
  idx = -1;
  document.querySelectorAll<HTMLElement>("[data-sc]").forEach((b) => b.classList.toggle("on", b.dataset.sc === id));
  $("#chat").innerHTML = "";
  $("#steps").innerHTML = SCENARIOS[id].steps.map((_, i) => `<button data-step="${i}" aria-label="Step ${i + 1}"></button>`).join("");
  clearBoard();
  next();
}

function clearBoard(): void {
  document.querySelectorAll(".node.on").forEach((e) => e.classList.remove("on"));
  $("#packets").innerHTML = "";
  $("#edges").innerHTML = "";
}

function go(i: number): void {
  const steps = SCENARIOS[scenario].steps;
  if (i < 0 || i >= steps.length) return;
  // rebuild chat up to step i so jumping around stays consistent
  if (i < idx) { $("#chat").innerHTML = ""; for (let j = 0; j < i; j++) addChat(steps[j], false); }
  else for (let j = idx + 1; j < i; j++) addChat(steps[j], false);
  idx = i;
  const st = steps[i];
  clearBoard();
  const lit = new Set<NodeId>();
  $("#edges").innerHTML = st.edges.map(([a, b]) => {
    const [x1, y1, x2, y2] = ends(a, b);
    return `<line x1="${x1}" y1="${y1}" x2="${x2}" y2="${y2}" class="edge on" marker-end="url(#ah)"/>`;
  }).join("");
  st.edges.forEach(([a, b], k) => {
    lit.add(a); lit.add(b);
    if (!reduced) packet(a, b, k * 380);
  });
  lit.forEach((n) => document.getElementById("n-" + n)?.classList.add("on"));
  $("#caption").textContent = st.caption;
  $("#detail").textContent = st.detail || "";
  document.querySelectorAll<HTMLElement>("[data-step]").forEach((b, j) => b.classList.toggle("on", j <= i));
  addChat(st, true);
  schedule();
}

function packet(a: NodeId, b: NodeId, delay: number): void {
  const [px, py, qx, qy] = ends(a, b);
  const c = document.createElementNS("http://www.w3.org/2000/svg", "circle");
  c.setAttribute("r", "6");
  c.setAttribute("class", "packet");
  $("#packets").appendChild(c);
  const t0 = performance.now() + delay, dur = 700;
  const tick = (now: number) => {
    const t = Math.min(1, Math.max(0, (now - t0) / dur));
    const e = t < .5 ? 2 * t * t : 1 - Math.pow(-2 * t + 2, 2) / 2;
    c.setAttribute("cx", String(px + (qx - px) * e));
    c.setAttribute("cy", String(py + (qy - py) * e));
    c.style.opacity = t <= 0 ? "0" : t >= 1 ? "0" : "1";
    if (t < 1) requestAnimationFrame(tick); else c.remove();
  };
  requestAnimationFrame(tick);
}

function addChat(st: Step, animate: boolean): void {
  const box = $("#chat");
  box.querySelector(".working")?.remove();
  if (st.status) {
    const w = document.createElement("div");
    w.className = "working";
    w.innerHTML = `<span class="pulse"></span>${esc(st.status)}…`;
    box.appendChild(w);
  }
  const c = st.chat;
  if (!c) { box.scrollTop = box.scrollHeight; return; }
  const d = document.createElement("div");
  d.className = "msg " + c.who;
  const name = c.who === "sauby" ? "Sauby" : c.who === "agent" ? "Priya · support" : "";
  const body = () => `${name ? `<div class="nm">${name}</div>` : ""}<div class="tx">${esc(c.text)}</div>` +
    (c.cards ? `<div class="mini">${c.cards.map((x) => `<span>${esc(x)}</span>`).join("")}</div>` : "");
  box.appendChild(d);
  if (animate && c.typing && !reduced) {
    if (c.who === "user") {
      let n = 0;
      const t = setInterval(() => { n++; d.innerHTML = `<div class="tx">${esc(c.text.slice(0, n))}<span class="caret"></span></div>`; box.scrollTop = box.scrollHeight; if (n >= c.text.length) { clearInterval(t); d.innerHTML = body(); } }, 45);
    } else {
      d.innerHTML = `${name ? `<div class="nm">${name}</div>` : ""}<div class="dots"><i></i><i></i><i></i></div>`;
      setTimeout(() => { d.innerHTML = body(); box.scrollTop = box.scrollHeight; }, 1200);
    }
  } else d.innerHTML = body();
  box.scrollTop = box.scrollHeight;
}

function schedule(): void {
  clearTimeout(timer);
  if (!playing) return;
  const last = idx >= SCENARIOS[scenario].steps.length - 1;
  timer = window.setTimeout(() => {
    if (last) { const keys = Object.keys(SCENARIOS); setScenario(keys[(keys.indexOf(scenario) + 1) % keys.length]); }
    else next();
  }, last ? 4200 : 2900);
}

const next = () => go(idx + 1);
const prev = () => go(idx - 1);

function setPlaying(p: boolean): void {
  playing = p;
  $("#play").textContent = p ? "Pause" : "Play";
  schedule();
}

function init(): void {
  drawBoard();
  $("#scenarios").innerHTML = Object.entries(SCENARIOS).map(([id, s]) => `<button data-sc="${id}">${esc(s.title)}</button>`).join("");
  $("#metrics").innerHTML = METRICS.map((m) => `<div><h3>${m.group}</h3><p>${m.items.map(esc).join(", ")}</p></div>`).join("");
  document.addEventListener("click", (e) => {
    const t = e.target as HTMLElement;
    const sc = t.closest<HTMLElement>("[data-sc]");
    if (sc) { setScenario(sc.dataset.sc!); return; }
    const sp = t.closest<HTMLElement>("[data-step]");
    if (sp) { setPlaying(false); go(+sp.dataset.step!); }
  });
  $("#play").addEventListener("click", () => setPlaying(!playing));
  $("#next").addEventListener("click", () => { setPlaying(false); next(); });
  $("#prev").addEventListener("click", () => { setPlaying(false); prev(); });
  $("#theme").addEventListener("click", () => {
    const r = document.documentElement;
    const dark = r.dataset.theme ? r.dataset.theme === "black" : matchMedia("(prefers-color-scheme: dark)").matches;
    r.dataset.theme = dark ? "light" : "black";
  });
  if (reduced) setPlaying(false);
  setScenario("search");
}
init();
