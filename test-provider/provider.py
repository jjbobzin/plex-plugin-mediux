#!/usr/bin/env python3
"""Throwaway Plex metadata providers for testing Mediux-style artwork in Plex.

Two providers run from this one server:

- Movies at  /     : solid pink (or POSTER_COLOR) poster and teal background.
- TV shows at /tv  : labeled images so mix-ups are easy to spot: the show
  poster reads "SHOW", season posters read "S1", "S2"..., and episode title
  cards read "S1E3". Show structure (seasons/episodes) comes from TVmaze.

Every request Plex makes is printed and appended to requests.log.

Run:  python3 provider.py            (listens on port 32600)
Add to Plex:  http://<this-mac-ip>:32600   and   http://<this-mac-ip>:32600/tv
"""

import json
import os
import re
import socket
import struct
import sys
import zlib
from datetime import datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.error import URLError
from urllib.parse import parse_qs, quote, urlparse
from urllib.request import Request, urlopen

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 32600
IDENTIFIER = "tv.plex.agents.custom.liftbridge.mediuxtest"
TV_IDENTIFIER = "tv.plex.agents.custom.liftbridge.mediuxtesttv"
HERE = Path(__file__).resolve().parent
LOG_FILE = HERE / "requests.log"
ITEMS_FILE = HERE / "items.json"


# --- images -------------------------------------------------------------

GLYPHS = {
    "0": [" ### ", "#   #", "#  ##", "# # #", "##  #", "#   #", " ### "],
    "1": ["  #  ", " ##  ", "  #  ", "  #  ", "  #  ", "  #  ", " ### "],
    "2": [" ### ", "#   #", "    #", "   # ", "  #  ", " #   ", "#####"],
    "3": ["#####", "   # ", "  #  ", "   # ", "    #", "#   #", " ### "],
    "4": ["   # ", "  ## ", " # # ", "#  # ", "#####", "   # ", "   # "],
    "5": ["#####", "#    ", "#### ", "    #", "    #", "#   #", " ### "],
    "6": ["  ## ", " #   ", "#    ", "#### ", "#   #", "#   #", " ### "],
    "7": ["#####", "    #", "   # ", "  #  ", " #   ", " #   ", " #   "],
    "8": [" ### ", "#   #", "#   #", " ### ", "#   #", "#   #", " ### "],
    "9": [" ### ", "#   #", "#   #", " ####", "    #", "   # ", " ##  "],
    "S": [" ####", "#    ", "#    ", " ### ", "    #", "    #", "#### "],
    "E": ["#####", "#    ", "#    ", "#### ", "#    ", "#    ", "#####"],
    "H": ["#   #", "#   #", "#   #", "#####", "#   #", "#   #", "#   #"],
    "O": [" ### ", "#   #", "#   #", "#   #", "#   #", "#   #", " ### "],
    "W": ["#   #", "#   #", "#   #", "# # #", "# # #", "## ##", "#   #"],
}


def make_png(width, height, rgb, text=""):
    """Build a PNG of one color, optionally with white block-letter text, without any image library."""
    bg = bytes(rgb) * width
    rows = [bg] * height
    text = "".join(c for c in text.upper() if c in GLYPHS)
    if text:
        cols = len(text) * 6 - 1
        scale = max(1, min(int(width * 0.8 / cols), int(height * 0.4 / 7)))
        x0 = (width - cols * scale) // 2
        y0 = (height - 7 * scale) // 2
        for r in range(7):
            line = bytearray(bg)
            for i, ch in enumerate(text):
                for c, px in enumerate(GLYPHS[ch][r]):
                    if px == "#":
                        x = x0 + (i * 6 + c) * scale
                        line[x * 3:(x + scale) * 3] = b"\xff" * 3 * scale
            for y in range(y0 + r * scale, y0 + (r + 1) * scale):
                rows[y] = bytes(line)
    raw = b"".join(b"\x00" + row for row in rows)

    def chunk(kind, data):
        body = kind + data
        return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body))

    header = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", header)
            + chunk(b"IDAT", zlib.compress(raw, 6)) + chunk(b"IEND", b""))


# ART_ONLY=1 leaves out titles/dates so Plex fills them from the next provider.
ART_ONLY = os.environ.get("ART_ONLY") == "1"
POSTER_HEX = os.environ.get("POSTER_COLOR", "ff00aa").lower()
POSTER_RGB = bytes.fromhex(POSTER_HEX)
# The color is part of the file name: Plex will not re-download an image whose URL it already has.
POSTER_NAME = "poster.png" if POSTER_HEX == "ff00aa" else f"poster-{POSTER_HEX}.png"

IMAGES = {
    POSTER_NAME: make_png(1000, 1500, POSTER_RGB),            # pink unless overridden
    "background.png": make_png(1920, 1080, (0, 170, 170)),  # teal
}
GENERATED = {}  # cache for labeled images: /img/<w>x<h>/<hex>/<label>.png

PALETTE = ["e6194b", "3cb44b", "4363d8", "f58231", "911eb4", "42d4f4",
           "f032e6", "9a6324", "800000", "008080", "000075", "808000"]


def lan_ip():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        s.connect(("192.0.2.1", 80))  # no packet is sent; just picks the LAN interface
        return s.getsockname()[0]
    except OSError:
        return "127.0.0.1"
    finally:
        s.close()


# PUBLIC_URL lets image links point at a tunnel when Plex can't reach this machine directly.
BASE_URL = (os.environ.get("PUBLIC_URL") or f"http://{lan_ip()}:{PORT}").rstrip("/")


def labeled_url(width, height, hex_color, label):
    return f"{BASE_URL}/img/{width}x{height}/{hex_color}/{label}.png"


# --- shared helpers -----------------------------------------------------

def load_items():
    try:
        return json.loads(ITEMS_FILE.read_text())
    except (OSError, ValueError):
        return {}


ITEMS = load_items()


def save_items():
    ITEMS_FILE.write_text(json.dumps(ITEMS, indent=2))


def container(identifier, metadata=None, images=None, offset=0, total=None):
    entries = metadata if metadata is not None else images
    out = {"offset": offset, "totalSize": len(entries) if total is None else total,
           "identifier": identifier, "size": len(entries)}
    out["Metadata" if metadata is not None else "Image"] = entries
    return {"MediaContainer": out}


def provider_definition(identifier, title, types):
    return {
        "MediaProvider": {
            "identifier": identifier,
            "title": title,
            "version": "0.0.2",
            "Types": [{"type": t, "Scheme": [{"scheme": identifier}]} for t in types],
            "Feature": [
                {"type": "metadata", "key": "/library/metadata"},
                {"type": "match", "key": "/library/metadata/matches"},
            ],
        }
    }


# --- movies -------------------------------------------------------------

def rating_key_for(hints):
    """Prefer an external ID from Plex's hints; fall back to title + year."""
    guid = str(hints.get("guid") or "")
    m = re.match(r"^(tmdb|imdb|tvdb)://(.+)$", guid)
    if m:
        key = f"{m.group(1)}-{m.group(2)}"
    else:
        key = f"title-{hints.get('title', 'unknown')}-{hints.get('year', '')}"
    return re.sub(r"[^a-zA-Z0-9_-]", "_", key).strip("_")[:120]


def image_list():
    return [
        {"type": "coverPoster", "url": f"{BASE_URL}/img/{POSTER_NAME}", "alt": "Mediux test poster"},
        {"type": "background", "url": f"{BASE_URL}/img/background.png", "alt": "Mediux test background"},
    ]


def movie_metadata(rating_key):
    info = ITEMS.get(rating_key, {})
    year = info.get("year")
    item = {
        "ratingKey": rating_key,
        "key": f"/library/metadata/{rating_key}",
        "guid": f"{IDENTIFIER}://movie/{rating_key}",
        "type": "movie",
        "title": info.get("title") or rating_key,
        "originallyAvailableAt": f"{year}-01-01" if year else "1900-01-01",
        "thumb": f"{BASE_URL}/img/{POSTER_NAME}",
        "art": f"{BASE_URL}/img/background.png",
        "Image": image_list(),
    }
    if year:
        item["year"] = int(year)
    if ART_ONLY:
        for field in ("title", "originallyAvailableAt", "year"):
            item.pop(field, None)
    return item


# --- TV -----------------------------------------------------------------

TVMAZE = {}  # cache: url -> parsed json (or None)


def tvmaze(path):
    if path in TVMAZE:
        return TVMAZE[path]
    try:
        req = Request(f"https://api.tvmaze.com{path}", headers={"User-Agent": "liftbridge-mediux-test"})
        with urlopen(req, timeout=10) as resp:
            data = json.loads(resp.read())
    except (URLError, ValueError, TimeoutError) as err:
        print(f"TVmaze {path} failed: {err}", flush=True)
        data = None
    TVMAZE[path] = data
    return data


def find_show(title, hints):
    """Return a TVmaze show id from any IDs in the hints, else by title."""
    blob = " ".join(str(hints.get(k) or "") for k in ("guid", "filename"))
    for pattern, param in ((r"tvdb[-:/=]+(\d+)", "thetvdb"), (r"(tt\d{5,})", "imdb")):
        m = re.search(pattern, blob)
        if m:
            show = tvmaze(f"/lookup/shows?{param}={m.group(1)}")
            if show:
                return show["id"]
    if title:
        show = tvmaze(f"/singlesearch/shows?q={quote(title)}")
        if show:
            return show["id"]
        results = tvmaze(f"/search/shows?q={quote(title)}") or []
        if results:
            return results[0]["show"]["id"]
    return None


def show_key_for(title, hints):
    show_id = find_show(title, hints)
    if show_id:
        key = f"tvm-{show_id}"
    else:
        key = "t-" + re.sub(r"[^a-zA-Z0-9]", "_", title or "unknown")[:80]
    ITEMS.setdefault(key, {"title": title, "hints": hints})
    save_items()
    return key


def episodes_of(show_key):
    """[(season, episode), ...] for regular episodes, from TVmaze."""
    m = re.match(r"^tvm-(\d+)$", show_key)
    if not m:
        return []
    eps = tvmaze(f"/shows/{m.group(1)}/episodes") or []
    return [(e["season"], e["number"]) for e in eps if e.get("number") is not None]


def tv_item(show_key, season=None, episode=None):
    show_guid = f"{TV_IDENTIFIER}://show/{show_key}"
    title = ITEMS.get(show_key, {}).get("title")
    if season is None:
        item = {
            "ratingKey": show_key, "key": f"/library/metadata/{show_key}", "guid": show_guid,
            "type": "show", "title": title or show_key,
            "thumb": labeled_url(1000, 1500, "ff8800", "SHOW"),
            "art": labeled_url(1920, 1080, "008080", "SHOW"),
        }
        item["Image"] = [{"type": "coverPoster", "url": item["thumb"]},
                         {"type": "background", "url": item["art"]}]
    else:
        season_key = f"{show_key}-s{season}"
        season_guid = f"{TV_IDENTIFIER}://season/{season_key}"
        if episode is None:
            item = {
                "ratingKey": season_key, "key": f"/library/metadata/{season_key}", "guid": season_guid,
                "type": "season", "title": f"Season {season}", "index": season,
                "parentRatingKey": show_key, "parentKey": f"/library/metadata/{show_key}",
                "parentGuid": show_guid, "parentType": "show", "parentTitle": title or show_key,
                "thumb": labeled_url(1000, 1500, PALETTE[season % len(PALETTE)], f"S{season}"),
            }
            item["Image"] = [{"type": "coverPoster", "url": item["thumb"]}]
        else:
            ep_key = f"{show_key}-s{season}e{episode}"
            item = {
                "ratingKey": ep_key, "key": f"/library/metadata/{ep_key}",
                "guid": f"{TV_IDENTIFIER}://episode/{ep_key}",
                "type": "episode", "title": f"Episode {episode}", "index": episode, "parentIndex": season,
                "parentRatingKey": season_key, "parentKey": f"/library/metadata/{season_key}",
                "parentGuid": season_guid, "parentType": "season", "parentTitle": f"Season {season}",
                "grandparentRatingKey": show_key, "grandparentKey": f"/library/metadata/{show_key}",
                "grandparentGuid": show_guid, "grandparentType": "show",
                "grandparentTitle": title or show_key,
                "thumb": labeled_url(1920, 1080, PALETTE[(season + episode) % len(PALETTE)],
                                      f"S{season}E{episode}"),
            }
            item["Image"] = [{"type": "snapshot", "url": item["thumb"]}]
    if ART_ONLY:
        for field in ("title", "parentTitle", "grandparentTitle"):
            item.pop(field, None)
    return item


def tv_children(show_key, season=None):
    eps = episodes_of(show_key)
    if season is None:
        return [tv_item(show_key, s) for s in sorted({s for s, _ in eps})]
    return [tv_item(show_key, s, e) for s, e in eps if s == season]


def tv_grandchildren(show_key):
    return [tv_item(show_key, s, e) for s, e in episodes_of(show_key)]


def parse_tv_key(key):
    m = re.match(r"^(tvm-\d+|t-[A-Za-z0-9_]+?)(?:-s(\d+)(?:e(\d+))?)?$", key)
    if not m:
        return None
    return m.group(1), (int(m.group(2)) if m.group(2) else None), (int(m.group(3)) if m.group(3) else None)


def with_children(item, show_key, season):
    kids = tv_children(show_key, season)
    item["Children"] = {"size": len(kids), "Metadata": kids}
    return item


def tv_match(hints):
    kind = str(hints.get("type"))
    want_children = str(hints.get("includeChildren")) == "1"
    if kind in ("2", "show"):
        show_key = show_key_for(hints.get("title"), hints)
        item = tv_item(show_key)
        return [with_children(item, show_key, None) if want_children else item]
    if kind in ("3", "season") and hints.get("index") is not None:
        show_key = show_key_for(hints.get("parentTitle"), hints)
        season = int(hints["index"])
        item = tv_item(show_key, season)
        return [with_children(item, show_key, season) if want_children else item]
    if kind in ("4", "episode") and hints.get("index") is not None and hints.get("parentIndex") is not None:
        show_key = show_key_for(hints.get("grandparentTitle"), hints)
        return [tv_item(show_key, int(hints["parentIndex"]), int(hints["index"]))]
    return []


# --- HTTP ---------------------------------------------------------------

def log_request(handler, body):
    entry = {
        "time": datetime.now().isoformat(timespec="seconds"),
        "from": handler.client_address[0],
        "method": handler.command,
        "path": handler.path,
        "plexHeaders": {k: v for k, v in handler.headers.items() if k.lower().startswith("x-plex")},
        "body": body,
    }
    text = json.dumps(entry, indent=2)
    print(text, flush=True)
    with LOG_FILE.open("a") as f:
        f.write(text + "\n")


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass  # our own logging covers it

    def send_json(self, payload, status=200):
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def send_png(self, data):
        self.send_response(200)
        self.send_header("Content-Type", "image/png")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def param(self, name, default=None):
        query = parse_qs(urlparse(self.path).query)
        return self.headers.get(name) or (query.get(name) or [default])[0]

    def do_GET(self):
        path = urlparse(self.path).path.rstrip("/") or "/"
        log_request(self, None)

        if path.startswith("/img/"):
            name = path[len("/img/"):]
            m = re.match(r"^(\d+)x(\d+)/([0-9a-f]{6})/([A-Z0-9]+)\.png$", name)
            if m:
                if name not in GENERATED:
                    GENERATED[name] = make_png(int(m.group(1)), int(m.group(2)),
                                               bytes.fromhex(m.group(3)), m.group(4))
                return self.send_png(GENERATED[name])
            if name in IMAGES:
                return self.send_png(IMAGES[name])
            return self.send_json({"error": "not found"}, 404)

        if path == "/tv" or path.startswith("/tv/"):
            return self.tv_get(path[len("/tv"):] or "/")

        if path == "/":
            return self.send_json(provider_definition(IDENTIFIER, "Mediux Test (Liftbridge)", [1]))

        m = re.match(r"^/library/metadata/([a-zA-Z0-9_-]+)(/images)?$", path)
        if m:
            if m.group(2):
                return self.send_json(container(IDENTIFIER, images=image_list()))
            return self.send_json(container(IDENTIFIER, metadata=[movie_metadata(m.group(1))]))

        self.send_json({"error": "not found"}, 404)

    def tv_get(self, path):
        if path == "/":
            return self.send_json(provider_definition(TV_IDENTIFIER, "Mediux Test TV (Liftbridge)", [2, 3, 4]))

        m = re.match(r"^/library/metadata/([a-zA-Z0-9_-]+)(/images|/children|/grandchildren)?$", path)
        parsed = parse_tv_key(m.group(1)) if m else None
        if not parsed:
            return self.send_json({"error": "not found"}, 404)
        show_key, season, episode = parsed
        sub = m.group(2)
        item = tv_item(show_key, season, episode)

        if sub == "/images":
            return self.send_json(container(TV_IDENTIFIER, images=item["Image"]))
        if sub in ("/children", "/grandchildren"):
            if sub == "/children":
                kids = tv_children(show_key, season)
            else:
                kids = tv_grandchildren(show_key)
            # Plex has been seen sending a 0-based start; page from there.
            start = int(self.param("X-Plex-Container-Start", "0"))
            size = int(self.param("X-Plex-Container-Size", "20"))
            return self.send_json(container(TV_IDENTIFIER, metadata=kids[start:start + size],
                                            offset=start, total=len(kids)))
        if self.param("includeChildren") == "1" and episode is None:
            item = with_children(item, show_key, season)
        return self.send_json(container(TV_IDENTIFIER, metadata=[item]))

    def do_POST(self):
        path = urlparse(self.path).path.rstrip("/")
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        try:
            hints = json.loads(raw or b"{}")
        except ValueError:
            hints = {}
        # Plex may send hints as query parameters instead of a JSON body.
        if not hints:
            hints = {k: v[0] for k, v in parse_qs(urlparse(self.path).query).items()
                     if not k.lower().startswith("x-plex")}
        log_request(self, hints)

        if path == "/tv/library/metadata/matches":
            return self.send_json(container(TV_IDENTIFIER, metadata=tv_match(hints)))
        if path != "/library/metadata/matches":
            return self.send_json({"error": "not found"}, 404)
        if str(hints.get("type")) not in ("1", "movie"):
            return self.send_json(container(IDENTIFIER, metadata=[]))

        key = rating_key_for(hints)
        ITEMS[key] = {"title": hints.get("title"), "year": hints.get("year"), "hints": hints}
        save_items()
        self.send_json(container(IDENTIFIER, metadata=[movie_metadata(key)]))


if __name__ == "__main__":
    print(f"Mediux test providers running. Movies: {BASE_URL}   TV: {BASE_URL}/tv", flush=True)
    print(f"Logging requests to {LOG_FILE}", flush=True)
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
