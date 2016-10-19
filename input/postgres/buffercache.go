package postgres

import (
	"database/sql"
	"fmt"

	"github.com/lib/pq"
	"github.com/pganalyze/collector/state"
	"github.com/pganalyze/collector/util"
)

const buffercacheSQL string = `WITH buffers AS (
	SELECT COUNT(*) AS block_count, reldatabase, relfilenode
	FROM pg_buffercache
	GROUP BY 2, 3
)
SELECT block_count * current_setting('block_size')::int, d.datname, nspname, relname, relkind
FROM buffers b
JOIN pg_database d ON (d.oid = reldatabase)
LEFT JOIN pg_class c ON (b.relfilenode = pg_relation_filenode(c.oid) AND (b.reldatabase = 0 OR d.datname = current_database()))
LEFT JOIN pg_namespace n ON (n.oid = c.relnamespace);
`

func GetBuffercache(logger *util.Logger, db *sql.DB) (report state.PostgresBuffercache, err error) {
	rows, err := db.Query(QueryMarkerSQL + buffercacheSQL)
	if err != nil {
		if err.(*pq.Error).Code == "42P01" { // undefined_table
			logger.PrintInfo("pg_buffercache relation does not exist, trying to create extension...")

			_, err = db.Exec(QueryMarkerSQL + "CREATE EXTENSION IF NOT EXISTS pg_buffercache")
			if err != nil {
				return
			}

			rows, err = db.Query(QueryMarkerSQL + buffercacheSQL)
			if err != nil {
				return
			}
		} else {
			return
		}
	}

	if err != nil {
		err = fmt.Errorf("Buffercache/Query: %s", err)
		return
	}

	defer rows.Close()

	for rows.Next() {
		var row state.PostgresBuffercacheEntry

		err = rows.Scan(&row.Bytes, &row.DatabaseName, &row.SchemaName,
			&row.ObjectName, &row.ObjectKind)
		if err != nil {
			err = fmt.Errorf("Buffercache/Scan: %s", err)
			return
		}

		report.Entries = append(report.Entries, row)
	}

	return
}
