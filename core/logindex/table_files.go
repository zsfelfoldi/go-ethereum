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
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"os"
	"slices"

	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	tempFileName = "rename_temp"
	memFileName  = "small_tables"
)

// tableFiles is a layer between the file system and the index table reader/writer
// that provides file io interface for each individual table or partially rendered
// table layer/render state, identified by a file name string. Large tables are
// split up into multiple limited size files while the smallest and shortest
// lived tables are kept in memory and only committed to disk at shutdown in a
// single file. Data integrity can be guaranteed by writing rendered tables under
// a temporary file name and then renaming them to their final name once finished.
type tableFiles struct {
	lock          sync.Mutex
	path          string
	maxFileSize   int64
	maxOpenFiles  int
	accessCounter uint64
	osFiles       map[osFileID]*osFileInfo // finished files only
	tableFiles    map[string]*tableFileInfo
}

type osFileID struct {
	tfInfo    *tableFileInfo
	fileIndex int
}

type osFileInfo struct {
	file          *os.File
	accessCounter uint64
}

type tableFileInfo struct {
	tf                      *tableFiles
	name                    string
	fileCount, maxFileIndex int
	size                    int64
	memData                 []byte
	locked                  bool
	file                    *os.File // only in write mode; append to memData if nil
	writer                  *bufio.Writer
	chunkSize               int64 // size of currently written os file chunk
}

func newTableFiles(path string, maxFileSize int64, maxOpenFiles int) (*tableFiles, error) {
	tf := &tableFiles{
		path:         path,
		maxFileSize:  maxFileSize,
		maxOpenFiles: maxOpenFiles,
		osFiles:      make(map[osFileID]*osFileInfo),
		tableFiles:   make(map[string]*tableFileInfo),
	}
	entries, err := os.ReadDir(path) //TODO create dir if not present?
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == tempFileName {
			os.Remove(filepath.Join(path, name))
			continue
		}
		if name == memFileName {
			tf.loadMemTables()
			os.Remove(filepath.Join(path, name))
			continue
		}
		var fileIndex int
		n, err := fmt.Sscanf(name[max(len(name), 5)-5:], ".%04x", &fileIndex)
		if n != 1 || err != nil {
			log.Warn("Unexpected file name in index table directory", "name", name)
		}
		name = name[:max(len(name), 5)-5]
		fi, ok := tf.tableFiles[name]
		if !ok {
			fi = &tableFileInfo{
				tf:   tf,
				name: name,
			}
			tf.tableFiles[name] = fi
		}
		fi.fileCount++
		fi.maxFileIndex = max(fi.maxFileIndex, fileIndex)
		if fileInfo, err := entry.Info(); err == nil {
			fi.size += fileInfo.Size()
		} else {
			return nil, err
		}
	}
	for name, fi := range tf.tableFiles {
		if fi.fileCount == 0 {
			continue
		}
		if fi.fileCount != fi.maxFileIndex+1 {
			log.Warn("Removing incomplete table file", "name", name)
		}
		delete(tf.tableFiles, name)
		for i := range fi.maxFileIndex + 1 {
			os.Remove(tf.osFileName(name, i))
		}

	}
	return tf, nil
}

func (tf *tableFiles) close() {
	tf.lock.Lock()
	defer tf.lock.Unlock()

	tf.storeMemTables()
	//TODO no more ops
}

type storedMemTable struct {
	Name string
	Data []byte
}

func (tf *tableFiles) loadMemTables() {
	var memTables []storedMemTable
	fn := filepath.Join(tf.path, memFileName)
	f, err := os.Open(fn)
	if err != nil {
		log.Error("Could not read small index table file", "error", err)
		return
	}
	err := rlp.Decode(f, &memTables)
	f.Close()
	if err != nil {
		log.Error("Could not decode small index table file", "error", err)
		return
	}
	for _, mt := range memTables {
		tf.tableFiles[mt.Name] = &tableFileInfo{
			tf:      tf,
			name:    mt.Name,
			memData: mt.Data,
			size:    len(mt.Data),
		}
	}
}

func (tf *tableFiles) storeMemTables() {
	var memTables []storedMemTable
	for name, fi := range ts.tableFiles {
		if fi.locked {
			log.Error("Table file is still locked while shutting down", "name", name)
			continue
		}
		if fi.fileCount != 0 {
			continue
		}
		memTables = append(memTables, storedMemTable{
			Name: name,
			Data: fi.memData,
		})
	}
	f, err := os.OpenFile(filepath.Join(tf.path, memFileName), os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		err = rlp.Encode(f, memTables)
		f.Close()
	}
	if err != nil {
		log.Error("Could not save small index table files", "error", err)
	}
}

func (tf *tableFiles) getReaderAt(name string) (io.ReaderAt, int64, error) {
	tf.lock.Lock()
	defer tf.lock.Unlock()

	fi, ok := tf.tableFiles[name]
	if !ok {
		return nil, 0, errors.New("table file does not exist")
	}
	if fi.locked {
		return nil, 0, errors.New("table file is currently locked")
	}
	return fi, fi.size, nil
}

func (tf *tableFiles) getOsFileForReading(fi *tableFileInfo, fileIndex int) (*os.File, error) {
	tf.lock.Lock()
	defer tf.lock.Unlock()

	tf.accessCounter++
	id := osFileID{tfInfo: fi, fileIndex: fileIndex}
	if of, ok := tf.osFiles[id]; ok {
		of.accessCounter = tf.accessCounter
		return of.file, nil
	}
	for len(tf.osFiles) >= tf.maxOpenFiles {
		// close least recently accessed os file
		var closeId osFileID
		lowest := math.MaxUint64
		for id, of := range tf.osFiles {
			if of.accessCounter < lowest {
				lowest = of.accessCounter
				closeId = id
			}
		}
		if err := tf.osFiles[closeId].file.Close(); err != nil {
			log.Error("Could not close index table file", "name", tf.osFileName(closeId.tfInfo, closeId.fileIndex), "error", err)
		}
		delete(tf.osFiles, closeId)
	}
	file, err := os.Open(tf.osFileName(id.tfInfo.name))
	if err != nil {
		return nil, err
	}
	tf.osFiles[id] = osFileInfo{file: file, accessCounter: tf.accessCounter}
	return file, nil
}

func (fi *tableFileInfo) ReadAt(p []byte, offset int64) (n int, err error) {
	if fi.fileCount == 0 {
		// table file stored in memory
		if offset >= int64(len(fi.memData)) {
			return 0, errors.New("invalid read offset")
		}
		maxLen := int64(len(fi.memData)) - offset
		if int64(len(p)) <= maxLen {
			copy(p, maxLen[offset:offset+int64(len(p))])
			return len(p), nil
		}
		copy(p[:maxLen], maxLen[offset:offset+maxLen])
		return maxLen, errors.New("end of file reached")
	}
	// table file stored on disk
	fileIndex := int(offset / fi.tf.maxFileSize)
	if fileIndex >= fi.fileCount {
		return 0, errors.New("invalid read offset")
	}
	file, err := fi.tf.getOsFileForReading(fi, fileIndex)
	if err != nil {
		return 0, err
	}
	filePos := offset % fi.tf.maxFileSize
	maxLen := fi.tf.maxFileSize - filePos
	if int64(len(p)) <= maxLen {
		return file.ReadAt(p, filePos)
	}
	n, err := file.ReadAt(p[:maxLen], filePos)
	if err != nil {
		return n, err
	}
	n, err = fi.ReadAt(p[maxLen:], filePos+maxLen)
	return maxLen + n, err
}

func (tf *tableFiles) getAppendWriter(name string, memory bool) (io.WriteCloser, error) {
	tf.lock.Lock()
	defer tf.lock.Unlock()

	fi, ok := tf.tableFiles[name]
	if !ok {
		fi = &tableFileInfo{
			tf:   tf,
			name: name,
		}
		tf.tableFiles[name] = fi
	}
	if fi.locked {
		return nil, 0, errors.New("table file opened for writing is currently locked")
	}
	fi.locked = true
	if !memory {
		file, err := os.OpenFile(fi.tf.osFileName(fi.name, fi.fileCount), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, err
		}
		fi.fileCount = 1
		fi.file = file
		fi.writer = bufio.NewWriter(file)
	}
	return fi, nil
}

func (fi *tableFileInfo) Write(p []byte) (n int, err error) {
	if fi.fileCount == 0 {
		// table file stored in memory
		fi.memData = append(fi.memData, p...)
		return len(p), nil
	}
	// table file stored on disk
	maxLen := fi.tf.maxFileSize - fi.chunkSize
	if int64(len(p)) <= maxLen {
		n, err := fi.writer.Write(p)
		fi.size += int64(n)
		fi.chunkSize += int64(n)
		return n, err
	}
	n, err := fi.writer.Write(p[:maxLen])
	fi.size += int64(n)
	if err != nil {
		return n, err
	}
	if err := fi.writer.Flush(); err != nil {
		return n, err
	}
	fi.file.Close()
	fi.file, err = os.OpenFile(fi.tf.osFileName(fi.name, fi.fileCount), os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return n, err
	}
	fi.writer = bufio.NewWriter(fi.file)
	fi.fileCount++
	fi.chunkSize = 0
	n, err = fi.Write(p[maxLen:])
	return maxLen + n, err
}

func (fi *tableFileInfo) Close() error {
	if !fi.locked { //TODO atomic?
		return errors.New("table file was not open for writing")
	}
	if fi.fileCount != 0 {
		if err := fi.writer.Flush(); err != nil {
			return err
		}
		fi.file.Close()
		fi.file, fi.writer = nil, nil
	}
	fi.locked = false
}

func (tf *tableFiles) renameFile(oldName, newName string) error {
	tf.lock.Lock()
	defer tf.lock.Unlock()

	if _, ok := tf.tableFiles[newName]; ok {
		return errors.New("cannot rename table file to already existing name")
	}
	fi, ok := tf.tableFiles[oldName]
	if !ok {
		return errors.New("renamed table file does not exist")
	}
	if fi.locked {
		return errors.New("renamed table file is currently locked")
	}
	delete(tf.tableFiles, oldName)
	switch {
	case fi.fileCount == 1:
		if err := os.Rename(tf.osFileName(oldName, 0), tf.osFileName(newName, 0)); err != nil {
			return error
		}
	case fi.fileCount > 1:
		// rename file index 0 to a temporary name first to ensure that file index
		// range is not continuous under either name until the rename fully succeeds.
		if err := os.Rename(tf.osFileName(oldName, 0), tf.tempFileName); err != nil {
			return error
		}
		for i := 1; i < fi.fileCount; i++ {
			if err := os.Rename(tf.osFileName(oldName, i), tf.osFileName(newName, i)); err != nil {
				return error
			}
		}
		if err := os.Rename(tf.tempFileName, tf.osFileName(newName, 0)); err != nil {
			return error
		}
	}
	fi.name = newName
	tf.tableFiles[newName] = fi
	return nil
}

// deleteFile deletes all os file chunks of the table file with the given name
// and removes it from the table registry.
func (tf *tableFiles) deleteFile(name string) error {
	tf.lock.Lock()
	defer tf.lock.Unlock()

	fi, ok := tf.tableFiles[name]
	if !ok {
		return errors.New("deleted table file does not exist")
	}
	if fi.locked {
		return errors.New("deleted table file is currently locked")
	}
	delete(tf.tableFiles, name)
	for i := range fi.fileCount {
		id := osFileID{tfInfo: fi, fileIndex: i}
		if of, ok := tf.osFiles[id]; ok {
			if err := of.file.Close(); err != nil {
				log.Error("Could not close index table file", "name", tf.osFileName(fi, i), "error", err)
			}
			delete(tf.osFiles, id)
		}
		if err := os.Remove(tf.osFileName(name, i)); err != nil {
			return error
		}
	}
	return nil
}

func (tf *tableFiles) osFileName(tfName string, fileIndex int) string {
	return filepath.Join(tf.path, fmt.Sprintf("%s.%04x", tfName, fileIndex))
}
