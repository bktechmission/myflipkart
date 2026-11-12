/*
 * Ask Sauby · MyFlipKart.com — product page reviews (ReviewGuard)
 * Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
 *
 * Signed-in shoppers write a review for an order they own once it's delivered, see
 * what happened to it (published, masked, waiting for a person, or the rule it broke)
 * and can appeal. The rating and published reviews are rendered by the server.
 */
import { $, api, esc } from "./lib.js";

const ratingEl = $("#rating");
const pid = ratingEl?.dataset.pid || "";
const box = $("#rv-mine");

const stars = (n: number) => "★".repeat(n) + "☆".repeat(5 - n);
const key = () => (crypto as any).randomUUID ? (crypto as any).randomUUID().replace(/-/g, "") : Date.now().toString(36) + Math.random().toString(36).slice(2, 12);

function paintRating(r: any): void {
  if (!r || !ratingEl) return;
  ratingEl.innerHTML = `<span class="stars" aria-hidden="true">${stars(Math.round(r.shown))}</span> <b>${r.shown.toFixed(1)}</b>
    <span class="muted">${r.n ? `based on ${r.n} verified review${r.n === 1 ? "" : "s"}` : "no verified reviews yet (category average)"}</span>`;
}

function mine(r: any): string {
  const tone = r.status === "published" ? (r.counted ? "good" : "warn") : r.status === "held" || r.status === "pending" ? "warn" : "bad";
  const rules = (r.rules || []).map((x: any) => `<li><b>${esc(x.id)}</b> ${esc(x.name.replace(/_/g, " "))}: ${esc(x.definition)}</li>`).join("");
  const canAppeal = !r.appeal && (r.status === "rejected" || r.status === "removed" || (r.status === "published" && !r.counted));
  const appeal = r.appeal ? `<p class="muted">Appeal ${esc(r.appeal.status)}${r.appeal.note ? `: ${esc(r.appeal.note)}` : ""}</p>`
    : canAppeal ? `<form class="row rv-appeal" data-id="${esc(r.id)}"><input class="inp" name="t" maxlength="1000" placeholder="Why should a second person look again?" style="flex:1" required>
      <button class="btn ghost sm">Appeal</button></form>` : "";
  return `<article class="rv mine"><div class="rv-head"><span class="stars">${stars(r.stars)}</span><span class="pill ${tone}">${esc(r.status)}</span>
    <span class="muted">Your review · checked by ${r.tagger === "llm" ? "the LLM tagger + rules" : r.tagger === "rules" ? "the rule tagger" : "rules (nothing to tag)"}</span></div>
    <p>${esc(r.shown || r.text)}</p><p class="note">${esc(r.message)}</p>${rules ? `<ul class="rv-rules">${rules}</ul>` : ""}${appeal}</article>`;
}

function form(reviewable: any[]): string {
  if (!reviewable.length) return "";
  const opts = reviewable.map((o) => `<option value="${esc(o.order_id)}">Order ${esc(o.order_id)} · delivered ${esc(o.delivered_on)}</option>`).join("");
  return `<form class="card rv-form" id="rvForm"><h3>Write a review</h3>
    <label class="muted">Order<select class="inp" name="order" style="display:block;margin:4px 0 10px">${opts}</select></label>
    <fieldset class="rv-stars"><legend class="muted">Your rating</legend>${[1, 2, 3, 4, 5].map((n) =>
      `<label><input type="radio" name="stars" value="${n}" ${n === 5 ? "checked" : ""}><span>${n}★</span></label>`).join("")}</fieldset>
    <textarea class="inp" name="text" maxlength="2000" placeholder="What did you like or dislike? Honest negative reviews are always published." style="min-height:110px"></textarea>
    <div class="row" style="margin-top:8px"><button class="btn">Post review</button><span class="muted" id="rvMsg"></span></div></form>`;
}

async function load(): Promise<void> {
  if (!pid || !box) return;
  const res = await fetch(`/api/reviews/mine?product=${encodeURIComponent(pid)}`, { credentials: "same-origin" });
  if (res.status === 401) {
    box.innerHTML = `<p class="muted"><a href="/login">Sign in</a> to review a product you bought (demo accounts: meera@example.com has delivered orders).</p>`;
    return;
  }
  const d = await res.json();
  box.innerHTML = (d.banned ? `<p class="bad">This account can't post reviews right now.</p>` : form(d.reviewable || [])) + (d.reviews || []).map(mine).join("");
  wire();
}

function wire(): void {
  const f = $<HTMLFormElement>("#rvForm");
  if (f) {
    const idem = key(); // one key per form: a double click or a retry can't post twice
    f.addEventListener("submit", async (e) => {
      e.preventDefault();
      const btn = f.querySelector("button")!;
      btn.disabled = true;
      $("#rvMsg").textContent = "Saving, then checking…";
      try {
        const fd = new FormData(f);
        const out = await api("/api/reviews", { method: "POST", body: JSON.stringify({ order_id: fd.get("order"), product_id: pid,
          stars: Number(fd.get("stars")), text: String(fd.get("text") || ""), idem_key: idem }) });
        paintRating(out.rating);
        await load();
      } catch (err: any) {
        $("#rvMsg").textContent = err.message;
        btn.disabled = false;
      }
    });
  }
  box.querySelectorAll<HTMLFormElement>(".rv-appeal").forEach((a) => a.addEventListener("submit", async (e) => {
    e.preventDefault();
    try {
      await api(`/api/reviews/${a.dataset.id}/appeal`, { method: "POST", body: JSON.stringify({ text: (a.elements.namedItem("t") as HTMLInputElement).value }) });
      await load();
    } catch (err: any) { alert(err.message); }
  }));
}

load();
