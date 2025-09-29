// Copyright 2026 The go-ethereum Authors
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

package logindex

import (
	"math/rand"
	"os"
	"testing"
)

func TestTableFiles(t *testing.T) {
	testTableFiles(t, 1000000, 2000000000, true)
	testTableFiles(t, 1000000, 2000000000, false)
	testTableFiles(t, 1000000, 100000, false)
}

func testTableFiles(t *testing.T, size, maxFileSize int64, memory bool) {
	testTableFilesWithFlags(t, size, maxFileSize, memory, false, false, false)
	testTableFilesWithFlags(t, size, maxFileSize, memory, true, false, false)
	testTableFilesWithFlags(t, size, maxFileSize, memory, true, true, false)
	testTableFilesWithFlags(t, size, maxFileSize, memory, true, false, true)
	testTableFilesWithFlags(t, size, maxFileSize, memory, true, true, true)
}

func testTableFilesWithFlags(t *testing.T, size, maxFileSize int64, memory, openClose, restart, deleteFile bool) {
	path, _ := os.MkdirTemp("", "index_table_test")
	defer os.RemoveAll(path)

	/*path := "test"
	os.RemoveAll(path)
	os.Mkdir(path, 0755)*/

	files, err := newTableFiles(path, maxFileSize, 4)
	if err != nil {
		t.Fatalf("Error during newTableFiles: %v", err)
	}
	defer func() {
		files.close()
	}()

	w, err := files.getAppendWriter("table_file_test", memory)
	if err != nil {
		t.Fatalf("Error during tableFiles.getAppendWriter: %v", err)
	}
	buf := make([]byte, 1000)
	for pos := int64(0); pos < size; {
		l := min(rand.Int63n(1000), size-pos)
		genTestBytes(buf[:l], pos)
		n, err := w.Write(buf[:l])
		if err != nil {
			t.Fatalf("Error during tableFileInfo.Write: %v", err)
		}
		if int64(n) != l {
			t.Fatalf("Not enough bytes written by tableFileInfo.Write (expected %d, got %d)", l, n)
		}
		pos += l
		if openClose && rand.Int63n(size) < 50000 {
			if err := w.Close(); err != nil {
				t.Fatalf("Error during tableFileInfo.Close: %v", err)
			}
			if deleteFile {
				if err := files.deleteFile("table_file_test"); err != nil {
					t.Fatalf("Error during tableFiles.deleteFile: %v", err)
				}
				pos = 0
				deleteFile = false
			}
			if restart && rand.Intn(2) == 0 {
				files.close()
				files, err = newTableFiles(path, maxFileSize, 4)
				if err != nil {
					t.Fatalf("Error during newTableFiles: %v", err)
				}
			}
			w, err = files.getAppendWriter("table_file_test", memory)
			if err != nil {
				t.Fatalf("Error during getAppendWriter: %v", err)
			}
		}
	}
	w.Close()
	r, rsize, err := files.getReaderAt("table_file_test")
	if err != nil {
		t.Fatalf("Error during tableFiles.getReaderAt: %v", err)
	}
	if rsize != size {
		t.Fatalf("Incorrect file size (expected %d, got %d)", size, rsize)
	}
	for range 10000 {
		pos := rand.Int63n(size)
		l := min(rand.Int63n(1000), size-pos)
		n, err := r.ReadAt(buf[:l], pos)
		if err != nil {
			t.Fatalf("Error during tableFileInfo.ReadAt: %v", err)
		}
		if int64(n) != l {
			t.Fatalf("Not enough bytes read by tableFileInfo.ReadAt (expected %d, got %d)", l, n)
		}
		if !checkTestBytes(buf[:l], pos) {
			t.Fatalf("Incorrect data read by tableFileInfo.ReadAt (offset %d, length %d)", pos, l)
		}
	}
}

func testByte(position int64) byte {
	return byte((position*0x37f8c99da27eea21 ^ 0x3a012ccd04562a34) * 0x7ae93788ddc97754)
}

func genTestBytes(b []byte, offset int64) {
	for i := range b {
		b[i] = testByte(offset)
		offset++
	}
}

func checkTestBytes(b []byte, offset int64) bool {
	for _, bb := range b {
		if bb != testByte(offset) {
			return false
		}
		offset++
	}
	return true
}
