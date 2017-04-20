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

package storage

import (
	"encoding/binary"
	"path/filepath"
)

type StoreParams struct {
	DbStorePath      string
	DbStoreCapacity  uint64
	MemStoreCapacity uint
	Hash             string
}

func NewStoreParams(path string) (self *StoreParams) {
	return &StoreParams{
		DbStorePath:      filepath.Join(path, "chunks"),
		DbStoreCapacity:  defaultDbCapacity,
		MemStoreCapacity: defaultCacheCapacity,
		Hash:             "SHA3",
	}
}

// LocalStore is a combination of inmemory db over a disk persisted db
// implements a Get/Put with fallback (caching) logic using any 2 ChunkStores
type LocalStore struct {
	*Hasher
	memStore *MemStore
	DbStore  *DbStore
}

// This constructor uses MemStore and DbStore as components
func NewLocalStore(params *StoreParams) (*LocalStore, error) {
	hasher := NewHasher(params.Hash)
	dbStore, err := NewDbStore(params.DbStorePath, hasher, params.DbStoreCapacity, 0)
	if err != nil {
		return nil, err
	}
	memStore := NewMemStore(dbStore, params.MemStoreCapacity)
	return &LocalStore{
		Hasher:   hasher,
		memStore: memStore,
		DbStore:  dbStore,
	}, nil
}

// LocalStore is itself a chunk store
// unsafe, in that the data is not integrity checked
func (self *LocalStore) Put(chunk *Chunk) {
	chunk.dbStored = make(chan bool)
	// if key is not specified, calculate it
	// impossible to send invalid chunk
	if len(chunk.Key) == 0 {
		chunk.Key = self.Hash(chunk.SData)
	}
	self.memStore.Put(chunk)
	if chunk.wg != nil {
		chunk.wg.Add(1)
	}
	// if the chunk is an open request, do not save it to db
	if chunk.SData != nil {
		go func() {
			self.DbStore.Put(chunk)
			if chunk.wg != nil {
				chunk.wg.Done()
			}
		}()
	}
}

// Get(chunk *Chunk) looks up a chunk in the local stores
// This method is blocking until the chunk is retrieved
// so additional timeout may be needed to wrap this call if
// ChunkStores are remote and can have long latency
func (self *LocalStore) Get(key Key) (chunk *Chunk, err error) {
	chunk, err = self.memStore.Get(key)
	if err == nil {
		return
	}
	chunk, err = self.DbStore.Get(key)
	if err != nil {
		return
	}
	chunk.Size = int64(binary.LittleEndian.Uint64(chunk.SData[0:8]))
	self.memStore.Put(chunk)
	return
}

// Close local store
func (self *LocalStore) Close() {
	return
}
