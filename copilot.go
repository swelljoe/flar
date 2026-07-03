package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const (
	copilotStoreRel       = ".flar"
	copilotSessionState   = "session-state"
	copilotSessionStoreDB = "session-store.db"
)

// prepareCopilotStore returns this workspace's private Copilot home under
// ~/.copilot/.flar/<project-slug>, creating it and seeding it once from the
// host's global session history for this cwd only. The whole directory is bound
// into the sandbox as ~/.copilot so Copilot's SQLite sidecars and per-session
// state persist together without exposing other projects' sessions.
func prepareCopilotStore(hostHome, absProjectDir, configSrc string) (string, error) {
	hostCopilot := filepath.Join(hostHome, ".copilot")
	store := filepath.Join(hostCopilot, copilotStoreRel, claudeProjectSlug(absProjectDir))

	if err := CopyDirExcept(configSrc, store, copilotSkipCopy); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(store, copilotSessionState), 0o700); err != nil {
		return "", err
	}

	marker := filepath.Join(store, ".seeded")
	if _, err := os.Stat(marker); err != nil {
		if err := seedCopilotStore(hostCopilot, store, absProjectDir); err != nil {
			return "", err
		}
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			return "", err
		}
	}

	return store, nil
}

func seedCopilotStore(hostCopilot, store, absProjectDir string) error {
	ids, err := seedCopilotSessionStore(
		filepath.Join(hostCopilot, copilotSessionStoreDB),
		filepath.Join(store, copilotSessionStoreDB),
		absProjectDir,
	)
	if err != nil {
		return err
	}

	for _, id := range ids {
		src := filepath.Join(hostCopilot, copilotSessionState, id)
		if !dirExists(src) {
			continue
		}
		if err := copyCopilotSessionDir(src, filepath.Join(store, copilotSessionState, id)); err != nil {
			return err
		}
	}
	return nil
}

func seedCopilotSessionStore(srcPath, dstPath, absProjectDir string) ([]string, error) {
	if !fileExists(srcPath) {
		return nil, nil
	}
	for _, path := range []string{dstPath, dstPath + "-wal", dstPath + "-shm"} {
		_ = os.Remove(path)
	}

	srcDB, err := openSQLite(srcPath)
	if err != nil {
		return nil, err
	}
	defer srcDB.Close()

	dstDB, err := openSQLite(dstPath)
	if err != nil {
		return nil, err
	}
	defer dstDB.Close()

	if err := cloneSQLiteSchema(srcDB, dstDB); err != nil {
		return nil, err
	}

	tx, err := dstDB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := copyCopilotSchemaVersion(srcDB, tx); err != nil {
		return nil, err
	}

	sessionRows, err := srcDB.Query(`
		SELECT id, cwd, repository, host_type, branch, summary, created_at, updated_at
		FROM sessions
		WHERE cwd = ?
		ORDER BY datetime(updated_at) DESC, id
	`, absProjectDir)
	if err != nil {
		return nil, err
	}
	defer sessionRows.Close()

	insertSession, err := tx.Prepare(`
		INSERT INTO sessions (id, cwd, repository, host_type, branch, summary, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return nil, err
	}
	defer insertSession.Close()

	var ids []string
	for sessionRows.Next() {
		var (
			id, cwd                               string
			repository, hostType, branch, summary sql.NullString
			createdAt, updatedAt                  sql.NullString
		)
		if err := sessionRows.Scan(&id, &cwd, &repository, &hostType, &branch, &summary, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		if _, err := insertSession.Exec(
			id,
			cwd,
			nullStringValue(repository),
			nullStringValue(hostType),
			nullStringValue(branch),
			nullStringValue(summary),
			nullStringValue(createdAt),
			nullStringValue(updatedAt),
		); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := sessionRows.Err(); err != nil {
		return nil, err
	}

	if err := copyCopilotTurns(srcDB, tx, ids); err != nil {
		return nil, err
	}
	if err := copyCopilotCheckpoints(srcDB, tx, ids); err != nil {
		return nil, err
	}
	if err := copyCopilotSessionFiles(srcDB, tx, ids); err != nil {
		return nil, err
	}
	if err := copyCopilotSessionRefs(srcDB, tx, ids); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func cloneSQLiteSchema(srcDB, dstDB *sql.DB) error {
	rows, err := srcDB.Query(`
		SELECT sql
		FROM sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		ORDER BY CASE type
			WHEN 'table' THEN 0
			WHEN 'index' THEN 1
			WHEN 'trigger' THEN 2
			WHEN 'view' THEN 3
			ELSE 4
		END, name
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var stmt string
		if err := rows.Scan(&stmt); err != nil {
			return err
		}
		if _, err := dstDB.Exec(stmt); err != nil {
			return fmt.Errorf("clone sqlite schema: %w", err)
		}
	}
	return rows.Err()
}

func copyCopilotSchemaVersion(srcDB *sql.DB, tx *sql.Tx) error {
	rows, err := srcDB.Query(`SELECT version FROM schema_version`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	stmt, err := tx.Prepare(`INSERT INTO schema_version (version) VALUES (?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return err
		}
		if _, err := stmt.Exec(version); err != nil {
			return err
		}
	}
	return rows.Err()
}

func copyCopilotTurns(srcDB *sql.DB, tx *sql.Tx, ids []string) error {
	stmt, err := tx.Prepare(`
		INSERT INTO turns (id, session_id, turn_index, user_message, assistant_response, timestamp)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, id := range ids {
		rows, err := srcDB.Query(`
			SELECT id, session_id, turn_index, user_message, assistant_response, timestamp
			FROM turns
			WHERE session_id = ?
			ORDER BY turn_index, id
		`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var (
				rowID, turnIndex               int
				sessionID                      string
				userMessage, assistantResponse sql.NullString
				timestamp                      sql.NullString
			)
			if err := rows.Scan(&rowID, &sessionID, &turnIndex, &userMessage, &assistantResponse, &timestamp); err != nil {
				rows.Close()
				return err
			}
			if _, err := stmt.Exec(rowID, sessionID, turnIndex, nullStringValue(userMessage), nullStringValue(assistantResponse), nullStringValue(timestamp)); err != nil {
				rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return nil
}

func copyCopilotCheckpoints(srcDB *sql.DB, tx *sql.Tx, ids []string) error {
	stmt, err := tx.Prepare(`
		INSERT INTO checkpoints (
			id, session_id, checkpoint_number, title, overview, history,
			work_done, technical_details, important_files, next_steps, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, id := range ids {
		rows, err := srcDB.Query(`
			SELECT id, session_id, checkpoint_number, title, overview, history,
			       work_done, technical_details, important_files, next_steps, created_at
			FROM checkpoints
			WHERE session_id = ?
			ORDER BY checkpoint_number, id
		`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var (
				rowID, checkpointNumber              int
				sessionID                            string
				title, overview, history             sql.NullString
				workDone, technicalDetails           sql.NullString
				importantFiles, nextSteps, createdAt sql.NullString
			)
			if err := rows.Scan(
				&rowID, &sessionID, &checkpointNumber, &title, &overview, &history,
				&workDone, &technicalDetails, &importantFiles, &nextSteps, &createdAt,
			); err != nil {
				rows.Close()
				return err
			}
			if _, err := stmt.Exec(
				rowID, sessionID, checkpointNumber,
				nullStringValue(title), nullStringValue(overview), nullStringValue(history),
				nullStringValue(workDone), nullStringValue(technicalDetails),
				nullStringValue(importantFiles), nullStringValue(nextSteps), nullStringValue(createdAt),
			); err != nil {
				rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return nil
}

func copyCopilotSessionFiles(srcDB *sql.DB, tx *sql.Tx, ids []string) error {
	stmt, err := tx.Prepare(`
		INSERT INTO session_files (id, session_id, file_path, tool_name, turn_index, first_seen_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, id := range ids {
		rows, err := srcDB.Query(`
			SELECT id, session_id, file_path, tool_name, turn_index, first_seen_at
			FROM session_files
			WHERE session_id = ?
			ORDER BY id
		`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var (
				rowID                 int
				sessionID, filePath   string
				toolName, firstSeenAt sql.NullString
				turnIndex             sql.NullInt64
			)
			if err := rows.Scan(&rowID, &sessionID, &filePath, &toolName, &turnIndex, &firstSeenAt); err != nil {
				rows.Close()
				return err
			}
			if _, err := stmt.Exec(rowID, sessionID, filePath, nullStringValue(toolName), nullInt64Value(turnIndex), nullStringValue(firstSeenAt)); err != nil {
				rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return nil
}

func copyCopilotSessionRefs(srcDB *sql.DB, tx *sql.Tx, ids []string) error {
	stmt, err := tx.Prepare(`
		INSERT INTO session_refs (id, session_id, ref_type, ref_value, turn_index, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, id := range ids {
		rows, err := srcDB.Query(`
			SELECT id, session_id, ref_type, ref_value, turn_index, created_at
			FROM session_refs
			WHERE session_id = ?
			ORDER BY id
		`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var (
				rowID                        int
				sessionID, refType, refValue string
				createdAt                    sql.NullString
				turnIndex                    sql.NullInt64
			)
			if err := rows.Scan(&rowID, &sessionID, &refType, &refValue, &turnIndex, &createdAt); err != nil {
				rows.Close()
				return err
			}
			if _, err := stmt.Exec(rowID, sessionID, refType, refValue, nullInt64Value(turnIndex), nullStringValue(createdAt)); err != nil {
				rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return nil
}

func copyCopilotSessionDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, info.Mode()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "inuse.") && strings.HasSuffix(name, ".lock") {
			continue
		}
		srcPath := filepath.Join(src, name)
		dstPath := filepath.Join(dst, name)
		if entry.IsDir() {
			if err := copyCopilotSessionDir(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		fi, err := entry.Info()
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			if err := CopyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}
	return nil
}

func nullStringValue(v sql.NullString) any {
	if v.Valid {
		return v.String
	}
	return nil
}

func nullInt64Value(v sql.NullInt64) any {
	if v.Valid {
		return v.Int64
	}
	return nil
}
