/*
 * Ask Sauby · MyFlipKart.com — Admin → Reviews (ReviewGuard)
 * Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
 *
 * Moderation queue ordered by severity and reach; cases group related reviews and
 * show the evidence, the tagger's labels with highlighted quotes, which tagger ran and
 * the suggested action. Moderators approve or dismiss with a note; sanctions are opt-in
 * and only a person applies them. Appeals go to a second moderator. Demo buttons run
 * clearly labelled simulations on synthetic data.
 */
import { $, api, esc, pct } from "./lib.js";

let selected = "";
let data: any = null;

const kpi = (v: string | number, label: string, cls = "") => `<div class="card kpi"><div class="v ${cls}">${v}</div><div class="l">${label}</div></div>`;
const table = (head: string[], rows: string[][]) =>
  `<tr>${head.map((h) => `<th>${h}</th>`).join("")}</tr>` + rows.map((r) => `<tr>${r.map((c) => `<td>${c}</td>`).join("")}</tr>`).join("");
const stars = (n: number) => "★".repeat(n) + "☆".repeat(5 - n);
const sev = (s: string) => `<span class="pill ${s === "high" ? "bad" : s === "medium" ? "warn" : ""}">${esc(s)}</span>`;
const SUGGEST: Record<string, string> = { remove: "Remove and keep out of the rating", reject: "Reject (rule cited)", publish: "Publish",
  keep_out: "Keep the burst out of the rating and unpause it" };
const SCEN: Record<string, [string, string]> = {
  fake5: ["Seller buys fake 5★ reviews", "P1005"], rival1: ["Rival sends 1★ attack", "P1001"],
  viral: ["Real viral day (genuine)", "P1019"], abuse: ["Abusive and tricky reviews", "P1032"], reset: ["Reset", ""],
};

/** Wrap each label quote in <mark>, using the verified rune offsets. */
export function highlight(text: string, labels: any[]): string {
  const runes = Array.from(text);
  const marks: { s: number; e: number; rule: string }[] = [];
  for (const l of labels || []) for (const q of l.quotes || []) if (q.start >= 0 && q.end <= runes.length) marks.push({ s: q.start, e: q.end, rule: l.rule_id });
  marks.sort((a, b) => a.s - b.s);
  let out = "", pos = 0;
  for (const m of marks) {
    if (m.s < pos) continue;
    out += esc(runes.slice(pos, m.s).join("")) + `<mark title="${esc(m.rule)}">${esc(runes.slice(m.s, m.e).join(""))}</mark>`;
    pos = m.e;
  }
  return out + esc(runes.slice(pos).join(""));
}

export function initReviews(): void {
  $("#rvSim").innerHTML = Object.entries(SCEN).map(([k, [label]]) =>
    `<button class="btn ${k === "reset" ? "ghost" : ""} sm" data-sim="${k}">${esc(label)}</button>`).join("");
  $("#t-reviews").addEventListener("click", async (e) => {
    const t = e.target as HTMLElement;
    const sim = t.closest<HTMLElement>("[data-sim]");
    if (sim) {
      const k = sim.dataset.sim!;
      $("#rvSimOut").textContent = "Running simulation…";
      try {
        const r = await api("/api/admin/reviews/sim", { method: "POST", body: JSON.stringify({ scenario: k }) });
        const acts = Object.entries(r.actions || {}).map(([a, n]) => `${n} ${a.replace("_", " ")}`).join(", ");
        const pidLink = SCEN[k][1] ? ` <a href="/p/${SCEN[k][1]}" target="_blank" rel="noopener">See the product page →</a>` : "";
        $("#rvSimOut").innerHTML = `${esc(r.note || "")} ${r.reviews ? `<b>${r.reviews} synthetic reviews:</b> ${esc(acts)}.` : ""}${r.rechecked ? ` ${r.rechecked} re-checked after seller payouts.` : ""}${pidLink}`;
        selected = "";
        await loadReviews();
      } catch (err: any) { $("#rvSimOut").textContent = err.message; }
      return;
    }
    const pick = t.closest<HTMLElement>("[data-case]");
    if (pick) { selected = pick.dataset.case!; await showCase(); renderQueue(); return; }
    const dec = t.closest<HTMLElement>("[data-decide]");
    if (dec) {
      const note = ($("#rvNote") as HTMLTextAreaElement).value;
      const sanctions = Array.from(document.querySelectorAll<HTMLInputElement>("[data-sanction]:checked")).map((x) => x.value);
      try {
        await api(`/api/admin/reviews/cases/${selected}`, { method: "POST", body: JSON.stringify({ decision: dec.dataset.decide, note, sanctions }) });
        selected = "";
        await loadReviews();
      } catch (err: any) { $("#rvDecMsg").textContent = err.message; }
      return;
    }
    const ap = t.closest<HTMLElement>("[data-appeal]");
    if (ap) {
      const id = ap.dataset.appeal!, d = ap.dataset.d!;
      try {
        if (d === "demo") await api(`/api/admin/reviews/appeals/${id}/demo`, { method: "POST" });
        else await api(`/api/admin/reviews/appeals/${id}`, { method: "POST", body: JSON.stringify({ decision: d,
          note: (document.querySelector<HTMLInputElement>(`[data-anote="${id}"]`)?.value || "") }) });
        await loadReviews();
      } catch (err: any) { alert(err.message); }
    }
  });
}

function renderQueue(): void {
  const cases = data.cases || [];
  $("#rvQueue").innerHTML = cases.length ? cases.map((c: any) => `<button class="item rv-case ${c.id === selected ? "on" : ""}" data-case="${esc(c.id)}">
      <span class="t"><b>${esc(c.title)}</b><small>${esc(c.kind.replace(/_/g, " "))} · ${c.review_ids.length} review(s) · suggested: ${esc(SUGGEST[c.suggested] || c.suggested)}${c.synthetic ? " · synthetic" : ""}</small></span>${sev(c.severity)}</button>`).join("")
    : `<p class="muted">Nothing waiting for a person. Run a demo above, or post a review as meera@example.com.</p>`;
}

async function showCase(): Promise<void> {
  if (!selected) { $("#rvCase").innerHTML = `<p class="muted">Pick a case to see its evidence.</p>`; return; }
  const d = await api(`/api/admin/reviews/cases/${selected}`);
  const c = d.case, r = c.rating;
  const isReport = c.kind === "report";
  const sanctions = (c.sanctions || []).map((s: string) => `<label class="row" style="gap:6px"><input type="checkbox" data-sanction value="${esc(s)}"> ${esc(s.replace(/_/g, " "))}</label>`).join("");
  const reviews = d.reviews.map((x: any) => `<article class="rv">
    <div class="rv-head"><span class="stars">${stars(x.stars)}</span><span class="pill">${esc(x.status)}</span>${x.counted ? '<span class="pill good">counted</span>' : '<span class="pill">not counted</span>'}
      <span class="muted">${esc(x.id)} · ${esc(x.customer_id)}${x.synthetic ? ' · <span class="pill">synthetic</span>' : ""} · ${x.verified ? "verified purchase" : "unverified"}</span></div>
    <p class="rv-text">${highlight(x.text, x.labels)}</p>${x.photo_text ? `<p class="muted">Photo text: ${esc(x.photo_text)}</p>` : ""}
    <p class="muted rv-meta">Tagger: <b>${esc(x.tagger)}</b>${x.tagger_status ? ` (${esc(x.tagger_status)})` : ""}${x.model ? ` · model ${esc(x.model)}` : ""} · policy ${esc(x.policy_version)}${x.prompt_version ? ` · prompt ${esc(x.prompt_version)}` : ""} · ${(x.latency_ms || 0).toFixed(1)} ms</p>
    ${(x.labels || []).length ? `<p>Labels: ${x.labels.map((l: any) => `<span class="pill">${esc(l.rule_id)} · ${esc(l.severity)}</span>`).join("")} target <b>${esc(x.target || "")}</b></p>` : ""}
    ${(x.discarded || []).length || (x.verifier_notes || []).length ? `<p class="warn">Verifier: ${(x.verifier_notes || []).map(esc).join("; ")}</p>` : ""}
    <details><summary class="muted">Signals (${(x.signals || []).length}) and reasons</summary><ul>${(x.signals || []).map((s: any) =>
      `<li><b>${esc(s.name)}</b> ${s.rule ? `<span class="pill">${esc(s.rule)}</span>` : ""}${esc(s.detail)}</li>`).join("")}${(x.reasons || []).map((s: string) => `<li class="muted">${esc(s)}</li>`).join("")}</ul></details>
  </article>`).join("");
  $("#rvCase").innerHTML = `<div class="row" style="justify-content:space-between"><h3>${esc(c.title)}</h3>${sev(c.severity)}</div>
    <p class="muted">${esc(c.kind.replace(/_/g, " "))} · ${esc(c.product_name || "")} (${esc(c.product_id)}) · seller ${esc(c.seller || "")} · case ${esc(c.id)}</p>
    <p>Rating shown <b>${r.shown.toFixed(1)}</b> from ${r.n} verified reviews · naive average of everything submitted <b>${r.naive.toFixed(1)}</b> (${r.naive_n})${r.paused ? ' · <span class="warn">paused (burst)</span>' : ""}${r.frozen ? ' · <span class="warn">frozen</span>' : ""}</p>
    ${(c.summary || []).length ? `<div class="scroll"><table>${table(["evidence", "rule", "reviews", "example"], c.summary.map((e: any) =>
      [esc(e.signal.replace(/_/g, " ")), e.rule ? `<span class="pill">${esc(e.rule)}</span>` : "", String(e.count), esc(e.detail)]))}</table></div>` : ""}
    ${(c.evidence || []).length && !c.summary.length ? `<ul>${c.evidence.map((e: string) => `<li>${esc(e)}</li>`).join("")}</ul>` : ""}
    ${c.status === "open" ? `<div class="card rv-decide"><p><b>Suggested:</b> ${esc(isReport ? "Keep the review (a report alone never removes one)" : SUGGEST[c.suggested] || c.suggested)}</p>
      ${sanctions ? `<p class="muted">Sanctions (only a person applies them; leave unticked unless the evidence justifies it):</p><div class="row">${sanctions}</div>` : ""}
      <textarea class="inp" id="rvNote" placeholder="Decision note (required, goes to the audit log)" style="min-height:60px"></textarea>
      <div class="row" style="margin-top:8px"><button class="btn sm" data-decide="approve">${isReport ? "Keep review" : "Approve suggestion"}</button>
      <button class="btn ghost sm" data-decide="dismiss">${isReport ? "Remove review (report holds)" : "Dismiss: evidence doesn't hold up"}</button><span id="rvDecMsg" class="bad"></span></div></div>`
      : `<p class="muted">Decided: <b>${esc(c.decision)}</b> by ${esc(c.decided_by)} · ${esc(c.note || "")}${(c.applied || []).length ? ` · sanctions: ${esc(c.applied.join(", "))}` : ""}</p>`}
    <h3 style="margin-top:12px">Reviews in this case</h3>${reviews || '<p class="muted">The burst case holds the rating; the reviews sit in their own cases.</p>'}`;
}

export async function loadReviews(): Promise<void> {
  data = await api("/api/admin/reviews");
  const m = data.metrics, lat = m.latency_ms || {};
  const taggerLabel = data.tagger === "llm" ? "LLM tagger (via gateway)" : "rule tagger (no LLM configured)";
  $("#rvKpis").innerHTML = kpi(esc(data.tagger), taggerLabel) + kpi(m.open_cases, "cases waiting", m.open_cases ? "warn" : "") +
    kpi(m.open_appeals, "appeals waiting") + kpi(m.reviews, `reviews (${m.synthetic} synthetic)`) + kpi(pct(m.llm_share), "reviews tagged by the LLM") +
    kpi(`$${(m.cost_per_review_usd || 0).toFixed(5)}`, `LLM cost per review @ $${m.cost_per_m_tokens_usd}/M`) +
    kpi(lat.clear ? `${lat.clear.p95} ms` : "–", "p95 clear review (target < 1 s)", lat.clear && lat.clear.p95 > 1000 ? "bad" : "good") +
    kpi(lat.llm ? `${lat.llm.p95} ms` : "–", "p95 LLM path (target < 5 s)", lat.llm && lat.llm.p95 > 5000 ? "bad" : "") +
    kpi(pct(m.overturn_rate), "overturned by people (dismissed + appeals)");
  $("#rvVersions").textContent = `policy ${data.policy_version} · prompt ${data.prompt_version} · router ${data.router_mode}`;
  renderQueue();
  if (!data.cases.some((c: any) => c.id === selected)) selected = data.cases.length ? data.cases[0].id : "";
  await showCase();
  renderQueue();
  const names = data.product_names || {};
  $("#rvRatings").innerHTML = table(["product", "shown (trusted)", "verified reviews", "naive average", "all reviews", "state"],
    data.ratings.map((r: any) => [`<a href="/p/${esc(r.product_id)}" target="_blank" rel="noopener">${esc(names[r.product_id] || r.product_id)}</a>`,
      `<b>${r.shown.toFixed(1)}</b>`, String(r.n), r.naive.toFixed(1), String(r.naive_n), r.paused ? '<span class="warn">paused: burst</span>' : r.frozen ? '<span class="warn">frozen</span>' : "—"]));
  $("#rvAppeals").innerHTML = (data.appeals || []).length ? table(["appeal", "from", "reviews", "says", "first decided by", "status", ""],
    data.appeals.map((a: any) => [esc(a.id), esc(a.by_role === "seller" ? `seller ${a.by}` : `author ${a.by}`), esc(a.review_ids.join(", ")) + (a.reason ? `<br><small class="muted">${esc(a.reason)}</small>` : ""),
      esc(a.text), esc(a.first_decider), a.status === "open" ? '<span class="warn">open</span>' : `${esc(a.status)} by ${esc(a.decided_by)}${a.decider_kind === "simulated" ? ' <span class="pill">Demo</span>' : ""}`,
      a.status === "open" ? `<span class="apbox"><input class="inp sm" data-anote="${esc(a.id)}" placeholder="Note"><button class="btn sm" data-appeal="${esc(a.id)}" data-d="overturn">Overturn</button>
        <button class="btn ghost sm" data-appeal="${esc(a.id)}" data-d="uphold">Uphold</button>${data.sim ? `<button class="btn ghost sm" data-appeal="${esc(a.id)}" data-d="demo" title="A simulated second moderator, labelled Demo">Demo second moderator</button>` : ""}</span>` : esc(a.note || "")]))
    : `<tr><td class="muted">No appeals. Authors appeal from the product page; sellers from the seller page (seller@example.com).</td></tr>`;
  $("#rvRecent").innerHTML = table(["review", "product", "stars", "action", "counted", "tagger", "rules", "ms"],
    data.recent.map((x: any) => [esc(x.id), esc(x.product_id), stars(x.stars), esc(x.action || x.status), x.counted ? "yes" : "no", esc(x.tagger || ""),
      esc((x.rules || []).join(" ")), (x.latency_ms || 0).toFixed(1)]));
  const live = m.live_precision || {};
  const ev = data.eval;
  const rules = Array.from(new Set([...Object.keys(live), ...Object.keys(ev?.per_rule || {})])).sort();
  $("#rvRules").innerHTML = rules.length ? table(["rule", "eval precision", "eval recall", "eval support", "live precision (people's decisions)", "decisions"],
    rules.map((k) => { const e = ev?.per_rule?.[k], l = live[k];
      return [esc(k), e ? e.precision.toFixed(3) : "–", e ? e.recall.toFixed(3) : "–", e ? String(e.support) : "–", l ? l.precision.toFixed(3) : "–", l ? String(l.decisions) : "0"]; }))
    : `<tr><td class="muted">Run the evals (Evals tab or <code>make eval</code>) to see per-rule precision and recall.</td></tr>`;
  $("#rvEval").innerHTML = ev ? `<p class="note">Last eval: ${esc(ev.tagger)} tagger, policy ${esc(ev.policy_version)}, ${ev.n} dev cases${ev.heldout ? `, ${ev.heldout.n} held-out` : ", held-out split empty"}. The dev set was written next to the rules, so expect lower numbers on held-out cases.</p>` +
    `<div class="scroll"><table>${table(["metric", "value"], Object.entries(ev.rates).sort().map(([k, v]: any) => [esc(k), `<span class="${v >= 0.99 ? "good" : "warn"}">${(+v).toFixed(4)}</span>`]))}</table></div>`
    : `<p class="muted">No eval run yet. Use the Evals tab (suite: reviews).</p>`;
  $("#rvAudit").innerHTML = table(["time", "actor", "action", "target", "note", "versions"],
    data.audit.map((a: any) => [new Date(a.ts).toLocaleTimeString(), `${esc(a.actor)}${a.actor_kind === "simulated" ? ' <span class="pill">Demo</span>' : ""}`, esc(a.action), esc(a.target),
      esc(a.note || ""), esc([a.tagger, a.model, a.prompt_version, a.policy_version].filter(Boolean).join(" · "))]));
  $("#rvDecided").innerHTML = (data.decided || []).length ? table(["case", "decision", "by", "note", "sanctions"],
    data.decided.map((c: any) => [esc(c.title), esc(c.decision), esc(c.decided_by), esc(c.note || ""), esc((c.applied || []).join(", "))])) : `<tr><td class="muted">No decisions yet.</td></tr>`;
}
