package bloombits

const SectionSize = 4096

type (
	BitVector  []byte
	CompVector []byte
)

func bvAnd(a, b BitVector) {
	for i, bb := range b {
		a[i] &= bb
	}
}

func bvOr(a, b BitVector) {
	for i, bb := range b {
		a[i] |= bb
	}
}

func bvZero() BitVector {
	return make(BitVector, SectionSize/8)
}

func bvCopy(a BitVector) BitVector {
	c := make(BitVector, SectionSize/8)
	copy(c, a)
	return c
}

func bvIsNonZero(a BitVector) bool {
	for _, b := range a {
		if b != 0 {
			return true
		}
	}
	return false
}

func DecompressBloomBits(bits CompVector) BitVector {
	if len(bits) == SectionSize/8 {
		// make a copy so that output is always detached from input
		return bvCopy(BitVector(bits))
	}
	dc, ofs := decompressBits(bits, SectionSize/8)
	if ofs != len(bits) {
		panic(nil)
	}
	return dc
}

func decompressBits(bits []byte, targetLen int) ([]byte, int) {
	lb := len(bits)
	dc := make([]byte, targetLen)
	if lb == 0 {
		return dc, 0
	}

	l := targetLen / 8
	var (
		b   []byte
		ofs int
	)
	if l == 1 {
		b = bits[0:1]
		ofs = 1
	} else {
		b, ofs = decompressBits(bits, l)
	}
	for i, _ := range dc {
		if b[i/8]&(1<<byte(7-i%8)) != 0 {
			if ofs == lb {
				panic(nil)
			}
			dc[i] = bits[ofs]
			ofs++
		}
	}
	return dc, ofs
}
