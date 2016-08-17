package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"os"
	"sort"
	"strconv"

	"github.com/ethereum/go-ethereum/crypto"
)

var xs, ys, maxTime int

func set(pic *image.NRGBA, x, y, c, v int) {
	if v > 255 {
		v = 255
	}
	if x >= 0 && x < xs && y >= 0 && y < ys {
		pic.Pix[y*pic.Stride+x*4+c] = uint8(v)
	}
}

type topicInfo struct {
	prefix    uint64
	nodes     uint64Slice
	nodeIdx   map[uint64]int
	pic, pic2 *image.NRGBA
	nodeRad   map[uint64]int
}

func main() {
	var nodes uint64Slice
	topics := make(map[string]*topicInfo)

	inputFile := "test.out"
	if len(os.Args) > 1 {
		inputFile = os.Args[1]
	}

	f, _ := os.Open(inputFile)
	scanner := bufio.NewScanner(f)
	scanner.Split(bufio.ScanWords)
	for scanner.Scan() {
		w := scanner.Text()
		if w == "*N" {
			scanner.Scan()
			prefix, _ := strconv.ParseUint(scanner.Text(), 16, 64)
			nodes = append(nodes, prefix)
		}
		if w == "*R" {
			scanner.Scan()
			time, _ := strconv.ParseInt(scanner.Text(), 10, 64)
			if int(time) > maxTime {
				maxTime = int(time)
			}
			scanner.Scan()
			topic := scanner.Text()
			if _, ok := topics[topic]; !ok {
				fmt.Println(topic)
				topicHash := crypto.Keccak256Hash([]byte(topic))
				topics[topic] = &topicInfo{prefix: binary.BigEndian.Uint64(topicHash[:8])}
			}
		}
	}
	f.Close()

	xs = maxTime / 10000
	ys = len(nodes)

	for _, t := range topics {
		t.nodes = make(uint64Slice, len(nodes))
		for i, v := range nodes {
			t.nodes[i] = v ^ t.prefix
		}
		sort.Sort(t.nodes)
		t.nodeIdx = make(map[uint64]int)
		for i, v := range t.nodes {
			t.nodeIdx[v^t.prefix] = i
		}

		t.pic = image.NewNRGBA(image.Rect(0, 0, xs, ys))
		for y := 0; y < ys; y++ {
			for x := 0; x < xs; x++ {
				set(t.pic, x, y, 3, 255)
			}
		}

		t.pic2 = image.NewNRGBA(image.Rect(0, 0, xs, ys))
		for y := 0; y < ys; y++ {
			for x := 0; x < xs; x++ {
				set(t.pic2, x, y, 3, 255)
			}
		}
		t.nodeRad = make(map[uint64]int)
	}

	f, _ = os.Open(inputFile)
	scanner = bufio.NewScanner(f)
	scanner.Split(bufio.ScanWords)

	for scanner.Scan() {
		w := scanner.Text()
		if w == "*R" {
			scanner.Scan()
			time, _ := strconv.ParseInt(scanner.Text(), 10, 64)
			scanner.Scan()
			t := topics[scanner.Text()]
			scanner.Scan()
			prefix, _ := strconv.ParseUint(scanner.Text(), 16, 64)
			scanner.Scan()
			rad, _ := strconv.ParseInt(scanner.Text(), 10, 64)
			if int(rad) != t.nodeRad[prefix] {
				t.nodeRad[prefix] = int(rad)
				radUint := uint64(rad) * ((^uint64(0)) / 1000000)
				x := int(time * int64(xs) / int64(maxTime))
				y := sort.Search(ys, func(i int) bool {
					return t.nodes[i] > radUint
				})
				set(t.pic, x, y, 1, 255)
			}
		}
		if w == "*MR" {
			scanner.Scan()
			time, _ := strconv.ParseInt(scanner.Text(), 10, 64)
			scanner.Scan()
			t := topics[scanner.Text()]
			scanner.Scan()
			scanner.Scan()
			rad, _ := strconv.ParseInt(scanner.Text(), 10, 64)
			radUint := uint64(rad) * ((^uint64(0)) / 1000000)
			x := int(time * int64(xs) / int64(maxTime))
			y := sort.Search(ys, func(i int) bool {
				return t.nodes[i] > radUint
			})
			set(t.pic, x, y, 0, 255)
		}
		if w == "*W" {
			scanner.Scan()
			time, _ := strconv.ParseInt(scanner.Text(), 10, 64)
			scanner.Scan()
			t := topics[scanner.Text()]
			scanner.Scan()
			prefix, _ := strconv.ParseUint(scanner.Text(), 16, 64)
			scanner.Scan()
			wp, _ := strconv.ParseInt(scanner.Text(), 10, 64)
			x := int(time * int64(xs) / int64(maxTime))
			y := t.nodeIdx[prefix]
			set(t.pic2, x, y, 0, int(wp/100000))
			set(t.pic2, x, y, 1, int(wp/10000))
			set(t.pic2, x, y, 2, int(wp/1000))
		}
		if w == "*+" {
			scanner.Scan()
			time, _ := strconv.ParseInt(scanner.Text(), 10, 64)
			scanner.Scan()
			t := topics[scanner.Text()]
			scanner.Scan()
			prefix, _ := strconv.ParseUint(scanner.Text(), 16, 64)
			x := int(time * int64(xs) / int64(maxTime))
			y := t.nodeIdx[prefix]
			set(t.pic, x, y, 2, 255)
			scanner.Scan()
		}
	}
	f.Close()

	for tt, t := range topics {
		f, _ = os.Create("test_" + tt + ".png")
		w := bufio.NewWriter(f)
		png.Encode(w, t.pic)
		w.Flush()
		f.Close()

		f, _ = os.Create("test2_" + tt + ".png")
		w = bufio.NewWriter(f)
		png.Encode(w, t.pic2)
		w.Flush()
		f.Close()
	}
}

type uint64Slice []uint64

// Len is the number of elements in the collection.
func (s uint64Slice) Len() int {
	return len(s)
}

// Less reports whether the element with
// index i should sort before the element with index j.
func (s uint64Slice) Less(i, j int) bool {
	return s[i] < s[j]
}

// Swap swaps the elements with indexes i and j.
func (s uint64Slice) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
