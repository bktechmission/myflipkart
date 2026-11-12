/*
 * Ask Sauby · MyFlipKart.com — seller view of reviews (ReviewGuard)
 * Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
 *
 * A seller sees each product's trusted rating and which reviews stopped counting,
 * grouped by reason in general terms (never who wrote them), and can appeal a group.
 * A person who isn't linked to the seller decides the appeal.
 */
import { $, api, esc, paintIcons, themeButton } from "./lib.js";

const stars = (n: number) => "★".repeat(n) + "☆".repeat(5 - n);

async function load(): Promise<void> {
  let d: any;
  try { d = await api("/api/seller/reviews"); }
  catch (e: any) { $("#sellerProducts").innerHTML = `<div class="card"><p>${esc(e.message)}. Sign in as seller@example.com.</p></div>`; return; }
  $("#sellerName").textContent = `${d.seller} · reviews`;
  const products = d.products || [];
  const notCounted = products.reduce((n: number, p: any) => n + p.not_counted.reduce((m: number, g: any) => m + g.count, 0), 0);
  const kpi = (v: any, l: string, cls = "") => `<div class="card kpi"><div class="v ${cls}">${v}</div><div class="l">${l}</div></div>`;
  $("#sellerKpis").innerHTML = kpi(products.length, "products") + kpi(notCounted, "reviews not counted", notCounted ? "warn" : "") +
    kpi(d.strikes, "review-fraud strikes", d.strikes ? "bad" : "good");
  $("#sellerProducts").innerHTML = products.map((p: any) => `<div class="card"><div class="row" style="justify-content:space-between">
      <h3><a href="/p/${esc(p.id)}" target="_blank" rel="noopener">${esc(p.name)}</a></h3>
      <span><span class="stars">${stars(Math.round(p.rating.shown))}</span> <b>${p.rating.shown.toFixed(1)}</b> <span class="muted">from ${p.rating.n} verified</span></span></div>
    ${p.rating.note ? `<p class="note">${esc(p.rating.note)}</p>` : ""}
    <p class="muted">${p.counted} counted · ${p.waiting} with our review team</p>
    ${p.not_counted.map((g: any) => `<div class="apcard"><p><b>${g.count} not counted:</b> ${esc(g.reason)}</p>
      <p class="muted" style="font-size:13px">${g.rows.map((r: any) => `${esc(r.date)} ${stars(r.stars)}${r.shown ? "" : " (hidden)"}`).join(" · ")}</p>
      ${g.appeal ? `<p class="muted">Appeal: ${esc(g.appeal)}</p>` : `<form class="row sl-appeal" data-p="${esc(p.id)}" data-r="${esc(g.reason)}">
        <input class="inp" name="t" maxlength="1000" placeholder="Why should these count? A second person will look." style="flex:1" required><button class="btn ghost sm">Appeal</button></form>`}</div>`).join("")}
    </div>`).join("") || `<div class="card"><p class="muted">No products.</p></div>`;
  document.querySelectorAll<HTMLFormElement>(".sl-appeal").forEach((f) => f.addEventListener("submit", async (e) => {
    e.preventDefault();
    try {
      await api("/api/seller/appeals", { method: "POST", body: JSON.stringify({ product_id: f.dataset.p, reason: f.dataset.r,
        text: (f.elements.namedItem("t") as HTMLInputElement).value }) });
      load();
    } catch (err: any) { alert(err.message); }
  }));
}

paintIcons();
themeButton($("#theme"));
load();
