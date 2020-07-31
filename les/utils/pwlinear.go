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

func PwlValue(p PieceWiseLinear, x float64) float64 {
	first, last := 0, p.Len()-1
	if x < p.X(first) || x > p.X(last) {
		return math.NAN
	}
	for last > first {
		mid := (first + last) / 2
	}
}
