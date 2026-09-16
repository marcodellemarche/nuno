#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Regenerate Nuno's brand assets.

This script is the source of truth for the mark. It draws the ring, outlines
"nun" from Space Grotesk Bold, and writes both the SVG sources here and the
raster assets the admin UI serves in internal/api/static/.

Construction, all in a 120x120 viewBox:

  * a circle at (60,60), r=44, stroke-width 20, round caps;
  * a foreground arc of 80% of the circumference, and an accent arc of 10%
    right after it, so the 10% gap between them sits at the top: a ring almost
    complete with a slice missing;
  * the gap is 10% rather than the ~2% a naive 88/10 split leaves, because
    round caps of radius 10 eat a small gap whole.

Colours are the three the app already uses, never new ones:

  foreground  #1c1c1a light / #e6e6e3 dark
  accent      #3f7d4f light / #6fae7d dark
  background  #fbfbfa light / #15161a dark

Requirements: fontTools, Pillow, and a headless Chrome to rasterise the SVGs.
The variable Space Grotesk font is downloaded from Google Fonts on first run
and cached next to this script.
"""

import math
import os
import shutil
import subprocess
import sys
import tempfile
import urllib.request

from fontTools.misc.transform import Transform
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.ttLib import TTFont
from fontTools.varLib.instancer import instantiateVariableFont
from PIL import Image

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BRAND = os.path.join(ROOT, "brand")
STATIC = os.path.join(ROOT, "internal", "api", "static")
FONT_URL = "https://raw.githubusercontent.com/google/fonts/main/ofl/spacegrotesk/SpaceGrotesk%5Bwght%5D.ttf"
FONT_CACHE = os.path.join(BRAND, "SpaceGrotesk.ttf")

FG_LIGHT, FG_DARK = "#1c1c1a", "#e6e6e3"
ACCENT_LIGHT, ACCENT_DARK = "#3f7d4f", "#6fae7d"
BG_LIGHT = "#fbfbfa"

CX = CY = 60.0
R = 44.0
STROKE = 20.0
SMALL_STROKE = 25.0   # the 16/32px favicon, where 20 would fade
GAP = 36.0            # 10% empty, centred at the top
ACCENT = 36.0         # 10% accent slice, clockwise from the gap
TOP = 270.0


def point(deg):
    a = math.radians(deg)
    return (CX + R * math.cos(a), CY + R * math.sin(a))


def arc(p1, p2, large):
    return "M%.2f %.2fA%.0f %.0f 0 %d 1 %.2f %.2f" % (p1[0], p1[1], R, R, large, p2[0], p2[1])


FG_PATH = arc(point(TOP + GAP / 2), point(TOP - GAP / 2), 1)
ACCENT_PATH = arc(point(TOP + GAP / 2), point(TOP + GAP / 2 + ACCENT), 0)

THEME = (
    "    .nuno-fg { color: %s; }\n"
    "    @media (prefers-color-scheme: dark) { .nuno-fg { color: %s; } }\n"
    "    .nuno-accent { stroke: %s; }\n"
    "    @media (prefers-color-scheme: dark) { .nuno-accent { stroke: %s; } }"
    % (FG_LIGHT, FG_DARK, ACCENT_LIGHT, ACCENT_DARK)
)


def ring(stroke_fg, stroke_accent, width, accent_class=True):
    accent = ' class="nuno-accent"' if accent_class else ""
    return (
        '    <g fill="none" stroke-width="%g" stroke-linecap="round">\n'
        '      <path class="nuno-fg" d="%s" stroke="%s"/>\n'
        '      <path d="%s"%s stroke="%s"/>\n'
        "    </g>\n" % (width, FG_PATH, stroke_fg, ACCENT_PATH, accent, stroke_accent)
    )


def write_mark():
    svg = (
        '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 120" role="img" aria-label="Nuno">\n'
        "  <style>\n"
        "    .nuno-fg { color: %s; }\n"
        "    @media (prefers-color-scheme: dark) { .nuno-fg { color: %s; } }\n"
        "  </style>\n"
        % (FG_LIGHT, FG_DARK)
        + ring("currentColor", "currentColor", STROKE, accent_class=False)
        + "</svg>\n"
    )
    open(os.path.join(BRAND, "mark.svg"), "w").write(svg)

    # The favicon is the same glyph with a thicker stroke so 16px still reads.
    favicon = (
        '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 120" role="img" aria-label="Nuno">\n'
        "  <style>\n"
        "    .nuno-fg { color: %s; }\n"
        "    @media (prefers-color-scheme: dark) { .nuno-fg { color: %s; } }\n"
        "  </style>\n"
        % (FG_LIGHT, FG_DARK)
        + ring("currentColor", "currentColor", SMALL_STROKE, accent_class=False)
        + "</svg>\n"
    )
    open(os.path.join(STATIC, "favicon.svg"), "w").write(favicon)


def glyph_path(glyphset, cmap, char, x, baseline):
    pen = SVGPathPen(glyphset)
    glyphset[cmap[ord(char)]].draw(TransformPen(pen, Transform(1, 0, 0, -1, x, baseline)))
    return pen.getCommands()


def write_wordmark():
    font = instantiateVariableFont(TTFont(FONT_CACHE), {"wght": 700})
    glyphset, cmap = font.getGlyphSet(), font.getBestCmap()

    baseline, advance, tracking = 520.0, 616.0, -20.0  # ~-2% of the em
    pen = 0.0
    glyphs = []
    for char in "nun":
        glyphs.append('    <path d="%s"/>' % glyph_path(glyphset, cmap, char, pen, baseline))
        pen += advance + tracking

    # The ring takes the o's advance and its overshoot.
    ring_advance, ring_outer = 612.0, 524.0
    scale = ring_outer / 108.0
    cx, cy = pen + ring_advance / 2, baseline - 248.0
    tx, ty = cx - 60 * scale, cy - 60 * scale
    ring_wm = (
        '    <g class="nuno-fg" transform="translate(%.2f %.2f) scale(%.4f)" fill="none" stroke-width="%g" stroke-linecap="round">\n'
        '      <path d="%s" stroke="currentColor"/>\n'
        '      <path d="%s" class="nuno-accent" stroke="%s"/>\n'
        "    </g>" % (tx, ty, scale, STROKE, FG_PATH, ACCENT_PATH, ACCENT_LIGHT)
    )

    svg = (
        '<svg xmlns="http://www.w3.org/2000/svg" viewBox="60 0 2306 544" role="img" aria-label="nuno">\n'
        "  <style>\n"
        + THEME
        + "\n  </style>\n"
        '  <g class="nuno-fg" fill="currentColor">\n' + "\n".join(glyphs) + "\n  </g>\n"
        + ring_wm
        + "\n</svg>\n"
    )
    open(os.path.join(BRAND, "wordmark.svg"), "w").write(svg)


def write_app_icon():
    svg = (
        '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 120">\n'
        '  <rect width="120" height="120" rx="26" fill="%s"/>\n'
        '  <g transform="translate(15 15) scale(0.75)">\n' % ACCENT_LIGHT
        + ring("#ffffff", "#ffffff", STROKE, accent_class=False)
        + "  </g>\n</svg>\n"
    )
    open(os.path.join(BRAND, "app-icon.svg"), "w").write(svg)


def chrome():
    for name in ("google-chrome", "chromium", "chromium-browser"):
        path = shutil.which(name)
        if path:
            return path
    sys.exit("no headless Chrome found to rasterise the SVGs")


def rasterise(svg_name, size, out, transparent=True):
    chrome_bin = chrome()
    with tempfile.TemporaryDirectory() as tmp:
        wrapper = os.path.join(tmp, "wrapper.html")
        open(wrapper, "w").write(
            "<!doctype html><html><head><meta charset=utf-8><style>"
            "html,body{margin:0;background:transparent;color-scheme:light}"
            "img{display:block;width:%dpx;height:%dpx}</style></head>"
            '<body><img src="file://%s"></body></html>' % (size, size, os.path.join(BRAND, svg_name))
        )
        cmd = [
            chrome_bin, "--headless=new", "--disable-gpu", "--no-sandbox", "--hide-scrollbars",
            "--user-data-dir=" + os.path.join(tmp, "profile"),
            "--window-size=%d,%d" % (size, size),
        ]
        if transparent:
            cmd.append("--default-background-color=00000000")
        cmd += ["--screenshot=" + out, "file://" + wrapper]
        subprocess.run(cmd, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def write_rasters():
    with tempfile.TemporaryDirectory() as tmp:
        # A fixed-colour copy for the tab icons: a static PNG cannot follow the
        # theme, so it takes the light foreground.
        fg = os.path.join(tmp, "favicon-fg.svg")
        open(fg, "w").write(
            '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 120">\n'
            '  <g fill="none" stroke="%s" stroke-width="%g" stroke-linecap="round">\n'
            '    <path d="%s"/>\n    <path d="%s"/>\n  </g>\n</svg>\n'
            % (FG_LIGHT, SMALL_STROKE, FG_PATH, ACCENT_PATH)
        )
        shutil.copy(fg, os.path.join(BRAND, "favicon-fg.svg"))
        master = os.path.join(tmp, "favicon-master.png")
        rasterise("favicon-fg.svg", 1024, master)
        favicon = Image.open(master).convert("RGBA")
        favicon.resize((32, 32), Image.LANCZOS).save(os.path.join(STATIC, "favicon-32.png"))
        favicon.resize((16, 16), Image.LANCZOS).save(os.path.join(STATIC, "favicon-16.png"))
        favicon.save(os.path.join(STATIC, "favicon.ico"), format="ICO", sizes=[(16, 16), (32, 32), (48, 48)])

        app_master = os.path.join(tmp, "app-master.png")
        rasterise("app-icon.svg", 1024, app_master)
        app = Image.open(app_master).convert("RGBA")
        app.resize((180, 180), Image.LANCZOS).save(os.path.join(STATIC, "apple-touch-icon.png"))
        app.resize((512, 512), Image.LANCZOS).save(os.path.join(STATIC, "icon-512.png"))


def ensure_font():
    if os.path.exists(FONT_CACHE):
        return
    print("downloading Space Grotesk to", FONT_CACHE)
    urllib.request.urlretrieve(FONT_URL, FONT_CACHE)


if __name__ == "__main__":
    os.makedirs(STATIC, exist_ok=True)
    ensure_font()
    write_mark()
    write_wordmark()
    write_app_icon()
    write_rasters()
    print("wrote brand/{mark,wordmark,app-icon,favicon-fg}.svg and internal/api/static/favicon.*")
