package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"time"

	gatewaypb "github.com/MeteorsLiu/successbox/api/gateway/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	_ "modernc.org/sqlite"
)

const gatewaySchema = `
CREATE TABLE IF NOT EXISTS sandbox_mcp_secrets (
  sandbox_id TEXT NOT NULL,
  mcp_id TEXT NOT NULL,
  mcp_name TEXT NOT NULL DEFAULT '',
  field_name TEXT NOT NULL,
  secret_value BLOB NOT NULL,
  created_at_ms INTEGER NOT NULL,
  updated_at_ms INTEGER NOT NULL,
  last_used_at_ms INTEGER,
  PRIMARY KEY (sandbox_id, mcp_id, field_name),
  CHECK (length(sandbox_id) > 0),
  CHECK (length(mcp_id) > 0),
  CHECK (length(field_name) > 0),
  CHECK (length(secret_value) > 0),
  CHECK (created_at_ms > 0),
  CHECK (updated_at_ms >= created_at_ms),
  CHECK (last_used_at_ms IS NULL OR last_used_at_ms >= created_at_ms)
);
CREATE INDEX IF NOT EXISTS idx_sandbox_mcp_secrets_lookup
  ON sandbox_mcp_secrets(sandbox_id, mcp_id, field_name);
CREATE INDEX IF NOT EXISTS idx_sandbox_mcp_secrets_listing
  ON sandbox_mcp_secrets(sandbox_id, mcp_id, mcp_name, field_name);
`

type gatewayStore struct{ db *sql.DB }

func openGatewayStore(path string) (*gatewayStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("gateway database path is required")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// This store owns one SQLite connection. Pending requests live in the
	// server, so a waiting Resolve never holds a database transaction.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(gatewaySchema); err != nil {
		_ = db.Close()
		return nil, errors.New("initialize gateway database failed")
	}
	return &gatewayStore{db: db}, nil
}

func (s *gatewayStore) close() error { return s.db.Close() }

func (s *gatewayStore) get(ctx context.Context, sandbox, mcp, field string, used bool) (*gatewaypb.MCPSecretRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, status.Error(codes.Internal, "read secret failed")
	}
	defer tx.Rollback()
	if used {
		_, err = tx.ExecContext(ctx, `UPDATE sandbox_mcp_secrets SET last_used_at_ms=MAX(created_at_ms,?) WHERE sandbox_id=? AND mcp_id=? AND field_name=?`, time.Now().UnixMilli(), sandbox, mcp, field)
		if err != nil {
			return nil, status.Error(codes.Internal, "record secret use failed")
		}
	}
	record := &gatewaypb.MCPSecretRecord{SandboxId: sandbox, McpId: mcp, FieldName: field, HasValue: true}
	var created, updated int64
	var lastUsed sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT mcp_name,secret_value,created_at_ms,updated_at_ms,last_used_at_ms FROM sandbox_mcp_secrets WHERE sandbox_id=? AND mcp_id=? AND field_name=?`, sandbox, mcp, field).Scan(&record.McpName, &record.SecretValue, &created, &updated, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "secret not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "read secret failed")
	}
	record.CreatedAt, record.UpdatedAt = timestampMillis(created), timestampMillis(updated)
	if lastUsed.Valid {
		record.LastUsedAt = timestampMillis(lastUsed.Int64)
	}
	if err := tx.Commit(); err != nil {
		return nil, status.Error(codes.Internal, "commit secret read failed")
	}
	return record, nil
}

func (s *gatewayStore) list(ctx context.Context, sandbox, mcp string) ([]*gatewaypb.MCPSecretRecord, error) {
	// Listing never reads secret_value into Go memory.
	rows, err := s.db.QueryContext(ctx, `SELECT mcp_id,mcp_name,field_name,created_at_ms,updated_at_ms,last_used_at_ms FROM sandbox_mcp_secrets WHERE sandbox_id=? AND (?='' OR mcp_id=?) ORDER BY mcp_id,field_name`, sandbox, mcp, mcp)
	if err != nil {
		return nil, status.Error(codes.Internal, "list secrets failed")
	}
	defer rows.Close()
	var records []*gatewaypb.MCPSecretRecord
	for rows.Next() {
		record := &gatewaypb.MCPSecretRecord{SandboxId: sandbox, HasValue: true}
		var created, updated int64
		var lastUsed sql.NullInt64
		if err := rows.Scan(&record.McpId, &record.McpName, &record.FieldName, &created, &updated, &lastUsed); err != nil {
			return nil, status.Error(codes.Internal, "read secret metadata failed")
		}
		record.CreatedAt, record.UpdatedAt = timestampMillis(created), timestampMillis(updated)
		if lastUsed.Valid {
			record.LastUsedAt = timestampMillis(lastUsed.Int64)
		}
		records = append(records, record)
	}
	if rows.Err() != nil {
		return nil, status.Error(codes.Internal, "list secrets failed")
	}
	return records, nil
}

func (s *gatewayStore) mutate(ctx context.Context, change gatewaypb.MCPSecretChange, record *gatewaypb.MCPSecretRecord) (*gatewaypb.MCPSecretRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, status.Error(codes.Internal, "write secret failed")
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	var result sql.Result
	switch change {
	case gatewaypb.MCPSecretChange_MCP_SECRET_CREATED:
		result, err = tx.ExecContext(ctx, `INSERT INTO sandbox_mcp_secrets(sandbox_id,mcp_id,mcp_name,field_name,secret_value,created_at_ms,updated_at_ms) VALUES(?,?,?,?,?,?,?) ON CONFLICT(sandbox_id,mcp_id,field_name) DO NOTHING`, record.SandboxId, record.McpId, record.McpName, record.FieldName, record.SecretValue, now, now)
	case gatewaypb.MCPSecretChange_MCP_SECRET_UPDATED:
		result, err = tx.ExecContext(ctx, `UPDATE sandbox_mcp_secrets SET secret_value=?,updated_at_ms=MAX(created_at_ms,?) WHERE sandbox_id=? AND mcp_id=? AND field_name=?`, record.SecretValue, now, record.SandboxId, record.McpId, record.FieldName)
	case gatewaypb.MCPSecretChange_MCP_SECRET_DELETED:
		result, err = tx.ExecContext(ctx, `DELETE FROM sandbox_mcp_secrets WHERE sandbox_id=? AND mcp_id=? AND field_name=?`, record.SandboxId, record.McpId, record.FieldName)
	default:
		return nil, status.Error(codes.InvalidArgument, "invalid secret mutation")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "write secret failed")
	}
	count, err := result.RowsAffected()
	if err != nil {
		return nil, status.Error(codes.Internal, "write secret failed")
	}
	if count == 0 {
		if change == gatewaypb.MCPSecretChange_MCP_SECRET_CREATED {
			return nil, status.Error(codes.AlreadyExists, "secret already exists")
		}
		return nil, status.Error(codes.NotFound, "secret not found")
	}
	if change != gatewaypb.MCPSecretChange_MCP_SECRET_DELETED && record.McpName != "" {
		// The single-table design repeats a display name on every field row.
		// Renaming api_key also renames password for the same sandbox/MCP.
		if _, err := tx.ExecContext(ctx, `UPDATE sandbox_mcp_secrets SET mcp_name=?,updated_at_ms=MAX(created_at_ms,?) WHERE sandbox_id=? AND mcp_id=?`, record.McpName, now, record.SandboxId, record.McpId); err != nil {
			return nil, status.Error(codes.Internal, "update MCP name failed")
		}
	}
	resultRecord := &gatewaypb.MCPSecretRecord{SandboxId: record.SandboxId, McpId: record.McpId, FieldName: record.FieldName}
	if change != gatewaypb.MCPSecretChange_MCP_SECRET_DELETED {
		var created, updated int64
		var used sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT mcp_name,created_at_ms,updated_at_ms,last_used_at_ms FROM sandbox_mcp_secrets WHERE sandbox_id=? AND mcp_id=? AND field_name=?`, record.SandboxId, record.McpId, record.FieldName).Scan(&resultRecord.McpName, &created, &updated, &used); err != nil {
			return nil, status.Error(codes.Internal, "read saved secret metadata failed")
		}
		resultRecord.HasValue = true
		resultRecord.CreatedAt, resultRecord.UpdatedAt = timestampMillis(created), timestampMillis(updated)
		if used.Valid {
			resultRecord.LastUsedAt = timestampMillis(used.Int64)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, status.Error(codes.Internal, "commit secret mutation failed")
	}
	return resultRecord, nil
}

func timestampMillis(value int64) *timestamppb.Timestamp {
	return timestamppb.New(time.UnixMilli(value))
}
