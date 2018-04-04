package db

import (
	"github.com/dgraph-io/badger"
	"github.com/dgraph-io/badger/options"
	log "github.com/inconshreveable/log15"
)

var blog = log.New("module", "db.gobadgerdb")

type GoBadgerDB struct {
	db *badger.DB
}

func init() {
	dbCreator := func(name string, dir string, cache int) (DB, error) {
		return NewGoBadgerDB(name, dir, cache)
	}
	registerDBCreator(GoBadgerDBBackendStr, dbCreator, false)
}

func NewGoBadgerDB(name string, dir string, cache int) (*GoBadgerDB, error) {
	opts := badger.DefaultOptions
	opts.Dir = dir
	opts.ValueDir = dir
	if cache <= 128 {
		opts.ValueLogLoadingMode = options.FileIO
		//opts.MaxTableSize = int64(cache) << 18 // cache = 128, MaxTableSize = 32M
		opts.NumCompactors = 1
		opts.NumMemtables = 1
		opts.NumLevelZeroTables = 1
		opts.NumLevelZeroTablesStall = 2
		opts.TableLoadingMode = options.MemoryMap
		opts.ValueLogFileSize = 1 << 28 // 256M
	}

	db, err := badger.Open(opts)
	if err != nil {
		blog.Error("NewGoBadgerDB", "error", err)
		return nil, err
	}

	return &GoBadgerDB{db: db}, nil
}

func (db *GoBadgerDB) Get(key []byte) []byte {
	var val []byte
	err := db.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			if err == badger.ErrKeyNotFound {
				return nil
			} else {
				blog.Error("Get", "txn.Get.error", err)
				return nil
			}
		}
		val, err = item.Value()
		if err != nil {
			blog.Error("Get", "item.Value.error", err)
			return nil
		}
		return nil
	})

	if err != nil {
		blog.Error("Get", "error", err)
		return nil
	}
	return val
}

func (db *GoBadgerDB) Set(key []byte, value []byte) {
	err := db.db.Update(func(txn *badger.Txn) error {
		err := txn.Set(key, value)
		return err
	})

	if err != nil {
		blog.Error("Set", "error", err)
	}
}

func (db *GoBadgerDB) SetSync(key []byte, value []byte) {
	err := db.db.Update(func(txn *badger.Txn) error {
		err := txn.Set(key, value)
		return err
	})

	if err != nil {
		blog.Error("SetSync", "error", err)
	}
}

func (db *GoBadgerDB) Delete(key []byte) {
	err := db.db.Update(func(txn *badger.Txn) error {
		err := txn.Delete(key)
		return err
	})

	if err != nil {
		blog.Error("Delete", "error", err)
	}
}

func (db *GoBadgerDB) DeleteSync(key []byte) {
	err := db.db.Update(func(txn *badger.Txn) error {
		err := txn.Delete(key)
		return err
	})

	if err != nil {
		blog.Error("DeleteSync", "error", err)
	}
}

func (db *GoBadgerDB) DB() *badger.DB {
	return db.db
}

func (db *GoBadgerDB) Close() {
	db.db.Close()
}

func (db *GoBadgerDB) Print() {
	// TODO: Returns statistics of the underlying DB
	err := db.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 10
		it := txn.NewIterator(opts)

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			k := item.Key()
			v, err := item.Value()
			if err != nil {
				return err
			}
			blog.Info("Print", "key", string(k), "value", string(v))
			//blog.Info("Print", "key", string(item.Key()))
		}
		return nil
	})
	if err != nil {
		blog.Error("Print", err)
	}
}

func (db *GoBadgerDB) Stats() map[string]string {
	//TODO
	return nil
}

func (db *GoBadgerDB) Iterator(prefix []byte, reserse bool) Iterator {
	txn := db.db.NewTransaction(false)
	opts := badger.DefaultIteratorOptions
	opts.Reverse = reserse
	it := txn.NewIterator(opts)
	if reserse {
		last := bytesPrefix(prefix)
		it.Seek(last)
	} else {
		it.Seek(prefix)
	}
	return &goBadgerDBIt{it, txn, nil, prefix, reserse}
}

type goBadgerDBIt struct {
	*badger.Iterator
	txn     *badger.Txn
	err     error
	prefix  []byte
	reserse bool
}

func (it *goBadgerDBIt) Next() bool {
	it.Iterator.Next()
	return it.Valid()
}

func (it *goBadgerDBIt) Rewind() bool {
	if it.reserse {
		last := bytesPrefix(it.prefix)
		it.Seek(last)
	} else {
		it.Seek(it.prefix)
	}
	return it.Valid()
}

func (it *goBadgerDBIt) Seek(key []byte) bool {
	it.Iterator.Seek(key)
	return it.Valid()
}

func (it *goBadgerDBIt) Close() {
	it.Iterator.Close()
	it.txn.Discard()
}

func (it *goBadgerDBIt) Valid() bool {
	return it.Iterator.ValidForPrefix(it.prefix)
}

func (it *goBadgerDBIt) Key() []byte {
	return it.Item().Key()
}

func (it *goBadgerDBIt) Value() []byte {
	value, err := it.Item().Value()
	if err != nil {
		it.err = err
	}
	return value
}

func (it *goBadgerDBIt) ValueCopy() []byte {
	value, err := it.Item().ValueCopy(nil)
	if err != nil {
		it.err = err
	}
	return value
}

func (it *goBadgerDBIt) Error() error {
	return it.err
}

type GoBadgerDBBatch struct {
	db    *GoBadgerDB
	batch *badger.Txn
	//wop   *opt.WriteOptions
}

func (db *GoBadgerDB) NewBatch(sync bool) Batch {
	batch := db.db.NewTransaction(true)
	return &GoBadgerDBBatch{db, batch}
}

func (mBatch *GoBadgerDBBatch) Set(key, value []byte) {
	mBatch.batch.Set(key, value)
}

func (mBatch *GoBadgerDBBatch) Delete(key []byte) {
	mBatch.batch.Delete(key)
}

func (mBatch *GoBadgerDBBatch) Write() {
	defer mBatch.batch.Discard()

	if err := mBatch.batch.Commit(nil); err != nil {
		blog.Error("Write", "error", err)
	}
}
