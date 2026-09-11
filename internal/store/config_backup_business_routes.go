package store

import (
	"database/sql"
	"encoding/json"

	"github.com/skrashevich/botmux/internal/configbackup"
	"github.com/skrashevich/botmux/internal/models"
)

func (s *Store) exportBusinessRoutesTx(tx *sql.Tx, snapshot *configbackup.Snapshot) error {
	err := scanConfigurationRows(tx, `SELECT r.route_key,r.display_name,COALESCE(b.config_ref,''),COALESCE(d.config_ref,''),r.inbound_target,r.inbound_enabled,r.inbound_backend_url,COALESCE(h.health_url,''),r.inbound_backend_token_ciphertext,r.outbound_enabled,r.allowed_callers,r.enabled,r.status
 FROM gateway_business_routes r LEFT JOIN gateway_bot_accounts a ON a.id=r.bot_account_id LEFT JOIN bots b ON b.id=a.native_bot_id
 LEFT JOIN gateway_telegram_destinations d ON d.id=r.destination_id AND d.bot_account_id=r.bot_account_id LEFT JOIN gateway_backend_health_config h ON h.route_id=r.id`, func(rows *sql.Rows) error {
		var r configbackup.BusinessRoute
		var ciphertext, callers string
		if err := rows.Scan(&r.RouteKey, &r.DisplayName, &r.BotRef, &r.DestinationRef, &r.InboundTarget, &r.InboundEnabled, &r.InboundBackendURL, &r.InboundBackendHealthURL, &ciphertext, &r.OutboundEnabled, &callers, &r.Enabled, &r.Status); err != nil {
			return err
		}
		var err error
		r.InboundBackendToken, err = s.openSecret(ciphertext)
		if err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(callers), &r.AllowedCallers); err != nil {
			return err
		}
		if r.AllowedCallers == nil {
			r.AllowedCallers = []string{}
		}
		snapshot.BusinessRoutes = append(snapshot.BusinessRoutes, r)
		return nil
	})
	if err != nil {
		return err
	}
	workloads := map[string]int{}
	for i, w := range snapshot.Workloads {
		workloads[w.Ref] = i
	}
	return scanConfigurationRows(tx, `SELECT COALESCE(w.config_ref,''),COALESCE(r.route_key,''),p.action FROM gateway_route_permissions p LEFT JOIN gateway_workloads w ON w.id=p.workload_id LEFT JOIN gateway_business_routes r ON r.id=p.route_id`, func(rows *sql.Rows) error {
		var ref string
		var p configbackup.RoutePermission
		if err := rows.Scan(&ref, &p.RouteKey, &p.Action); err != nil {
			return err
		}
		i, ok := workloads[ref]
		if !ok {
			return &configbackup.FieldError{Kind: configbackup.ErrInvalid, Path: "snapshot.workloads", Reason: "permission source missing or reserved"}
		}
		snapshot.Workloads[i].RoutePermissions = append(snapshot.Workloads[i].RoutePermissions, p)
		return nil
	})
}

func (s *Store) restoreBusinessRoutesTx(tx *sql.Tx, snapshot configbackup.Snapshot) error {
	for _, r := range snapshot.BusinessRoutes {
		var destinationID, accountID int64
		if err := tx.QueryRow(`SELECT id,bot_account_id FROM gateway_telegram_destinations WHERE config_ref=?`, r.DestinationRef).Scan(&destinationID, &accountID); err != nil {
			return err
		}
		// The normal insertion path seals the backend credential with this Store's
		// key and creates revision 1. No source validation or health state is copied.
		_, err := insertBusinessRoute(s, tx, models.BusinessRoute{
			RouteKey: r.RouteKey, DisplayName: r.DisplayName, BotAccountID: accountID, DestinationID: destinationID,
			InboundTarget: r.InboundTarget, InboundEnabled: r.InboundEnabled, InboundBackendURL: r.InboundBackendURL,
			InboundBackendHealthURL: r.InboundBackendHealthURL, InboundBackendToken: r.InboundBackendToken,
			OutboundEnabled: r.OutboundEnabled, AllowedCallers: r.AllowedCallers, Enabled: r.Enabled, Status: models.GatewayRouteStatus(r.Status),
		})
		if err != nil {
			return err
		}
	}
	for _, w := range snapshot.Workloads {
		for _, p := range w.RoutePermissions {
			if _, err := tx.Exec(`INSERT INTO gateway_route_permissions(workload_id,route_id,action,created_at) VALUES((SELECT id FROM gateway_workloads WHERE config_ref=?),(SELECT id FROM gateway_business_routes WHERE route_key=?),?,?)`, w.Ref, p.RouteKey, p.Action, nowRFC3339()); err != nil {
				return err
			}
		}
	}
	return nil
}
