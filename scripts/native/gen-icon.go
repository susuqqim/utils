package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

type RGBA struct {
	R, G, B, A float64
}

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func distToSegment(px, py, x0, y0, x1, y1 float64) float64 {
	dx, dy := x1-x0, y1-y0
	l2 := dx*dx + dy*dy
	if l2 == 0 {
		return math.Hypot(px-x0, py-y0)
	}
	t := clamp(((px-x0)*dx+(py-y0)*dy)/l2, 0, 1)
	return math.Hypot(px-(x0+t*dx), py-(y0+t*dy))
}

func distToRoundedBox(px, py, w, h, r float64) float64 {
	dx := math.Max(math.Abs(px)-(w-r), 0)
	dy := math.Max(math.Abs(py)-(h-r), 0)
	return math.Hypot(dx, dy) - r
}

func pointInTriangle(px, py, x0, y0, x1, y1, x2, y2 float64) bool {
	d1 := (px-x1)*(y0-y1) - (x0-x1)*(py-y1)
	d2 := (px-x2)*(y1-y2) - (x1-x2)*(py-y2)
	d3 := (px-x0)*(y2-y0) - (x2-x0)*(py-y0)
	hasNeg := (d1 < 0) || (d2 < 0) || (d3 < 0)
	hasPos := (d1 > 0) || (d2 > 0) || (d3 > 0)
	return !(hasNeg && hasPos)
}

func distToTriangleEdges(px, py, x0, y0, x1, y1, x2, y2 float64) float64 {
	d0 := distToSegment(px, py, x0, y0, x1, y1)
	d1 := distToSegment(px, py, x1, y1, x2, y2)
	d2 := distToSegment(px, py, x2, y2, x0, y0)
	return math.Min(d0, math.Min(d1, d2))
}

// samplePixel 返回单个归一化坐标点 (px, py) ∈ [-1, 1] 的颜色
func samplePixel(px, py float64) RGBA {
	// 1. 底座 Badge: rounded box with r=0.38, w=0.88, h=0.88
	dBadge := distToRoundedBox(px, py, 0.88, 0.88, 0.38)
	if dBadge > 0.02 {
		return RGBA{0, 0, 0, 0}
	}

	// 渐变底色: 顶部午夜蓝紫 -> 底部深深空灰蓝
	tY := (py + 1.0) / 2.0 // 0=top, 1=bottom
	tX := (px + 1.0) / 2.0
	// Base background: #1E1B4B (30, 27, 75) -> #0F172A (15, 23, 42)
	bgR := 30.0*(1.0-tY) + 15.0*tY
	bgG := 27.0*(1.0-tY) + 23.0*tY
	bgB := 75.0*(1.0-tY) + 42.0*tY

	// Outer border (dBadge between -0.04 and 0.0)
	if dBadge >= -0.04 {
		// Border gradient from rose-violet (#F43F5E) to cyan (#38BDF8)
		bGradR := 244.0*(1.0-tX) + 56.0*tX
		bGradG := 63.0*(1.0-tX) + 189.0*tX
		bGradB := 94.0*(1.0-tX) + 248.0*tX
		return RGBA{bGradR / 255.0, bGradG / 255.0, bGradB / 255.0, 1.0}
	}

	cur := RGBA{bgR / 255.0, bgG / 255.0, bgB / 255.0, 1.0}

	// 2. 猫耳 (Robot Kitty Ears)
	// 左耳: (-0.60, 0.15) 到 (-0.46, -0.68) 到 (-0.16, -0.32)
	// 右耳: (0.60, 0.15) 到 (0.46, -0.68) 到 (0.16, -0.32)
	inLeftEar := pointInTriangle(px, py, -0.60, 0.15, -0.46, -0.68, -0.16, -0.32)
	inRightEar := pointInTriangle(px, py, 0.60, 0.15, 0.46, -0.68, 0.16, -0.32)

	// 内耳发光面:
	inLeftInner := pointInTriangle(px, py, -0.52, 0.08, -0.44, -0.58, -0.22, -0.30)
	inRightInner := pointInTriangle(px, py, 0.52, 0.08, 0.44, -0.58, 0.22, -0.30)

	if inLeftEar || inRightEar {
		// 外耳色: 科技深靛蓝 #312E81 (49, 46, 129)
		cur = RGBA{49.0 / 255.0, 46.0 / 255.0, 129.0 / 255.0, 1.0}
		// 耳沿边缘高光
		dEdge := math.Min(
			distToTriangleEdges(px, py, -0.60, 0.15, -0.46, -0.68, -0.16, -0.32),
			distToTriangleEdges(px, py, 0.60, 0.15, 0.46, -0.68, 0.16, -0.32),
		)
		if dEdge < 0.025 {
			cur = RGBA{129.0 / 255.0, 140.0 / 255.0, 248.0 / 255.0, 1.0}
		}
		if inLeftInner || inRightInner {
			// 内耳渐变霓虹粉 #FB7185 (251, 113, 133)
			cur = RGBA{251.0 / 255.0, 113.0 / 255.0, 133.0 / 255.0, 1.0}
		}
	}

	// 3. 头部面罩基底: rounded box at (0, 0.16), w=0.62, h=0.48, r=0.22
	dHead := distToRoundedBox(px, py-0.16, 0.62, 0.48, 0.22)
	if dHead <= 0 {
		cur = RGBA{30.0 / 255.0, 27.0 / 255.0, 75.0 / 255.0, 1.0}
		if dHead >= -0.03 {
			// 头部边缘描边 #6366F1
			cur = RGBA{99.0 / 255.0, 102.0 / 255.0, 241.0 / 255.0, 1.0}
		}
	}

	// 4. 赛博护目镜 (Cyber Visor): (0, 0.04), w=0.52, h=0.18, r=0.08
	dVisor := distToRoundedBox(px, py-0.04, 0.52, 0.18, 0.08)
	if dVisor <= 0 {
		// 内部黑晶底色 #090D16
		cur = RGBA{9.0 / 255.0, 13.0 / 255.0, 22.0 / 255.0, 1.0}
		// 护目镜边缘霓虹描边: 渐变 金黄(#F59E0B) -> 翠绿(#10B981) -> 青蓝(#06B6D4)
		if dVisor >= -0.03 {
			vx := (px + 0.52) / 1.04 // 0 to 1
			if vx < 0.5 {
				t := vx * 2.0
				cur = RGBA{(245*(1-t) + 16*t) / 255.0, (158*(1-t) + 185*t) / 255.0, (11*(1-t) + 129*t) / 255.0, 1.0}
			} else {
				t := (vx - 0.5) * 2.0
				cur = RGBA{(16*(1-t) + 6*t) / 255.0, (185*(1-t) + 182*t) / 255.0, (129*(1-t) + 212*t) / 255.0, 1.0}
			}
		}

		// 护目镜内部元素:
		// A. 终端提示符 `>`: (-0.36, -0.06) -> (-0.24, 0.03) -> (-0.36, 0.12)
		dPrompt1 := distToSegment(px, py, -0.36, -0.06, -0.24, 0.03)
		dPrompt2 := distToSegment(px, py, -0.24, 0.03, -0.36, 0.12)
		dPrompt := math.Min(dPrompt1, dPrompt2)
		// 光标下划线 `_`: (-0.18, 0.12) -> (-0.08, 0.12)
		dCursor := distToSegment(px, py, -0.18, 0.12, -0.08, 0.12)
		if dPrompt <= 0.022 || dCursor <= 0.022 {
			// 亮绿色终端 #10B981
			cur = RGBA{16.0 / 255.0, 185.0 / 255.0, 129.0 / 255.0, 1.0}
		}

		// B. 右侧脉冲心跳线 (Pulse Line):
		// (0.08, 0.03) -> (0.16, 0.03) -> (0.22, -0.07) -> (0.30, 0.13) -> (0.36, 0.03) -> (0.44, 0.03)
		p0 := distToSegment(px, py, 0.08, 0.03, 0.16, 0.03)
		p1 := distToSegment(px, py, 0.16, 0.03, 0.22, -0.07)
		p2 := distToSegment(px, py, 0.22, -0.07, 0.30, 0.13)
		p3 := distToSegment(px, py, 0.30, 0.13, 0.36, 0.03)
		p4 := distToSegment(px, py, 0.36, 0.03, 0.44, 0.03)
		dPulse := math.Min(p0, math.Min(p1, math.Min(p2, math.Min(p3, p4))))
		if dPulse <= 0.022 {
			// 青蓝色电波 #38BDF8
			cur = RGBA{56.0 / 255.0, 189.0 / 255.0, 248.0 / 255.0, 1.0}
		}
	}

	// 5. 嘴部 W 弧线 (W-Mouth):
	// 左半弧: (-0.14, 0.34) -> (-0.07, 0.42) -> (0.0, 0.36)
	// 右半弧: (0.0, 0.36) -> (0.07, 0.42) -> (0.14, 0.34)
	w0 := distToSegment(px, py, -0.14, 0.34, -0.07, 0.42)
	w1 := distToSegment(px, py, -0.07, 0.42, 0.0, 0.36)
	w2 := distToSegment(px, py, 0.0, 0.36, 0.07, 0.42)
	w3 := distToSegment(px, py, 0.07, 0.42, 0.14, 0.34)
	dWMouth := math.Min(w0, math.Min(w1, math.Min(w2, w3)))
	if dWMouth <= 0.022 {
		cur = RGBA{241.0 / 255.0, 245.0 / 255.0, 249.0 / 255.0, 1.0}
	}

	// 6. 脸颊科技腮红点: (-0.42, 0.38) and (0.42, 0.38), r=0.035
	dCheekL := math.Hypot(px+0.42, py-0.38)
	dCheekR := math.Hypot(px-0.42, py-0.38)
	if dCheekL <= 0.035 || dCheekR <= 0.035 {
		cur = RGBA{251.0 / 255.0, 113.0 / 255.0, 133.0 / 255.0, 0.9}
	}

	return cur
}

func renderSize(size int) []byte {
	ss := 4 // 4x4 supersampling
	out := make([]byte, 0, size*size*4)
	for y := size - 1; y >= 0; y-- { // Windows DIB 自下而上
		for x := 0; x < size; x++ {
			var sumR, sumG, sumB, sumA float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					fx := (float64(x) + (float64(sx)+0.5)/float64(ss)) / float64(size)
					fy := (float64(y) + (float64(sy)+0.5)/float64(ss)) / float64(size)
					px := fx*2.0 - 1.0
					py := fy*2.0 - 1.0
					c := samplePixel(px, py)
					sumR += c.R * c.A
					sumG += c.G * c.A
					sumB += c.B * c.A
					sumA += c.A
				}
			}
			tot := float64(ss * ss)
			avgA := sumA / tot
			if avgA <= 0.001 {
				out = append(out, 0, 0, 0, 0)
			} else {
				// 预乘 Alpha (Premultiplied BGRA)
				r := (sumR / tot) * 255.0
				g := (sumG / tot) * 255.0
				b := (sumB / tot) * 255.0
				a := avgA * 255.0
				out = append(out, byte(clamp(b, 0, 255)), byte(clamp(g, 0, 255)), byte(clamp(r, 0, 255)), byte(clamp(a, 0, 255)))
			}
		}
	}
	return out
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: go run test_icon_render.go <out.ico>")
		os.Exit(1)
	}
	sizes := []int{16, 24, 32, 48, 64, 128, 256}

	type imgEntry struct {
		size int
		data []byte
	}
	imgs := make([]imgEntry, 0, len(sizes))
	for _, s := range sizes {
		fmt.Printf("Rendering %dx%d...\n", s, s)
		imgs = append(imgs, imgEntry{s, renderSize(s)})
	}

	var buf bytes.Buffer
	// ICONDIR
	binary.Write(&buf, binary.LittleEndian, uint16(0))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(len(imgs)))

	offset := 6 + 16*len(imgs)
	for _, im := range imgs {
		w := byte(im.size)
		if im.size >= 256 {
			w = 0
		}
		buf.WriteByte(w)
		buf.WriteByte(w)
		buf.WriteByte(0) // colorCount
		buf.WriteByte(0) // reserved
		binary.Write(&buf, binary.LittleEndian, uint16(1))  // planes
		binary.Write(&buf, binary.LittleEndian, uint16(32)) // bitCount
		andMask := ((im.size + 31) / 32) * 4 * im.size
		bytesInRes := uint32(40 + len(im.data) + andMask)
		binary.Write(&buf, binary.LittleEndian, bytesInRes)
		binary.Write(&buf, binary.LittleEndian, uint32(offset))
		offset += int(bytesInRes)
	}

	for _, im := range imgs {
		// BITMAPINFOHEADER
		binary.Write(&buf, binary.LittleEndian, uint32(40))
		binary.Write(&buf, binary.LittleEndian, int32(im.size))
		binary.Write(&buf, binary.LittleEndian, int32(im.size*2)) // 含 AND mask
		binary.Write(&buf, binary.LittleEndian, uint16(1))
		binary.Write(&buf, binary.LittleEndian, uint16(32))
		binary.Write(&buf, binary.LittleEndian, uint32(0)) // BI_RGB
		binary.Write(&buf, binary.LittleEndian, uint32(len(im.data)))
		binary.Write(&buf, binary.LittleEndian, int32(0))
		binary.Write(&buf, binary.LittleEndian, int32(0))
		binary.Write(&buf, binary.LittleEndian, uint32(0))
		binary.Write(&buf, binary.LittleEndian, uint32(0))
		buf.Write(im.data)
		// AND mask (全 0)
		andMask := ((im.size + 31) / 32) * 4 * im.size
		buf.Write(make([]byte, andMask))
	}

	if err := os.WriteFile(os.Args[1], buf.Bytes(), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("Wrote %s: %d bytes, %d sizes\n", os.Args[1], buf.Len(), len(imgs))
}
