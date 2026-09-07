package store

import "fmt"

// migrateSecretStorage upgrades legacy plaintext credentials in-place. Each
// row is sealed before its plaintext column is blanked, inside one transaction.
func (s *Store) migrateSecretStorage() error {
	columns := []struct{ table, name, definition string }{
		{"bots", "token_ciphertext", "TEXT NOT NULL DEFAULT ''"},
		{"bots", "token_fingerprint", "TEXT NOT NULL DEFAULT ''"},
		{"bots", "secret_token_ciphertext", "TEXT NOT NULL DEFAULT ''"},
		{"gateway_bot_accounts", "token_ciphertext", "TEXT NOT NULL DEFAULT ''"},
		{"gateway_bot_accounts", "token_fingerprint", "TEXT NOT NULL DEFAULT ''"},
		{"gateway_bot_accounts", "revision", "INTEGER NOT NULL DEFAULT 1"},
		{"gateway_telegram_destinations", "revision", "INTEGER NOT NULL DEFAULT 1"},
		{"gateway_business_routes", "inbound_backend_token_ciphertext", "TEXT NOT NULL DEFAULT ''"},
		{"gateway_route_revisions", "inbound_backend_token_ciphertext", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, column := range columns {
		if err := addColumnIfMissing(s.db, column.table, column.name, column.definition); err != nil {
			return err
		}
	}

	type native struct {
		id             int64
		token, backend string
	}
	natives := []native{}
	rows, err := s.db.Query(`SELECT id,token,secret_token FROM bots WHERE token<>'' OR secret_token<>''`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var item native
		if err := rows.Scan(&item.id, &item.token, &item.backend); err != nil {
			rows.Close()
			return err
		}
		natives = append(natives, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	type account struct {
		id    int64
		token string
	}
	accounts := []account{}
	rows, err = s.db.Query(`SELECT id,token FROM gateway_bot_accounts WHERE token<>''`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var item account
		if err := rows.Scan(&item.id, &item.token); err != nil {
			rows.Close()
			return err
		}
		accounts = append(accounts, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	type routeSecret struct {
		id, revision int64
		secret       string
	}
	routes := []routeSecret{}
	rows, err = s.db.Query(`SELECT id,revision,inbound_backend_token FROM gateway_business_routes WHERE inbound_backend_token<>''`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var item routeSecret
		if err := rows.Scan(&item.id, &item.revision, &item.secret); err != nil {
			rows.Close()
			return err
		}
		routes = append(routes, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	revisions := []routeSecret{}
	rows, err = s.db.Query(`SELECT route_id,revision,inbound_backend_token FROM gateway_route_revisions WHERE inbound_backend_token<>''`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var item routeSecret
		if err := rows.Scan(&item.id, &item.revision, &item.secret); err != nil {
			rows.Close()
			return err
		}
		revisions = append(revisions, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, item := range natives {
		token, err := s.sealSecret(item.token)
		if err != nil {
			return err
		}
		backend, err := s.sealSecret(item.backend)
		if err != nil {
			return err
		}
		fingerprint := ""
		if item.token != "" {
			fingerprint = s.secretFingerprint(item.token)
		}
		if _, err := tx.Exec(`UPDATE bots SET token='',secret_token='',token_ciphertext=?,token_fingerprint=?,secret_token_ciphertext=? WHERE id=?`, token, fingerprint, backend, item.id); err != nil {
			return err
		}
	}
	for _, item := range accounts {
		sealed, err := s.sealSecret(item.token)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE gateway_bot_accounts SET token='',token_ciphertext=?,token_fingerprint=? WHERE id=?`, sealed, s.secretFingerprint(item.token), item.id); err != nil {
			return err
		}
	}
	for _, item := range routes {
		sealed, err := s.sealSecret(item.secret)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE gateway_business_routes SET inbound_backend_token='',inbound_backend_token_ciphertext=? WHERE id=?`, sealed, item.id); err != nil {
			return err
		}
	}
	for _, item := range revisions {
		sealed, err := s.sealSecret(item.secret)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE gateway_route_revisions SET inbound_backend_token='',inbound_backend_token_ciphertext=? WHERE route_id=? AND revision=?`, sealed, item.id, item.revision); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func decryptNullable(s *Store, ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	value, err := s.openSecret(ciphertext)
	if err != nil {
		return "", fmt.Errorf("decrypt stored credential: %w", err)
	}
	return value, nil
}
