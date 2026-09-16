<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# Nuno brand

The mark is drawn, not typed. `generate.py` is the source of truth: it draws
the ring, outlines `nun` from Space Grotesk Bold, and writes both the SVG
sources here and the raster assets the admin UI serves.

## The ring

In a 120x120 viewBox: a circle at (60,60) with r=44, stroke-width 20, round
caps. It is two arcs, not one:

- a **foreground** arc of 80% of the circumference, `currentColor`;
- an **accent** arc of 10%, immediately after it;
- the remaining **10%** is the gap, centred at the top.

It reads as a ring almost complete with a slice missing — "almost full", which
is what a quota tool is about. The gap is 10% rather than the ~2% a naive
88/10 split leaves, because round caps of radius 10 swallow anything smaller:
the two ends would touch and the ring would look closed.

## Colours

The three the app already uses, never new ones:

| Role | Light | Dark |
|---|---|---|
| Foreground | `#1c1c1a` | `#e6e6e3` |
| Accent (the slice) | `#3f7d4f` | `#6fae7d` |
| Background | `#fbfbfa` | `#15161a` |

`wordmark.svg` uses `currentColor` for the letters and the foreground arc, so
it recolours from CSS. The accent slice carries the light/dark pair through an
embedded `prefers-color-scheme` query, because an SVG loaded as an image
cannot inherit the page's variables.

## Files

| File | What it is |
|---|---|
| `mark.svg` | the ring alone, monochrome `currentColor` |
| `wordmark.svg` | `nun` outlined + the ring as the final `o` |
| `app-icon.svg` | the green rounded tile with a white ring |
| `favicon-fg.svg` | generated: the favicon ring in a fixed colour |

The served assets — `favicon.svg`, `favicon.ico`, the 16/32px PNGs, the
180px touch icon and the 512px icon — live in `internal/api/static/`.

## Regenerating

```sh
python3 brand/generate.py
```

It needs `fontTools` and `Pillow`, and a headless Chrome to rasterise. The
variable Space Grotesk font is downloaded from Google Fonts on first run.
