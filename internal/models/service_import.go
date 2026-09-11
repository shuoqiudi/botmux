package models

// ServiceImport pins the observed source and physical recipients before cutover.
// MigrationID makes retries safe even after subscriptions are later cancelled.
type ServiceImport struct {
	MigrationID string               `json:"migration_id"`
	Services    []ServiceImportEntry `json:"services"`
}

type ServiceImportEntry struct {
	WorkloadID   int64                      `json:"workload_id"`
	Fingerprint  string                     `json:"fingerprint"`
	DisplayName  string                     `json:"display_name"`
	Destinations []ServiceImportDestination `json:"destinations"`
}

type ServiceImportDestination struct {
	DestinationID int64 `json:"destination_id"`
	BotAccountID  int64 `json:"bot_account_id"`
	TelegramBotID int64 `json:"telegram_bot_id"`
	ChatID        int64 `json:"chat_id"`
}
