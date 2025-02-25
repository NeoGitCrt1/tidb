package errormanager

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/br/pkg/lightning/common"
	"github.com/pingcap/tidb/br/pkg/lightning/config"
	"github.com/pingcap/tidb/br/pkg/lightning/log"
	"github.com/pingcap/tidb/br/pkg/redact"
	"go.uber.org/multierr"
	"go.uber.org/zap"
)

const (
	createSchema = `
		CREATE SCHEMA IF NOT EXISTS %s;
	`

	syntaxErrorTableName   = "syntax_error_v1"
	typeErrorTableName     = "type_error_v1"
	conflictErrorTableName = "conflict_error_v1"

	createSyntaxErrorTable = `
		CREATE TABLE IF NOT EXISTS %s.` + syntaxErrorTableName + ` (
			task_id     bigint NOT NULL,
			create_time datetime(6) NOT NULL DEFAULT now(6),
			table_name  varchar(261) NOT NULL,
			path        varchar(2048) NOT NULL,
			offset      bigint NOT NULL,
			error       text NOT NULL,
			context     text
		);
	`

	createTypeErrorTable = `
		CREATE TABLE IF NOT EXISTS %s.` + typeErrorTableName + ` (
			task_id     bigint NOT NULL,
			create_time datetime(6) NOT NULL DEFAULT now(6),
			table_name  varchar(261) NOT NULL,
			path        varchar(2048) NOT NULL,
			offset      bigint NOT NULL,
			error       text NOT NULL,
			row_data    text NOT NULL
		);
	`

	createConflictErrorTable = `
		CREATE TABLE IF NOT EXISTS %s.` + conflictErrorTableName + ` (
			task_id     bigint NOT NULL,
			create_time datetime(6) NOT NULL DEFAULT now(6),
			table_name  varchar(261) NOT NULL,
			index_name  varchar(128) NOT NULL,
			key_data    text NOT NULL COMMENT 'decoded from raw_key, human readable only, not for machine use',
			row_data    text NOT NULL COMMENT 'decoded from raw_row, human readable only, not for machine use',
			raw_key     mediumblob NOT NULL COMMENT 'the conflicted key',
			raw_value   mediumblob NOT NULL COMMENT 'the value of the conflicted key',
			raw_handle  mediumblob NOT NULL COMMENT 'the data handle derived from the conflicted key or value',
			raw_row     mediumblob NOT NULL COMMENT 'the data retrieved from the handle',
			KEY (task_id, table_name)
		);
	`

	insertIntoTypeError = `
		INSERT INTO %s.` + typeErrorTableName + `
		(task_id, table_name, path, offset, error, row_data)
		VALUES (?, ?, ?, ?, ?, ?);
	`

	insertIntoConflictErrorData = `
		INSERT INTO %s.` + conflictErrorTableName + `
		(task_id, table_name, index_name, key_data, row_data, raw_key, raw_value, raw_handle, raw_row)
		VALUES (?, ?, 'PRIMARY', ?, ?, ?, ?, raw_key, raw_value);
	`

	insertIntoConflictErrorIndex = `
		INSERT INTO %s.` + conflictErrorTableName + `
		(task_id, table_name, index_name, key_data, row_data, raw_key, raw_value, raw_handle, raw_row)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);
	`

	// not sure if we really want to DISTINCT.
	// having DISTINCT requires TiDB to introduce a HashAgg in memory but reduces response size.
	selectConflictKeys = `
		SELECT DISTINCT raw_handle, raw_row
		FROM %s.` + conflictErrorTableName + `
		WHERE task_id = ? AND table_name = ?;
	`
)

type ErrorManager struct {
	db             *sql.DB
	taskID         int64
	schemaEscaped  string
	remainingError config.MaxError
}

// New creates a new error manager.
func New(db *sql.DB, cfg *config.Config) *ErrorManager {
	em := &ErrorManager{
		taskID:         cfg.TaskID,
		remainingError: cfg.App.MaxError,
	}
	if len(cfg.App.TaskInfoSchemaName) != 0 {
		em.db = db
		em.schemaEscaped = common.EscapeIdentifier(cfg.App.TaskInfoSchemaName)
	}
	return em
}

// Init creates the schemas and tables to store the task information.
func (em *ErrorManager) Init(ctx context.Context) error {
	if em.db == nil {
		return nil
	}

	exec := common.SQLWithRetry{
		DB:     em.db,
		Logger: log.L(),
	}

	sqls := [][2]string{
		{"create task info schema", createSchema},
		{"create syntax error table", createSyntaxErrorTable},
		{"create type error table", createTypeErrorTable},
		{"create conflict error table", createConflictErrorTable},
	}

	for _, sql := range sqls {
		err := exec.Exec(ctx, sql[0], fmt.Sprintf(sql[1], em.schemaEscaped))
		if err != nil {
			return err
		}
	}

	return nil
}

// RecordTypeError records a type error.
// If the number of recorded type errors exceed the max-error count, also returns `err` directly.
func (em *ErrorManager) RecordTypeError(
	ctx context.Context,
	logger log.Logger,
	tableName string,
	path string,
	offset int64,
	rowText string,
	encodeErr error,
) error {
	if em.db != nil {
		errMsg := encodeErr.Error()
		logger = logger.With(
			zap.Int64("offset", offset),
			zap.String("row", redact.String(rowText)),
			zap.String("message", errMsg))

		// put it into the database.
		exec := common.SQLWithRetry{
			DB:           em.db,
			Logger:       logger,
			HideQueryLog: redact.NeedRedact(),
		}
		if err := exec.Exec(ctx, "insert type error record",
			fmt.Sprintf(insertIntoTypeError, em.schemaEscaped),
			em.taskID,
			tableName,
			path,
			offset,
			errMsg,
			rowText,
		); err != nil {
			return multierr.Append(encodeErr, err)
		}
	}

	// elide the encode error if needed.
	if em.remainingError.Type.Dec() < 0 {
		return encodeErr
	}
	return nil
}

type DataConflictInfo struct {
	RawKey   []byte
	RawValue []byte
	KeyData  string
	Row      string
}

func (em *ErrorManager) RecordDataConflictError(
	ctx context.Context,
	logger log.Logger,
	tableName string,
	conflictInfos []DataConflictInfo,
) error {
	if em.db == nil {
		return nil
	}

	exec := common.SQLWithRetry{
		DB:           em.db,
		Logger:       logger,
		HideQueryLog: redact.NeedRedact(),
	}
	return exec.Transact(ctx, "insert data conflict error record", func(c context.Context, txn *sql.Tx) error {
		stmt, err := txn.PrepareContext(c, fmt.Sprintf(insertIntoConflictErrorData, em.schemaEscaped))
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, conflictInfo := range conflictInfos {
			_, err = stmt.ExecContext(c,
				em.taskID,
				tableName,
				conflictInfo.KeyData,
				conflictInfo.Row,
				conflictInfo.RawKey,
				conflictInfo.RawValue,
			)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (em *ErrorManager) RecordIndexConflictError(
	ctx context.Context,
	logger log.Logger,
	tableName string,
	indexNames []string,
	conflictInfos []DataConflictInfo,
	rawHandles, rawRows [][]byte,
) error {
	if em.db == nil {
		return nil
	}

	exec := common.SQLWithRetry{
		DB:           em.db,
		Logger:       logger,
		HideQueryLog: redact.NeedRedact(),
	}
	return exec.Transact(ctx, "insert index conflict error record", func(c context.Context, txn *sql.Tx) error {
		stmt, err := txn.PrepareContext(c, fmt.Sprintf(insertIntoConflictErrorIndex, em.schemaEscaped))
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i, conflictInfo := range conflictInfos {
			_, err = stmt.ExecContext(c,
				em.taskID,
				tableName,
				indexNames[i],
				conflictInfo.KeyData,
				conflictInfo.Row,
				conflictInfo.RawKey,
				conflictInfo.RawValue,
				rawHandles[i],
				rawRows[i],
			)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// GetConflictKeys obtains all (distinct) conflicting rows (handle and their
// values) from the current error report.
func (em *ErrorManager) GetConflictKeys(ctx context.Context, tableName string) ([][2][]byte, error) {
	if em.db == nil {
		return nil, nil
	}
	rows, err := em.db.QueryContext(
		ctx,
		fmt.Sprintf(selectConflictKeys, em.schemaEscaped),
		em.taskID,
		tableName,
	)
	if err != nil {
		return nil, errors.Trace(err)
	}
	defer rows.Close()

	var handleRows [][2][]byte
	for rows.Next() {
		var handleRow [2][]byte
		if err := rows.Scan(&handleRow[0], &handleRow[1]); err != nil {
			return nil, errors.Trace(err)
		}
		handleRows = append(handleRows, handleRow)
	}
	return handleRows, errors.Trace(rows.Err())
}
