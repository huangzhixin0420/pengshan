package pairing

import (
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// renderQRASCII 生成 QR 矩阵并渲染为终端 half-block ASCII。
func renderQRASCII(data string) (string, error) {
	qr, err := qrcode.New(data, qrcode.Medium)
	if err != nil {
		return "", err
	}
	m := qr.Bitmap()
	var b strings.Builder
	for y := 0; y < len(m); y += 2 {
		for x := 0; x < len(m[y]); x++ {
			top := m[y][x]
			bottom := y+1 < len(m) && m[y+1][x]
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}
