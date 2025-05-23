// Adapted version of https://github.com/ipfs/go-ds-badger4
package db

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	ds "github.com/ipfs/go-datastore"
	dsq "github.com/ipfs/go-datastore/query"
	"github.com/rs/zerolog/log"
)

var ErrClosed = errors.New("datastore closed")

type Datastore struct {
	DB        *badger.DB
	tableName TableName

	closeLk   sync.RWMutex
	closed    bool
	closeOnce sync.Once
	closing   chan struct{}

	gcDiscardRatio float64

	syncWrites bool
	ttl        time.Duration
}

// Implements the datastore.Batch interface, enabling batching support for
// the badger Datastore.
type batch struct {
	ds         *Datastore
	writeBatch *badger.WriteBatch
}

// Implements the datastore.Txn interface, enabling transaction support for
// the badger Datastore.
type txn struct {
	ds  *Datastore
	txn *badger.Txn

	// Whether this transaction has been implicitly created as a result of a direct Datastore
	// method invocation.
	implicit bool
}

// Options are the badger datastore options, reexported here for convenience.
type Options struct {
	// Please refer to the Badger docs to see what this is for
	GcDiscardRatio float64

	// TTL sets the expiration time for all newly added keys. After expiration,
	// the keys will no longer be retrievable and will be removed by garbage
	// collection.
	//
	// The default value is 0, which means no TTL.
	TTL time.Duration
}

// DefaultOptions are the default options for the badger datastore.
var DefaultOptions Options

// WithGcDiscardRatio returns a new Options value with GcDiscardRatio set to the given value.
//
// # Please refer to the Badger docs to see what this is for
//
// Default value is 0.2
func (opt Options) WithGcDiscardRatio(ratio float64) Options {
	opt.GcDiscardRatio = ratio
	return opt
}

// WithTTL returns a new Options value with TTL set to the given value.
//
// TTL sets the expiration time for all newly added keys. After expiration,
// the keys will no longer be retrievable and will be removed by garbage
// collection.
//
// Default value is 0, which means no TTL.
func (opt Options) WithTTL(ttl time.Duration) Options {
	opt.TTL = ttl
	return opt
}

func init() {
	DefaultOptions = Options{
		GcDiscardRatio: 0.2,
	}
}

var (
	_ ds.Datastore           = (*Datastore)(nil)
	_ ds.PersistentDatastore = (*Datastore)(nil)
	_ ds.TxnDatastore        = (*Datastore)(nil)
	_ ds.Txn                 = (*txn)(nil)
	_ ds.TTLDatastore        = (*Datastore)(nil)
	_ ds.GCDatastore         = (*Datastore)(nil)
	_ ds.Batching            = (*Datastore)(nil)
)

func NewDatastoreFromDB(kv DB, tableName TableName, options *Options) *Datastore {
	if kv == nil {
		return nil
	}
	db, ok := kv.(*badgerDB)
	if !ok {
		return nil // Possible only for tests
	}
	return NewDatastore(db.db, tableName, options)
}

// NewDatastore creates a new badger datastore.
func NewDatastore(kv *badger.DB, tableName TableName, options *Options) *Datastore {
	if options == nil {
		options = &DefaultOptions
	}

	return &Datastore{
		DB:             kv,
		tableName:      tableName,
		closing:        make(chan struct{}),
		gcDiscardRatio: options.GcDiscardRatio,
		syncWrites:     kv.Opts().SyncWrites,
		ttl:            options.TTL,
	}
}

// NewTransaction starts a new transaction. The resulting transaction object
// can be mutated without incurring changes to the underlying Datastore until
// the transaction is Committed.
func (d *Datastore) NewTransaction(ctx context.Context, readOnly bool) (ds.Txn, error) {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return nil, ErrClosed
	}

	return &txn{d, d.DB.NewTransaction(!readOnly), false}, nil
}

// newImplicitTransaction creates a transaction marked as 'implicit'.
// Implicit transactions are created by Datastore methods performing single operations.
func (d *Datastore) newImplicitTransaction(readOnly bool) *txn {
	return &txn{d, d.DB.NewTransaction(!readOnly), true}
}

func (d *Datastore) makeKey(key ds.Key) []byte {
	return MakeKey(d.tableName, key.Bytes())
}

func (d *Datastore) Put(ctx context.Context, key ds.Key, value []byte) error {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return ErrClosed
	}

	txn := d.newImplicitTransaction(false)
	defer txn.discard()

	if d.ttl > 0 {
		if err := txn.putWithTTL(key, value, d.ttl); err != nil {
			return err
		}
	} else {
		if err := txn.put(key, value); err != nil {
			return err
		}
	}

	return txn.commit()
}

func (d *Datastore) Sync(ctx context.Context, prefix ds.Key) error {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return ErrClosed
	}

	if d.syncWrites {
		return nil
	}

	return d.DB.Sync()
}

func (d *Datastore) PutWithTTL(ctx context.Context, key ds.Key, value []byte, ttl time.Duration) error {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return ErrClosed
	}

	txn := d.newImplicitTransaction(false)
	defer txn.discard()

	if err := txn.putWithTTL(key, value, ttl); err != nil {
		return err
	}

	return txn.commit()
}

func (d *Datastore) SetTTL(ctx context.Context, key ds.Key, ttl time.Duration) error {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return ErrClosed
	}

	txn := d.newImplicitTransaction(false)
	defer txn.discard()

	if err := txn.setTTL(key, ttl); err != nil {
		return err
	}

	return txn.commit()
}

func (d *Datastore) GetExpiration(ctx context.Context, key ds.Key) (time.Time, error) {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return time.Time{}, ErrClosed
	}

	txn := d.newImplicitTransaction(false)
	defer txn.discard()

	return txn.getExpiration(key)
}

func (d *Datastore) Get(ctx context.Context, key ds.Key) (value []byte, err error) {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return nil, ErrClosed
	}

	txn := d.newImplicitTransaction(true)
	defer txn.discard()

	return txn.get(key)
}

func (d *Datastore) Has(ctx context.Context, key ds.Key) (bool, error) {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return false, ErrClosed
	}

	txn := d.newImplicitTransaction(true)
	defer txn.discard()

	return txn.has(key)
}

func (d *Datastore) GetSize(ctx context.Context, key ds.Key) (size int, err error) {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return -1, ErrClosed
	}

	txn := d.newImplicitTransaction(true)
	defer txn.discard()

	return txn.getSize(key)
}

func (d *Datastore) Delete(ctx context.Context, key ds.Key) error {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()

	txn := d.newImplicitTransaction(false)
	defer txn.discard()

	err := txn.delete(key)
	if err != nil {
		return err
	}

	return txn.commit()
}

func (d *Datastore) Query(ctx context.Context, q dsq.Query) (dsq.Results, error) {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return nil, ErrClosed
	}

	txn := d.newImplicitTransaction(true)
	// We cannot defer txn.Discard() here, as the txn must remain active while the iterator is open.
	// https://github.com/hypermodeinc/badger/commit/b1ad1e93e483bbfef123793ceedc9a7e34b09f79
	// The closing logic in the query function takes care of discarding the implicit transaction.
	return txn.query(q)
}

// DiskUsage implements the PersistentDatastore interface.
// It returns the sum of lsm and value log files sizes in bytes.
func (d *Datastore) DiskUsage(ctx context.Context) (uint64, error) {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return 0, ErrClosed
	}
	lsm, vlog := d.DB.Size()

	// Print disk layout information on debug
	log.Debug().Msgf("\n\nDisk usage stats\nLSM: %d, ValueLog: %d\n", lsm, vlog)
	log.Debug().Msgf("\n%s\n\n", d.DB.LevelsToString())

	return uint64(lsm + vlog), nil
}

func (d *Datastore) Close() error {
	d.closeOnce.Do(func() {
		close(d.closing)
	})
	d.closeLk.Lock()
	defer d.closeLk.Unlock()
	if d.closed {
		return ErrClosed
	}
	d.closed = true
	return d.DB.Close()
}

// Batch creates a new Batch object. This provides a way to do many writes, when
// there may be too many to fit into a single transaction.
func (d *Datastore) Batch(ctx context.Context) (ds.Batch, error) {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return nil, ErrClosed
	}

	b := &batch{d, d.DB.NewWriteBatch()}
	// Ensure that incomplete transaction resources are cleaned up in case
	// batch is abandoned.
	runtime.SetFinalizer(b, func(b *batch) {
		b.cancel()
		log.Error().Msg("batch not committed or canceled")
	})

	return b, nil
}

func (d *Datastore) CollectGarbage(ctx context.Context) (err error) {
	// The idea is to keep calling DB.RunValueLogGC() till Badger no longer has any log files
	// to GC(which would be indicated by an error, please refer to Badger GC docs).
	for err == nil {
		err = d.gcOnce()
	}

	if errors.Is(err, badger.ErrNoRewrite) {
		err = nil
	}

	return err
}

func (d *Datastore) gcOnce() error {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return ErrClosed
	}
	return d.DB.RunValueLogGC(d.gcDiscardRatio)
}

var _ ds.Batch = (*batch)(nil)

func (b *batch) Put(ctx context.Context, key ds.Key, value []byte) error {
	b.ds.closeLk.RLock()
	defer b.ds.closeLk.RUnlock()
	if b.ds.closed {
		return ErrClosed
	}

	if b.ds.ttl > 0 {
		return b.putWithTTL(key, value, b.ds.ttl)
	}

	return b.put(key, value)
}

func (b *batch) put(key ds.Key, value []byte) error {
	return b.writeBatch.Set(b.ds.makeKey(key), value)
}

func (b *batch) putWithTTL(key ds.Key, value []byte, ttl time.Duration) error {
	return b.writeBatch.SetEntry(badger.NewEntry(b.ds.makeKey(key), value).WithTTL(ttl))
}

func (b *batch) Delete(ctx context.Context, key ds.Key) error {
	b.ds.closeLk.RLock()
	defer b.ds.closeLk.RUnlock()
	if b.ds.closed {
		return ErrClosed
	}

	return b.delete(key)
}

func (b *batch) delete(key ds.Key) error {
	return b.writeBatch.Delete(b.ds.makeKey(key))
}

func (b *batch) Commit(ctx context.Context) error {
	b.ds.closeLk.RLock()
	defer b.ds.closeLk.RUnlock()
	if b.ds.closed {
		return ErrClosed
	}

	return b.commit()
}

func (b *batch) commit() error {
	err := b.writeBatch.Flush()
	if err != nil {
		// Discard incomplete transaction held by b.writeBatch
		b.cancel()
		return err
	}
	runtime.SetFinalizer(b, nil)
	return nil
}

func (b *batch) Cancel() error {
	b.ds.closeLk.RLock()
	defer b.ds.closeLk.RUnlock()
	if b.ds.closed {
		return ErrClosed
	}

	b.cancel()
	return nil
}

func (b *batch) cancel() {
	b.writeBatch.Cancel()
	runtime.SetFinalizer(b, nil)
}

var (
	_ ds.Datastore    = (*txn)(nil)
	_ ds.TTLDatastore = (*txn)(nil)
)

func (t *txn) Put(ctx context.Context, key ds.Key, value []byte) error {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return ErrClosed
	}

	if t.ds.ttl > 0 {
		return t.putWithTTL(key, value, t.ds.ttl)
	}

	return t.put(key, value)
}

func (t *txn) put(key ds.Key, value []byte) error {
	return t.txn.Set(t.ds.makeKey(key), value)
}

func (t *txn) Sync(ctx context.Context, prefix ds.Key) error {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return ErrClosed
	}

	return nil
}

func (t *txn) PutWithTTL(ctx context.Context, key ds.Key, value []byte, ttl time.Duration) error {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return ErrClosed
	}
	return t.putWithTTL(key, value, ttl)
}

func (t *txn) putWithTTL(key ds.Key, value []byte, ttl time.Duration) error {
	return t.txn.SetEntry(badger.NewEntry(t.ds.makeKey(key), value).WithTTL(ttl))
}

func (t *txn) GetExpiration(ctx context.Context, key ds.Key) (time.Time, error) {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return time.Time{}, ErrClosed
	}

	return t.getExpiration(key)
}

func (t *txn) getExpiration(key ds.Key) (time.Time, error) {
	item, err := t.txn.Get(t.ds.makeKey(key))
	if errors.Is(err, badger.ErrKeyNotFound) {
		return time.Time{}, ds.ErrNotFound
	} else if err != nil {
		return time.Time{}, err
	}
	return time.Unix(int64(item.ExpiresAt()), 0), nil
}

func (t *txn) SetTTL(ctx context.Context, key ds.Key, ttl time.Duration) error {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return ErrClosed
	}

	return t.setTTL(key, ttl)
}

func (t *txn) setTTL(key ds.Key, ttl time.Duration) error {
	item, err := t.txn.Get(t.ds.makeKey(key))
	if err != nil {
		return err
	}
	return item.Value(func(data []byte) error {
		return t.putWithTTL(key, data, ttl)
	})
}

func (t *txn) Get(ctx context.Context, key ds.Key) ([]byte, error) {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return nil, ErrClosed
	}

	return t.get(key)
}

func (t *txn) get(key ds.Key) ([]byte, error) {
	item, err := t.txn.Get(t.ds.makeKey(key))
	if errors.Is(err, badger.ErrKeyNotFound) {
		err = ds.ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	return item.ValueCopy(nil)
}

func (t *txn) Has(ctx context.Context, key ds.Key) (bool, error) {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return false, ErrClosed
	}

	return t.has(key)
}

func (t *txn) has(key ds.Key) (bool, error) {
	_, err := t.txn.Get(t.ds.makeKey(key))
	switch {
	case errors.Is(err, badger.ErrKeyNotFound):
		return false, nil
	case err == nil:
		return true, nil
	default:
		return false, err
	}
}

func (t *txn) GetSize(ctx context.Context, key ds.Key) (int, error) {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return -1, ErrClosed
	}

	return t.getSize(key)
}

func (t *txn) getSize(key ds.Key) (int, error) {
	item, err := t.txn.Get(t.ds.makeKey(key))
	switch {
	case err == nil:
		return int(item.ValueSize()), nil
	case errors.Is(err, badger.ErrKeyNotFound):
		return -1, ds.ErrNotFound
	default:
		return -1, err
	}
}

func (t *txn) Delete(ctx context.Context, key ds.Key) error {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return ErrClosed
	}

	return t.delete(key)
}

func (t *txn) delete(key ds.Key) error {
	return t.txn.Delete(t.ds.makeKey(key))
}

func (t *txn) Query(ctx context.Context, q dsq.Query) (dsq.Results, error) {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return nil, ErrClosed
	}

	return t.query(q)
}

func (t *txn) query(q dsq.Query) (dsq.Results, error) {
	opt := badger.DefaultIteratorOptions
	opt.PrefetchValues = !q.KeysOnly

	prefix := ds.NewKey(q.Prefix).String()
	if prefix != "/" {
		opt.Prefix = []byte(prefix + "/")
	}

	tablePrefix := MakeTablePrefix(t.ds.tableName)
	opt.Prefix = MakeKey(t.ds.tableName, opt.Prefix)

	// Handle ordering
	if len(q.Orders) > 0 {
		switch q.Orders[0].(type) {
		case dsq.OrderByKey, *dsq.OrderByKey:
		// We order by key by default.
		case dsq.OrderByKeyDescending, *dsq.OrderByKeyDescending:
			// Reverse order by key
			opt.Reverse = true
		default:
			// Ok, we have a weird order we can't handle. Let's
			// perform the _base_ query (prefix, filter, etc.), then
			// handle sort/offset/limit later.

			// Skip the stuff we can't apply.
			baseQuery := q
			baseQuery.Limit = 0
			baseQuery.Offset = 0
			baseQuery.Orders = nil

			// perform the base query.
			res, err := t.query(baseQuery)
			if err != nil {
				return nil, err
			}

			// fix the query
			res = dsq.ResultsReplaceQuery(res, q)

			// Remove the parts we've already applied.
			naiveQuery := q
			naiveQuery.Prefix = ""
			naiveQuery.Filters = nil

			// Apply the rest of the query
			return dsq.NaiveQueryApply(naiveQuery, res), nil
		}
	}

	it := t.txn.NewIterator(opt)
	results := dsq.ResultsWithContext(q, func(ctx context.Context, output chan<- dsq.Result) {
		t.ds.closeLk.RLock()
		closedEarly := false
		defer func() {
			t.ds.closeLk.RUnlock()
			if closedEarly {
				select {
				case output <- dsq.Result{
					Error: ErrClosed,
				}:
				case <-ctx.Done():
				}
			}
		}()
		if t.ds.closed {
			closedEarly = true
			return
		}

		// this iterator is part of an implicit transaction, so when
		// we're done we must discard the transaction. It's safe to
		// discard the txn it because it contains the iterator only.
		if t.implicit {
			defer t.discard()
		}

		defer it.Close()

		// All iterators must be started by rewinding.
		if opt.Reverse {
			// See for details https://github.com/hypermodeinc/badger/issues/347
			it.Seek(append(bytes.Clone(opt.Prefix), 0xff))
		} else {
			it.Rewind()
		}

		// skip to the offset
		for skipped := 0; skipped < q.Offset && it.Valid(); it.Next() {
			// On the happy path, we have no filters and we can go
			// on our way.
			if len(q.Filters) == 0 {
				skipped++
				continue
			}

			// On the sad path, we need to apply filters before
			// counting the item as "skipped" as the offset comes
			// _after_ the filter.
			item := it.Item()

			matches := true
			check := func(value []byte) error {
				key := strings.TrimPrefix(string(item.Key()), tablePrefix)
				e := dsq.Entry{
					Key:   key,
					Value: value,
					Size:  int(item.ValueSize()), // this function is basically free
				}

				// Only calculate expirations if we need them.
				if q.ReturnExpirations {
					e.Expiration = expires(item)
				}
				matches = filter(q.Filters, e)
				return nil
			}

			// Maybe check with the value, only if we need it.
			var err error
			if q.KeysOnly {
				err = check(nil)
			} else {
				err = item.Value(check)
			}

			if err != nil {
				select {
				case output <- dsq.Result{Error: err}:
				case <-t.ds.closing: // datastore closing.
					closedEarly = true
					return
				case <-ctx.Done(): // client told us to close early
					return
				}
			}
			if !matches {
				skipped++
			}
		}

		for sent := 0; (q.Limit <= 0 || sent < q.Limit) && it.Valid(); it.Next() {
			item := it.Item()
			key := strings.TrimPrefix(string(item.Key()), tablePrefix)
			e := dsq.Entry{Key: key}

			// Maybe get the value
			var result dsq.Result
			if !q.KeysOnly {
				b, err := item.ValueCopy(nil)
				if err != nil {
					result = dsq.Result{Error: err}
				} else {
					e.Value = b
					e.Size = len(b)
					result = dsq.Result{Entry: e}
				}
			} else {
				e.Size = int(item.ValueSize())
				result = dsq.Result{Entry: e}
			}

			if q.ReturnExpirations {
				result.Expiration = expires(item)
			}

			// Finally, filter it (unless we're dealing with an error).
			if result.Error == nil && filter(q.Filters, e) {
				continue
			}

			select {
			case output <- result:
				sent++
			case <-t.ds.closing: // datastore closing.
				closedEarly = true
				return
			case <-ctx.Done(): // client told us to close early
				return
			}
		}
	})

	return results, nil
}

func (t *txn) Commit(ctx context.Context) error {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return ErrClosed
	}

	return t.commit()
}

func (t *txn) commit() error {
	return t.txn.Commit()
}

// Alias to commit
func (t *txn) Close() error {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return ErrClosed
	}
	return t.close()
}

func (t *txn) close() error {
	return t.txn.Commit()
}

func (t *txn) Discard(ctx context.Context) {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return
	}

	t.discard()
}

func (t *txn) discard() {
	t.txn.Discard()
}

// filter returns _true_ if we should filter (skip) the entry
func filter(filters []dsq.Filter, entry dsq.Entry) bool {
	for _, f := range filters {
		if !f.Filter(entry) {
			return true
		}
	}
	return false
}

func expires(item *badger.Item) time.Time {
	return time.Unix(int64(item.ExpiresAt()), 0)
}
