"""Generates docs/architecture.excalidraw (open it at https://excalidraw.com to edit).

Ask Sauby · MyFlipKart.com
Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake

Run:  python3 docs/tools/make_architecture.py
"""
import json
import random
from pathlib import Path

rng = random.Random(42)
els = []

INK = "#1e1e1e"
FILLS = {"edge": "#a5d8ff", "core": "#d0bfff", "llm": "#ffd8a8", "rag": "#b2f2bb",
         "data": "#c3fae8", "loop": "#fff3bf", "human": "#ffc9c9"}


def base(t, x, y, w, h, **kw):
    e = {"id": f"e{len(els)}", "type": t, "x": x, "y": y, "width": w, "height": h, "angle": 0,
         "strokeColor": kw.pop("stroke", INK), "backgroundColor": kw.pop("bg", "transparent"),
         "fillStyle": "solid", "strokeWidth": kw.pop("sw", 2), "strokeStyle": kw.pop("style", "solid"),
         "roughness": 1, "opacity": kw.pop("opacity", 100), "groupIds": [], "frameId": None,
         "roundness": kw.pop("roundness", {"type": 3}), "seed": rng.randint(1, 2**31),
         "version": 1, "versionNonce": rng.randint(1, 2**31), "isDeleted": False,
         "boundElements": [], "updated": 1, "link": None, "locked": False}
    e.update(kw)
    els.append(e)
    return e


def text(x, y, s, size=20, color=INK, container=None, align="left", w=None):
    lines = s.split("\n")
    width = w or max(len(l) for l in lines) * size * 0.55
    height = len(lines) * size * 1.25
    t = base("text", x, y, width, height, stroke=color, roundness=None,
             text=s, originalText=s, fontSize=size, fontFamily=1, textAlign=align,
             verticalAlign="middle" if container else "top", containerId=container,
             lineHeight=1.25, baseline=int(size * 0.9))
    return t


def box(x, y, w, h, label, kind, size=18):
    r = base("rectangle", x, y, w, h, bg=FILLS[kind])
    lines = label.split("\n")
    th = len(lines) * size * 1.25
    # vertical centering: Excalidraw's export offsets bound text by line count, compensate
    t = text(x + 6, y + (h - th) / 2 + size * 0.6 + (len(lines) - 2) * size * 0.8, label, size,
             container=r["id"], align="center", w=w - 12)
    r["boundElements"].append({"type": "text", "id": t["id"]})
    return r


def zone(x, y, w, h, title, color):
    base("rectangle", x, y, w, h, bg=color, stroke=color, sw=1, opacity=35)
    text(x + 14, y + 8, title, 20, color="#495057")


def arrow(x1, y1, x2, y2, label=None, dashed=False, color=INK):
    a = base("arrow", x1, y1, x2 - x1, y2 - y1, stroke=color, roundness={"type": 2},
             style="dashed" if dashed else "solid", points=[[0, 0], [x2 - x1, y2 - y1]],
             lastCommittedPoint=None, startBinding=None, endBinding=None,
             startArrowhead=None, endArrowhead="arrow")
    if label:
        text((x1 + x2) / 2 + 6, (y1 + y2) / 2 - 22, label, 15, color="#495057")
    return a


text(30, 20, "Ask Sauby · MyFlipKart.com — architecture", 30)
text(30, 62, "One chat turn: authenticate → route with an LLM → run a tool (RAG search or order action) → answer → measure → improve", 17, color="#495057")

# Row 1: edge + identity + humans
zone(20, 100, 1660, 150, "Customer and edge", "#dbe4ff")
box(40, 140, 220, 90, "Customer\nTypeScript web app", "edge")
box(320, 140, 260, 90, "Edge\nTLS · WAF · API gateway", "edge")
box(640, 140, 250, 90, "Identity\nGoogle / MockID (OIDC)", "edge")
box(1380, 140, 280, 90, "Human agent console\nsame chat · cid + sid + context", "human")
arrow(260, 185, 320, 185)
arrow(580, 185, 640, 185)

# Row 2: Go server
zone(20, 280, 1660, 200, "Ask Sauby server (Go)", "#e5dbff")
box(40, 335, 240, 110, "API\nauthN · authZ\nrate limit · SSE", "core")
box(340, 335, 300, 110, "Orchestrator\nconfirm gate → route →\ntool → answer", "core")
box(700, 335, 280, 110, "LLM gateway\ncomplexity routing\nbreakers · failover", "llm")
box(1040, 320, 300, 60, "Internal open-source LLM\n(Ollama, e.g. qwen2.5)", "llm", 16)
box(1040, 400, 300, 60, "External LLM (complex turns)\nany OpenAI-compatible API", "llm", 16)
box(1380, 335, 280, 110, "Live handoff hub\nqueue · join · close", "human")
arrow(450, 230, 160, 335)
arrow(280, 390, 340, 390)
arrow(640, 390, 700, 390)
arrow(980, 370, 1040, 350)
arrow(980, 410, 1040, 430)
arrow(1520, 335, 1520, 230, dashed=True, color="#c92a2a")

# Row 3: tools
zone(20, 510, 1660, 170, "Tools", "#d3f9d8")
box(40, 548, 200, 52, "BM25 keyword index", "rag", 16)
box(40, 614, 200, 52, "Vector index", "rag", 16)
box(300, 565, 180, 90, "RRF fusion\nmerge ranks", "rag")
box(530, 565, 200, 90, "Reranker\ntop 20 → top 5", "rag")
box(790, 560, 420, 100, "Order tools + guardrails\nown orders only · confirm first\nrefunds over $30 go to a person", "human", 16)
box(1270, 560, 390, 100, "Filters\nprice range · category", "rag", 16)
arrow(240, 574, 300, 600)
arrow(240, 640, 300, 625)
arrow(480, 610, 530, 610)
arrow(420, 445, 140, 548)
arrow(560, 445, 980, 560)
arrow(1460, 445, 1100, 560, dashed=True, color="#c92a2a")

# Row 4: data
zone(20, 710, 1660, 150, "Data stores", "#c5f6fa")
box(40, 750, 360, 100, "PostgreSQL (strong consistency)\norders · refunds · approvals\nproducts · policy docs", "data", 16)
box(440, 755, 320, 90, "NoSQL (DynamoDB / Mongo)\nconversations by cid + sid", "data", 16)
box(800, 755, 260, 90, "Prometheus\nlatency · SLOs · KPIs", "data", 16)
box(1100, 755, 260, 90, "Logs (Loki / S3)\none JSON line per turn", "data", 16)
box(1400, 755, 260, 90, "BM25 + vector indexes\nrebuilt when docs change", "data", 16)
arrow(980, 660, 260, 755)
arrow(505, 445, 505, 755, dashed=True)

# Row 5: loop
zone(20, 890, 1660, 120, "Continuous improvement loop", "#fff9db")
for i, (lbl) in enumerate(["Feedback +\ninsights", "Golden set\n(+ mined cases)", "Evals +\nrelease gates", "Canary\n% of chats", "Promote or\nroll back"]):
    box(40 + i * 330, 930, 260, 64, lbl, "loop", 17)
    if i:
        arrow(40 + i * 330 - 70, 960, 40 + i * 330, 960)
arrow(1230, 845, 170, 930, dashed=True, color="#e67700")

scene = {"type": "excalidraw", "version": 2, "source": "https://github.com/bktechmission/myflipkart",
         "elements": els, "appState": {"viewBackgroundColor": "#ffffff", "gridSize": None}, "files": {}}
out = Path(__file__).resolve().parent.parent / "architecture.excalidraw"
out.write_text(json.dumps(scene, indent=1))
print(f"wrote {out} ({len(els)} elements)")
