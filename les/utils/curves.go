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
	"math/big"
	"math/bits"
)

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

var LinHyperCurve = func() *Curve128 {
	var c Curve128
	var last uint128
	for i := 0; i < 4095; i++ { //TODO last value
		v := LinHyperIntegral(float64(i)/2048, float64(1)/2048)
		shift := 63 - math.Floor(math.Log2(v)+0.01)
		v *= math.Pow(2, shift)
		u := uint64(v)
		shl := uint(124 - shift)
		last = add128(last, uint128{u << shl, u >> (64 - shl)})
		c[i+1] = last
	}
	return &c
}()

type uint128 [2]uint64

func add128(a, b uint128) uint128 {
	var r uint128
	var c uint64
	r[0], c = bits.Add64(a[0], b[0], 0)
	r[1], _ = bits.Add64(a[1], b[1], c)
	return r
}

func sub128(a, b uint128) uint128 {
	var r uint128
	var c uint64
	r[0], c = bits.Sub64(a[0], b[0], 0)
	r[1], _ = bits.Sub64(a[1], b[1], c)
	return r
}

func mul128high(a, b uint128) uint128 {
	var res [4]uint64
	for i := 0; i < 2; i++ {
		for j := 0; j < 2; j++ {
			h, l := bits.Mul64(a[i], b[j])
			p := i + j
			var c uint64
			res[p], c = bits.Add64(res[p], l, 0)
			p++
			res[p], c = bits.Add64(res[p], h, c)
			for c > 0 && p < 3 {
				p++
				res[p], c = bits.Add64(res[p], 0, c)
			}
		}
	}
	return uint128{res[2], res[3]}
}

type Curve128 [4097]uint128

func (c *Curve128) Value(a, b uint64) uint128 {
	h, r := bits.Div64(a, 0, b)
	l, _ := bits.Div64(r, 0, b)
	pos := int(h >> 52)
	y0 := (*c)[pos]
	y1 := (*c)[pos+1]
	var dy uint128
	dy = sub128(y1, y0)
	x := uint128{l << 12, (h << 12) + (l >> 52)}
	return add128(y0, mul128high(x, dy))

}

type BondingCurve struct { //TODO ??? move to lps package?
	curve                                   *Curve128
	tokenAmount, tokenLimit, currencyAmount int64   // tokenLimit > 0
	shiftAmount                             uint    // right shift currency amount for internal representation
	logBasePrice                            Fixed64 //TODO use longer fractional part?
}

func NewBondingCurve(curve *Curve128, tokenLimit int64, logBasePrice Fixed64) BondingCurve {
	if tokenLimit < 1 {
		tokenLimit = 1
	}
	b := BondingCurve{
		curve:        curve,
		tokenLimit:   tokenLimit,
		logBasePrice: logBasePrice,
	}
	b.update()
	b.Adjust(0, tokenLimit, logBasePrice) // sets shiftAmount
	return b
}

func (bc *BondingCurve) calculate() int64 {
	if bc.tokenAmount >= bc.tokenLimit {
		return math.MaxInt64
	}
	v := bc.curve.Value(uint64(bc.tokenAmount), uint64(bc.tokenLimit))

	var shlLimit uint
	lbp := uint(bc.logBasePrice.ToUint64())
	if lbp > bc.shiftAmount+62 {
		shlLimit = lbp - bc.shiftAmount - 62
	}
	if shlLimit > uint(bits.LeadingZeros64(uint64(bc.tokenLimit))) {
		return math.MaxInt64
	}
	bp := uint64((bc.logBasePrice - Uint64ToFixed64(uint64(bc.shiftAmount+shlLimit))).Pow2())
	hi, lo := bits.Mul64(uint64(bc.tokenLimit)<<shlLimit, bp)
	m := mul128high(v, uint128{lo, hi})
	if m[1] >= (uint64(1)<<59)-1 {
		return math.MaxInt64
	}
	return int64((m[1] << 4) + (m[0] >> 60))
}

func (bc *BondingCurve) update() {
	bc.currencyAmount = bc.calculate()
	if bc.currencyAmount == math.MaxInt64 || bc.currencyAmount < 0 {
		panic(nil)
	}
}

func (bc *BondingCurve) Price(tokenAmount int64) *big.Int {
	oldTokenAmount := bc.tokenAmount
	bc.tokenAmount += tokenAmount
	cost := bc.calculate()
	if cost == math.MaxInt64 {
		return nil
	}
	c := big.NewInt(cost - bc.currencyAmount)
	c.Lsh(c, bc.shiftAmount)
	bc.tokenAmount = oldTokenAmount
	return c
}

// call Adjust before Exchange
// maxCost == nil means no upper limit
func (bc *BondingCurve) Exchange(minAmount, maxAmount int64, maxCost *big.Int) (int64, *big.Int) {
	var mcost int64
	if maxCost == nil {
		mcost = math.MaxInt64
	} else {
		var mc big.Int
		mc.Rsh(maxCost, bc.shiftAmount)
		if mc.IsInt64() {
			mcost = mc.Int64()
		} else {
			if mc.Sign() == 1 {
				mcost = math.MaxInt64
			} else {
				mcost = math.MinInt64
			}
		}
	}
	if mcost >= math.MaxInt64-bc.currencyAmount {
		mcost = math.MaxInt64 - bc.currencyAmount - 1
	}
	oldTokenAmount := bc.tokenAmount
	if minAmount < -bc.tokenAmount {
		minAmount = -bc.tokenAmount
	}
	if maxAmount >= bc.tokenLimit {
		maxAmount = bc.tokenLimit - 1
	}
	if minAmount > maxAmount {
		return 0, nil
	}
	if maxAmount < math.MaxInt64-bc.tokenAmount {
		bc.tokenAmount += maxAmount
		if c := bc.calculate(); c-bc.currencyAmount <= mcost {
			cost := big.NewInt(c - bc.currencyAmount)
			cost.Lsh(cost, bc.shiftAmount)
			bc.currencyAmount = c
			return maxAmount, cost
		}
	}
	newCurrencyAmount := bc.currencyAmount + mcost
	bc.tokenAmount = reverseFunction(oldTokenAmount+minAmount, oldTokenAmount+maxAmount, false, func(i int64) int64 {
		bc.tokenAmount = i
		if c := bc.calculate(); c != math.MaxInt64 {
			return c - newCurrencyAmount
		} else {
			return math.MaxInt64
		}
	})
	newCurrencyAmount = bc.calculate()
	if cost := newCurrencyAmount - bc.currencyAmount; cost <= mcost {
		bc.currencyAmount = newCurrencyAmount
		c := big.NewInt(cost)
		c.Lsh(c, bc.shiftAmount)
		return bc.tokenAmount - oldTokenAmount, c
	}
	bc.tokenAmount = oldTokenAmount
	return 0, nil
}

func (bc *BondingCurve) Adjust(tokenAmount, targetLimit int64, targetLogBasePrice Fixed64) {
	if targetLimit < 1 {
		targetLimit = 1
	}
	if tokenAmount < bc.tokenAmount {
		// token amount can only be increased by purchase
		tokenAmount = bc.tokenAmount
	}

	// ensure that basePrice*tokenLimit <= 2^124 by adjusting shiftAmount
	bp := bc.logBasePrice
	if targetLogBasePrice > bp {
		bp = targetLogBasePrice
	}
	tl := bc.tokenLimit
	if targetLimit > tl {
		tl = targetLimit
	}
	var minShift uint
	logMax := uint(bp.ToUint64()) + 65 - uint(bits.LeadingZeros64(uint64(tl)))
	if logMax > 124 {
		minShift = logMax - 124
		if minShift > bc.shiftAmount {
			bc.shiftAmount = minShift
			bc.update()
		}
	}
	if minShift+1 < bc.shiftAmount {
		bc.shiftAmount = minShift + 1
		bc.update()
	}

	oldTokenLimit := bc.tokenLimit
	oldLogBasePrice := bc.logBasePrice
	bc.tokenLimit = targetLimit
	bc.logBasePrice = targetLogBasePrice
	newCurrencyAmount := bc.calculate()
	if newCurrencyAmount <= bc.currencyAmount {
		// currency amount did not increase, all adjustments are fully performed
		bc.currencyAmount = newCurrencyAmount
		return
	}
	if targetLimit >= oldTokenLimit {
		// currency amount exceeded old value only because of base price increase; revert and do a partial increase
		bc.logBasePrice = oldLogBasePrice
		bc.partialBasePriceIncrease(targetLogBasePrice)
		bc.update()
		return
	}
	if targetLogBasePrice <= oldLogBasePrice {
		// currency amount exceeded old value only because of limit decrease; revert and do a partial decrease
		bc.tokenLimit = oldTokenLimit
		bc.partialLimitDecrease(targetLimit)
		bc.update()
		return
	}
	// revert base price increase and try only decreasing limit first
	bc.logBasePrice = oldLogBasePrice
	if bc.calculate() > bc.currencyAmount {
		// limit decrease alone is too much; revert it, do a partial decrease and return
		bc.tokenLimit = oldTokenLimit
		bc.partialLimitDecrease(targetLimit)
		bc.update()
		return
	}
	// limit adjustment was fully performed; do a partial base price increase and return
	bc.partialBasePriceIncrease(targetLogBasePrice)
	bc.update()
}

func (bc *BondingCurve) TokenAmount() int64 {
	return bc.tokenAmount
}

func (bc *BondingCurve) TokenLimit() int64 {
	return bc.tokenLimit
}

func (bc *BondingCurve) LogBasePrice() Fixed64 {
	return bc.logBasePrice
}

func (bc *BondingCurve) partialLimitDecrease(targetLimit int64) {
	bc.tokenLimit = reverseFunction(targetLimit, bc.tokenLimit-1, true, func(i int64) int64 {
		bc.tokenLimit = i
		if c := bc.calculate(); c != math.MaxInt64 {
			return bc.currencyAmount - c
		} else {
			return math.MinInt64
		}
	})
}

func (bc *BondingCurve) partialBasePriceIncrease(targetLogBasePrice Fixed64) {
	bc.logBasePrice = Fixed64(reverseFunction(int64(bc.logBasePrice), int64(targetLogBasePrice), false, func(i int64) int64 {
		bc.logBasePrice = Fixed64(i)
		if c := bc.calculate(); c != math.MaxInt64 {
			return c - bc.currencyAmount
		} else {
			return math.MaxInt64
		}
	}))
}

// errFn should increase monotonically
func reverseFunction(min, max int64, upper bool, errFn func(int64) int64) int64 {
	minErr := errFn(min)
	maxErr := errFn(max)
	if minErr >= 0 {
		return min
	}
	if maxErr <= 0 {
		return max
	}
	for min < max-1 {
		var d float64
		if minErr == math.MinInt64 || maxErr == math.MaxInt64 {
			d = 0.5
		} else {
			d = float64(minErr) / (float64(minErr) - float64(maxErr)) // minErr < 0, maxErr > 0
			if d < 0.01 {
				d = 0.01
			}
			if d > 0.99 {
				d = 0.99
			}
		}
		mid := min + int64(float64(max-min)*d)
		midErr := errFn(mid)
		if midErr == 0 {
			return mid
		}
		if midErr < 0 {
			min, minErr = mid, midErr
		} else {
			max, maxErr = mid, midErr
		}
	}
	if upper {
		return max
	} else {
		return min
	}
}
