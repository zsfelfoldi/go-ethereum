// Copyright 2020 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package utils

import (
	"math"
)

type PieceWiseLinear interface {
	X(int) float64
	Y(int) float64
	Len() int
}

type PieceWiseCubic interface {
	PieceWiseLinear
	DY(int) float64
}

func pwPosition(p PieceWiseLinear, x float64, inverse bool) (l int, lx float64, h int, hx float64) {
	h = p.Len() - 1
	if h < 0 {
		return -1, 0, 0, 0
	}
	if inverse {
		lx, hx = p.Y(l), p.Y(h)
	} else {
		lx, hx = p.X(l), p.X(h)
	}
	if x < lx || x > hx {
		return -1, 0, 0, 0
	}
	for h > l+1 {
		m := (l + h) / 2
		var mx float64
		if inverse {
			mx = p.Y(m)
		} else {
			mx = p.X(m)
		}
		if x > mx {
			l, lx = m, mx
		} else {
			h, hx = m, mx
		}
	}
	return
}

func PwlValue(p PieceWiseLinear, x float64) float64 {
	l, lx, h, hx := pwPosition(p, x, false)
	if l == -1 {
		return math.NaN()
	}
	dx := hx - lx
	ly := p.Y(l)
	if dx < 1e-50 {
		return ly
	}
	hy := p.Y(h)
	return ly + (hy-ly)*(x-lx)/dx
}

func PwcValue(p PieceWiseCubic, x float64) float64 {
	l, lx, h, hx := pwPosition(p, x, false)
	if l == -1 {
		return math.NaN()
	}
	dx := hx - lx
	ly := p.Y(l)
	if dx < 1e-50 {
		return ly
	}
	dy := p.Y(h) - ly
	ld, hd := p.DY(l), p.DY(h)
	dd := hd - ld
	b := 3*dy/(dx*dx) - dd*4/dx
	a := dd/(3*dx*dx) - b*2/(3*dx)
	x -= lx
	return ly + ((a*x+b)*x+ld)*x
}

func PwcInverse(p PieceWiseCubic, y float64) float64 {
	l, ly, h, hy := pwPosition(p, y, true)
	if l == -1 {
		return math.NaN()
	}
	lx, hx := p.X(l), p.X(h)
	dx := hx - lx
	if dx < 1e-50 {
		return lx
	}
	dy := hy - ly
	ld, hd := p.DY(l), p.DY(h)
	dd := hd - ld
	b := 3*dy/(dx*dx) - dd*4/dx
	a := dd/(3*dx*dx) - b*2/(3*dx)
	y -= ly
	minx, maxx, miny, maxy, maxdiff := float64(0), dx, float64(0), dy, dy*1e-9
	var midx float64
	for maxy-miny > maxdiff {
		midx = minx + (maxx-minx)*(y-miny)/(maxy-miny)
		midy := ((a*midx+b)*midx + ld) * midx
		if midy > y {
			maxx, maxy = midx, midy
		} else {
			minx, miny = midx, midy
		}
		if miny > maxy {
			minx, maxx = maxx, minx
			miny, maxy = maxy, miny
		}
	}
	return lx + midx
}

type diffExpFilter struct {
	sum, diffSum int64
}

func (df *diffExpFilter) add(x uint64, y int64, scale float64) {
	df.diffSum += int64(float64(df.sum) * float64(x) / scale)
	df.sum += y
}

func (df *diffExpFilter) shift(bits int64) {
	if bits > 0 {
		if bits > 64 {
			bits = 64
		}
		df.sum <<= uint(bits)
		df.diffSum <<= uint(bits)
	}
	if bits < 0 {
		if bits < -64 {
			bits = -64
		}
		df.sum >>= uint(-bits)
		df.diffSum >>= uint(-bits)
	}
}

type singleMirrorCurve struct {
	lastX   uint64
	filters []diffExpFilter // one for each scale
}

type MirrorCurve struct {
	scales []float64 // might be shared across instances
	exp    []uint64  // one for each scale
	curves map[int]singleMirrorCurve
}

func NewMirrorCurve(scales []float64) *MirrorCurve {
	return &MirrorCurve{
		scales: scales,
		exp:    make([]uint64, len(scales)),
		curves: make(map[int]singleMirrorCurve),
	}
}

func (mc *MirrorCurve) setExp(scale int, exp uint64) {
	if shift := int64(mc.exp[scale] - exp); shift != 0 {
		for _, curve := range mc.curves {
			curve.filters[scale].shift(shift)
		}
		mc.exp[scale] = exp
	}
}

func (mc *MirrorCurve) Add(curve int, x uint64, y ...float64) {
	factors := make([]float64, len(mc.scales))
	xf := float64(x)
	for i, scale := range mc.scales {
		e := ExpFactor(LogToFixed64(xf / scale))
		mc.setExp(i, e.Exp)
		factors[i] = e.Factor
	}
	for i, y := range y {
		c, ok := mc.curves[curve+i]
		if !ok {
			c = singleMirrorCurve{
				lastX:   x,
				filters: make([]diffExpFilter, len(mc.scales)),
			}
		}
		dx := x - c.lastX
		c.lastX = x
		for j, factor := range factors {
			c.filters[j].add(dx, int64(y*factor), mc.scales[j])
		}
		mc.curves[curve+i] = c
	}
}

func (mc *MirrorCurve) Snapshot(x uint64) map[int]*MirrorCurveSnapshot {
	snap := make(map[int]*MirrorCurveSnapshot)
	xf := float64(x)
	for i, curve := range mc.curves {
		points := make([][2]float64, len(mc.scales))
		for j, scale := range mc.scales {
			e := ExpFactor(LogToFixed64(xf / scale))
			p := curve.filters[j]
			p.add(x-curve.lastX, 0, scale)
			points[j] = [2]float64{
				e.Value(float64(p.sum), mc.exp[j]),
				e.Value(float64(p.diffSum)/scale, mc.exp[j]),
			}
		}
		snap[i] = &MirrorCurveSnapshot{
			scales: mc.scales,
			points: points,
		}
	}
	return snap
}

type MirrorCurveSnapshot struct {
	scales []float64 // shared across instances
	points [][2]float64
}

func (ms *MirrorCurveSnapshot) Len() int {
	return len(ms.scales) + 1
}

func (ms *MirrorCurveSnapshot) X(i int) float64 {
	if i == 0 {
		return 0
	}
	return ms.scales[i-1]
}

func (ms *MirrorCurveSnapshot) Y(i int) float64 {
	if i == 0 {
		return 0
	}
	return ms.points[i-1][0]
}

func (ms *MirrorCurveSnapshot) DY(i int) float64 {
	if i == 0 {
		return 0
	}
	return ms.points[i-1][1]
}

func LinHyper(x float64) float64 {
	if x <= 1 {
		return x
	}
	x = 2 - x
	if x <= 0 {
		return math.Inf(1)
	}
	return 1 / x
}

func InvLinHyper(x float64) float64 {
	if x <= 1 {
		return x
	}
	return 2 - 1/x
}

func LinIntegral(x, dx float64) float64 {
	return dx * (x + (dx * 0.5))
}

func LinHyperIntegral(x, dx float64) float64 {
	var sum float64
	if x <= 1 {
		if x+dx <= 1 {
			return LinIntegral(x, dx)
		} else {
			dx1 := 1 - x
			sum = LinIntegral(x, dx1)
			dx -= dx1
			x = 1
		}
	}
	xx := 2 - x - dx
	if xx > 0 {
		sum += math.Log1p(dx / xx)
	} else {
		sum = math.Inf(1)
	}
	return sum
}

func InvLinIntegral(x, i float64) float64 {
	sq := x*x + 2*i
	if sq < 0 {
		return math.NaN()
	}
	if c := sq * 1e-10; i < c && i > -c {
		return x * i
	} else {
		return math.Sqrt(sq) - x
	}
}

func InvLinHyperIntegral(x, i float64) float64 {
	if x >= 2 {
		return 0
	}
	var dx float64
	if x < 1 {
		dx = InvLinIntegral(x, i)
		if x+dx <= 1 {
			return dx
		}
		dx = 1 - x
		i -= LinIntegral(x, dx)
		if i <= 0 {
			return dx
		}
		x = 1
	}
	r := math.Expm1(i)
	return dx + (2-x)*r/(r+1)
}
