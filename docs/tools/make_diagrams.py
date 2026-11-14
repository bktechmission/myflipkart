"""Generates the system diagrams in docs/architecture/: an editable Excalidraw file
(open it at https://excalidraw.com) and a rendered SVG from the same spec, so the two
never drift. The chat diagram is docs/architecture.excalidraw (make_architecture.py).

Ask Sauby · MyFlipKart.com
Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake

Run:  python3 docs/tools/make_diagrams.py
"""
import json
import random
from html import escape
from pathlib import Path

OUT = Path(__file__).resolve().parent.parent / "architecture"
INK, MUTE = "#1e1e1e", "#495057"
FILLS = {"edge": "#a5d8ff", "core": "#d0bfff", "llm": "#ffd8a8", "rag": "#b2f2bb",
         "data": "#c3fae8", "loop": "#fff3bf", "human": "#ffc9c9", "guard": "#e9ecef"}


class Diagram:
    def __init__(self, name, title, subtitle, width, height):
        self.name, self.title, self.subtitle, self.w, self.h = name, title, subtitle, width, height
        self.items = []  # ("zone"|"box"|"arrow", dict)

    def zone(self, x, y, w, h, title, color):
        self.items.append(("zone", dict(x=x, y=y, w=w, h=h, title=title, color=color)))

    def box(self, x, y, w, h, label, kind, size=16):
        self.items.append(("box", dict(x=x, y=y, w=w, h=h, label=label, kind=kind, size=size)))

    def arrow(self, x1, y1, x2, y2, label=None, dashed=False, color=INK):
        self.items.append(("arrow", dict(x1=x1, y1=y1, x2=x2, y2=y2, label=label, dashed=dashed, color=color)))

    # ---------- Excalidraw ----------
    def excalidraw(self):
        rng = random.Random(self.name)
        els = []

        def base(t, x, y, w, h, **kw):
            e = {"id": f"{self.name}-{len(els)}", "type": t, "x": x, "y": y, "width": w, "height": h, "angle": 0,
                 "strokeColor": kw.pop("stroke", INK), "backgroundColor": kw.pop("bg", "transparent"),
                 "fillStyle": "solid", "strokeWidth": kw.pop("sw", 2), "strokeStyle": kw.pop("style", "solid"),
                 "roughness": 1, "opacity": kw.pop("opacity", 100), "groupIds": [], "frameId": None,
                 "roundness": kw.pop("roundness", {"type": 3}), "seed": rng.randint(1, 2**31),
                 "version": 1, "versionNonce": rng.randint(1, 2**31), "isDeleted": False,
                 "boundElements": [], "updated": 1, "link": None, "locked": False}
            e.update(kw)
            els.append(e)
            return e

        def text(x, y, s, size=18, color=INK, align="left", w=None):
            lines = s.split("\n")
            width = w or max(len(l) for l in lines) * size * 0.55
            base("text", x, y, width, len(lines) * size * 1.25, stroke=color, roundness=None, text=s, originalText=s,
                 fontSize=size, fontFamily=1, textAlign=align, verticalAlign="top", containerId=None,
                 lineHeight=1.25, baseline=int(size * 0.9))

        text(30, 20, self.title, 28)
        text(30, 58, self.subtitle, 16, color=MUTE)
        for kind, d in self.items:
            if kind == "zone":
                base("rectangle", d["x"], d["y"], d["w"], d["h"], stroke=d["color"], bg=d["color"], opacity=35, sw=1)
                text(d["x"] + 12, d["y"] + 8, d["title"], 16, color=MUTE)
            elif kind == "box":
                base("rectangle", d["x"], d["y"], d["w"], d["h"], bg=FILLS[d["kind"]])
                n = d["label"].count("\n") + 1
                text(d["x"] + 6, d["y"] + d["h"] / 2 - n * d["size"] * 0.62, d["label"], d["size"], align="center", w=d["w"] - 12)
            else:
                dx, dy = d["x2"] - d["x1"], d["y2"] - d["y1"]
                base("arrow", d["x1"], d["y1"], dx, dy, stroke=d["color"], roundness={"type": 2},
                     style="dashed" if d["dashed"] else "solid", points=[[0, 0], [dx, dy]], lastCommittedPoint=None,
                     startBinding=None, endBinding=None, startArrowhead=None, endArrowhead="arrow")
                if d["label"]:
                    text((d["x1"] + d["x2"]) / 2 + 6, (d["y1"] + d["y2"]) / 2 - 20, d["label"], 13, color=MUTE)
        return {"type": "excalidraw", "version": 2, "source": "https://github.com/bktechmission/myflipkart",
                "elements": els, "appState": {"viewBackgroundColor": "#ffffff", "gridSize": None}, "files": {}}

    # ---------- SVG ----------
    def svg(self):
        o = [f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {self.w} {self.h}" font-family="Helvetica, Arial, sans-serif">',
             f'<title>{escape(self.title)}</title>',
             '<defs><marker id="a" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">'
             '<path d="M0 0L10 5L0 10z" fill="context-stroke"/></marker></defs>',
             f'<rect width="{self.w}" height="{self.h}" fill="#ffffff"/>',
             f'<text x="30" y="46" font-size="28" font-weight="600" fill="{INK}">{escape(self.title)}</text>',
             f'<text x="30" y="76" font-size="16" fill="{MUTE}">{escape(self.subtitle)}</text>']
        arrows = []
        for kind, d in self.items:
            if kind == "zone":
                o.append(f'<rect x="{d["x"]}" y="{d["y"]}" width="{d["w"]}" height="{d["h"]}" rx="14" fill="{d["color"]}" fill-opacity=".35" stroke="{d["color"]}"/>')
                o.append(f'<text x="{d["x"] + 12}" y="{d["y"] + 24}" font-size="16" fill="{MUTE}">{escape(d["title"])}</text>')
            elif kind == "box":
                o.append(f'<rect x="{d["x"]}" y="{d["y"]}" width="{d["w"]}" height="{d["h"]}" rx="12" fill="{FILLS[d["kind"]]}" stroke="{INK}" stroke-width="2"/>')
                lines = d["label"].split("\n")
                top = d["y"] + d["h"] / 2 - (len(lines) - 1) * d["size"] * 0.62
                for i, l in enumerate(lines):
                    weight = ' font-weight="600"' if i == 0 else ""
                    o.append(f'<text x="{d["x"] + d["w"] / 2}" y="{top + i * d["size"] * 1.25 + d["size"] * 0.35}" font-size="{d["size"]}"{weight} '
                             f'text-anchor="middle" fill="{INK}">{escape(l)}</text>')
            else:
                arrows.append(d)
        for d in arrows:  # arrows on top
            dash = ' stroke-dasharray="8 6"' if d["dashed"] else ""
            o.append(f'<line x1="{d["x1"]}" y1="{d["y1"]}" x2="{d["x2"]}" y2="{d["y2"]}" stroke="{d["color"]}" stroke-width="2"{dash} marker-end="url(#a)"/>')
            if d["label"]:
                o.append(f'<text x="{(d["x1"] + d["x2"]) / 2 + 6}" y="{(d["y1"] + d["y2"]) / 2 - 6}" font-size="13" fill="{MUTE}">{escape(d["label"])}</text>')
        o.append("</svg>")
        return "\n".join(o)

    def write(self):
        OUT.mkdir(exist_ok=True)
        (OUT / f"{self.name}.excalidraw").write_text(json.dumps(self.excalidraw(), indent=1))
        (OUT / f"{self.name}.svg").write_text(self.svg())
        print(f"wrote docs/architecture/{self.name}.excalidraw + .svg")


RED, ORANGE = "#c92a2a", "#e67700"

# ---------------- ReviewGuard ----------------
d = Diagram("reviewguard", "ReviewGuard: one review, end to end",
            "Saved first, checked by rules and behaviour, labelled by an LLM only when needed, verified by code, decided by plain rules, people for anything severe",
            1700, 1060)
d.zone(20, 100, 1660, 150, "Submit (API)", "#dbe4ff")
d.box(40, 140, 240, 90, "Shopper\nown delivered order", "edge")
d.box(340, 140, 280, 90, "POST /api/reviews\nauth · CSRF · rate limit\nidempotency key", "core", 15)
d.box(680, 140, 300, 90, "WAL (fsync)\nreview saved PENDING\nbefore any reply", "data", 15)
d.box(1040, 140, 300, 90, "Moderation job\nruns now; Recover() finishes\nanything left pending", "core", 15)
d.arrow(280, 185, 340, 185)
d.arrow(620, 185, 680, 185)
d.arrow(980, 185, 1040, 185)
d.box(1380, 140, 280, 90, "ReviewGuard Lab · /guard\nPOST /api/reviews/lab\ndry run: nothing saved", "edge", 15)
d.arrow(1440, 230, 1100, 330, dashed=True, label="dry run: Analyze() on a snapshot copy")
d.zone(20, 280, 1660, 200, "Deterministic checks (code)", "#e9ecef")
d.box(40, 330, 280, 120, "Normalize\nNFKC subset · look-alikes\nl33t · zero-width · spacing\n(original kept)", "guard", 15)
d.box(370, 330, 320, 120, "Content rules\ncontact · links · placeholder slurs\ninsults by target · threats\ninjection · language · duplicates", "guard", 15)
d.box(740, 330, 420, 120, "Authenticity (behaviour)\nverified purchase · seller-linked · paid\ntoo fast · new single-seller · templated\nburst (rating pauses) · rival 1★ · rings", "guard", 15)
d.box(1210, 330, 450, 120, "Data it reads\norders + delivery times · identities\n(device, card, address, network)\nseller payouts · other reviews", "data", 15)
d.arrow(1190, 230, 180, 330)
d.arrow(320, 390, 370, 390)
d.arrow(690, 390, 740, 390)
d.arrow(1210, 390, 1160, 390)
d.zone(20, 510, 1660, 200, "Tagger (only for unclear cases, and to label what was found)", "#fff4e6")
d.box(40, 560, 380, 120, "Input guard\nreview + photo text as quoted data\nbetween delimiters · size caps\ninjection flag stated, never hidden", "llm", 15)
d.box(470, 560, 330, 120, "LLM tagger (via gateway)\nonly matching policy clauses\ntemperature 0 · no tools\ncache: hash + policy version", "llm", 15)
d.box(850, 560, 330, 120, "Output guard\nstrict JSON · rule_id enum\nsevere = 2 agreeing runs\nconfidence never read", "llm", 15)
d.box(1230, 560, 430, 120, "Rule tagger (no LLM configured)\nsame JSON from the rule findings\nROUTER=rules: the free public demo\nUI says which tagger ran", "guard", 15)
d.arrow(530, 450, 230, 560, "unclear or flagged")
d.arrow(420, 620, 470, 620)
d.arrow(800, 620, 850, 620)
d.arrow(1060, 450, 1440, 560, dashed=True, label="no LLM")
d.zone(20, 740, 1660, 150, "Decide (code) and act", "#d3f9d8")
d.box(40, 780, 360, 90, "Verifier\nquotes exact at offsets · rule exists\nagrees with the signals", "guard", 15)
d.box(450, 780, 400, 90, "Decision engine (plain rules)\nsame inputs → same action · star-blind\nLLM down/slow/unsure → hold", "core", 15)
d.box(900, 780, 380, 90, "Action\npublish · mask then publish · not counted\nhold for a person · reject (rule cited)", "rag", 15)
d.box(1330, 780, 330, 90, "Trust-weighted rating\nverified + trusted only, Bayesian\n'based on N verified reviews'", "rag", 15)
d.arrow(1000, 680, 220, 780)
d.arrow(400, 825, 450, 825)
d.arrow(850, 825, 900, 825)
d.arrow(1280, 825, 1330, 825)
d.zone(20, 910, 1660, 130, "People, appeals, audit, evals", "#ffe3e3")
d.box(40, 945, 360, 80, "Moderation queue (Admin → Reviews)\ncases by severity and reach", "human", 15)
d.box(450, 945, 330, 80, "Sanctions need a person\nstrike · freeze · delist · ban", "human", 15)
d.box(830, 945, 330, 80, "Appeals\nauthor or seller → a 2nd moderator", "human", 15)
d.box(1210, 945, 220, 80, "Audit log\nmodel · prompt · policy", "data", 15)
d.box(1460, 945, 200, 80, "Evals + gates\nmake gate", "loop", 15)
d.arrow(1090, 870, 220, 945, dashed=True, color=RED, label="held")
d.arrow(400, 985, 450, 985)
d.arrow(780, 985, 830, 985)
d.arrow(1160, 985, 1210, 985)
d.arrow(1430, 985, 1460, 985)
d.write()

# ---------------- Data stores ----------------
d = Diagram("data-stores", "Data stores", "One machine in the demo, the same boundaries as production", 1500, 640)
d.zone(20, 100, 1460, 230, "Strongly consistent (store.Tx: one lock, WAL fsync before reply)", "#c5f6fa")
d.box(40, 150, 420, 150, "WAL: data/wal.jsonl\norders · refunds · approvals · tickets\nusers · policies · product edits\nreviews · cases · appeals · sanctions\nidentities · payouts · audit log", "data", 15)
d.box(520, 150, 420, 150, "In-memory maps (one RWMutex)\nTx stages records, writes the WAL,\nthen applies; replay on start,\ncompaction to a snapshot", "core", 15)
d.box(1000, 150, 460, 150, "Production: PostgreSQL\nrow locks or SERIALIZABLE\nsame Tx boundaries\n(one transaction per action)", "data", 15)
d.arrow(460, 225, 520, 225, "replay")
d.arrow(940, 225, 1000, 225, dashed=True, label="swap")
d.zone(20, 360, 1460, 250, "Other stores", "#e9ecef")
d.box(40, 410, 330, 160, "Conversations\ndata/conversations/<cid>/<sid>.jsonl\nfsync per message\n(prod: DynamoDB / Mongo)", "data", 15)
d.box(400, 410, 330, 160, "Telemetry + feedback\ndata/events.jsonl, no fsync\n(prod: Loki / S3)", "data", 15)
d.box(760, 410, 330, 160, "Search indexes\nBM25 + vectors in memory\nrebuilt and swapped on edit\n(prod: OpenSearch / pgvector)", "rag", 15)
d.box(1120, 410, 340, 160, "Photos + evals\ndata/photos/<cid>/<id>.jpg\ndata/evals/*.json\ngolden_extra.jsonl", "data", 15)
d.write()

# ---------------- Auth ----------------
d = Diagram("auth", "Sign-in and authorization", "OIDC code + PKCE, signed sessions, roles, ownership checked in the tools", 1500, 560)
d.zone(20, 100, 1460, 200, "Authentication", "#dbe4ff")
d.box(40, 150, 260, 110, "Browser\n/login", "edge")
d.box(350, 150, 320, 110, "Google or MockID (OIDC)\ncode + PKCE · state · nonce\nRS256 ID token", "edge", 15)
d.box(720, 150, 330, 110, "/auth/callback\nverify issuer, audience,\nexpiry, signature (JWKS)", "core", 15)
d.box(1100, 150, 360, 110, "Signed HttpOnly cookie\ncid · role · expiry\nSameSite=Lax · Secure on https", "core", 15)
d.arrow(300, 205, 350, 205)
d.arrow(670, 205, 720, 205)
d.arrow(1050, 205, 1100, 205)
d.zone(20, 330, 1460, 200, "Authorization", "#ffe3e3")
d.box(40, 380, 330, 120, "Roles\nguest · customer · seller · admin\n(ADMIN_EMAILS, SELLER_ACCOUNTS)", "human", 15)
d.box(400, 380, 330, 120, "Middleware\nwithAuth: 401 / 403\nX-Sauby CSRF header on writes\nrate limits per session/IP", "core", 15)
d.box(760, 380, 330, 120, "Ownership in the tools\nown orders, chats, photos only\nother customers' ids → 404", "guard", 15)
d.box(1120, 380, 340, 120, "ReviewGuard\nreview own delivered orders only\nsellers: general terms, no identity\nno moderating your own seller", "guard", 15)
d.arrow(1280, 260, 220, 380, dashed=True)
d.write()

# ---------------- Observability + evals ----------------
d = Diagram("observability-evals", "Observability, evals and release gates", "Measure every turn and review; ship only what passes the gates", 1500, 560)
d.zone(20, 100, 1460, 190, "Run time", "#c5f6fa")
d.box(40, 150, 330, 110, "Prometheus /metrics\nsauby_* and reviewguard_*\nlatency · tokens · flags", "data", 15)
d.box(400, 150, 330, 110, "JSON logs + events.jsonl\none line per chat turn", "data", 15)
d.box(760, 150, 330, 110, "Admin dashboards\nOps · Business · Reviews\nSLOs · KPIs · audit log", "human", 15)
d.box(1120, 150, 340, 110, "Feedback + insights\nthumbs-down, zero results,\noverturned decisions", "loop", 15)
d.zone(20, 320, 1460, 210, "Evals (make eval / make gate, CI)", "#fff9db")
d.box(40, 370, 300, 120, "Golden sets\nchat: retrieval, router, e2e\nreviews: dev + held-out", "loop", 15)
d.box(380, 370, 340, 120, "Suites\nretrieval · router · e2e\nreviews (scenarios, fake LLM,\noutput guards, concurrency)", "loop", 15)
d.box(760, 370, 300, 120, "Release gates\nevals/gates.json\nzero-tolerance where it matters", "guard", 15)
d.box(1100, 370, 360, 120, "Canary → promote or roll back\nCANARY_PERCENT pins sessions\nto a candidate agent", "core", 15)
d.arrow(340, 430, 380, 430)
d.arrow(720, 430, 760, 430)
d.arrow(1060, 430, 1100, 430)
d.arrow(1290, 260, 190, 370, dashed=True, color=ORANGE, label="label failures into the golden set")
d.write()
