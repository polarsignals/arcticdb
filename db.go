package arcticdb

import (
	"sync"
	"sync/atomic"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/atomic"

	"github.com/polarsignals/arcticdb/query/logicalplan"
)

type ColumnStore struct {
	mtx *sync.RWMutex
	dbs map[string]*DB
	reg prometheus.Registerer
}

func New(reg prometheus.Registerer) *ColumnStore {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}

	return &ColumnStore{
		mtx: &sync.RWMutex{},
		dbs: map[string]*DB{},
		reg: reg,
	}
}

type DB struct {
	name string

	mtx    *sync.RWMutex
	tables map[string]*Table
	reg    prometheus.Registerer

	// Databases monotonically increasing transaction id
	tx uint64

	//TxPool is a waiting area for finished transactions that haven't been added to the watermark
	txPool *TxPool

	// highWatermark maintains the highest consecutively completed tx number
	highWatermark uint64
}

func (s *ColumnStore) DB(name string) *DB {
	s.mtx.RLock()
	db, ok := s.dbs[name]
	s.mtx.RUnlock()
	if ok {
		return db
	}

	s.mtx.Lock()
	defer s.mtx.Unlock()

	// Need to double-check that in the meantime a database with the same name
	// wasn't concurrently created.
	db, ok = s.dbs[name]
	if ok {
		return db
	}

	db = &DB{
		name:   name,
		mtx:    &sync.RWMutex{},
		tables: map[string]*Table{},
		reg:    prometheus.WrapRegistererWith(prometheus.Labels{"db": name}, s.reg),
	}

	db.txPool = NewTxPool(&db.highWatermark)

	s.dbs[name] = db
	return db
}

func (db *DB) Table(name string, config *TableConfig, logger log.Logger) *Table {
	db.mtx.RLock()
	table, ok := db.tables[name]
	db.mtx.RUnlock()
	if ok {
		return table
	}

	db.mtx.Lock()
	defer db.mtx.Unlock()

	// Need to double-check that in the meantime another table with the same
	// name wasn't concurrently created.
	table, ok = db.tables[name]
	if ok {
		return table
	}

	table = newTable(db, name, config, db.reg, logger)
	db.tables[name] = table
	return table
}

func (db *DB) TableProvider() *DBTableProvider {
	return NewDBTableProvider(db)
}

type DBTableProvider struct {
	db *DB
}

func NewDBTableProvider(db *DB) *DBTableProvider {
	return &DBTableProvider{
		db: db,
	}
}

func (p *DBTableProvider) GetTable(name string) logicalplan.TableReader {
	p.db.mtx.RLock()
	defer p.db.mtx.RUnlock()
	return p.db.tables[name]
}

// beginRead returns the high watermark. Reads can safely access any write that has a lower or equal tx id than the returned number.
func (db *DB) beginRead() uint64 {
	return atomic.LoadUint64(&db.highWatermark)
}

// begin is an internal function that Tables call to start a transaction for writes.
// It returns:
//   the write tx id
//   The current high watermark
//   A function to complete the transaction
func (db *DB) begin() (uint64, uint64, func()) {
	tx := atomic.AddUint64(&db.tx, 1)
	watermark := atomic.LoadUint64(&db.highWatermark)
	return tx, watermark, func() {

		if mark := atomic.LoadUint64(&db.highWatermark); mark+1 == tx { // This is the next consecutive transaction; increate the watermark
			atomic.AddUint64(&db.highWatermark, 1)
		}

		// place completed transaction in the waiting pool
		db.txPool.Prepend(tx)
	}
}
