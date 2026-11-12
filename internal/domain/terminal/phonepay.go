package terminal

// ActionPhoneCharge journals each step of a "Send to phone" charge (contract
// §2.10z: sent, opened, recorded, declined, cancelled, expired, unrecorded).
// A Pi action: a terminal can't post it itself.
const ActionPhoneCharge = "phone.charge"

func init() { reserved[ActionPhoneCharge] = true }
