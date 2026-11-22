package capture

import (
	"bytes"
	"image"
	"image/jpeg"
)

// EncodeJPEG 把图像编码成 JPEG（质量 1～100）。视觉通道用它压体积。
func EncodeJPEG(img image.Image, quality int) ([]byte, error) {
	if quality < 1 {
		quality = 85
	}
	if quality > 100 {
		quality = 100
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
