package store

import (
	"database/sql"
	"errors"
)

var ErrBotInUse = errors.New("Bot is used by notification destinations or business routes; remove those references before deleting it")
var ErrBotIdentityConflict = errors.New("another Bot already uses this Telegram credential")

// Account IDs remain stable for historical destinations and deliveries. Bots
// own the configuration; accounts are compatibility records for Gateway APIs.
func (s *Store) migrateBotAccounts() error {
	if err := addColumnIfMissing(s.db, "gateway_bot_accounts", "native_bot_id", "INTEGER REFERENCES bots(id)"); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id,name,username,token_ciphertext,token_fingerprint FROM gateway_bot_accounts WHERE native_bot_id IS NULL ORDER BY id`)
	if err != nil {
		return err
	}
	type legacy struct {
		id                                  int64
		name, username, sealed, fingerprint string
	}
	var accounts []legacy
	for rows.Next() {
		var a legacy
		if err := rows.Scan(&a.id, &a.name, &a.username, &a.sealed, &a.fingerprint); err != nil {
			rows.Close()
			return err
		}
		accounts = append(accounts, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, a := range accounts {
		id, err := ensureNativeBotTx(tx, a.name, a.username, a.sealed, a.fingerprint)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE gateway_bot_accounts SET native_bot_id=? WHERE id=?`, id, a.id); err != nil {
			return err
		}
	}
	// Preserve revisions and all references during the backfill. Existing Bot
	// names win over old acceptance aliases; no Telegram calls or polling changes.
	_, err = tx.Exec(`UPDATE gateway_bot_accounts SET
	 name=(SELECT name FROM bots WHERE id=native_bot_id),
	 username=(SELECT bot_username FROM bots WHERE id=native_bot_id),
	 token_ciphertext=(SELECT token_ciphertext FROM bots WHERE id=native_bot_id),
	 token_fingerprint=(SELECT token_fingerprint FROM bots WHERE id=native_bot_id);
	 CREATE INDEX IF NOT EXISTS idx_gateway_account_native_bot ON gateway_bot_accounts(native_bot_id);`)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO gateway_bot_accounts(name,username,token,token_ciphertext,token_fingerprint,native_bot_id,revision,created_at,updated_at)
	 SELECT b.name,b.bot_username,'',b.token_ciphertext,b.token_fingerprint,b.id,1,?,? FROM bots b
	 WHERE NOT EXISTS(SELECT 1 FROM gateway_bot_accounts a WHERE a.native_bot_id=b.id)`, nowRFC3339(), nowRFC3339()); err != nil {
		return err
	}
	return tx.Commit()
}

func ensureNativeBotTx(tx *sql.Tx, name, username, sealed, fingerprint string) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT id FROM bots WHERE token_fingerprint=? AND token_ciphertext<>'' ORDER BY id LIMIT 1`, fingerprint).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	res, err := tx.Exec(`INSERT INTO bots(name,bot_username,token,token_ciphertext,token_fingerprint,manage_enabled,proxy_enabled,source) VALUES(?,?,'',?,?,0,0,'gateway')`, name, username, sealed, fingerprint)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func ensureGatewayAccountTx(tx *sql.Tx, botID int64) error {
	_, err := tx.Exec(`INSERT INTO gateway_bot_accounts(name,username,token,token_ciphertext,token_fingerprint,native_bot_id,revision,created_at,updated_at)
	 SELECT name,bot_username,'',token_ciphertext,token_fingerprint,id,1,?,? FROM bots
	 WHERE id=? AND NOT EXISTS(SELECT 1 FROM gateway_bot_accounts WHERE native_bot_id=?)`, nowRFC3339(), nowRFC3339(), botID, botID)
	return err
}

// syncBotAccountsTx updates every legacy alias and invalidates stale editors.
// Credential and route changes commit together with the native Bot mutation.
func (s *Store) syncBotAccountsTx(tx *sql.Tx, botID int64, actorID string, force bool) error {
	var changed bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM gateway_bot_accounts a JOIN bots b ON b.id=a.native_bot_id
	 WHERE b.id=? AND (a.name<>b.name OR a.username<>b.bot_username OR a.token_fingerprint<>b.token_fingerprint))`, botID).Scan(&changed); err != nil {
		return err
	}
	if !changed && !force {
		return ensureGatewayAccountTx(tx, botID)
	}
	_, err := tx.Exec(`UPDATE gateway_bot_accounts SET name=(SELECT name FROM bots WHERE id=?),username=(SELECT bot_username FROM bots WHERE id=?),
	 token='',token_ciphertext=(SELECT token_ciphertext FROM bots WHERE id=?),token_fingerprint=(SELECT token_fingerprint FROM bots WHERE id=?),
	 revision=revision+1,updated_at=? WHERE native_bot_id=?`, botID, botID, botID, botID, nowRFC3339(), botID)
	if err != nil {
		return err
	}
	if err := s.validateServiceRecipients(tx); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id,revision FROM gateway_business_routes WHERE bot_account_id IN (SELECT id FROM gateway_bot_accounts WHERE native_bot_id=?)`, botID)
	if err != nil {
		return err
	}
	type revision struct{ id, version int64 }
	var routes []revision
	for rows.Next() {
		var r revision
		if err := rows.Scan(&r.id, &r.version); err != nil {
			rows.Close()
			return err
		}
		routes = append(routes, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, r := range routes {
		next := r.version + 1
		if _, err := tx.Exec(`UPDATE gateway_business_routes SET revision=?,updated_at=? WHERE id=?`, next, nowRFC3339(), r.id); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO gateway_route_revisions(route_id,revision,display_name,bot_account_id,destination_id,inbound_enabled,inbound_target,inbound_backend_url,inbound_backend_token,inbound_backend_token_ciphertext,outbound_enabled,allowed_callers,enabled,status,validated_at,created_at)
		 SELECT id,?,display_name,bot_account_id,destination_id,inbound_enabled,inbound_target,inbound_backend_url,'',inbound_backend_token_ciphertext,outbound_enabled,allowed_callers,enabled,status,last_validated_at,? FROM gateway_business_routes WHERE id=?`, next, nowRFC3339(), r.id); err != nil {
			return err
		}
		if err := appendGatewayAudit(tx, r.id, next, "admin", actorID, "bot.update", map[string]any{"native_bot_id": botID}); err != nil {
			return err
		}
	}
	return nil
}
