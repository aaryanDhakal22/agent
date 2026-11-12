package terminal

// The terminal's Dispatch and Pickup bars (contract/agent.md §2.10v):
// journal actions the Pi writes for them.
const (
	// ActionSendOut: a driver sent out from a terminal {driver_id,
	// driver_name, order_ids, numbers, mark_ready}.
	ActionSendOut = "delivery.send_out"
	// ActionDriverBack: a driver marked back from a terminal {driver_id,
	// order_ids (delivered)}.
	ActionDriverBack = "driver.returned"
	// ActionOrderTime: an order's ready / delivery time moved {number, from?,
	// to, type}.
	ActionOrderTime = "order.time"
)
