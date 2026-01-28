package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

type Device struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type DNSLog struct {
	ID        string          `json:"-"` // Generated, not from API
	Timestamp time.Time       `json:"timestamp"`
	Domain    string          `json:"domain"` // Changed from "name"
	Type      string          `json:"type"`
	Status    string          `json:"status"`
	Blocked   bool            `json:"blocked"`
	ClientIP  string          `json:"clientIp"`
	Protocol  string          `json:"protocol"`
	Device    json.RawMessage `json:"device"`
	Root      string          `json:"root"`
	Tracker   string          `json:"tracker"`
	Encrypted bool            `json:"encrypted"`
}

// GenerateID creates a unique ID from timestamp, domain, and client IP
func (log *DNSLog) GenerateID() {
	// Create a deterministic ID from key fields
	log.ID = fmt.Sprintf("%d-%s-%s", log.Timestamp.UnixNano(), log.Domain, log.ClientIP)
}

func (log *DNSLog) DeviceName() string {
	if len(log.Device) == 0 {
		return ""
	}
	// Try to parse as object
	var device Device
	if err := json.Unmarshal(log.Device, &device); err == nil {
		return device.Name
	}
	// Try as string
	var deviceStr string
	if err := json.Unmarshal(log.Device, &deviceStr); err == nil {
		return deviceStr
	}
	return ""
}

type Config struct {
	ProfileID string
	APIKey    string
	DBURL     string
}

func main() {
	var (
		profileID = flag.String("profile", os.Getenv("NEXTDNS_PROFILE_ID"), "NextDNS Profile ID")
		apiKey    = flag.String("apikey", os.Getenv("NEXTDNS_API_KEY"), "NextDNS API Key")
		dbURL     = flag.String("db", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")
	)
	flag.Parse()

	if *profileID == "" || *apiKey == "" || *dbURL == "" {
		log.Fatal("Missing required configuration. Set profile, apikey, and db flags or environment variables.")
	}

	config := &Config{
		ProfileID: *profileID,
		APIKey:    *apiKey,
		DBURL:     *dbURL,
	}

	db, err := sql.Open("postgres", config.DBURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	if err := initDB(db); err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	log.Println("Starting NextDNS log sync...")
	if err := syncLogs(config, db); err != nil {
		log.Fatalf("Sync failed: %v", err)
	}
}

func initDB(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS dns_logs (
		id VARCHAR(255) PRIMARY KEY,
		timestamp TIMESTAMP NOT NULL,
		domain TEXT NOT NULL,
		type VARCHAR(50),
		status VARCHAR(50),
		blocked BOOLEAN,
		client_ip VARCHAR(50),
		protocol VARCHAR(50),
		device JSONB,
		root TEXT,
		tracker TEXT,
		encrypted BOOLEAN,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_timestamp ON dns_logs(timestamp);
	CREATE INDEX IF NOT EXISTS idx_domain ON dns_logs(domain);
	CREATE INDEX IF NOT EXISTS idx_root ON dns_logs(root);
	
	CREATE TABLE IF NOT EXISTS sync_state (
		key VARCHAR(50) PRIMARY KEY,
		value TEXT,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	`
	_, err := db.Exec(schema)
	return err
}

func syncLogs(config *Config, db *sql.DB) error {
	ctx := context.Background()

	// Try streaming first
	streamID, err := getStreamCursor(db)
	if err != nil {
		log.Printf("Warning: could not get stream ID: %v", err)
	}

	if streamID != "" {
		log.Printf("Attempting to use stream API with stream ID: %s", streamID)

		// Keep trying to stream with exponential backoff
		retryDelay := 1 * time.Second
		maxRetryDelay := 60 * time.Second

		for {
			err := streamLogs(ctx, config, db, streamID)
			if err != nil {
				log.Printf("Stream disconnected: %v", err)
				log.Printf("Reconnecting in %v...", retryDelay)
				time.Sleep(retryDelay)

				// Exponential backoff
				retryDelay *= 2
				if retryDelay > maxRetryDelay {
					retryDelay = maxRetryDelay
				}

				// Get the latest stream ID before reconnecting
				streamID, err = getStreamCursor(db)
				if err != nil {
					log.Printf("Warning: could not get stream ID: %v", err)
				}

				continue
			}

			// If stream ended cleanly, exit
			return nil
		}
	} else {
		log.Println("No stream ID available, using pagination")
	}

	// Fallback to cursor-based pagination
	return paginateLogs(ctx, config, db)
}

func streamLogs(ctx context.Context, config *Config, db *sql.DB, streamID string) error {
	url := fmt.Sprintf("https://api.nextdns.io/profiles/%s/logs/stream?id=%s", config.ProfileID, streamID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", config.APIKey)

	client := &http.Client{Timeout: 0} // No timeout for streaming
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream API returned status %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer

	log.Println("Connected to stream API, receiving logs...")
	count := 0
	currentStreamID := streamID
	var lastEventID string
	lineCount := 0
	lastActivity := time.Now()

	// Start a heartbeat goroutine to show we're still alive
	done := make(chan bool)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				elapsed := time.Since(lastActivity)
				log.Printf("Stream alive: %d lines, %d logs processed (last activity: %v ago)", lineCount, count, elapsed.Round(time.Second))
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	for scanner.Scan() {
		line := scanner.Text()
		lineCount++
		lastActivity = time.Now()

		if line == "" {
			continue
		}

		// Parse SSE format: "id: ..." or "data: ..."
		if strings.HasPrefix(line, "id: ") {
			lastEventID = strings.TrimPrefix(line, "id: ")
			log.Printf("Received event ID: %s", lastEventID)
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			dataJSON := strings.TrimPrefix(line, "data: ")

			var logEntry DNSLog
			if err := json.Unmarshal([]byte(dataJSON), &logEntry); err != nil {
				log.Printf("Failed to parse log entry: %v", err)
				continue
			}

			logEntry.GenerateID() // Generate ID for streamed entry

			if err := insertLog(db, &logEntry); err != nil {
				if err == sql.ErrNoRows {
					log.Printf("Duplicate log entry (domain: %s)", logEntry.Domain)
				} else {
					log.Printf("Failed to insert log: %v", err)
				}
				continue
			}

			count++
			log.Printf("✓ Inserted log #%d: %s -> %s", count, logEntry.Domain, logEntry.Status)

			// Update stream ID with the last event ID we received
			if lastEventID != "" {
				currentStreamID = lastEventID
				if count%100 == 0 {
					if err := updateStreamCursor(db, currentStreamID); err != nil {
						log.Printf("Warning: failed to update stream ID: %v", err)
					} else {
						log.Printf("Updated stream cursor to: %s", currentStreamID)
					}
				}
			}
		}
	}

	// Save the last event ID before exiting
	if lastEventID != "" {
		log.Printf("Saving final stream cursor: %s", lastEventID)
		if err := updateStreamCursor(db, lastEventID); err != nil {
			log.Printf("Warning: failed to update stream ID: %v", err)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("stream error: %v", err)
	}

	log.Printf("Stream ended cleanly after processing %d logs", count)
	return nil
}

func paginateLogs(ctx context.Context, config *Config, db *sql.DB) error {
	cursor := ""
	count := 0
	totalInserted := 0

	for {
		url := fmt.Sprintf("https://api.nextdns.io/profiles/%s/logs?limit=1000", config.ProfileID)
		if cursor != "" {
			url += fmt.Sprintf("&cursor=%s", cursor)
		}

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("X-Api-Key", config.APIKey)

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("API returned status %d", resp.StatusCode)
		}

		var result struct {
			Data []DNSLog `json:"data"`
			Meta struct {
				Pagination struct {
					Cursor string `json:"cursor"`
				} `json:"pagination"`
				Stream struct {
					ID string `json:"id"`
				} `json:"stream"`
			} `json:"meta"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return fmt.Errorf("failed to parse response: %v", err)
		}
		resp.Body.Close()

		if len(result.Data) == 0 {
			break
		}

		inserted := 0
		duplicates := 0
		for _, logEntry := range result.Data {
			if err := insertLog(db, &logEntry); err != nil {
				if err == sql.ErrNoRows {
					// This was a duplicate
					duplicates++
					continue
				}
				log.Printf("Error inserting log %s: %v", logEntry.ID, err)
				continue
			}
			inserted++
		}

		totalInserted += inserted
		count += len(result.Data)
		log.Printf("Fetched %d logs (%d new, %d duplicates), total new: %d",
			len(result.Data), inserted, duplicates, totalInserted)

		// If we're seeing all duplicates, we've caught up
		if inserted == 0 && duplicates == len(result.Data) {
			log.Println("All logs are duplicates, caught up with existing data")
			break
		}

		// Save the stream ID from the first response for future streaming
		if cursor == "" && result.Meta.Stream.ID != "" {
			log.Printf("Saving stream ID for future use: %s", result.Meta.Stream.ID)
			if err := updateStreamCursor(db, result.Meta.Stream.ID); err != nil {
				log.Printf("Warning: failed to update stream cursor: %v", err)
			}
		}

		cursor = result.Meta.Pagination.Cursor
		if cursor == "" {
			break
		}

		// Small delay to avoid rate limiting
		time.Sleep(100 * time.Millisecond)
	}

	log.Printf("Pagination complete. Total new logs inserted: %d", totalInserted)
	return nil
}

func insertLog(db *sql.DB, log *DNSLog) error {
	// Generate ID if not already set
	if log.ID == "" {
		log.GenerateID()
	}

	query := `
		INSERT INTO dns_logs (id, timestamp, domain, type, status, blocked, client_ip, protocol, device, root, tracker, encrypted)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (id) DO NOTHING
	`
	deviceJSON := string(log.Device)
	if deviceJSON == "" {
		deviceJSON = "null"
	}
	result, err := db.Exec(query, log.ID, log.Timestamp, log.Domain, log.Type, log.Status,
		log.Blocked, log.ClientIP, log.Protocol, deviceJSON, log.Root, log.Tracker, log.Encrypted)
	if err != nil {
		return err
	}

	// Check if row was actually inserted (not a duplicate)
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if rows == 0 {
		return sql.ErrNoRows // Signal this was a duplicate
	}

	return nil
}

func getStreamCursor(db *sql.DB) (string, error) {
	var cursor string
	err := db.QueryRow("SELECT value FROM sync_state WHERE key = 'stream_id'").Scan(&cursor)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return cursor, err
}

func updateStreamCursor(db *sql.DB, cursor string) error {
	query := `
		INSERT INTO sync_state (key, value, updated_at)
		VALUES ('stream_id', $1, CURRENT_TIMESTAMP)
		ON CONFLICT (key) DO UPDATE SET value = $1, updated_at = CURRENT_TIMESTAMP
	`
	_, err := db.Exec(query, cursor)
	return err
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
