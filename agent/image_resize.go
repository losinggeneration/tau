package agent

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"

	"github.com/losinggeneration/tau/ai"
)

// resizeResult is the outcome of attempting to resize image data. The
// caller distinguishes "I did work" from "no work needed" by checking
// whether Data is nil — nil means no resize was applied (either because
// the format is unsupported, decode failed, or the image already fits
// within the configured max dimensions).
type resizeResult struct {
	// Data is the resized, re-encoded image bytes. Nil if no resize was
	// performed (unsupported format, decode failure, or already-small
	// image). When non-nil, the mime type of Data matches the input
	// mimeType parameter to resizeImageData.
	Data []byte

	// OrigW/OrigH are the original image dimensions in pixels. Always
	// populated when the format is decodable, even when Data is nil.
	OrigW, OrigH int

	// NewW/NewH are the post-resize dimensions. Equal to OrigW/OrigH
	// when no resize was performed.
	NewW, NewH int

	// Note is a human-readable description of what happened. Empty when
	// no resize was performed; populated when an actual resize changed
	// the image dimensions.
	Note string
}

// supportedImageMimeTypes is the set of mime types that resizeImageData
// can decode and re-encode. Other formats (e.g. WebP) are returned as
// nil Data with OrigW/OrigH unset so the caller falls back to the
// original-image path.
func isSupportedImageMime(mimeType string) bool {
	switch strings.ToLower(mimeType) {
	case "image/png", "image/jpeg", "image/gif":
		return true
	default:
		return false
	}
}

// decodeImagePixels decodes the full image at mimeType.
func decodeImagePixels(data []byte, mimeType string) (image.Image, error) {
	switch strings.ToLower(mimeType) {
	case "image/png":
		return png.Decode(bytes.NewReader(data))
	case "image/jpeg":
		return jpeg.Decode(bytes.NewReader(data))
	case "image/gif":
		return gif.Decode(bytes.NewReader(data))
	default:
		return nil, fmt.Errorf("unsupported image mime type: %s", mimeType)
	}
}

// encodeImage re-encodes the image at mimeType with sensible defaults.
// JPEG uses quality 85 (standard for moderately-compressed images).
// Other formats use their stdlib defaults.
func encodeImage(img image.Image, mimeType string, quality ...int) ([]byte, error) {
	jpegQuality := 85
	if len(quality) > 0 && quality[0] > 0 {
		jpegQuality = quality[0]
	}
	var buf bytes.Buffer
	switch strings.ToLower(mimeType) {
	case "image/png":
		if err := png.Encode(&buf, img); err != nil {
			return nil, err
		}
	case "image/jpeg":
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
			return nil, err
		}
	case "image/gif":
		if err := gif.Encode(&buf, img, nil); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported image mime type: %s", mimeType)
	}
	return buf.Bytes(), nil
}

// computeFitDimensions returns the (newW, newH) that fit the source
// dimensions within (maxW, maxH) while preserving aspect ratio. If the
// source already fits, returns the source dimensions unchanged.
func computeFitDimensions(srcW, srcH, maxW, maxH int) (int, int) {
	if srcW <= maxW && srcH <= maxH {
		return srcW, srcH
	}
	// Scale to the more constraining axis.
	wRatio := float64(maxW) / float64(srcW)
	hRatio := float64(maxH) / float64(srcH)
	scale := wRatio
	if hRatio < wRatio {
		scale = hRatio
	}
	newW := int(float64(srcW) * scale)
	newH := int(float64(srcH) * scale)
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}
	return newW, newH
}

// resizeImageData attempts to resize the supplied image data to fit within
// the given max dimensions while preserving aspect ratio. The output
// mime type matches the input mime type (PNG stays PNG, etc.).
//
// ResizeResult.Data is nil when:
//   - the format is unsupported (WebP, etc.)
//   - the data cannot be decoded
//   - the image already fits within the max dimensions (no work to do)
//
// ResizeResult.OrigW/OrigH are populated whenever the format is decodable,
// even when no resize is performed, so the caller can include the actual
// image dimensions in its note to the model.
func resizeImageData(data []byte, mimeType string, maxW, maxH int, quality ...int) resizeResult {
	if !isSupportedImageMime(mimeType) {
		return resizeResult{}
	}
	if maxW <= 0 || maxH <= 0 {
		// Defensive: zero/negative max dimensions would cause issues.
		return resizeResult{}
	}

	// Phase 1: read the header to learn original dimensions without
	// paying the cost of full pixel decode.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return resizeResult{}
	}
	origW, origH := cfg.Width, cfg.Height
	if origW == 0 || origH == 0 {
		return resizeResult{}
	}

	// Already small enough? No resize needed.
	if origW <= maxW && origH <= maxH {
		return resizeResult{
			Data:  nil,
			OrigW: origW, OrigH: origH,
			NewW: origW, NewH: origH,
		}
	}

	// Phase 2: full pixel decode and resize.
	srcImg, err := decodeImagePixels(data, mimeType)
	if err != nil {
		return resizeResult{
			OrigW: origW, OrigH: origH,
			NewW: origW, NewH: origH,
		}
	}

	newW, newH := computeFitDimensions(origW, origH, maxW, maxH)
	resized := resizeNRGBA(toNRGBA(srcImg), newW, newH)

	encoded, err := encodeImage(resized, mimeType, quality...)
	if err != nil {
		return resizeResult{
			OrigW: origW, OrigH: origH,
			NewW: origW, NewH: origH,
		}
	}

	return resizeResult{
		Data:  encoded,
		OrigW: origW, OrigH: origH,
		NewW: newW, NewH: newH,
		Note: fmt.Sprintf("Image resized from %dx%d to %dx%d", origW, origH, newW, newH),
	}
}

// toNRGBA converts an arbitrary image.Image to *image.NRGBA. NRGBA is
// the fastest format for the resize path because every pixel is
// 4 contiguous bytes in the same memory layout.
func toNRGBA(src image.Image) *image.NRGBA {
	if n, ok := src.(*image.NRGBA); ok {
		return n
	}
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

// resizeNRGBA performs a bilinear resize of src to the given dimensions.
// The src image is expected to be in NRGBA format (see toNRGBA). The
// output is a new NRGBA image with the requested dimensions.
//
// Bilinear: for each destination pixel (x, y), sample the source at
// (sx, sy) = ((x+0.5)*srcW/dstW - 0.5, (y+0.5)*srcH/dstH - 0.5) and
// blend the 4 nearest source pixels weighted by sub-pixel distance.
func resizeNRGBA(src *image.NRGBA, dstW, dstH int) *image.NRGBA {
	srcBounds := src.Bounds()
	srcW := srcBounds.Dx()
	srcH := srcBounds.Dy()
	if srcW == 0 || srcH == 0 || dstW == 0 || dstH == 0 {
		return image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	}

	dst := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	// Precompute source X lookup for each destination column.
	// sx(x) = (x+0.5)*srcW/dstW - 0.5, clamped to [0, srcW-1].
	xLookup := make([]float64, dstW)
	for x := 0; x < dstW; x++ {
		sx := (float64(x)+0.5)*float64(srcW)/float64(dstW) - 0.5
		if sx < 0 {
			sx = 0
		}
		if sx > float64(srcW-1) {
			sx = float64(srcW - 1)
		}
		xLookup[x] = sx
	}
	// Y lookup is the same formula on height.
	yLookup := make([]float64, dstH)
	for y := 0; y < dstH; y++ {
		sy := (float64(y)+0.5)*float64(srcH)/float64(dstH) - 0.5
		if sy < 0 {
			sy = 0
		}
		if sy > float64(srcH-1) {
			sy = float64(srcH - 1)
		}
		yLookup[y] = sy
	}

	for dy := 0; dy < dstH; dy++ {
		sy := yLookup[dy]
		y0 := int(sy)
		y1 := y0 + 1
		if y1 >= srcH {
			y1 = srcH - 1
		}
		wy := sy - float64(y0)

		srcRow0 := src.PixOffset(srcBounds.Min.X, srcBounds.Min.Y+y0)
		srcRow1 := src.PixOffset(srcBounds.Min.X, srcBounds.Min.Y+y1)
		dstRow := dst.PixOffset(0, dy)

		for dx := 0; dx < dstW; dx++ {
			sx := xLookup[dx]
			x0 := int(sx)
			x1 := x0 + 1
			if x1 >= srcW {
				x1 = srcW - 1
			}
			wx := sx - float64(x0)

			// Bilinear blend: top-row (y0,y0+1) and bottom-row (y0,y0+1)
			// at (x0, x0+1) blended in x then y.
			p00 := srcRow0 + x0*4
			p01 := srcRow0 + x1*4
			p10 := srcRow1 + x0*4
			p11 := srcRow1 + x1*4

			for c := 0; c < 4; c++ {
				top := float64(src.Pix[p00+c])*(1-wx) + float64(src.Pix[p01+c])*wx
				bot := float64(src.Pix[p10+c])*(1-wx) + float64(src.Pix[p11+c])*wx
				v := top*(1-wy) + bot*wy
				if v < 0 {
					v = 0
				}
				if v > 255 {
					v = 255
				}
				dst.Pix[dstRow+dx*4+c] = uint8(v)
			}
		}
	}
	return dst
}

// ResizeImageBlock applies a model profile before an image enters persistent history.
func ResizeImageBlock(block ai.ContentBlock, profile *ai.ModelImageResizeOptions) (ai.ContentBlock, error) {
	if block.Type != ai.ContentBlockImage || profile == nil {
		return block, nil
	}
	if profile.JPEGQuality < 0 || profile.JPEGQuality > 100 {
		return block, fmt.Errorf("invalid JPEG quality %d", profile.JPEGQuality)
	}
	width, height := profile.MaxWidth, profile.MaxHeight
	if width <= 0 {
		width = 2000
	}
	if height <= 0 {
		height = 2000
	}
	data, err := base64.StdEncoding.DecodeString(block.Data)
	if err != nil {
		return block, fmt.Errorf("decode image: %w", err)
	}
	resized := resizeImageData(data, block.MimeType, width, height, profile.JPEGQuality)
	if resized.Data != nil {
		block.Data = base64.StdEncoding.EncodeToString(resized.Data)
	}
	if profile.MaxBytes > 0 && len(block.Data) > profile.MaxBytes {
		return block, fmt.Errorf("image exceeds model payload limit of %d bytes", profile.MaxBytes)
	}
	return block, nil
}
