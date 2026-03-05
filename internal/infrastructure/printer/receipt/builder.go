package receipt

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/order"
)

const (
	lineWidth = 48

	// ESC/POS commands
	cmdInit       = "\x1b\x40"         // ESC @ - initialize printer
	cmdCenter     = "\x1b\x61\x01"     // ESC a 1 - center align
	cmdLeft       = "\x1b\x61\x00"     // ESC a 0 - left align
	cmdRight      = "\x1b\x61\x02"     // ESC a 2 - right align
	cmdBoldOn     = "\x1b\x45\x01"     // ESC E 1 - bold on
	cmdBoldOff    = "\x1b\x45\x00"     // ESC E 0 - bold off
	cmdDoubleSz   = "\x1d\x21\x11"     // GS ! 0x11 - double width+height
	cmdNormalSz   = "\x1d\x21\x00"     // GS ! 0x00 - normal size
	cmdFeed       = "\x0a"             // line feed
	cmdCut        = "\x1d\x56\x42\x03" // GS V 66 3 - full cut
)

// Build converts an OrderRequest into raw ESC/POS bytes for an 80mm thermal printer.
func Build(o order.OrderRequest) []byte {
	var buf bytes.Buffer

	w := func(s string) { buf.WriteString(s) }
	nl := func() { w(cmdFeed) }

	w(cmdInit)

	// --- Header ---
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdDoubleSz)
	serviceLabel := formatServiceType(o.ServiceType)
	w(serviceLabel)
	nl()
	w(cmdNormalSz)

	if o.StoreName != "" {
		w(o.StoreName)
		nl()
	}
	w(cmdBoldOff)

	// --- Order # and date ---
	w(cmdLeft)
	orderNum := fmt.Sprintf("Order#%d", o.OrderID)
	placedOn := formatDate(o.SubmittedDate)
	w(leftRight(orderNum, placedOn, lineWidth))
	nl()

	// --- Customer info ---
	cust := o.Customer
	fullName := strings.ToUpper(fmt.Sprintf("%s, %s", cust.LastName, cust.FirstName))
	if cust.FirstName != "" || cust.LastName != "" {
		w(fmt.Sprintf("Customer Name: %s", fullName))
		nl()
	}
	if cust.Phone != "" {
		w(cmdBoldOn)
		w(fmt.Sprintf("Phone: %s", cust.Phone))
		w(cmdBoldOff)
		nl()
	}
	if cust.Email != "" {
		w(fmt.Sprintf("Email: %s", cust.Email))
		nl()
	}

	// --- Delivery address ---
	if o.DeliveryAddress != nil {
		da := o.DeliveryAddress
		if da.Street != "" {
			w(cmdBoldOn)
			w(fmt.Sprintf("Street: %s", da.Street))
			w(cmdBoldOff)
			nl()
		}
		if da.City != "" || da.State != "" || da.Zip != "" {
			w(fmt.Sprintf("City/State: %s, %s %s", da.City, da.State, da.Zip))
			nl()
		}
	}

	// --- Notes ---
	if o.Notes != "" {
		w(separator())
		nl()
		w(fmt.Sprintf("Notes: %s", o.Notes))
		nl()
	}

	// --- Payments ---
	w(separator())
	nl()
	w(cmdCenter)
	w(cmdBoldOn)
	w("PAYMENTS")
	w(cmdBoldOff)
	nl()
	w(cmdLeft)

	for _, p := range o.Payments {
		w(fmt.Sprintf("Payment Type: %s", p.Type))
		nl()
		if p.CardNumber != "" {
			w(fmt.Sprintf("Card: %s", p.CardNumber))
			nl()
		}
	}
	nl()
	w(cmdBoldOn)
	w(fmt.Sprintf("Balance Owing: $%.2f", o.BalanceOwing))
	w(cmdBoldOff)
	nl()

	// --- Items ---
	w(separator())
	nl()
	w(cmdBoldOn)
	w(columnHeader())
	w(cmdBoldOff)
	nl()

	itemsSubtotal := 0.0
	for _, item := range o.Items {
		w(separator())
		nl()
		itemTotal := float64(item.Quantity) * item.Price
		itemsSubtotal += itemTotal

		// Item name line: qty | name + size | price
		nameWithSize := item.Name
		if item.SizeName != "" {
			nameWithSize += " (" + item.SizeName + ")"
		}
		w(itemLine(item.Quantity, nameWithSize, item.Price))
		nl()

		// Modifiers
		for _, mod := range item.Modifiers {
			modLabel := mod.Name
			w(modifierLine(modLabel, mod.Price))
			nl()
		}

		// Item sum
		w(rightAlign(fmt.Sprintf("Sum: $%.2f", itemTotal), lineWidth))
		nl()
	}

	w(separator())
	nl()

	// --- Misc charges ---
	for _, mc := range o.MiscCharges {
		label := mc.MiscChargeName
		if label == "" {
			label = mc.MiscChargeDesc
		}
		w(rightPair(label+":", fmt.Sprintf("$%.2f", mc.MiscChargeAmount)))
		nl()
	}

	// --- Totals ---
	subtotal := itemsSubtotal
	for _, mc := range o.MiscCharges {
		subtotal += mc.MiscChargeAmount
	}

	w(rightPair("Subtotal:", fmt.Sprintf("$%.2f", subtotal)))
	nl()

	for _, tax := range o.Taxes {
		w(rightPair(tax.TaxName+":", fmt.Sprintf("$%.2f", tax.TaxAmount)))
		nl()
	}

	taxTotal := 0.0
	for _, t := range o.Taxes {
		taxTotal += t.TaxAmount
	}
	total := subtotal + taxTotal

	w(rightPair("Total:", fmt.Sprintf("$%.2f", total)))
	nl()

	if o.Tip > 0 {
		w(rightPair("Gratuity:", fmt.Sprintf("$%.2f", o.Tip)))
		nl()
	}

	w(cmdBoldOn)
	w(rightPair("Order Total:", fmt.Sprintf("$%.2f", o.OrderTotal)))
	w(cmdBoldOff)
	nl()

	// --- Feed and cut ---
	for i := 0; i < 4; i++ {
		nl()
	}
	w(cmdCut)

	return buf.Bytes()
}

func separator() string {
	return strings.Repeat("-", lineWidth)
}

// leftRight prints left and right strings on the same line padded to width.
func leftRight(left, right string, width int) string {
	space := width - len(left) - len(right)
	if space < 1 {
		space = 1
	}
	return left + strings.Repeat(" ", space) + right
}

func rightAlign(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return strings.Repeat(" ", width-len(s)) + s
}

// rightPair formats "Label:      $X.XX" right-aligned in lineWidth.
func rightPair(label, value string) string {
	return leftRight(strings.Repeat(" ", 20)+label, value, lineWidth)
}

// columnHeader returns the items table header.
func columnHeader() string {
	// Qty(4) + space(1) + Item(37) + Price(6)
	return fmt.Sprintf("%-4s %-37s %6s", "Qty", "Item", "Price")
}

// itemLine formats a single item row.
func itemLine(qty int, name string, price float64) string {
	maxName := 37
	if len(name) > maxName {
		name = name[:maxName]
	}
	return fmt.Sprintf("%-4d %-37s %6s", qty, name, fmt.Sprintf("$%.2f", price))
}

// modifierLine formats a modifier indented under an item.
func modifierLine(name string, price float64) string {
	indent := "     " // 5 spaces (align under item name)
	maxName := 37
	if len(name) > maxName {
		name = name[:maxName]
	}
	return fmt.Sprintf("%s%-37s %6s", indent, name, fmt.Sprintf("$%.2f", price))
}

// formatServiceType returns a human-readable header for the service type.
func formatServiceType(t string) string {
	switch strings.ToLower(t) {
	case "delivery":
		return "Online Order (Delivery)"
	case "pickup":
		return "Online Order (Pickup)"
	default:
		if t != "" {
			return "Online Order (" + t + ")"
		}
		return "Online Order"
	}
}

// formatDate parses ISO datetime and returns a human-readable string.
func formatDate(s string) string {
	formats := []string{
		"2006-01-02T15:04:05-0700",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		time.RFC3339,
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t.Format("Mon, Jan 2 2006 @ 3:04 PM")
		}
	}
	return s
}
