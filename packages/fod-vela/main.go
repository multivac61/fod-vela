package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/nix-community/go-nix/pkg/derivation"
)

// FOD represents a fixed-output derivation
type FOD struct {
	DrvPath       string
	OutputPath    string
	HashAlgorithm string
	Hash          string
	RevisionID    int64 // New field to link to revision
}

// Revision represents a nixpkgs revision
type Revision struct {
	ID        int64
	Rev       string
	Timestamp time.Time
}

// initDB initializes the SQLite database
func initDB() *sql.DB {
	// Create the directory if it doesn't exist
	dbDir := "./db"
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		log.Fatalf("Failed to create database directory: %v", err)
	}

	dbPath := filepath.Join(dbDir, "fods.db")
	log.Printf("Using database at: %s", dbPath)

	// Open database with optimized settings
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_synchronous=NORMAL&_cache_size=100000&_temp_store=MEMORY")
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}

	// Set connection pool settings
	db.SetMaxOpenConns(runtime.NumCPU() * 2)
	db.SetMaxIdleConns(runtime.NumCPU())
	db.SetConnMaxLifetime(time.Hour)

	// Create revisions table first
	createRevisionsTable := `
	CREATE TABLE IF NOT EXISTS revisions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		rev TEXT NOT NULL UNIQUE,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_rev ON revisions(rev);
	`

	_, err = db.Exec(createRevisionsTable)
	if err != nil {
		log.Fatalf("Failed to create revisions table: %v", err)
	}

	// Create FODs table with revision_id foreign key
	createFodsTable := `
	CREATE TABLE IF NOT EXISTS fods (
		drv_path TEXT NOT NULL,
		output_path TEXT NOT NULL,
		hash_algorithm TEXT NOT NULL,
		hash TEXT NOT NULL,
		revision_id INTEGER NOT NULL,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (drv_path, revision_id),
		FOREIGN KEY (revision_id) REFERENCES revisions(id)
	);
	CREATE INDEX IF NOT EXISTS idx_hash ON fods(hash);
	CREATE INDEX IF NOT EXISTS idx_hash_algo ON fods(hash_algorithm);
	CREATE INDEX IF NOT EXISTS idx_revision_id ON fods(revision_id);
	`

	_, err = db.Exec(createFodsTable)
	if err != nil {
		log.Fatalf("Failed to create fods table: %v", err)
	}

	// Set pragmas for better performance
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA cache_size=100000",
		"PRAGMA temp_store=MEMORY",
		"PRAGMA mmap_size=30000000000",
		"PRAGMA page_size=32768",
		"PRAGMA foreign_keys=ON", // Enable foreign key constraints
	}

	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			log.Printf("Warning: Failed to set pragma %s: %v", pragma, err)
		}
	}

	return db
}

// getOrCreateRevision gets or creates a revision in the database
func getOrCreateRevision(db *sql.DB, rev string) (int64, error) {
	var id int64

	// Check if revision already exists
	err := db.QueryRow("SELECT id FROM revisions WHERE rev = ?", rev).Scan(&id)
	if err == nil {
		// Revision already exists
		return id, nil
	} else if err != sql.ErrNoRows {
		// Unexpected error
		return 0, fmt.Errorf("error checking for existing revision: %w", err)
	}

	// Revision doesn't exist, create it
	result, err := db.Exec("INSERT INTO revisions (rev) VALUES (?)", rev)
	if err != nil {
		return 0, fmt.Errorf("failed to insert revision: %w", err)
	}

	id, err = result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get last insert ID: %w", err)
	}

	return id, nil
}

// DBBatcher handles batched database operations
type DBBatcher struct {
	db           *sql.DB
	batch        []FOD
	batchSize    int
	mutex        sync.Mutex
	commitTicker *time.Ticker
	wg           sync.WaitGroup
	done         chan struct{}
	stmt         *sql.Stmt
	revisionID   int64 // Store the current revision ID
	stats        struct {
		drvs int
		fods int
		sync.Mutex
	}
}

// NewDBBatcher creates a new database batcher
func NewDBBatcher(db *sql.DB, batchSize int, commitInterval time.Duration, revisionID int64) (*DBBatcher, error) {
	// Prepare statement once
	stmt, err := db.Prepare(`
		INSERT INTO fods (drv_path, output_path, hash_algorithm, hash, revision_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(drv_path, revision_id) DO UPDATE SET 
		output_path = ?,
		hash_algorithm = ?,
		hash = ?
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare statement: %w", err)
	}

	batcher := &DBBatcher{
		db:           db,
		batch:        make([]FOD, 0, batchSize),
		batchSize:    batchSize,
		commitTicker: time.NewTicker(commitInterval),
		done:         make(chan struct{}),
		stmt:         stmt,
		revisionID:   revisionID,
	}

	batcher.wg.Add(1)
	go batcher.periodicCommit()

	return batcher, nil
}

// periodicCommit commits batches periodically
func (b *DBBatcher) periodicCommit() {
	defer b.wg.Done()

	for {
		select {
		case <-b.commitTicker.C:
			b.Flush()
			b.logStats()
		case <-b.done:
			b.Flush()
			b.logStats()
			return
		}
	}
}

// logStats logs the current statistics
func (b *DBBatcher) logStats() {
	b.stats.Lock()
	defer b.stats.Unlock()
	log.Printf("Stats: processed %d derivations, found %d FODs", b.stats.drvs, b.stats.fods)
}

// AddFOD adds a FOD to the batch
func (b *DBBatcher) AddFOD(fod FOD) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	// Set the revision ID for this FOD
	fod.RevisionID = b.revisionID

	b.batch = append(b.batch, fod)

	b.stats.Lock()
	b.stats.fods++
	b.stats.Unlock()

	if len(b.batch) >= b.batchSize {
		b.commitBatch()
	}
}

// IncrementDrvCount increments the derivation count (for stats only)
func (b *DBBatcher) IncrementDrvCount() {
	b.stats.Lock()
	b.stats.drvs++
	b.stats.Unlock()
}

// commitBatch commits the current batch
func (b *DBBatcher) commitBatch() {
	if len(b.batch) == 0 {
		return
	}

	tx, err := b.db.Begin()
	if err != nil {
		log.Printf("Failed to begin transaction: %v", err)
		return
	}

	stmt := tx.Stmt(b.stmt)

	for _, fod := range b.batch {
		_, err := stmt.Exec(
			fod.DrvPath, fod.OutputPath, fod.HashAlgorithm, fod.Hash, fod.RevisionID,
			fod.OutputPath, fod.HashAlgorithm, fod.Hash,
		)
		if err != nil {
			log.Printf("Failed to insert FOD %s: %v", fod.DrvPath, err)
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Failed to commit transaction: %v", err)
		tx.Rollback()
		return
	}

	b.batch = b.batch[:0]
}

// Flush commits all pending batches
func (b *DBBatcher) Flush() {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	b.commitBatch()
}

// Close closes the batcher
func (b *DBBatcher) Close() error {
	close(b.done)
	b.commitTicker.Stop()
	b.wg.Wait()

	if err := b.stmt.Close(); err != nil {
		return err
	}

	return nil
}

// processDerivation processes a derivation
func processDerivation(inputFile string, batcher *DBBatcher, visited *sync.Map, workQueue chan<- string) {
	// Increment derivation count for statistics
	batcher.IncrementDrvCount()

	file, err := os.Open(inputFile)
	if err != nil {
		log.Printf("Error opening file %s: %v", inputFile, err)
		return
	}
	defer file.Close()

	drv, err := derivation.ReadDerivation(file)
	if err != nil {
		log.Printf("Error reading derivation %s: %v", inputFile, err)
		return
	}

	// Find output hash and store FODs in the database
	for name, out := range drv.Outputs {
		if out.HashAlgorithm != "" {
			// Now we know it's a FOD, store it in the database
			fod := FOD{
				DrvPath:       inputFile,
				OutputPath:    out.Path,
				HashAlgorithm: out.HashAlgorithm,
				Hash:          out.Hash,
				// RevisionID is set by the batcher
			}
			batcher.AddFOD(fod)

			// If we're in verbose mode, log the FOD
			if os.Getenv("VERBOSE") == "1" {
				log.Printf("Found FOD: %s (output: %s, hash: %s)",
					filepath.Base(inputFile), name, out.Hash)
			}

			// Since FODs typically have only one output, we can break after finding one
			break
		}
	}

	// Process input derivations
	for path := range drv.InputDerivations {
		// Only process if not already visited
		if _, alreadyVisited := visited.LoadOrStore(path, true); !alreadyVisited {
			select {
			case workQueue <- path:
				// Successfully added to queue
			default:
				// Queue is full, process it directly to avoid deadlock
				go processDerivation(path, batcher, visited, workQueue)
			}
		}
	}
}

// callNixEvalJobs calls nix-eval-jobs and sends derivation paths to the worker pool
func callNixEvalJobs(rev string, workQueue chan<- string, visited *sync.Map) error {
	expr := fmt.Sprintf("import (builtins.fetchTarball \"https://github.com/NixOS/nixpkgs/archive/%s.tar.gz\") { allowAliases = false; }", rev)
	cmd := exec.Command("nix-eval-jobs", "--expr", expr, "--workers", "8") // Customize arguments as needed
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("failed to start nix-eval-jobs: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	// Increase scanner buffer size for large outputs
	const maxScannerSize = 10 * 1024 * 1024 // 10MB
	buf := make([]byte, maxScannerSize)
	scanner.Buffer(buf, maxScannerSize)

	for scanner.Scan() {
		line := scanner.Text()
		var result struct {
			DrvPath string `json:"drvPath"`
		}
		if err := json.Unmarshal([]byte(line), &result); err != nil {
			log.Printf("Failed to parse JSON: %v", err)
			continue
		}
		if result.DrvPath != "" {
			if _, alreadyVisited := visited.LoadOrStore(result.DrvPath, true); !alreadyVisited {
				workQueue <- result.DrvPath
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading stdout: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("nix-eval-jobs command failed: %w", err)
	}

	return nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("Starting FOD finder...")

	// Initialize database
	db := initDB()
	defer db.Close()

	// Get the revision from command line arguments
	if len(os.Args) < 2 {
		log.Fatalf("Usage: %s <nixpkgs-revision>", os.Args[0])
	}
	rev := os.Args[1]

	// Get or create the revision in the database
	revisionID, err := getOrCreateRevision(db, rev)
	if err != nil {
		log.Fatalf("Failed to get or create revision: %v", err)
	}
	log.Printf("Using nixpkgs revision: %s (ID: %d)", rev, revisionID)

	// Create batcher for efficient database operations
	batcher, err := NewDBBatcher(db, 5000, 3*time.Second, revisionID)
	if err != nil {
		log.Fatalf("Failed to create database batcher: %v", err)
	}
	defer batcher.Close()

	// Start the process
	startTime := time.Now()
	log.Println("Starting to find all FODs...")

	// Create a shared visited map and work queue
	visited := &sync.Map{}
	workQueue := make(chan string, 100000) // Large buffer to avoid blocking

	// Start worker goroutines
	numWorkers := runtime.NumCPU() * 2
	log.Printf("Starting %d worker goroutines", numWorkers)

	var wg sync.WaitGroup
	for range numWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for drvPath := range workQueue {
				processDerivation(drvPath, batcher, visited, workQueue)
			}
		}()
	}

	// Call nix-eval-jobs to populate the work queue
	go func() {
		if err := callNixEvalJobs(rev, workQueue, visited); err != nil {
			log.Printf("Error calling nix-eval-jobs: %v", err)
		}

		// Wait a bit to ensure all jobs are processed
		time.Sleep(5 * time.Second)

		// Check if there are still jobs being processed
		for {
			time.Sleep(1 * time.Second)

			// Get current stats
			currentDrvs := batcher.stats.drvs
			time.Sleep(2 * time.Second)
			newDrvs := batcher.stats.drvs

			// If no new derivations were processed in 2 seconds, we're done
			if newDrvs == currentDrvs {
				close(workQueue)
				break
			}
		}
	}()

	// Wait for all workers to finish
	wg.Wait()

	// Ensure all data is written
	batcher.Flush()

	elapsed := time.Since(startTime)
	log.Printf("Process completed in %v", elapsed)

	// Print final statistics
	var fodCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM fods WHERE revision_id = ?", revisionID).Scan(&fodCount); err != nil {
		log.Printf("Error counting FODs: %v", err)
	}

	log.Printf("Final database stats for revision %s: %d FODs", rev, fodCount)
	log.Printf("Average processing rate: %.2f derivations/second",
		float64(batcher.stats.drvs)/elapsed.Seconds())

	// Print some useful queries for analysis
	log.Println("Useful queries for analysis:")
	log.Println("- Count FODs by hash algorithm for this revision: SELECT hash_algorithm, COUNT(*) FROM fods WHERE revision_id = ? GROUP BY hash_algorithm ORDER BY COUNT(*) DESC;")
	log.Println("- Find most common hashes for this revision: SELECT hash, COUNT(*) FROM fods WHERE revision_id = ? GROUP BY hash HAVING COUNT(*) > 1 ORDER BY COUNT(*) DESC LIMIT 20;")
	log.Println("- Compare FODs across revisions: SELECT r1.rev, r2.rev, COUNT(*) FROM fods f1 JOIN fods f2 ON f1.hash = f2.hash JOIN revisions r1 ON f1.revision_id = r1.id JOIN revisions r2 ON f2.revision_id = r2.id WHERE r1.id < r2.id GROUP BY r1.id, r2.id;")
}
