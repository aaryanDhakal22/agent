"""Render ESC/POS bytes (the subset receipt/builder.go emits) to PNG images that
look like an 80 mm thermal printout, so ticket layout can be reviewed without
paper:

    RECEIPT_PREVIEW_DIR=/tmp/rp go test ./internal/infra/printer/receipt -run TestPreviewDump
    uv run --with pillow python scripts/escpos_preview.py /tmp/rp/*.bin

Writes <name>.png next to each .bin (plus combined.png when given several).
Handles ESC @, ESC ! n, ESC E n, ESC a n, GS ! n, GS B n, GS V (cut), LF.
"""
from __future__ import annotations

import sys
from dataclasses import dataclass, replace
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

DOTS = 576            # printable width of an 80 mm head at 203 dpi
FONT_A = (12, 24)     # cell size in dots
FONT_B = (9, 17)
MARGIN = 24           # paper edge around the printable area in the image
FONT_DIRS = ["/usr/share/fonts", "/usr/local/share/fonts", str(Path.home() / ".local/share/fonts")]


def _font_file(bold: bool) -> str:
    names = (["DejaVuSansMono-Bold.ttf", "AdwaitaMono-Bold.ttf", "LiberationMono-Bold.ttf"] if bold
             else ["DejaVuSansMono.ttf", "AdwaitaMono-Regular.ttf", "LiberationMono-Regular.ttf"])
    for d in FONT_DIRS:
        for n in names:
            hits = list(Path(d).rglob(n)) if Path(d).exists() else []
            if hits:
                return str(hits[0])
    raise SystemExit("no monospace TTF font found")


@dataclass(frozen=True)
class Style:
    font_b: bool = False
    bold: bool = False
    w: int = 1          # width multiplier
    h: int = 1          # height multiplier
    invert: bool = False


@dataclass
class Seg:
    text: str
    style: Style


def parse(data: bytes) -> list[tuple[int, list[Seg]] | str]:
    """Returns lines: (align, segments) or the marker 'CUT'."""
    lines: list[tuple[int, list[Seg]] | str] = []
    st = Style()
    align = 0
    cur: list[Seg] = []
    line_align = None
    i = 0
    text = bytearray()

    def flush_text():
        nonlocal text, line_align
        if text:
            if line_align is None:
                line_align = align
            cur.append(Seg(text.decode("cp437", "replace"), st))
            text = bytearray()

    def end_line():
        nonlocal cur, line_align
        flush_text()
        lines.append((align if line_align is None else line_align, cur))
        cur, line_align = [], None

    while i < len(data):
        b = data[i]
        if b == 0x0A:
            end_line(); i += 1
        elif b == 0x1B and i + 1 < len(data):
            flush_text()
            c = data[i + 1]
            if c == 0x40:                        # ESC @ init
                st, align = Style(), 0; i += 2
            elif c == 0x21:                      # ESC ! n  print mode
                n = data[i + 2]
                st = replace(st, font_b=bool(n & 0x01), bold=bool(n & 0x08),
                             h=2 if n & 0x10 else 1, w=2 if n & 0x20 else 1); i += 3
            elif c == 0x45:                      # ESC E n  bold
                st = replace(st, bold=bool(data[i + 2] & 1)); i += 3
            elif c == 0x61:                      # ESC a n  align
                align = data[i + 2] % 48 if data[i + 2] >= 48 else data[i + 2]
                if not cur:
                    line_align = None
                i += 3
            else:
                i += 3
        elif b == 0x1D and i + 1 < len(data):
            flush_text()
            c = data[i + 1]
            if c == 0x21:                        # GS ! n  character size
                n = data[i + 2]
                st = replace(st, w=((n >> 4) & 7) + 1, h=(n & 7) + 1); i += 3
            elif c == 0x42:                      # GS B n  reverse
                st = replace(st, invert=bool(data[i + 2] & 1)); i += 3
            elif c == 0x56:                      # GS V m [n]  cut
                if cur or text:
                    end_line()
                lines.append("CUT")
                i += 4 if data[i + 2] in (0x41, 0x42, 65, 66) else 3
            else:
                i += 3
        else:
            text.append(b); i += 1
    if cur or text:
        end_line()
    return lines


def render(data: bytes) -> Image.Image:
    fonts: dict[tuple[bool, int], ImageFont.FreeTypeFont] = {}

    def font(bold: bool, px: int) -> ImageFont.FreeTypeFont:
        key = (bold, px)
        if key not in fonts:
            fonts[key] = ImageFont.truetype(_font_file(bold), px)
        return fonts[key]

    rows: list[Image.Image] = []
    for line in parse(data):
        if line == "CUT":
            cut = Image.new("RGB", (DOTS, 30), "white")
            d = ImageDraw.Draw(cut)
            for x in range(0, DOTS, 16):
                d.line([(x, 15), (x + 8, 15)], fill=(160, 160, 160), width=2)
            d.text((DOTS - 70, 0), "✂ cut", fill=(160, 160, 160), font=font(False, 14))
            rows.append(cut)
            continue
        align, segs = line
        if not segs:
            rows.append(Image.new("RGB", (DOTS, FONT_A[1]), "white"))
            continue
        # Lay characters out like the printer: a line that runs past the paper
        # width wraps onto the next line (same alignment and sizes).
        phys: list[list[tuple[str, Style, int, int]]] = [[]]
        x = 0
        for s in segs:
            base_cw, base_ch = FONT_B if s.style.font_b else FONT_A
            cw, ch = base_cw * s.style.w, base_ch * s.style.h
            for chr_ in s.text:
                if x + cw > DOTS:
                    phys.append([])
                    x = 0
                phys[-1].append((chr_, s.style, cw, ch))
                x += cw
        for chars in phys:
            if not chars:
                continue
            width = sum(c[2] for c in chars)
            height = max(c[3] for c in chars)
            row = Image.new("RGB", (DOTS, height), "white")
            x = {0: 0, 1: max(0, (DOTS - width) // 2), 2: max(0, DOTS - width)}.get(align, 0)
            for chr_, st_, cw, ch in chars:
                base_cw, base_ch = FONT_B if st_.font_b else FONT_A
                f = font(st_.bold, int(base_ch * 0.92))
                fg, bg = ((255, 255, 255), (0, 0, 0)) if st_.invert else ((0, 0, 0), (255, 255, 255))
                g = Image.new("RGB", (base_cw, base_ch), bg)
                gd = ImageDraw.Draw(g)
                gd.text((0, -1), chr_, fill=fg, font=f)
                if st_.bold:
                    gd.text((1, -1), chr_, fill=fg, font=f)
                row.paste(g.resize((cw, ch), Image.NEAREST), (x, height - ch))
                x += cw
            rows.append(row)
    total_h = sum(r.height for r in rows) + 2 * MARGIN
    page = Image.new("RGB", (DOTS + 2 * MARGIN, total_h), "white")
    y = MARGIN
    for r in rows:
        page.paste(r, (MARGIN, y))
        y += r.height
    d = ImageDraw.Draw(page)
    d.rectangle([0, 0, page.width - 1, page.height - 1], outline=(200, 200, 200))
    return page


def main(paths: list[str]) -> None:
    pages = []
    for p in paths:
        img = render(Path(p).read_bytes())
        out = Path(p).with_suffix(".png")
        img.save(out)
        pages.append(img)
        print(out)
    if len(pages) > 1:
        gap = 30
        w = sum(i.width for i in pages) + gap * (len(pages) - 1)
        h = max(i.height for i in pages)
        combo = Image.new("RGB", (w, h), (235, 235, 235))
        x = 0
        for i in pages:
            combo.paste(i, (x, 0))
            x += i.width + gap
        out = Path(paths[0]).parent / "combined.png"
        combo.save(out)
        print(out)


if __name__ == "__main__":
    main(sys.argv[1:])
