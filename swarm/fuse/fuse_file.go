// Copyright 2017 The go-ethereum Authors
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

// +build linux darwin freebsd

package fuse

import (
	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"errors"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/swarm/storage"
	"golang.org/x/net/context"
	"io"
	"os"
	"sync"
	"time"
)

const (
	MaxAppendFileSize = 10485760 // 10Mb
)

var (
	errInvalidOffset           = errors.New("Invalid offset during write")
	errFileSizeMaxLimixReached = errors.New("File size exceeded max limit")
)

var (
	_ fs.Node         = (*SwarmFile)(nil)
	_ fs.HandleReader = (*SwarmFile)(nil)
	_ fs.HandleWriter = (*SwarmFile)(nil)
)

type SwarmFile struct {
	inode    uint64
	name     string
	path     string
	fmode    os.FileMode
	modTime  time.Time
	key      storage.Key
	fileSize int64
	reader   storage.LazySectionReader

	mountInfo *MountInfo
	lock      *sync.RWMutex
}

func NewSwarmFile(path, fname string, mode int64, modificationTime time.Time, fileSize int64, minfo *MountInfo) *SwarmFile {
	// Some old uploads dont have the mode and size field.. Then set it to some default
	fMode := os.FileMode(0700)
	if mode != 0 {
		fMode = os.FileMode(mode)
	}

	fSize := fileSize
	if fSize <= 0 {
		fSize = -1
	}

	newFile := &SwarmFile{
		inode:     NewInode(),
		name:      fname,
		path:      path,
		fmode:     fMode,
		modTime:   modificationTime,
		key:       nil,
		fileSize:  -1, // -1 means , file already exists in swarm and you need to just get the size from swarm
		reader:    nil,
		mountInfo: minfo,
		lock:      &sync.RWMutex{},
	}
	return newFile
}

func (file *SwarmFile) Attr(ctx context.Context, a *fuse.Attr) error {

	a.Inode = file.inode
	a.Mode = file.fmode
	a.Mtime = file.modTime
	a.Uid = uint32(os.Getuid())
	a.Gid = uint32(os.Getegid())

	if file.fileSize == -1 {
		reader := file.mountInfo.swarmApi.Retrieve(file.key)
		quitC := make(chan bool)
		size, err := reader.Size(quitC)
		if err != nil {
			log.Warn("Couldnt get size of file %s : %v", file.path, err)
			size = 0
		}
		file.fileSize = int64(size)

	}
	a.Size = uint64(file.fileSize)
	return nil
}

func (file *SwarmFile) GetFileSize() uint64 {

	if file.fileSize == -1 {
		reader := file.mountInfo.swarmApi.Retrieve(file.key)
		quitC := make(chan bool)
		size, err := reader.Size(quitC)
		if err != nil {
			log.Warn("Couldnt get size of file %s : %v", file.path, err)
		}
		file.fileSize = int64(size)
	}
	return uint64(file.fileSize)
}

func (sf *SwarmFile) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {

	sf.lock.RLock()
	defer sf.lock.RUnlock()
	if sf.reader == nil {
		sf.reader = sf.mountInfo.swarmApi.Retrieve(sf.key)
	}
	buf := make([]byte, req.Size)
	n, err := sf.reader.ReadAt(buf, req.Offset)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		err = nil
	}
	resp.Data = buf[:n]
	sf.reader = nil
	return err

}

func (sf *SwarmFile) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {

	if sf.fileSize == 0 && req.Offset == 0 {

		// A new file is created
		err := addFileToSwarm(sf, req.Data, len(req.Data))
		if err != nil {
			return err
		}
		resp.Size = len(req.Data)

	} else if req.Offset <= sf.fileSize {

		totalSize := sf.fileSize + int64(len(req.Data))
		if totalSize > MaxAppendFileSize {
			log.Warn("Append file size reached (%v) : (%v)", sf.fileSize, len(req.Data))
			return errFileSizeMaxLimixReached
		}

		err := appendToExistingFileInSwarm(sf, req.Data, req.Offset, int64(len(req.Data)))
		if err != nil {
			return err
		}
		resp.Size = int(sf.fileSize)
	} else {
		log.Warn("Invalid write request size(%v) : off(%v)", sf.fileSize, req.Offset)
		return errInvalidOffset
	}

	return nil
}
