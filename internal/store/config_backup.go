package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/skrashevich/botmux/internal/configbackup"
)

func (s *Store) migrateConfigurationBackup() error {
	for _, table := range []string{"bots", "gateway_telegram_destinations", "routes"} {
		if err := addColumnIfMissing(s.db, table, "config_ref", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"bots", "gateway_telegram_destinations", "routes"} {
		// Existing and future objects receive a persisted random identity. Export is
		// read-only; credential rotation and activity never change this identity.
		_, err = tx.Exec(`UPDATE ` + table + ` SET config_ref=lower(hex(randomblob(16))) WHERE config_ref='';
   CREATE UNIQUE INDEX IF NOT EXISTS idx_` + table + `_config_ref ON ` + table + `(config_ref) WHERE config_ref<>'';
   CREATE TRIGGER IF NOT EXISTS ` + table + `_config_ref_insert AFTER INSERT ON ` + table + ` WHEN NEW.config_ref='' BEGIN
    UPDATE ` + table + ` SET config_ref=lower(hex(randomblob(16))) WHERE id=NEW.id;
   END;`)
		if err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS configuration_restores(digest TEXT PRIMARY KEY, bots INTEGER NOT NULL, destinations INTEGER NOT NULL)`); err != nil {
		return err
	}
	return tx.Commit()
}

// ExportConfiguration reads all collections from one SQLite snapshot.
func (s *Store) ExportConfiguration() (configbackup.Snapshot, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return configbackup.Snapshot{}, err
	}
	defer tx.Rollback()
	snapshot, err := s.exportConfigurationTx(tx)
	if err != nil {
		return snapshot, err
	}
	return snapshot, tx.Commit()
}

func unsupportedConfigurationTx(tx *sql.Tx) error {
	for _, table := range []string{"gateway_notification_services", "gateway_service_subscriptions", "gateway_business_routes", "gateway_workloads", "gateway_workload_credentials", "gateway_route_permissions"} {
		var nonempty bool
		query := `SELECT EXISTS(SELECT 1 FROM ` + table + `)`
		if table == "gateway_workloads" {
			// The embedded adapter is a system initialization record, not a source.
			query = `SELECT EXISTS(SELECT 1 FROM gateway_workloads WHERE NOT (id=-1234 AND name='Embedded Gateway Adapter' AND status='active' AND service_publish=0 AND service_query=0))`
		}
		if err := tx.QueryRow(query).Scan(&nonempty); err != nil {
			return err
		}
		if nonempty {
			return fmt.Errorf("%w: %s is not empty", configbackup.ErrUnsupported, table)
		}
	}
	return nil
}
func (s *Store) exportConfigurationTx(tx *sql.Tx) (configbackup.Snapshot, error) {
	result := configbackup.Empty()
	if err := unsupportedConfigurationTx(tx); err != nil {
		return result, err
	}
	rows, err := tx.Query(`SELECT config_ref,name,token_ciphertext,bot_username,description,manage_enabled,proxy_enabled,long_poll_enabled,disabled,backend_url,secret_token_ciphertext,polling_timeout,source FROM bots ORDER BY config_ref`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var b configbackup.Bot
		var token, secret string
		if err := rows.Scan(&b.Ref, &b.Name, &token, &b.Username, &b.Description, &b.ManageEnabled, &b.ProxyEnabled, &b.LongPollEnabled, &b.Disabled, &b.BackendURL, &secret, &b.PollingTimeout, &b.Source); err != nil {
			rows.Close()
			return result, err
		}
		b.Token, err = s.openSecret(token)
		if err != nil {
			rows.Close()
			return result, err
		}
		b.SecretToken, err = s.openSecret(secret)
		if err != nil {
			rows.Close()
			return result, err
		}
		result.Bots = append(result.Bots, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	rows, err = tx.Query(`SELECT d.config_ref,b.config_ref,d.name,d.chat_id,d.status FROM gateway_telegram_destinations d
  LEFT JOIN gateway_bot_accounts a ON a.id=d.bot_account_id LEFT JOIN bots b ON b.id=a.native_bot_id ORDER BY d.config_ref`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var d configbackup.Destination
		var chatID int64
		if err := rows.Scan(&d.Ref, &d.BotRef, &d.Name, &chatID, &d.Status); err != nil {
			rows.Close()
			return result, err
		}
		d.ChatID = strconv.FormatInt(chatID, 10)
		result.Destinations = append(result.Destinations, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	rows, err = tx.Query(`SELECT r.config_ref,COALESCE(s.config_ref,''),COALESCE(t.config_ref,''),r.source_chat_id,r.target_chat_id,r.condition_type,r.condition_value,r.action,r.description,r.enabled
 FROM routes r LEFT JOIN bots s ON s.id=r.source_bot_id LEFT JOIN bots t ON t.id=r.target_bot_id ORDER BY r.id`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var r configbackup.ConditionalRoute
		var sourceChat, targetChat int64
		if err := rows.Scan(&r.Ref, &r.SourceBotRef, &r.TargetBotRef, &sourceChat, &targetChat, &r.ConditionType, &r.ConditionValue, &r.Action, &r.Description, &r.Enabled); err != nil {
			rows.Close()
			return result, err
		}
		r.SourceChatID = strconv.FormatInt(sourceChat, 10)
		r.TargetChatID = strconv.FormatInt(targetChat, 10)
		result.ConditionalRoutes = append(result.ConditionalRoutes, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	return result, result.Validate()
}

// RestoreConfiguration prepares and validates the whole snapshot before entering
// a single configuration/receipt transaction. Runtime loading belongs to Server.
func (s *Store) RestoreConfiguration(snapshot configbackup.Snapshot) (configbackup.Receipt, error) {
	receipt := configbackup.Receipt{RuntimeFailedRefs: []string{}, ExternalHealth: "not_verified"}
	if err := snapshot.Validate(); err != nil {
		return receipt, err
	}
	_, digest, err := snapshot.Canonical()
	if err != nil {
		return receipt, err
	}
	type sealedBot struct {
		configbackup.Bot
		token, secret, fingerprint string
	}
	prepared := make([]sealedBot, 0, len(snapshot.Bots))
	for _, b := range snapshot.Bots {
		token, err := s.sealSecret(b.Token)
		if err != nil {
			return receipt, err
		}
		secret, err := s.sealSecret(b.SecretToken)
		if err != nil {
			return receipt, err
		}
		prepared = append(prepared, sealedBot{b, token, secret, s.secretFingerprint(b.Token)})
	}
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	// Acquire SQLite's writer lock before checking emptiness/current state. This
	// also serializes against ordinary CRUD callers that do not use gatewayMu.
	if _, err = tx.Exec(`UPDATE configuration_restores SET digest=digest WHERE 0`); err != nil {
		return receipt, err
	}
	current, err := s.exportConfigurationTx(tx)
	if err != nil {
		if errors.Is(err, configbackup.ErrUnsupported) || errors.Is(err, configbackup.ErrInvalid) {
			return receipt, configbackup.ErrConflict
		}
		return receipt, err
	}
	var bots, destinations int
	err = tx.QueryRow(`SELECT bots,destinations FROM configuration_restores WHERE digest=?`, digest).Scan(&bots, &destinations)
	if err == nil {
		_, currentDigest, err := current.Canonical()
		if err != nil {
			return receipt, err
		}
		if currentDigest != digest {
			return receipt, configbackup.ErrConflict
		}
		receipt.Digest = digest
		receipt.Bots = bots
		receipt.Destinations = destinations
		receipt.ConditionalRoutes = len(snapshot.ConditionalRoutes)
		receipt.ConfigurationCommitted = true
		receipt.Replayed = true
		return receipt, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return receipt, err
	}
	var accountCount int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM gateway_bot_accounts`).Scan(&accountCount); err != nil {
		return receipt, err
	}
	if len(current.Bots)+len(current.Destinations)+len(current.ConditionalRoutes)+accountCount > 0 {
		return receipt, configbackup.ErrConflict
	}
	botIDs := map[string]int64{}
	for _, b := range prepared {
		res, err := tx.Exec(`INSERT INTO bots(config_ref,name,token,token_ciphertext,token_fingerprint,bot_username,description,manage_enabled,proxy_enabled,long_poll_enabled,disabled,backend_url,secret_token,secret_token_ciphertext,polling_timeout,source)
   VALUES(?,?,'',?,?,?,?,?,?,?,?,?,'',?,?,?)`, b.Ref, b.Name, b.token, b.fingerprint, b.Username, b.Description, b.ManageEnabled, b.ProxyEnabled, b.LongPollEnabled, b.Disabled, b.BackendURL, b.secret, b.PollingTimeout, b.Source)
		if err != nil {
			return receipt, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return receipt, err
		}
		botIDs[b.Ref] = id
		if err = ensureGatewayAccountTx(tx, id); err != nil {
			return receipt, err
		}
	}
	for _, d := range snapshot.Destinations {
		botID := botIDs[d.BotRef]
		var accountID int64
		err := tx.QueryRow(`SELECT a.id FROM gateway_bot_accounts a WHERE a.native_bot_id=? AND NOT EXISTS(SELECT 1 FROM gateway_telegram_destinations d WHERE d.bot_account_id=a.id AND d.chat_id=?) ORDER BY a.id LIMIT 1`, botID, d.ChatID).Scan(&accountID)
		if errors.Is(err, sql.ErrNoRows) {
			// Multiple historical aliases may name the same Bot/chat. Preserve every
			// destination while all compatibility accounts still reference one Bot.
			res, e := tx.Exec(`INSERT INTO gateway_bot_accounts(name,username,token,token_ciphertext,token_fingerprint,native_bot_id,revision,created_at,updated_at)
    SELECT name,bot_username,'',token_ciphertext,token_fingerprint,id,1,?,? FROM bots WHERE id=?`, nowRFC3339(), nowRFC3339(), botID)
			if e != nil {
				return receipt, e
			}
			accountID, err = res.LastInsertId()
		}
		if err != nil {
			return receipt, err
		}
		if _, err = tx.Exec(`INSERT INTO gateway_telegram_destinations(config_ref,name,bot_account_id,chat_id,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, d.Ref, d.Name, accountID, d.ChatID, d.Status, nowRFC3339(), nowRFC3339()); err != nil {
			return receipt, err
		}
	}
	// Inserting in snapshot order recreates the existing ORDER BY id execution
	// order, independently of source database IDs and stable configuration refs.
	for _, r := range snapshot.ConditionalRoutes {
		if _, err = tx.Exec(`INSERT INTO routes(config_ref,source_bot_id,target_bot_id,source_chat_id,target_chat_id,condition_type,condition_value,action,description,enabled,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			r.Ref, botIDs[r.SourceBotRef], botIDs[r.TargetBotRef], r.SourceChatID, r.TargetChatID, r.ConditionType, r.ConditionValue, r.Action, r.Description, r.Enabled, nowRFC3339()); err != nil {
			return receipt, err
		}
	}
	if _, err = tx.Exec(`INSERT INTO configuration_restores(digest,bots,destinations) VALUES(?,?,?)`, digest, len(snapshot.Bots), len(snapshot.Destinations)); err != nil {
		return receipt, err
	}
	if err = tx.Commit(); err != nil {
		return receipt, err
	}
	receipt.Digest = digest
	receipt.Bots = len(snapshot.Bots)
	receipt.Destinations = len(snapshot.Destinations)
	receipt.ConditionalRoutes = len(snapshot.ConditionalRoutes)
	receipt.ConfigurationCommitted = true
	return receipt, nil
}

// ConfigurationBotIDs resolves portable identities only after a full commit.
func (s *Store) ConfigurationBotIDs() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT config_ref,id FROM bots`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[string]int64{}
	for rows.Next() {
		var ref string
		var id int64
		if err := rows.Scan(&ref, &id); err != nil {
			return nil, err
		}
		ids[ref] = id
	}
	return ids, rows.Err()
}
