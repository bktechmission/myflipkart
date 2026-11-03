/*
 * Ask Sauby · MyFlipKart.com — admin console
 * Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
 *
 * Tabs: Ops (SLOs, latency, availability), Business (deflection, resolution, handoff,
 * escalation, recontact, CSAT per agent version), Live chats (human agent console),
 * Approvals (refunds over the limit), Insights (feedback loop), Evals (precision/recall,
 * confusion matrix, release gates) and Knowledge (edit docs, live re-index).
 */
import { $, api, bars, esc, f3, heat, money, pct, paintIcons, themeButton } from "./lib.js";

const TOOLS = ["search_products", "price_search", "get_product", "get_order", "cancel_order", "return_order",
  "request_refund", "policy_search", "handoff", "chitchat"];
let current = "ops";
let latestEval: any = null;
let knowledge: any = null;

const kpi = (v: string | number, label: string, cls = "") =>
  `<div class="card kpi"><div class="v ${cls}">${v}</div><div class="l">${label}</div></div>`;
const table = (head: string[], rows: string[][]) =>
  `<tr>${head.map((h) => `<th>${h}</th>`).join("")}</tr>` + rows.map((r) => `<tr>${r.map((c) => `<td>${c}</td>`).join("")}</tr>`).join("");

// ---------------- tabs ----------------
document.querySelectorAll<HTMLElement>(".tab").forEach((b) => b.addEventListener("click", () => {
  current = b.dataset.t!;
  document.querySelectorAll(".tab").forEach((x) => x.classList.toggle("on", x === b));
  for (const t of ["ops", "biz", "live", "approvals", "insights", "evals", "knowledge"]) $("#t-" + t).classList.toggle("hide", t !== current);
  load();
}));

async function load(): Promise<void> {
  if (current === "ops" || current === "biz") await loadMetrics();
  if (current === "approvals") await loadApprovals();
  if (current === "live") await loadLive();
  if (current === "insights") await loadInsights();
  if (current === "evals") await loadEvals();
  if (current === "knowledge") await loadKnowledge();
}

// ---------------- ops + business ----------------
async function loadMetrics(): Promise<void> {
  const m = await api("/api/admin/metrics");
  const h = m.health;
  const llm = Object.entries(h.llm || {}).map(([k, v]: [string, any]) => `${k} ${v.model} ${v.up ? "up" : "down"}`).join(" · ");
  $("#health").textContent = `${h.router} router · ${llm} · ${h.embeddings}${h.canary ? ` · canary ${h.canary.percent}%` : ""}`;
  const pb = $("#pendingBadge");
  pb.textContent = m.pending_approvals; pb.classList.toggle("hide", !m.pending_approvals);
  const av = m.availability;
  $("#opsKpis").innerHTML =
    kpi(m.requests, `turns (${m.window_hours}h)`) + kpi(m.sessions, "conversations") +
    kpi(`${m.latency.p95} ms`, "latency p95 (all)") + kpi(`${m.latency.p99} ms`, "latency p99 (all)") +
    kpi((100 * av.value).toFixed(3) + "%", `availability (target ${(100 * av.target).toFixed(2)}%)`, av.met ? "good" : "bad") +
    kpi(m.rate_limited, "rate-limited (429)", m.rate_limited ? "warn" : "") +
    kpi(pct(m.zero_result_rate), "zero-result searches", m.zero_result_rate > 0.1 ? "bad" : "") +
    kpi(`${m.degraded} / ${m.failovers}`, "degraded / failovers", m.degraded ? "warn" : "") +
    kpi(m.injection_suspects, "prompt-injection flags", m.injection_suspects ? "warn" : "") +
    kpi(`${m.tokens} · $${m.est_cost_usd}`, `LLM tokens · est. cost @ $${m.cost_per_m}/M`);
  $("#sloTable").innerHTML = table(["path", "turns", "p50", "p95", "p99", "first status p95", "target p95 / p99", "status"],
    m.slos.map((s: any) => [s.path, s.n, `${s.p50} ms`, `${s.p95} ms`, `${s.p99} ms`, `${s.ttft_p95} ms`, `${s.target_p95} / ${s.target_p99} ms`,
      s.n ? `<span class="${s.met ? "good" : "bad"}">${s.met ? "met" : "breached"}</span>` : '<span class="muted">no traffic</span>']));
  $("#perHour").innerHTML = bars(m.per_hour.map((x: any) => ({ label: x.hour, value: x.n })));
  $("#toolMix").innerHTML = bars(Object.entries(m.tools).map(([k, v]) => ({ label: k, value: v as number })).sort((a, b) => b.value - a.value), { horizontal: true, color: "var(--s2)" });
  $("#recent").innerHTML = table(["time", "customer", "query", "tool", "path", "agent", "ok", "ms", "flags"],
    m.recent.map((e: any) => [new Date(e.ts).toLocaleTimeString(), esc(e.customer_id), `<span class="q">${esc(e.query)}</span>`, esc(e.tool), esc(e.path),
      esc(e.variant || ""), e.status === 429 ? '<span class="warn">429</span>' : `<span class="${e.ok ? "good" : "bad"}">${e.ok ? "yes" : "no"}</span>`,
      (e.latency_ms || 0).toFixed(0), esc((e.flags || []).join(", "))]));
  const b = m.business;
  $("#bizKpis").innerHTML =
    kpi(b.sessions, "conversations") + kpi(pct(b.deflection_rate), "deflection rate", "good") + kpi(pct(b.resolution_rate), "resolution rate") +
    kpi(pct(b.handoff_rate), "handoff rate (asked for human)") + kpi(pct(b.escalation_rate), "escalation rate (guardrail)") +
    kpi(pct(b.recontact_rate), "recontact within 72h", b.recontact_rate > 0.2 ? "bad" : "") +
    kpi(b.feedback ? pct(b.csat) : "–", `CSAT (${b.feedback} ratings)`) + kpi(pct(b.feedback_response_rate), "feedback response rate") +
    kpi(m.pending_approvals, "refunds waiting for approval", m.pending_approvals ? "warn" : "");
  $("#variants").innerHTML = table(["agent", "conversations", "deflection", "resolution", "handoff", "escalation", "recontact", "CSAT", "p95"],
    Object.entries(m.variants).map(([k, v]: [string, any]) => [esc(k), v.sessions, pct(v.deflection_rate), pct(v.resolution_rate), pct(v.handoff_rate),
      pct(v.escalation_rate), pct(v.recontact_rate), v.feedback ? pct(v.csat) : "–", `${v.p95_ms} ms`]));
  $("#guards").innerHTML = bars(Object.entries(m.guardrails).map(([k, v]) => ({ label: k, value: v as number })), { horizontal: true, color: "var(--s3)" });
}

// ---------------- human approval queue ----------------
/** Approve / reject controls. Returns and refunds need the item's condition (and, over the
 *  customer's photo threshold, a photo): the server enforces it, this just makes it easy. */
function apControls(a: any): string {
  const needsCond = !a.no_return && (a.kind === "" || !a.kind || a.kind === "return" || a.kind === "refund");
  const ev = (a.evidence || []).map((id: string) => { const u = `/api/photos/${esc(a.customer_id)}/${esc(id)}`;
    return `<a href="${u}" target="_blank" rel="noopener"><img class="evthumb" src="${u}" alt="customer photo"></a>`; }).join("");
  return `<span class="apbox">${ev || (needsCond ? '<span class="muted">no photo yet</span> ' : "")}
    ${needsCond ? `<select class="inp sm" data-cond><option value="">Condition…</option><option>unused</option><option>defective</option><option>used</option></select>
    <input class="inp sm" data-note placeholder="Note">` : ""}
    <button class="btn sm" data-ap="${esc(a.id)}" data-d="approve">Approve</button> <button class="btn ghost sm" data-ap="${esc(a.id)}" data-d="reject">Reject</button></span>`;
}
const tierTag = (t: string) => `<span class="tier ${esc(t)}">${esc(t)}</span>`;
async function loadApprovals(): Promise<void> {
  const d = await api("/api/admin/approvals");
  $("#apNote").textContent = `Refunds above $${d.limit}, missing-item claims, large or frequent orders and big replacements wait here. A refund is paid only once the item is back (or confirmed lost).`;
  const pending = d.approvals.filter((a: any) => a.status === "pending");
  $("#pending").innerHTML = pending.length ? table(["approval", "type", "order", "customer", "amount", "why held", "customer said", "since", ""],
    pending.map((a: any) => [esc(a.id), esc(a.kind || "refund"), esc(a.order_id), esc(a.customer_id), money(a.amount), esc(a.why), esc(a.reason || ""), new Date(a.created_at).toLocaleString(),
      apControls(a)]))
    : `<tr><td class="muted">Nothing waiting. Try “I want a refund for O5008” as aarav@example.com.</td></tr>`;
  $("#decided").innerHTML = table(["approval", "order", "amount", "decision", "by"],
    d.approvals.filter((a: any) => a.status !== "pending").map((a: any) => [esc(a.id), esc(a.order_id), money(a.amount),
      `<span class="${a.status === "approved" ? "good" : "bad"}">${a.status}</span>`, esc(a.decided_by)]));
  $("#returns").innerHTML = (d.returns || []).length ? table(["order", "customer", "RMA", "status", "refund on arrival", ""],
    d.returns.map((o: any) => [esc(o.id), esc(o.customer_id), esc(o.rma || ""), esc(o.status.replace(/_/g, " ")), o.refund_on_receipt ? money(o.total) : "—",
      `<button class="btn sm" data-rcv="${esc(o.id)}">Mark received</button>`]))
    : `<tr><td class="muted">No returns in transit. Try “return O5009” as aarav@example.com.</td></tr>`;
  $("#sellers").innerHTML = table(["seller", "products", "rating", "orders", "returned", "complaints", "status", "why"],
    (d.sellers || []).map((s: any) => [esc(s.seller), s.products, s.avg_rating.toFixed(1), s.orders, `${Math.round(s.return_rate * 100)}%`, s.complaints,
      `<span class="${s.status === "suspended" ? "bad" : s.status === "watch" ? "warn" : "good"}">${esc(s.status)}</span>`, esc((s.reasons || []).join("; "))]));
  $("#tickets").innerHTML = table(["ticket", "customer", "summary"], (d.tickets || []).slice(-15).reverse().map((t: any) => [esc(t.id), esc(t.customer_id), esc(t.summary)]));
}

// ---------------- insights / feedback loop ----------------
async function loadInsights(): Promise<void> {
  const d = await api("/api/admin/insights");
  $("#actions").innerHTML = d.actions.length ? d.actions.map((a: string) => `<li>${esc(a)}</li>`).join("") : "<li class='muted'>No problems detected yet. Chat a bit, then come back.</li>";
  const addBtn = (q: string, tool = "") => `<button class="btn ghost sm" data-golden="${esc(q)}" data-tool="${esc(tool)}">Add to golden set</button>`;
  $("#zero").innerHTML = table(["query", "tool", "count", ""], d.zero_results.map((r: any) => [esc(r.text), esc(r.extra), r.count, addBtn(r.text, r.extra)]));
  $("#handoffs").innerHTML = table(["query", "count", ""], d.handoffs.map((r: any) => [esc(r.text), r.count, addBtn(r.text)]));
  $("#failedTx").innerHTML = table(["query", "tool", "count"], d.failed_transactions.map((r: any) => [esc(r.text), esc(r.extra), r.count]));
  $("#down").innerHTML = table(["when", "outcome", "first question", "comment", ""], (d.thumbs_down || []).map((f: any) => [new Date(f.ts).toLocaleString(),
    esc(f.outcome || ""), esc(f.first_query || ""), esc(f.comment || ""),
    `<button class="btn ghost sm" data-tx="${esc(f.customer_id)}/${esc(f.session_id)}">Transcript</button> ${addBtn(f.first_query || "", f.tool || "")}`]));
}

async function addGolden(query: string, guess: string): Promise<void> {
  showModal("Add to golden set", `<p class="muted">Label what Sauby <b>should</b> have done. Every future agent version is tested on this case before it can ship.</p>
    <p><b>${esc(query)}</b></p><div class="row"><select class="inp" id="gTool">${TOOLS.map((t) => `<option ${t === guess ? "selected" : ""}>${t}</option>`).join("")}</select>
    <input class="inp" id="gRel" placeholder="relevant IDs, e.g. P1005,P1001 or returns (optional)" style="flex:1"></div>
    <div class="row" style="margin-top:10px"><button class="btn" id="gSave">Save case</button><span id="gMsg" class="muted"></span></div>`);
  $("#gSave").addEventListener("click", async () => {
    const rel = $<HTMLInputElement>("#gRel").value.split(",").map((s) => s.trim()).filter(Boolean);
    const index = rel.length ? (rel[0].match(/^P\d{4}$/) ? "products" : "policies") : "";
    await api("/api/admin/golden", { method: "POST", body: JSON.stringify({ query, tool: $<HTMLSelectElement>("#gTool").value, relevant: rel, index }) });
    $("#gMsg").textContent = "Saved to data/golden_extra.jsonl — it runs in the next eval.";
  });
}

// ---------------- evals ----------------
async function loadEvals(): Promise<void> {
  const { state, result } = await api("/api/admin/evals/latest");
  $("#estate").textContent = state.running ? "running… " + ((state.log || []).at(-1) || "") : state.error ? "error: " + state.error : "";
  $<HTMLButtonElement>("#run").disabled = !!state.running;
  if (state.running) setTimeout(() => current === "evals" && loadEvals(), 1200);
  if (!result) { $("#evalKpis").innerHTML = `<div class="card muted">No results yet — click Run.</div>`; return; }
  latestEval = result;
  const R = result.retrieval, rt = result.router, e = result.e2e;
  let k = "";
  if (result.gate) k += kpi(result.gate.pass ? "PASS" : "FAIL", "release gates", result.gate.pass ? "good" : "bad");
  if (R) k += kpi(f3(R["hybrid+filter"].overall["recall@5"]), "hybrid+filter recall@5") + kpi(f3(R["hybrid+filter"].overall["precision@5"]), "precision@5");
  if (rt) k += kpi(pct(rt.accuracy), `router accuracy (${rt.router})`);
  if (e) k += kpi(pct(e.rates.pass), "e2e pass", e.rates.pass < 0.9 ? "bad" : "good") + kpi(pct(e.rates.no_leak), "authZ: no cross-customer leaks", e.rates.no_leak < 1 ? "bad" : "good");
  $("#evalKpis").innerHTML = k + kpi(esc(result.run_id), `${esc(result.label)} · ${esc(result.config.embeddings)}`);
  $("#gates").innerHTML = result.gate ? table(["gate", "value", "min", ""], result.gate.checks.map((c: any) =>
    [esc(c.metric), f3(c.value), f3(c.min), `<span class="${c.pass ? "good" : "bad"}">${c.pass ? "pass" : "FAIL"}</span> <span class="muted">${esc(c.note || "")}</span>`])) : "";
  if (R) {
    const order: string[] = result.retrieval_order || Object.keys(R);
    const cols = ["precision@5", "recall@5", "hit@5", "mrr", "ndcg@10"];
    $("#rtab").innerHTML = table(["config", ...cols], order.map((n) => [n, ...cols.map((c) => f3(R[n].overall[c]))]));
    const types = Object.keys(R[order[0]].by_type).sort();
    $("#rtype").innerHTML = `<tr><th>config</th>${types.map((t) => `<th>${t}</th>`).join("")}</tr>` +
      order.map((n) => `<tr><td>${n}</td>${types.map((t) => { const v = R[n].by_type[t]["recall@5"]; return `<td class="cell" style="${heat(v)}">${f3(v)}</td>`; }).join("")}</tr>`).join("");
    $("#pqType").innerHTML = `<option value="">all types</option>` + types.map((t) => `<option>${t}</option>`).join("");
    renderPQ();
  }
  if (rt) {
    $("#rnote").textContent = `Rows = expected tool, columns = predicted. Off-diagonal = misroutes. Accuracy ${pct(rt.accuracy)}, macro F1 ${f3(rt.macro_f1)}, argument accuracy ${pct(rt.arg_accuracy)}.`;
    const mx = Math.max(1, ...rt.matrix.flat());
    $("#cm").innerHTML = `<tr><th>expected \\ predicted</th>${rt.labels.map((l: string) => `<th title="${l}">${l.replace(/_/g, " ").slice(0, 9)}</th>`).join("")}</tr>` +
      rt.matrix.map((row: number[], i: number) => `<tr><th>${rt.labels[i]}</th>${row.map((c, j) =>
        `<td class="cell" style="${c ? (i === j ? heat(c / mx) : "background:color-mix(in srgb, var(--bad) 40%, transparent)") : ""}">${c || ""}</td>`).join("")}</tr>`).join("");
    $("#pc").innerHTML = table(["tool", "precision", "recall", "F1", "support"], rt.labels.map((l: string) => {
      const c = rt.per_class[l]; return [l, f3(c.precision), f3(c.recall), f3(c.f1), c.support];
    }));
    $("#mist").innerHTML = rt.mistakes.length ? "<h3 style='margin-top:10px'>Mistakes</h3>" + rt.mistakes.map((m: any) =>
      `<div class="muted">✗ “${esc(m.query)}”: expected <code>${esc(m.expected)}</code> got <code>${esc(m.predicted)}</code></div>`).join("") : `<p class="good">No routing mistakes.</p>`;
  }
  if (e) {
    $("#e2eRates").innerHTML = Object.entries(e.rates).map(([k, v]) => `<span class="pill ${(v as number) < 1 ? "bad" : "good"}">${k} ${pct(v as number)}</span>`).join("") +
      `<p class="note">Consistency: ${Object.entries(e.consistency).map(([k, v]) => `${k}=${v}`).join(" · ")}</p>` +
      e.slo.map((s: any) => `<span class="pill ${s.met ? "good" : "bad"}">${s.path} p95 ${s.p95}ms / p99 ${s.p99}ms</span>`).join("");
    $("#e2e").innerHTML = e.failures?.length ? table(["query", "as", "tool", "failed checks"], e.failures.map((f: any) =>
      [`<span title="${esc(f.answer)}">${esc(f.query)}</span>`, esc(f.as), esc(f.tool), `<span class="bad">${Object.entries(f.checks).filter(([, v]) => !v).map(([k]) => k).join(", ")}</span>`]))
      : `<tr><td class="good">All end-to-end checks passed.</td></tr>`;
  }
  const hist = await api<any[]>("/api/admin/evals/history");
  $("#hist").innerHTML = table(["run", "label", "router", "R@5 (h+f)", "router acc", "e2e pass", "gates"], hist.slice(-12).reverse().map((h) => [
    esc(h.run_id), esc(h.label), esc(h.config?.router), f3(h.retrieval?.["hybrid+filter"]?.["recall@5"]), f3(h.router?.accuracy), f3(h.e2e?.pass),
    h.gate_pass == null ? "–" : `<span class="${h.gate_pass ? "good" : "bad"}">${h.gate_pass ? "pass" : "fail"}</span>`]));
}

function renderPQ(): void {
  const R = latestEval?.retrieval; if (!R) return;
  const metric = $<HTMLSelectElement>("#pqMetric").value, type = $<HTMLSelectElement>("#pqType").value;
  const names: string[] = latestEval.retrieval_order || Object.keys(R);
  const rows = R[names[0]].per_query.map((q: any, i: number) => ({ q, vals: names.map((n) => R[n].per_query[i]) })).filter((r: any) => !type || r.q.type === type);
  $("#pq").innerHTML = `<tr><th>type</th><th>query</th><th>relevant</th>${names.map((n) => `<th>${n}</th>`).join("")}</tr>` + rows.map((r: any) =>
    `<tr><td>${r.q.type}</td><td class="q">${esc(r.q.query)}</td><td title="${esc(r.q.relevant.join(", "))}">${r.q.relevant.length}</td>` +
    r.vals.map((v: any) => `<td class="cell" style="${heat(v.m[metric])}" title="got: ${esc((v.retrieved || []).join(", "))}">${f3(v.m[metric])}</td>`).join("") + `</tr>`).join("");
}

// ---------------- knowledge base ----------------
async function loadKnowledge(): Promise<void> {
  knowledge = await api("/api/admin/knowledge");
  const ix = knowledge.index;
  $("#ixStats").innerHTML = `<span class="pill">embeddings: ${esc(ix.embeddings)}</span><span class="pill">${ix.products_docs} product pages</span>
    <span class="pill">${ix.policy_chunks} policy chunks</span>` + Object.entries(ix.versions).map(([k, v]) =>
      `<span class="pill">${k} index v${v} · built in ${ix.build_ms[k]} ms</span>`).join("");
  const ps = $<HTMLSelectElement>("#polSel"), keep = ps.value;
  ps.innerHTML = knowledge.policies.map((p: any) => `<option value="${esc(p.id)}">${esc(p.title)}</option>`).join("");
  if (keep) ps.value = keep;
  fillPolicy();
  const pr = $<HTMLSelectElement>("#prodSel"), keepP = pr.value;
  pr.innerHTML = knowledge.products.map((p: any) => `<option value="${p.id}">${p.id} · ${esc(p.name)}</option>`).join("");
  if (keepP) pr.value = keepP;
  fillProduct();
}
function fillPolicy(): void {
  const p = knowledge.policies.find((x: any) => x.id === $<HTMLSelectElement>("#polSel").value); if (!p) return;
  $<HTMLInputElement>("#polTitle").value = p.title;
  $<HTMLTextAreaElement>("#polBody").value = p.paras.join("\n\n");
  $("#polMeta").textContent = `v${p.version}${p.updated_by ? " · edited by " + p.updated_by : ""}`;
}
function fillProduct(): void {
  const p = knowledge.products.find((x: any) => x.id === $<HTMLSelectElement>("#prodSel").value); if (!p) return;
  $<HTMLInputElement>("#prodPrice").value = String(p.price);
  $<HTMLTextAreaElement>("#prodDesc").value = p.description;
  $<HTMLTextAreaElement>("#prodFeat").value = p.features.join("\n");
  $<HTMLAnchorElement>("#prodLink").href = "/p/" + p.id;
}
$("#polSel").addEventListener("change", fillPolicy);
$("#prodSel").addEventListener("change", fillProduct);
$("#polSave").addEventListener("click", async () => {
  const id = $<HTMLSelectElement>("#polSel").value;
  const cur = knowledge.policies.find((x: any) => x.id === id);
  try {
    const r = await api(`/api/admin/policies/${id}`, { method: "PUT", body: JSON.stringify({ title: $<HTMLInputElement>("#polTitle").value, version: cur.version,
      paras: $<HTMLTextAreaElement>("#polBody").value.split(/\n\s*\n/) }) });
    $("#polMsg").textContent = `Saved v${r.policy.version}; BM25 + vectors rebuilt in ${r.reindex_ms} ms. Live now.`;
    await loadKnowledge();
  } catch (e) { $("#polMsg").textContent = (e as Error).message; }
});
$("#prodSave").addEventListener("click", async () => {
  const id = $<HTMLSelectElement>("#prodSel").value;
  try {
    const r = await api(`/api/admin/products/${id}`, { method: "PUT", body: JSON.stringify({ description: $<HTMLTextAreaElement>("#prodDesc").value,
      price: parseFloat($<HTMLInputElement>("#prodPrice").value), features: $<HTMLTextAreaElement>("#prodFeat").value.split("\n") }) });
    $("#prodMsg").textContent = `Saved; product index rebuilt in ${r.reindex_ms} ms. Live now.`;
    await loadKnowledge();
  } catch (e) { $("#prodMsg").textContent = (e as Error).message; }
});
$("#probeGo").addEventListener("click", async () => {
  const q = $<HTMLInputElement>("#probeQ").value, ix = $<HTMLSelectElement>("#probeIx").value;
  const r = await api(`/api/admin/search?index=${ix}&q=${encodeURIComponent(q)}`);
  const rows = ix === "policies" ? r.hits.map((h: any) => [esc(h.hit.id), h.hit.bm25_rank ?? "–", h.hit.vec_rank ?? "–", f3(h.hit.score), esc(h.text)])
    : r.hits.map((h: any) => [esc(h.id), h.bm25_rank ?? "–", h.vec_rank ?? "–", f3(h.score), ""]);
  $("#probeOut").innerHTML = `<table>${table(["doc", "BM25 rank", "vec rank", "RRF", "text"], rows)}</table>`;
});

// ---------------- shared actions ----------------
function showModal(title: string, html: string): void {
  $("#mTitle").textContent = title; $("#mBody").innerHTML = html; $("#modal").classList.remove("hide");
}
$("#mClose").addEventListener("click", () => $("#modal").classList.add("hide"));
document.addEventListener("click", async (ev) => {
  const t = ev.target as HTMLElement;
  const ap = t.closest<HTMLElement>("[data-ap]");
  if (ap) {
    const box = ap.closest(".apbox");
    const condition = (box?.querySelector("[data-cond]") as HTMLSelectElement | null)?.value || "";
    const note = (box?.querySelector("[data-note]") as HTMLInputElement | null)?.value || "";
    try { await api(`/api/admin/approvals/${ap.dataset.ap}`, { method: "POST", body: JSON.stringify({ decision: ap.dataset.d, condition, note }) }); }
    catch (e) { alert((e as Error).message); }
    loadApprovals(); return;
  }
  const rcv = t.closest<HTMLElement>("[data-rcv]");
  if (rcv) {
    try { const r = await api<any>(`/api/admin/orders/${rcv.dataset.rcv}/receive`, { method: "POST" }); alert(r.message); }
    catch (e) { alert((e as Error).message); }
    loadApprovals(); return;
  }
  const g = t.closest<HTMLElement>("[data-golden]");
  if (g) { addGolden(g.dataset.golden!, g.dataset.tool || ""); return; }
  const tx = t.closest<HTMLElement>("[data-tx]");
  if (tx) {
    const msgs = await api<any[]>(`/api/admin/transcripts/${tx.dataset.tx}`);
    showModal("Transcript", msgs.map((m) => `<p><b>${m.role === "user" ? "Customer" : "Sauby"}</b> <span class="muted">${esc(m.tool || "")}</span><br>${esc(m.content)}</p>`).join(""));
  }
});
$("#run").addEventListener("click", async () => {
  try {
    await api("/api/admin/evals/run", { method: "POST", body: JSON.stringify({ suite: $<HTMLSelectElement>("#suite").value,
      router: $<HTMLSelectElement>("#router").value, label: $<HTMLInputElement>("#label").value }) });
    loadEvals();
  } catch (e) { $("#estate").textContent = (e as Error).message; }
});
$("#pqMetric").addEventListener("change", renderPQ);
$("#pqType").addEventListener("change", renderPQ);
$("#resetOrders").addEventListener("click", async () => { await api("/api/admin/reset-orders", { method: "POST" }); alert("Demo orders, refunds and approvals reset."); });

// ---------------- live chats: the human agent console ----------------
let liveSel = "";
let liveTimer = 0;

async function loadLive(): Promise<void> {
  if (liveTimer) clearTimeout(liveTimer);
  const d = await api("/api/admin/live");
  const open = d.tickets.filter((c: any) => c.ticket.status !== "closed");
  const badge = $("#liveBadge");
  badge.textContent = String(open.filter((c: any) => c.ticket.status === "waiting").length);
  badge.classList.toggle("hide", badge.textContent === "0");
  $("#liveNote").textContent = d.sim_agents
    ? `Demo mode: a simulated teammate joins if nobody picks a chat within ${d.sim_join_after_sec}s.`
    : "Chats wait here until a teammate joins.";
  $("#liveList").innerHTML = d.tickets.length ? d.tickets.map((c: any) => {
    const t = c.ticket;
    const who = t.agent_name ? `${esc(t.agent_name)}${t.agent_kind === "simulated" ? " (simulated)" : ""}` : "unassigned";
    return `<button class="item${t.id === liveSel ? " on" : ""}" data-live="${esc(t.id)}"><span class="t">${esc(c.customer.name || t.customer_id)}
      <small>${esc(t.status)} · ${who}</small></span></button>`;
  }).join("") : `<p class="muted">No handoffs yet. In the chat, ask Sauby to talk to a person.</p>`;
  const cur = d.tickets.find((c: any) => c.ticket.id === liveSel);
  if (cur) renderLive(cur, d);
  liveTimer = window.setTimeout(() => current === "live" && loadLive(), 2000);
}

function renderLive(c: any, d: any): void {
  const t = c.ticket;
  const me = d.me || "";
  $("#liveTitle").textContent = `${c.customer.name || t.customer_id} · ${t.id}`;
  $("#liveJoin").classList.toggle("hide", t.status !== "waiting");
  $("#liveClose").classList.toggle("hide", t.status !== "active");
  $("#liveForm").classList.toggle("hide", !(t.status === "active" && t.agent_kind === "human"));
  const box = $("#liveMsgs");
  const atBottom = box.scrollTop + box.clientHeight >= box.scrollHeight - 20;
  box.innerHTML = c.messages.map((m: any) => {
    const who = m.role === "user" ? c.customer.name : m.role === "assistant" ? "Sauby" : m.meta?.agent_name || "";
    if (m.role === "system") return `<div class="lm system">${esc(m.content)}</div>`;
    const pic = m.meta?.photo ? `<a href="${esc(m.meta.photo)}" target="_blank" rel="noopener"><img class="shot" src="${esc(m.meta.photo)}" alt="customer photo"></a>` : esc(m.content);
    return `<div class="lm ${m.role}"><small>${esc(who)}</small>${pic}</div>`;
  }).join("");
  if (atBottom) box.scrollTop = box.scrollHeight;
  $("#liveCtx").innerHTML = `<p><b>${esc(c.customer.name)}</b><br><span class="muted">${esc(c.customer.email)}<br>cid ${esc(t.customer_id)} · sid ${esc(t.session_id.slice(0, 8))}…</span></p>
    <p><b>Customer standing</b> ${tierTag(c.standing?.tier || "standard")}<br><span class="muted">${esc((c.standing?.reasons || []).join("; ") || "No returns or claims on record")}<br>
      Photo needed above ${c.standing?.photo_over ? money(c.standing.photo_over) : "any amount"}</span></p>
    <p><b>Why they need us</b><br>${esc(t.summary || "Asked for a person")}</p>
    <p><b>Recent orders</b></p>${(c.orders || []).slice().reverse().map((o: any) => `<div class="muted">${o.id} · ${esc(o.status.replace(/_/g, " "))} · ${money(o.total)}</div>`).join("") || '<div class="muted">None</div>'}
    ${(c.approvals || []).length ? `<p><b>Waiting for your decision</b><br><span class="muted">Ask about the item's condition (unused, defective, used) and check the photo before approving.</span></p>` +
      c.approvals.map((a: any) => `<div class="apcard"><b>${esc(a.kind || "refund")}</b> · ${a.order_id} · ${money(a.amount)}<br><span class="muted">${esc(a.why)}</span><br>${apControls(a)}</div>`).join("") : ""}`;
  void me;
}

document.addEventListener("click", (ev) => {
  const el = (ev.target as HTMLElement).closest<HTMLElement>("[data-live]");
  if (el) { liveSel = el.dataset.live!; $("#liveMsgs").innerHTML = ""; loadLive(); }
});
$("#liveJoin").addEventListener("click", async () => {
  try { await api(`/api/admin/live/${liveSel}/join`, { method: "POST" }); } catch (e) { alert((e as Error).message); }
  loadLive();
});
$("#liveClose").addEventListener("click", async () => { await api(`/api/admin/live/${liveSel}/close`, { method: "POST" }); loadLive(); });
$("#liveForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const inp = $<HTMLInputElement>("#liveText");
  if (!inp.value.trim()) return;
  try { await api(`/api/admin/live/${liveSel}/message`, { method: "POST", body: JSON.stringify({ text: inp.value }) }); inp.value = ""; }
  catch (err) { alert((err as Error).message); }
  loadLive();
});

paintIcons();
themeButton($("#theme"));
load();
setInterval(() => { if (current === "ops" || current === "biz") loadMetrics().catch(() => {}); }, 10000);
