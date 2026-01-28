# NextDNS Log Sync

A small Go application that continuously syncs DNS query logs from **NextDNS** into a local **PostgreSQL** database.

It supports:

* **Initial backfill using cursor-based pagination**
* **Real-time streaming using NextDNS Server-Sent Events (SSE)**
* **Automatic resume** using a persisted stream cursor
* **Deduplication** via deterministic log IDs

This makes it suitable for long-running deployments where you want a durable, queryable history of NextDNS activity.

---

## Features

* 📥 Fetch historical logs using the NextDNS REST API (pagination)
* 🔄 Switches automatically to the streaming API once caught up
* 💾 Persists sync state in Postgres (`sync_state` table)
* ♻️ Safe to restart — resumes where it left off
* 🚫 Duplicate-safe inserts using `ON CONFLICT DO NOTHING`
* 📊 Indexed schema for fast queries by time, domain, and root

---

## How It Works

1. **Startup**

   * Connects to PostgreSQL
   * Ensures required tables and indexes exist

2. **Sync strategy**

   * If a saved stream cursor exists, attempts **SSE streaming** immediately
   * If not (or if streaming fails), falls back to **pagination**

3. **Pagination mode**

   * Fetches logs in batches (`limit=1000`)
   * Stops when all entries are duplicates
   * Saves the stream ID returned by NextDNS for future streaming

4. **Streaming mode (SSE)**

   * Maintains a persistent connection to NextDNS
   * Handles reconnects with exponential backoff
   * Periodically saves the latest stream cursor

---

## Requirements

* Go **1.25+** (recommended)
* PostgreSQL **16+**
* A NextDNS account with:

  * Profile ID
  * API key

---

## Installation

Clone the repository and build:

```bash
go build -o nextdns-log-sync
```

---

## Configuration

Configuration can be provided via **flags** or **environment variables**.

### Command-line flags

```bash
./nextdns-log-sync \
  -profile <NEXTDNS_PROFILE_ID> \
  -apikey <NEXTDNS_API_KEY> \
  -db <DATABASE_URL>
```

### Environment variables

```bash
export NEXTDNS_PROFILE_ID=xxxxxx
export NEXTDNS_API_KEY=xxxxxxxxxxxxxxxx
export DATABASE_URL=postgres://user:pass@localhost:5432/dbname?sslmode=disable

./nextdns-log-sync
```

---

## Database Schema

### `dns_logs`

Stores individual DNS queries.

| Column     | Type      | Description                           |
| ---------- | --------- | ------------------------------------- |
| id         | varchar   | Deterministic unique ID (primary key) |
| timestamp  | timestamp | Query timestamp                       |
| domain     | text      | Queried domain                        |
| type       | varchar   | DNS record type                       |
| status     | varchar   | Resolution status                     |
| blocked    | boolean   | Whether the query was blocked         |
| client_ip  | varchar   | Client IP address                     |
| protocol   | varchar   | DNS protocol used                     |
| device     | jsonb     | Device metadata from NextDNS          |
| root       | text      | Root domain                           |
| tracker    | text      | Tracker classification                |
| encrypted  | boolean   | Encrypted DNS flag                    |
| created_at | timestamp | Insert time                           |

Indexes:

* `timestamp`
* `domain`
* `root`

---

### `sync_state`

Persists synchronization metadata.

| Key         | Value                       |
| ----------- | --------------------------- |
| `stream_id` | Last processed SSE event ID |

---

## Log ID Strategy

Each log entry ID is generated deterministically using:

```
<timestamp>-<domain>-<client_ip>
```

This allows:

* Idempotent inserts
* Safe retries
* Simple duplicate detection

---

## Running in Production

Recommended setup:

* Run as a **long-lived service** (systemd, Docker, Kubernetes, etc.)
* Ensure PostgreSQL has sufficient disk space
* Monitor logs for streaming reconnects

The application is designed to:

* Recover from network interruptions
* Resume streaming automatically
* Avoid data loss or duplication

A `Dockerfile` and `compose.yaml` file have been provided for your convenience.  Copy the `.env.example` file to `.env` and update the API key and profile id as needed.  Then simply run `docker compose up` to get started.

---

## Limitations & Notes

* Only a **single NextDNS profile** is supported per instance
* No automatic log retention / cleanup (handled via SQL)
* Assumes NextDNS API stability and SSE support

---

## Example Queries

```sql
-- Latest blocked domains
SELECT timestamp, domain
FROM dns_logs
WHERE blocked = true
ORDER BY timestamp DESC
LIMIT 50;

-- Top queried domains
SELECT root, COUNT(*)
FROM dns_logs
GROUP BY root
ORDER BY COUNT(*) DESC
LIMIT 20;
```

---

## License

MIT License

---

## Acknowledgements

* [NextDNS](https://nextdns.io) for the API
* PostgreSQL for being rock solid 🐘
