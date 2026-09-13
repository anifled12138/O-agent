from pathlib import Path

from PIL import Image, ImageDraw


root = Path(__file__).resolve().parents[1]
target = root / "assets" / "icon.ico"
target.parent.mkdir(parents=True, exist_ok=True)

canvas = Image.new("RGBA", (256, 256), (255, 255, 255, 0))
draw = ImageDraw.Draw(canvas)
draw.rounded_rectangle((20, 20, 236, 236), radius=58, fill=(17, 17, 17, 255))
draw.ellipse((73, 73, 183, 183), outline=(255, 255, 255, 255), width=22)
canvas.save(target, format="ICO", sizes=[(16, 16), (24, 24), (32, 32), (48, 48), (64, 64), (128, 128), (256, 256)])
