/*
 * Ask Sauby · MyFlipKart.com — ReviewGuard Lab
 * Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
 *
 * Type any review, pick who "wrote" it, and see every stage of ReviewGuard:
 * normalization, content rules, behaviour signals, tagger, verifier and the
 * decision with the rule cited. The server runs the real pipeline as a dry run
 * (POST /api/reviews/lab): nothing is saved and no rating changes.
 */
import { $, api, esc, paintIcons, themeButton } from "./lib.js";

type Signal = { name: string; rule?: string; kind: string; strength: string; detail: string; start: number; end: number };
type Label = { rule_id: string; severity: string; quotes: { text: string; start: number; end: number }[] };
type Result = {
  product: string; seller: string; stars: number; author: { id: string; name: string }; author_note?: string;
  action: string; status: string; counted: boolean; verified: boolean; severity: string; suggested?: string;
  rules: { id: string; name: string; action: string }[]; reasons: string[]; signals: Signal[];
  labels: Label[]; discarded: Label[]; verifier_notes: string[]; tagger: string; tagger_status?: string;
  language: string; target?: string; normalized?: string; shown?: string;
  stages: { name: string; state: string; detail: string }[]; policy_version: string; took_ms: number;
};
type Info = {
  products: { id: string; name: string; seller: string; category: string }[];
  authors: { id: string; name: string; note: string }[];
  presets: { label: string; author: string; stars: number; text: string; expect: string }[];
  default_product: string; tagger: string; policy_version: string;
};

const ACTION: Record<string, { label: string; cls: string; say: string }> = {
  publish: { label: "Published", cls: "good", say: "Shown on the product page and counted in the rating." },
  mask_publish: { label: "Published, masked", cls: "good", say: "Contact details or links are masked, then it's shown." },
  not_counted: { label: "Shown, not counted", cls: "warn", say: "Visible, but left out of the product's rating." },
  hold: { label: "Held for a person", cls: "warn", say: "Not shown yet. A moderator decides; the rating is unaffected." },
  reject: { label: "Rejected", cls: "bad", say: "Not shown. The author sees the rule it broke and can appeal." },
};
const STATE_ICON: Record<string, string> = { ok: "✓", flag: "!", block: "✕", skip: "–" };
const stars = (n: number) => "★".repeat(n) + "☆".repeat(5 - n);

let info: Info;
const history: { text: string; res: Result }[] = [];

/** Highlight signal spans (rune offsets into the original text). */
function highlight(text: string, sigs: Signal[]): string {
  const runes = Array.from(text);
  const spans = sigs.filter((s) => s.start >= 0 && s.end > s.start && s.end <= runes.length)
    .map((s) => [s.start, s.end, s.name] as const).sort((a, b) => a[0] - b[0]);
  let out = "", at = 0;
  for (const [s, e, name] of spans) {
    if (s < at) continue; // overlapping span: keep the first
    out += esc(runes.slice(at, s).join("")) + `<mark title="${esc(name)}">${esc(runes.slice(s, e).join(""))}</mark>`;
    at = e;
  }
  return out + esc(runes.slice(at).join(""));
}

function render(text: string, r: Result): void {
  const a = ACTION[r.action] || { label: r.action, cls: "", say: "" };
  const pill = (t: string, cls = "") => `<span class="pill ${cls}">${esc(t)}</span>`;
  const sigRows = r.signals.map((s) => `<tr><td><b>${esc(s.name)}</b>${s.rule ? ` <span class="tag">${esc(s.rule)}</span>` : ""}</td>
      <td>${esc(s.kind)}</td><td><span class="lab-str ${esc(s.strength)}">${esc(s.strength)}</span></td><td>${esc(s.detail)}</td></tr>`).join("");
  const labels = (ls: Label[], cls: string) => ls.map((l) => `<span class="pill ${cls}">${esc(l.rule_id)} · ${esc(l.severity)}${
    l.quotes.length ? ` · “${esc(l.quotes.map((q) => q.text).join("”, “"))}”` : ""}</span>`).join("");

  $("#result").innerHTML = `
    <div class="lab-verdict ${a.cls}">
      <div><div class="lab-action">${esc(a.label)}</div><div class="lab-say">${esc(a.say)}</div></div>
      <div class="row">${pill(r.counted ? "counted" : "not counted", r.counted ? "good" : "warn")}${
        pill(r.verified ? "verified purchase" : "unverified", r.verified ? "good" : "warn")}${pill("severity " + r.severity, r.severity === "high" ? "bad" : "")}</div>
    </div>
    <p class="muted lab-meta">${esc(r.author.name)} · <span class="stars">${stars(r.stars)}</span> · ${esc(r.product)} (${esc(r.seller)}) · ${r.took_ms} ms</p>
    ${r.rules.length ? `<div class="lab-sec"><b>Rules cited</b><ul class="lab-list">${r.rules.map((x) =>
      `<li><span class="tag">${esc(x.id)}</span> ${esc(x.name)} <span class="muted">→ ${esc(x.action)}</span></li>`).join("")}</ul></div>` : ""}
    ${r.reasons.length ? `<div class="lab-sec"><b>Why</b><ul class="lab-list">${r.reasons.map((x) => `<li>${esc(x)}</li>`).join("")}</ul></div>` : ""}
    ${r.suggested ? `<p class="lab-sec"><b>Suggested to the moderator:</b> ${esc(r.suggested)}</p>` : ""}
    ${r.author_note ? `<p class="note">${esc(r.author_note)}</p>` : ""}

    <div class="lab-sec"><b>Pipeline</b>
      <ol class="lab-steps">${r.stages.map((s) => `<li class="${esc(s.state)}"><span class="dot" aria-label="${esc(s.state)}">${STATE_ICON[s.state] || "·"}</span>
        <div><b>${esc(s.name)}</b><div class="muted">${esc(s.detail)}</div></div></li>`).join("")}</ol></div>

    <div class="lab-sec"><b>What was found in the text</b>
      <p class="rv-text lab-text">${highlight(text, r.signals)}</p>
      ${r.normalized ? `<p class="muted lab-hint">Normalized: <code>${esc(r.normalized)}</code></p>` : ""}</div>
    ${r.shown != null && r.shown !== "" && r.shown !== text ? `<div class="lab-sec"><b>As shoppers would see it</b><p class="lab-text">${esc(r.shown)}</p></div>` : ""}

    ${r.signals.length ? `<div class="lab-sec"><b>Signals</b><div class="scroll"><table class="lab-table">
      <thead><tr><th>Signal</th><th>Kind</th><th>Strength</th><th>Detail</th></tr></thead><tbody>${sigRows}</tbody></table></div></div>` : ""}
    ${r.labels.length || r.discarded.length || r.verifier_notes.length ? `<div class="lab-sec"><b>Tagger labels</b>
      <div>${labels(r.labels, "")}${labels(r.discarded, "bad")}</div>
      ${r.verifier_notes.length ? `<ul class="lab-list muted">${r.verifier_notes.map((n) => `<li>${esc(n)}</li>`).join("")}</ul>` : ""}</div>` : ""}
    <p class="muted lab-hint">Tagger: ${esc(r.tagger)}${r.tagger_status ? ` (${esc(r.tagger_status)})` : ""} · language ${esc(r.language)}${
      r.target ? ` · aimed at ${esc(r.target)}` : ""} · policy ${esc(r.policy_version)}</p>`;
}

function renderHistory(): void {
  $("#history").innerHTML = history.length ? `<div class="lab-hist">${history.map((h, i) => {
    const a = ACTION[h.res.action] || { label: h.res.action, cls: "" };
    return `<button class="lab-hrow" data-i="${i}"><span class="pill ${a.cls}">${esc(a.label)}</span>
      <span class="stars">${stars(h.res.stars)}</span><span class="muted">${esc(h.res.author.name)}</span>
      <span class="t">${esc(h.text)}</span></button>`;
  }).join("")}</div>` : `<p class="muted">No checks yet.</p>`;
  document.querySelectorAll<HTMLButtonElement>(".lab-hrow").forEach((b) => b.addEventListener("click", () => {
    const h = history[+b.dataset.i!];
    $<HTMLTextAreaElement>("#text").value = h.text;
    render(h.text, h.res);
    $("#result").scrollIntoView({ behavior: "smooth", block: "start" });
  }));
}

function setStars(n: number): void {
  const el = document.querySelector<HTMLInputElement>(`#stars input[value="${n}"]`);
  if (el) el.checked = true;
}

function showAuthorNote(): void {
  const a = info.authors.find((x) => x.id === $<HTMLSelectElement>("#author").value);
  $("#authorNote").textContent = a ? a.note : "";
}

async function run(e?: Event): Promise<void> {
  e?.preventDefault();
  const text = $<HTMLTextAreaElement>("#text").value;
  const star = document.querySelector<HTMLInputElement>("#stars input:checked");
  if (!text.trim() || !star) return;
  const btn = $<HTMLButtonElement>("#run");
  btn.disabled = true;
  $("#result").classList.add("busy");
  try {
    const res = await api<Result>("/api/reviews/lab", { method: "POST", body: JSON.stringify({
      product_id: $<HTMLSelectElement>("#product").value, author: $<HTMLSelectElement>("#author").value, stars: +star.value, text }) });
    render(text, res);
    history.unshift({ text, res });
    if (history.length > 30) history.pop();
    renderHistory();
    if (window.matchMedia("(max-width:820px)").matches) $("#result").scrollIntoView({ behavior: "smooth", block: "start" });
  } catch (err: any) {
    $("#result").innerHTML = `<h3>Result</h3><p class="err">${esc(err.message)}</p>`;
  } finally {
    btn.disabled = false;
    $("#result").classList.remove("busy");
  }
}

async function init(): Promise<void> {
  themeButton($("#theme"));
  try { info = await api<Info>("/api/reviews/lab"); }
  catch (e: any) { $("#result").innerHTML = `<p class="err">${esc(e.message)}</p>`; return; }

  $("#product").innerHTML = info.products.map((p) =>
    `<option value="${esc(p.id)}"${p.id === info.default_product ? " selected" : ""}>${esc(p.name)} · ${esc(p.seller)}</option>`).join("");
  $("#author").innerHTML = info.authors.map((a) => `<option value="${esc(a.id)}">${esc(a.name)}</option>`).join("");
  $("#stars").innerHTML = `<legend>Stars</legend>` + [1, 2, 3, 4, 5].map((n) =>
    `<label><input type="radio" name="stars" value="${n}"${n === 5 ? " checked" : ""}><span>${n}★</span></label>`).join("");
  $("#tagger").textContent = info.tagger === "llm" ? "Tagger: LLM (with rule fallback)" : "Tagger: rules (no LLM in this demo)";
  $("#presets").innerHTML = info.presets.map((p, i) => `<button type="button" class="chip sm" data-i="${i}">${esc(p.label)}</button>`).join("");
  document.querySelectorAll<HTMLButtonElement>("#presets .chip").forEach((b) => b.addEventListener("click", () => {
    const p = info.presets[+b.dataset.i!];
    $<HTMLSelectElement>("#product").value = info.default_product;
    $<HTMLSelectElement>("#author").value = p.author;
    $<HTMLTextAreaElement>("#text").value = p.text;
    setStars(p.stars);
    showAuthorNote();
    run();
  }));
  $("#author").addEventListener("change", showAuthorNote);
  $("#labForm").addEventListener("submit", run);
  $("#clear").addEventListener("click", () => { history.length = 0; renderHistory(); });
  showAuthorNote();
  renderHistory();
  paintIcons();
}

init();
