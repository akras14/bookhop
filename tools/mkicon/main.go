// mkicon turns the generated icon artwork (a JPEG of a cream rounded square
// on a white background) into the app's icon files:
//
//	assets/icon.png            1024px, transparent corners (Windows icon source)
//	web/icon.png               64px browser tab icon
//	<iconset>/icon_*.png       macOS iconset, padded to Apple's icon grid
//
// Run via `make icons`.
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

// cream is the rounded square's background color in the artwork.
var cream = color.NRGBA{246, 240, 232, 255}

func main() {
	src := flag.String("src", "assets/icon-source.jpg", "source artwork")
	iconset := flag.String("iconset", "build/AppIcon.iconset", "macOS iconset directory to write")
	flag.Parse()

	f, err := os.Open(*src)
	check(err)
	img, _, err := image.Decode(f)
	f.Close()
	check(err)

	masked := cutCorners(img)
	savePNG("assets/icon.png", resize(masked, 1024))
	savePNG("web/icon.png", resize(masked, 64))

	// Apple's grid: the rounded square is 824/1024 of the canvas, centered.
	check(os.MkdirAll(*iconset, 0o755))
	for _, size := range []int{16, 32, 128, 256, 512} {
		for _, scale := range []int{1, 2} {
			px := size * scale
			name := fmt.Sprintf("icon_%dx%d.png", size, size)
			if scale == 2 {
				name = fmt.Sprintf("icon_%dx%d@2x.png", size, size)
			}
			savePNG(filepath.Join(*iconset, name), pad(masked, px, 824.0/1024.0))
		}
	}
}

// cutCorners makes the white area outside the rounded square transparent.
// Pixels well inside the rounded square are left alone; near the corners,
// alpha comes from how white the pixel is (cream -> opaque, white -> clear),
// which follows the artwork's own antialiased edge and soft shadow.
func cutCorners(img image.Image) *image.NRGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)

	// Anything within this radius of a corner's circle center is inside the
	// shape. The artwork's corner radius is ~18% of its width.
	r := 0.18 * float64(w)
	safe := r - 0.03*float64(w)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			cx, cy := float64(x)+0.5, float64(y)+0.5
			// Nearest corner circle center.
			ccx := math.Min(math.Max(cx, r), float64(w)-r)
			ccy := math.Min(math.Max(cy, r), float64(h)-r)
			if cx == ccx || cy == ccy { // not in a corner zone
				continue
			}
			if math.Hypot(cx-ccx, cy-ccy) <= safe {
				continue
			}
			c := out.NRGBAAt(x, y)
			// Blue separates the two best: cream ~232, white ~254.
			// The low end is raised above white so JPEG noise in the white
			// area comes out fully transparent instead of faint speckles.
			a := (248.0 - float64(c.B)) / (248.0 - float64(cream.B) - 2)
			a = math.Max(0, math.Min(1, a))
			// Recolor to cream so antialiased edges don't keep a white halo.
			out.SetNRGBA(x, y, color.NRGBA{cream.R, cream.G, cream.B, uint8(math.Round(a * 255))})
		}
	}
	return out
}

// resize scales a square image down to size x size by area averaging in
// premultiplied alpha, which is sharp and artifact-free for downscaling.
func resize(src *image.NRGBA, size int) *image.NRGBA {
	sw := src.Bounds().Dx()
	scale := float64(sw) / float64(size)
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		y0, y1 := float64(y)*scale, float64(y+1)*scale
		for x := 0; x < size; x++ {
			x0, x1 := float64(x)*scale, float64(x+1)*scale
			var r, g, bl, a, wsum float64
			for sy := int(y0); sy < int(math.Ceil(y1)); sy++ {
				wy := math.Min(y1, float64(sy+1)) - math.Max(y0, float64(sy))
				for sx := int(x0); sx < int(math.Ceil(x1)); sx++ {
					wx := math.Min(x1, float64(sx+1)) - math.Max(x0, float64(sx))
					c := src.NRGBAAt(sx, sy)
					wt := wx * wy
					ca := float64(c.A) / 255
					r += float64(c.R) * ca * wt
					g += float64(c.G) * ca * wt
					bl += float64(c.B) * ca * wt
					a += ca * wt
					wsum += wt
				}
			}
			if a == 0 {
				continue
			}
			dst.SetNRGBA(x, y, color.NRGBA{
				uint8(math.Round(r / a)), uint8(math.Round(g / a)), uint8(math.Round(bl / a)),
				uint8(math.Round(a / wsum * 255)),
			})
		}
	}
	return dst
}

// pad scales the artwork to fraction of a size x size transparent canvas.
func pad(src *image.NRGBA, size int, fraction float64) *image.NRGBA {
	inner := int(math.Round(float64(size) * fraction))
	small := resize(src, inner)
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	off := (size - inner) / 2
	draw.Draw(dst, image.Rect(off, off, off+inner, off+inner), small, image.Point{}, draw.Src)
	return dst
}

func savePNG(path string, img image.Image) {
	check(os.MkdirAll(filepath.Dir(path), 0o755))
	f, err := os.Create(path)
	check(err)
	check(png.Encode(f, img))
	check(f.Close())
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkicon:", err)
		os.Exit(1)
	}
}
