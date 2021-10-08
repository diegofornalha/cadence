/*
 * Cadence - The resource-oriented smart contract programming language
 *
 * Copyright 2019-2020 Dapper Labs, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package runtime

import (
	"bytes"
	"runtime"
	"sort"
	"time"

	"github.com/onflow/atree"

	"github.com/onflow/cadence/runtime/common"
	"github.com/onflow/cadence/runtime/errors"
	"github.com/onflow/cadence/runtime/interpreter"
)

type Storage struct {
	*atree.PersistentSlabStorage
	// NOTE: temporary, will be refactored to dictionary
	deltas          map[interpreter.StorageKey]interpreter.Value
	cache           map[interpreter.StorageKey]interpreter.Value
	contractUpdates map[interpreter.StorageKey]interpreter.Value
	Ledger          atree.Ledger
	reportMetric    func(f func(), report func(metrics Metrics, duration time.Duration))
}

var _ atree.SlabStorage = &Storage{}
var _ interpreter.Storage = &Storage{}

func NewStorage(
	ledger atree.Ledger,
	reportMetric func(f func(), report func(metrics Metrics, duration time.Duration)),
) *Storage {
	ledgerStorage := atree.NewLedgerBaseStorage(ledger)
	persistentSlabStorage := atree.NewPersistentSlabStorage(
		ledgerStorage,
		interpreter.CBOREncMode,
		interpreter.CBORDecMode,
		interpreter.DecodeStorable,
		interpreter.DecodeTypeInfo,
	)
	return &Storage{
		Ledger:                ledger,
		PersistentSlabStorage: persistentSlabStorage,
		deltas:                map[interpreter.StorageKey]interpreter.Value{},
		cache:                 map[interpreter.StorageKey]interpreter.Value{},
		contractUpdates:       map[interpreter.StorageKey]interpreter.Value{},
		reportMetric:          reportMetric,
	}
}

// ValueExists returns true if a value exists in account storage.
//
func (s *Storage) ValueExists(
	_ *interpreter.Interpreter,
	address common.Address,
	key string,
) bool {

	storageKey := interpreter.StorageKey{
		Address: address,
		Key:     key,
	}

	// Check locally

	if value, ok := s.deltas[storageKey]; ok {
		return value != nil
	}

	if value, ok := s.cache[storageKey]; ok {
		return value != nil
	}

	// Ask interface

	var exists bool
	var err error
	wrapPanic(func() {
		exists, err = s.Ledger.ValueExists(address[:], []byte(key))
	})
	if err != nil {
		panic(err)
	}

	if !exists {
		s.cache[storageKey] = nil
	}

	return exists
}

// ReadValue returns a value from account storage.
//
func (s *Storage) ReadValue(
	_ *interpreter.Interpreter,
	address common.Address,
	key string,
) interpreter.OptionalValue {

	storageKey := interpreter.StorageKey{
		Address: address,
		Key:     key,
	}

	// Check locally

	if value, ok := s.deltas[storageKey]; ok {
		if value == nil {
			return interpreter.NilValue{}
		}

		return interpreter.NewSomeValueNonCopying(value)
	}

	if value, ok := s.cache[storageKey]; ok {
		if value == nil {
			return interpreter.NilValue{}
		}

		return interpreter.NewSomeValueNonCopying(value)
	}

	// Load and deserialize the stored value (if any)
	// through the runtime interface

	var storedData []byte
	var err error
	wrapPanic(func() {
		storedData, err = s.Ledger.GetValue(address[:], []byte(key))
	})
	if err != nil {
		panic(err)
	}

	if len(storedData) == 0 {
		s.cache[storageKey] = nil
		return interpreter.NilValue{}
	}

	var storable atree.Storable
	var storedValue atree.Value

	decoder := interpreter.CBORDecMode.NewByteStreamDecoder(storedData)

	s.reportMetric(
		func() {
			storable, err = interpreter.DecodeStorable(decoder, atree.StorageIDUndefined)
		},
		func(metrics Metrics, duration time.Duration) {
			metrics.ValueDecoded(duration)
		},
	)
	if err != nil {
		panic(err)
	}

	storedValue, err = storable.StoredValue(s)
	if err != nil {
		panic(err)
	}

	value := interpreter.MustConvertStoredValue(storedValue)

	s.cache[storageKey] = value

	return interpreter.NewSomeValueNonCopying(value)
}

func (s *Storage) WriteValue(
	_ *interpreter.Interpreter,
	address common.Address,
	key string,
	value interpreter.OptionalValue,
) {
	storageKey := interpreter.StorageKey{
		Address: address,
		Key:     key,
	}

	// Only write locally.
	// The value is eventually written back through the runtime interface in `Commit`.

	var writtenValue interpreter.Value

	switch typedValue := value.(type) {
	case *interpreter.SomeValue:
		writtenValue = typedValue.Value

	case interpreter.NilValue:
		writtenValue = nil

	default:
		panic(errors.NewUnreachableError())
	}

	s.deltas[storageKey] = writtenValue
}

func (s *Storage) recordContractUpdate(
	address common.Address,
	key string,
	contract interpreter.Value,
) {
	storageKey := interpreter.StorageKey{
		Address: address,
		Key:     key,
	}

	s.contractUpdates[storageKey] = contract
}

type accountStorageEntry struct {
	storageKey interpreter.StorageKey
	value      interpreter.Value
}

// TODO: bring back concurrent encoding
// Commit serializes/saves all values in the cache in storage (through the runtime interface).
//
func (s *Storage) Commit() error {

	var accountStorageEntries []accountStorageEntry

	// First, write all values in the account storage and the contract updates

	for storageKey, value := range s.deltas { //nolint:maprangecheck
		accountStorageEntries = append(
			accountStorageEntries,
			accountStorageEntry{
				storageKey: storageKey,
				value:      value,
			},
		)
	}

	for storageKey, value := range s.contractUpdates { //nolint:maprangecheck
		accountStorageEntries = append(accountStorageEntries, accountStorageEntry{
			storageKey: storageKey,
			value:      value,
		})
	}

	// Sort the account storage entries by storage key in lexicographic order

	sort.Slice(accountStorageEntries, func(i, j int) bool {
		a := accountStorageEntries[i].storageKey
		b := accountStorageEntries[j].storageKey

		if bytes.Compare(a.Address[:], b.Address[:]) < 0 {
			return true
		}

		if a.Key < b.Key {
			return true
		}

		return false
	})

	// Write account storage entries in order

	// TODO: bring back concurrent encoding
	for _, entry := range accountStorageEntries {
		var encoded []byte
		address := entry.storageKey.Address

		if entry.value != nil {
			storable, err := entry.value.Storable(s, atree.Address(address), atree.MaxInlineElementSize)
			if err != nil {
				return err
			}

			var buf bytes.Buffer
			encoder := atree.NewEncoder(&buf, interpreter.CBOREncMode)

			s.reportMetric(
				func() {
					err = storable.Encode(encoder)
				},
				func(metrics Metrics, duration time.Duration) {
					metrics.ValueEncoded(duration)
				},
			)
			if err != nil {
				return err
			}

			err = encoder.CBOR.Flush()
			if err != nil {
				return err
			}

			encoded = buf.Bytes()
		}

		var err error
		wrapPanic(func() {
			err = s.Ledger.SetValue(
				address[:],
				[]byte(entry.storageKey.Key),
				encoded,
			)
		})
		if err != nil {
			return err
		}
	}

	// Commit the underlying slab storage's deltas

	// TODO: report encoding metric for all encoded slabs
	return s.PersistentSlabStorage.FastCommit(runtime.NumCPU())
}