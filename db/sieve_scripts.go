package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/migadu/sora/consts"
)

type SieveScript struct {
	ID        int64
	UserID    int64
	Name      string
	Script    string
	Active    bool
	UpdatedAt time.Time
}

func (db *Database) GetUserScripts(ctx context.Context, userID int64) ([]*SieveScript, error) {
	rows, err := db.GetReadPool().Query(ctx, "SELECT id, account_id, name, script, active, updated_at FROM sieve_scripts WHERE account_id = $1", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var scripts []*SieveScript
	for rows.Next() {
		var script SieveScript
		if err := rows.Scan(&script.ID, &script.UserID, &script.Name, &script.Script, &script.Active, &script.UpdatedAt); err != nil {
			return nil, err
		}
		scripts = append(scripts, &script)
	}

	return scripts, rows.Err()
}

func (db *Database) GetScript(ctx context.Context, scriptID, userID int64) (*SieveScript, error) {
	var script SieveScript
	err := db.GetReadPool().QueryRow(ctx, "SELECT id, account_id, name, script, active, updated_at FROM sieve_scripts WHERE id = $1 AND account_id = $2",
		scriptID, userID).Scan(&script.ID, &script.UserID, &script.Name, &script.Script, &script.Active, &script.UpdatedAt)
	if err != nil {
		return nil, err
	}

	return &script, nil
}

func (db *Database) GetActiveScript(ctx context.Context, userID int64) (*SieveScript, error) {
	var script SieveScript
	err := db.GetReadPool().QueryRow(ctx, "SELECT id, account_id, name, script, active, updated_at FROM sieve_scripts WHERE account_id = $1 AND active = true", userID).Scan(&script.ID, &script.UserID, &script.Name, &script.Script, &script.Active, &script.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, consts.ErrDBNotFound
		}
		return nil, err
	}

	return &script, nil
}

func (db *Database) GetScriptByName(ctx context.Context, name string, userID int64) (*SieveScript, error) {
	var script SieveScript
	err := db.GetReadPool().QueryRow(ctx, "SELECT id, name, script, active, updated_at FROM sieve_scripts WHERE name = $1 AND account_id = $2", name, userID).Scan(&script.ID, &script.Name, &script.Script, &script.Active, &script.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, consts.ErrDBNotFound
		}
		return nil, err
	}
	script.UserID = userID

	return &script, nil
}

func (db *Database) CreateScript(ctx context.Context, tx pgx.Tx, userID int64, name, script string) (*SieveScript, error) {
	var s SieveScript
	err := tx.QueryRow(ctx, `
		INSERT INTO sieve_scripts (account_id, name, script, active) 
		VALUES ($1, $2, $3, false) 
		RETURNING id, account_id, name, script, active, updated_at
	`, userID, name, script).Scan(&s.ID, &s.UserID, &s.Name, &s.Script, &s.Active, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (db *Database) UpdateScript(ctx context.Context, tx pgx.Tx, scriptID, userID int64, name, script string) (*SieveScript, error) {
	var s SieveScript
	err := tx.QueryRow(ctx, `
		UPDATE sieve_scripts SET name = $1, script = $2, updated_at = now() 
		WHERE id = $3 AND account_id = $4
		RETURNING id, account_id, name, script, active, updated_at
	`, name, script, scriptID, userID).Scan(&s.ID, &s.UserID, &s.Name, &s.Script, &s.Active, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (db *Database) DeleteScript(ctx context.Context, tx pgx.Tx, scriptID, userID int64) error {
	_, err := tx.Exec(ctx, "DELETE FROM sieve_scripts WHERE id = $1 AND account_id = $2", scriptID, userID)
	return err
}

func (db *Database) SetScriptActive(ctx context.Context, tx pgx.Tx, scriptID, userID int64, active bool) error {
	// If we are activating a script, we must first deactivate all other scripts for this user
	// to ensure the UNIQUE index on (account_id, active=true) is not violated.
	if active {
		_, err := tx.Exec(ctx, "UPDATE sieve_scripts SET active = false WHERE account_id = $1 AND active = true", userID)
		if err != nil {
			return fmt.Errorf("failed to deactivate other scripts: %w", err)
		}
	}

	// Now, set the active status for the target script.
	_, err := tx.Exec(ctx, "UPDATE sieve_scripts SET active = $1, updated_at = now() WHERE id = $2 AND account_id = $3", active, scriptID, userID)
	return err
}
