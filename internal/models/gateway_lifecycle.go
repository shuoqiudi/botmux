package models

// GatewayDirection, GatewayAction, GatewayDeliveryStatus, and GatewayDLQState
// keep lifecycle vocabulary explicit across HTTP, workers, Redis, and SQLite.
type GatewayDirection string
type GatewayAction string
type GatewayDeliveryStatus string
type GatewayDLQState string
type GatewayRouteStatus string

const (
	GatewayDirectionInbound  GatewayDirection = "inbound"
	GatewayDirectionOutbound GatewayDirection = "outbound"

	GatewayActionMessagesSend    GatewayAction = "messages.send"
	GatewayActionCallbacksAnswer GatewayAction = "callbacks.answer"
	GatewayActionDeliveriesRead  GatewayAction = "deliveries.read"

	GatewayDeliveryPendingEnqueue GatewayDeliveryStatus = "pending_enqueue"
	GatewayDeliveryAccepted       GatewayDeliveryStatus = "accepted"
	GatewayDeliveryProcessing     GatewayDeliveryStatus = "processing"
	GatewayDeliveryRetrying       GatewayDeliveryStatus = "retrying"
	GatewayDeliverySucceeded      GatewayDeliveryStatus = "succeeded"
	GatewayDeliveryReconciling    GatewayDeliveryStatus = "reconciling"
	GatewayDeliveryDeadLettered   GatewayDeliveryStatus = "dead-lettered"
	GatewayDeliveryReplayPending  GatewayDeliveryStatus = "replay_pending"
	GatewayDeliveryDiscarded      GatewayDeliveryStatus = "discarded"

	GatewayDLQActive        GatewayDLQState = "active"
	GatewayDLQReplayPending GatewayDLQState = "replay_pending"
	GatewayDLQReplayed      GatewayDLQState = "replayed"
	GatewayDLQDiscarded     GatewayDLQState = "discarded"

	GatewayRouteActive   GatewayRouteStatus = "active"
	GatewayRouteDisabled GatewayRouteStatus = "disabled"
)
