#!/usr/bin/env python3
# Makes the macOS app icon master (1024 px, transparent corners) from the
# Canva picture (1264 px, the tile on black).  The black outside the tile is
# found by flood fill from the four corners and made transparent, with a
# soft edge.  The tile is then set at 824 px in the middle of a 1024 canvas,
# the size of the tile in Apple's macOS icon grid, so it sits in the Dock at
# the same size as other apps.
# Usage: make_icon.py SOURCE.png OUT.png
import sys
from collections import deque
from PIL import Image, ImageFilter

src, out = sys.argv[1], sys.argv[2]
im = Image.open(src).convert("RGB")
w, h = im.size
px = im.load()
outside = bytearray(w * h)
q = deque()
for x, y in ((0, 0), (w - 1, 0), (0, h - 1), (w - 1, h - 1)):
    q.append((x, y))
    outside[y * w + x] = 1
while q:
    x, y = q.popleft()
    for nx, ny in ((x + 1, y), (x - 1, y), (x, y + 1), (x, y - 1)):
        if 0 <= nx < w and 0 <= ny < h and not outside[ny * w + nx]:
            r, g, b = px[nx, ny]
            if r + g + b < 120:
                outside[ny * w + nx] = 1
                q.append((nx, ny))
mask = Image.frombytes("L", (w, h), bytes(0 if o else 255 for o in outside))
mask = mask.filter(ImageFilter.GaussianBlur(1.2))
rgba = im.convert("RGBA")
rgba.putalpha(mask)
bbox = mask.point(lambda v: 255 if v > 128 else 0).getbbox()
tile = rgba.crop(bbox).resize((824, 824), Image.LANCZOS)
canvas = Image.new("RGBA", (1024, 1024), (0, 0, 0, 0))
canvas.paste(tile, (100, 100), tile)
canvas.save(out)
print(out, "tile bbox in source:", bbox)
