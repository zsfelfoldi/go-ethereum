package main

import (
	"bufio"
	"encoding/binary"
	"image"
	"image/png"
	"os"
	"strconv"

	"github.com/ethereum/go-ethereum/crypto"
)

const (
	xs = 1000
	ys = 1000
)

func main() {
	pic := image.NewNRGBA(image.Rect(0, 0, xs, ys))
	set := func(x,y,c,v int) {
		pic.Pix[y*pic.Stride+x*4+c] = uint8(v)
	}
	for y:=0;y<ys;y++ {
		for x:=0;x<xs;x++ {
			set(x,y,3,255)
		}
	}

    f, _ := os.Open("1")
    scanner := bufio.NewScanner(f)

    // Set the Split method to ScanWords.
    scanner.Split(bufio.ScanWords)

	topicHash := crypto.Keccak256Hash([]byte("foo"))
	topicPrefix := binary.BigEndian.Uint64(topicHash[:8])

    // Scan all words from the file.
    for scanner.Scan() {
		w := scanner.Text()
		if w == "*R" {
			scanner.Scan()
			time, _ := strconv.ParseInt(scanner.Text(), 10, 64)
			scanner.Scan()
			scanner.Scan()
			rad, _ := strconv.ParseInt(scanner.Text(), 10, 64)
			x := int(time * xs / 10000)
			y := int(rad * ys / 1000000)
			set(x,y,2,255)
		}
		if w == "*+" {
			scanner.Scan()
			time, _ := strconv.ParseInt(scanner.Text(), 10, 64)
			scanner.Scan()
			prefix, _ := strconv.ParseUint(scanner.Text(), 16, 64)
			x := int(time * xs / 10000)
			y := int((prefix ^ topicPrefix)/((^uint64(0))/ys+1))
			set(x,y,1,255)
			scanner.Scan()
		}
    }	
	f.Close()

	f, _ = os.Create("test.png")
	w := bufio.NewWriter(f)
	png.Encode(w, pic)
	w.Flush()
	f.Close()
}