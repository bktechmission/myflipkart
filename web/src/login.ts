/*
 * Ask Sauby · MyFlipKart.com — sign-in page
 * Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
 *
 * Shows the configured identity providers. Both buttons start the same OpenID Connect
 * flow on the server (/auth/login?provider=google|mock); guests get a browse-only session.
 */
import { $, api, themeButton } from "./lib.js";

async function init(): Promise<void> {
  const err = new URLSearchParams(location.search).get("error");
  if (err) { const e = $("#err"); e.textContent = err; e.classList.remove("hide"); }
  const res = await fetch("/auth/providers").then((r) => r.json()).catch(() => ({ google_configured: false }));
  if (!(res.providers || []).some((p: any) => p.name === "mock")) $("#mock").classList.add("hide");
  if (!res.google_configured) {
    const g = $<HTMLAnchorElement>("#google");
    g.removeAttribute("href");
    g.setAttribute("aria-disabled", "true");
    g.style.opacity = ".5";
    g.style.pointerEvents = "none";
    $("#googleHint").classList.remove("hide");
  }
  $("#guest").addEventListener("click", async () => {
    await api("/auth/guest", { method: "POST" }).catch(() => fetch("/auth/guest", { method: "POST" }));
    location.href = "/";
  });
}
themeButton($("#theme"));
init();
