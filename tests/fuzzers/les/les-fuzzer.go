package les

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/les"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/nodestate"
	//	"github.com/ethereum/go-ethereum/rlp"
)

// The function must return
// 1 if the fuzzer should increase priority of the
//    given input during subsequent fuzzing (for example, the input is lexically
//    correct and was parsed successfully);
// -1 if the input must not be added to corpus even if gives new coverage; and
// 0  otherwise
// other values are reserved for future use.
func Fuzz(data []byte) int {
	f := fuzzer{
		input:     bytes.NewReader(data),
		exhausted: false,
	}
	return f.fuzz()
}

type fuzzer struct {
	input     io.Reader
	exhausted bool
	debugging bool
	enodes    []*enode.Node
	setup     *nodestate.Setup
}

func (f *fuzzer) read(size int) []byte {
	out := make([]byte, size)
	if _, err := f.input.Read(out); err != nil {
		f.exhausted = true
	}
	return out
}

func (f *fuzzer) randomByte() byte {
	d := f.read(1)
	return d[0]
}
func (f *fuzzer) randomBool() bool {
	d := f.read(1)
	return d[0]&1 == 1
}

func (f *fuzzer) randomInt(max int) int {
	if max == 0 {
		return 0
	}
	var a uint16
	if err := binary.Read(f.input, binary.LittleEndian, &a); err != nil {
		f.exhausted = true
	}
	return int(a % uint16(max))
}

//TODO block generator/count, ??indexer epoch, protocol version

func (f *fuzzer) fuzz() int {
	clock := &mclock.Simulated{}
	server := les.NewFuzzerServer(clock)
	client := les.NewFuzzerClient(clock)
	serverPipe, clientPipe := p2p.MsgPipe()
	les.NewFuzzerConnection(server, client, serverPipe, clientPipe)
	for {
		b := f.randomByte()
		if b == 255 {
			return 0
		}
		req := &les.BlockRequest{Number: uint64(b)}
		client.Request(context.Background(), server, req)
	}
	return 0

}
