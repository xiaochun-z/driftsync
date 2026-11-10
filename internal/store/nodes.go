package store

import (
	"context"
	"database/sql"
)

type RemoteNode struct {
	ID, ParentID, Name, PathHint string
	IsDir, IsRoot                bool
}

func ListRemoteNodes(ctx context.Context, db *sql.DB) (map[string]RemoteNode, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,parent_id,name,path_hint,is_dir,is_root FROM remote_nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes := map[string]RemoteNode{}
	for rows.Next() {
		var n RemoteNode
		if err := rows.Scan(&n.ID, &n.ParentID, &n.Name, &n.PathHint, &n.IsDir, &n.IsRoot); err != nil {
			return nil, err
		}
		nodes[n.ID] = n
	}
	return nodes, rows.Err()
}

// CommitCheckpoint atomically publishes the hierarchy and the completed cursor.
func CommitCheckpoint(ctx context.Context, db *sql.DB, link string, nodes map[string]RemoteNode, deleted []string, full bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if full {
		if _, err := tx.ExecContext(ctx, `DELETE FROM remote_nodes`); err != nil {
			return err
		}
	}
	for _, n := range nodes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO remote_nodes(id,parent_id,name,path_hint,is_dir,is_root) VALUES(?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET parent_id=excluded.parent_id,name=excluded.name,path_hint=excluded.path_hint,is_dir=excluded.is_dir,is_root=excluded.is_root`, n.ID, n.ParentID, n.Name, n.PathHint, n.IsDir, n.IsRoot); err != nil {
			return err
		}
	}
	for _, id := range deleted {
		if _, err := tx.ExecContext(ctx, `WITH RECURSIVE removed(id) AS (SELECT ? UNION SELECT n.id FROM remote_nodes n JOIN removed r ON n.parent_id=r.id) DELETE FROM remote_nodes WHERE id IN (SELECT id FROM removed)`, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key='pending_renames'`); err != nil {
		return err
	}
	if full {
		if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key='needs_cloud_baseline'`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('delta_link',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, link); err != nil {
		return err
	}
	return tx.Commit()
}
