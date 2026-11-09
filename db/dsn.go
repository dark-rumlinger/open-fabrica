package db

import (
	"database/sql"
	"fmt"
)

// openReadWritePools opens the read pool (concurrent reads via WAL) and the write
// pool (a single serialized connection) for a SQLite file, pinging each. On any
// failure it closes whatever it already opened. Shared by the global and
// per-project openers.
func openReadWritePools(path string) (read, write *sql.DB, err error) {
	readDSN, writeDSN := buildDSN(path)

	readDB, err := sql.Open("sqlite", readDSN)
	if err != nil {
		return nil, nil, fmt.Errorf("opening database (read): %w", err)
	}
	readDB.SetMaxOpenConns(4)
	readDB.SetMaxIdleConns(4) // match MaxOpenConns so read conns stay warm instead of churning
	if err := readDB.Ping(); err != nil {
		readDB.Close()
		return nil, nil, fmt.Errorf("pinging database (read): %w", err)
	}

	// Write pool: MaxOpenConns=1 serializes all writes at the DB level, so no
	// Go-level mutex is needed (sql.DB queues callers).
	writeDB, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		readDB.Close()
		return nil, nil, fmt.Errorf("opening database (write): %w", err)
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)
	if err := writeDB.Ping(); err != nil {
		readDB.Close()
		writeDB.Close()
		return nil, nil, fmt.Errorf("pinging database (write): %w", err)
	}
	return readDB, writeDB, nil
}

// buildDSN returns the read and write DSNs for a SQLite file opened with
// modernc.org/sqlite (which uses _pragma=name(value), not mattn's _name=value).
//
// Beyond the baseline WAL + busy_timeout + foreign_keys, it adds the WAL
// companion pragmas:
//   - synchronous(NORMAL): safe under WAL (loses only the last commits on OS
//     crash, never corrupts) and the biggest write win, fsync at checkpoint.
//   - temp_store(memory): keep ORDER BY / GROUP BY spill B-trees in RAM.
//   - cache_size(-8000): ~8 MB page cache per connection, kept modest because
//     this process can hold many project DBs (read pool + writer) at once.
//   - mmap_size(128 MB): memory-mapped reads (virtual space, cheap on 64-bit).
//
// The writer additionally uses _txlock=immediate so BEGIN takes the write lock up
// front instead of at the first write, preventing SQLITE_BUSY mid-transaction.
func buildDSN(path string) (readDSN, writeDSN string) {
	base := path + "?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(30000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=temp_store(memory)" +
		"&_pragma=cache_size(-8000)" +
		"&_pragma=mmap_size(134217728)"
	return base, base + "&_txlock=immediate"
}
