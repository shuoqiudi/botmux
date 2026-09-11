package store

import (
	"database/sql"

	"github.com/skrashevich/botmux/internal/configbackup"
)

// The embedded adapter has no portable source identity. It may only be omitted
// in its pristine initialization state and with no source configuration attached.
const portableWorkload = `NOT (w.id=-1234 AND w.name='Embedded Gateway Adapter' AND w.status='active' AND w.service_publish=0 AND w.service_query=0)`

func scanConfigurationRows(tx *sql.Tx, query string, scan func(*sql.Rows) error) error {
	rows, err := tx.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportNotificationsTx(tx *sql.Tx, snapshot *configbackup.Snapshot) error {
	workloads := map[string]int{}
	err := scanConfigurationRows(tx, `SELECT w.config_ref,w.name,w.status,w.service_publish,w.service_query FROM gateway_workloads w WHERE `+portableWorkload, func(rows *sql.Rows) error {
		w := configbackup.Workload{Credentials: []configbackup.Credential{}, RoutePermissions: []configbackup.RoutePermission{}}
		if err := rows.Scan(&w.Ref, &w.Name, &w.Status, &w.ServicePermissions.Publish, &w.ServicePermissions.Query); err != nil {
			return err
		}
		workloads[w.Ref] = len(snapshot.Workloads)
		snapshot.Workloads = append(snapshot.Workloads, w)
		return nil
	})
	if err != nil {
		return err
	}
	err = scanConfigurationRows(tx, `SELECT COALESCE(w.config_ref,''),c.name,c.credential_hash,c.enabled FROM gateway_workload_credentials c LEFT JOIN gateway_workloads w ON w.id=c.workload_id`, func(rows *sql.Rows) error {
		var ref string
		c := configbackup.Credential{Algorithm: "sha256"}
		if err := rows.Scan(&ref, &c.Name, &c.Verifier, &c.Enabled); err != nil {
			return err
		}
		i, ok := workloads[ref]
		if !ok {
			return &configbackup.FieldError{Kind: configbackup.ErrInvalid, Path: "snapshot.workloads", Reason: "credential source missing or reserved"}
		}
		snapshot.Workloads[i].Credentials = append(snapshot.Workloads[i].Credentials, c)
		return nil
	})
	if err != nil {
		return err
	}
	err = scanConfigurationRows(tx, `SELECT COALESCE(w.config_ref,''),n.fingerprint,n.display_name FROM gateway_notification_services n LEFT JOIN gateway_workloads w ON w.id=n.workload_id`, func(rows *sql.Rows) error {
		var n configbackup.NotificationService
		if err := rows.Scan(&n.WorkloadRef, &n.Fingerprint, &n.DisplayName); err != nil {
			return err
		}
		snapshot.NotificationServices = append(snapshot.NotificationServices, n)
		return nil
	})
	if err != nil {
		return err
	}
	return scanConfigurationRows(tx, `SELECT p.config_ref,COALESCE(w.config_ref,''),COALESCE(n.fingerprint,''),COALESCE(d.config_ref,''),p.active FROM gateway_service_subscriptions p LEFT JOIN gateway_notification_services n ON n.id=p.service_id LEFT JOIN gateway_workloads w ON w.id=n.workload_id LEFT JOIN gateway_telegram_destinations d ON d.id=p.destination_id`, func(rows *sql.Rows) error {
		var p configbackup.Subscription
		if err := rows.Scan(&p.Ref, &p.WorkloadRef, &p.Fingerprint, &p.DestinationRef, &p.Active); err != nil {
			return err
		}
		snapshot.Subscriptions = append(snapshot.Subscriptions, p)
		return nil
	})
}

func restoreNotificationsTx(tx *sql.Tx, snapshot configbackup.Snapshot) error {
	workloadIDs := map[string]int64{}
	for _, w := range snapshot.Workloads {
		res, err := tx.Exec(`INSERT INTO gateway_workloads(config_ref,name,status,service_publish,service_query,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, w.Ref, w.Name, w.Status, w.ServicePermissions.Publish, w.ServicePermissions.Query, nowRFC3339(), nowRFC3339())
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		workloadIDs[w.Ref] = id
		for _, c := range w.Credentials {
			// Install the verifier directly. Hashing it again would make the upstream
			// credential invalid and incorrectly authenticate the hash as a token.
			if _, err := tx.Exec(`INSERT INTO gateway_workload_credentials(workload_id,name,credential_hash,enabled,created_at) VALUES(?,?,?,?,?)`, id, c.Name, c.Verifier, c.Enabled, nowRFC3339()); err != nil {
				return err
			}
		}
	}
	serviceIDs := map[[2]string]int64{}
	for _, n := range snapshot.NotificationServices {
		res, err := tx.Exec(`INSERT INTO gateway_notification_services(workload_id,fingerprint,display_name,last_received_at) VALUES(?,?,?,'')`, workloadIDs[n.WorkloadRef], n.Fingerprint, n.DisplayName)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		serviceIDs[[2]string{n.WorkloadRef, n.Fingerprint}] = id
	}
	for _, p := range snapshot.Subscriptions {
		var destinationID int64
		if err := tx.QueryRow(`SELECT id FROM gateway_telegram_destinations WHERE config_ref=?`, p.DestinationRef).Scan(&destinationID); err != nil {
			return err
		}
		cancelled := ""
		if !p.Active {
			cancelled = nowRFC3339()
		}
		if _, err := tx.Exec(`INSERT INTO gateway_service_subscriptions(config_ref,service_id,destination_id,active,created_at,cancelled_at) VALUES(?,?,?,?,?,?)`, p.Ref, serviceIDs[[2]string{p.WorkloadRef, p.Fingerprint}], destinationID, p.Active, nowRFC3339(), cancelled); err != nil {
			return err
		}
	}
	return nil
}
