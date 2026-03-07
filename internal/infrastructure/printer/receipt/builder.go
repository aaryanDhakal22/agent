// filename: receipt.go
// breadcrumb: quiccpos/agent/internal/domain/receipt/receipt.go

package receipt

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/rs/zerolog/log"
)

const (
	lineWidth    = 41 // Max characters per line for 2x font on 80mm paper
	marginSpaces = 0  // Equal gap on both sides for centering

	// ESC/POS commands
	cmdInit     = "\x1b\x40"         // ESC @ - initialize printer
	cmdCenter   = "\x1b\x61\x01"     // ESC a 1 - center align
	cmdLeft     = "\x1b\x61\x00"     // ESC a 0 - left align
	cmdRight    = "\x1b\x61\x02"     // ESC a 2 - right align
	cmdBoldOn   = "\x1b\x45\x01"     // ESC E 1 - bold on
	cmdBoldOff  = "\x1b\x45\x00"     // ESC E 0 - bold off
	cmdDoubleSz = "\x1d\x21\x11"     // GS ! 0x11 - double width+height
	cmdQuadSz   = "\x1d\x21\x22"     // GS ! 0x22 - quad size (3x3)
	cmdBaseSz   = "\x1b\x21\x10"     // GS ! 0x01 - base 2x size for entire receipt
	cmdFeed     = "\x0a"             // line feed
	cmdCut      = "\x1d\x56\x42\x03" // GS V 66 3 - full cut
)

// Build converts an OrderRequest into raw ESC/POS bytes for an 80mm thermal printer.
func Build(o order.OrderRequest) []byte {
	rLogger := log.With().Str("module", "receipt-builder").Logger()

	// Print all the data that is going to be printed
	rLogger.Debug().Int("order_id", o.OrderID).Msg("Printing receipt")
	fmt.Println(o.String())
	fmt.Printf("%+v\n", o)

	var buf bytes.Buffer

	// w writes content with margins on both sides
	w := func(s string) {
		// Don't add margins to ESC/POS commands (they start with escape chars)
		if len(s) > 0 && (s[0] == '\x1b' || s[0] == '\x1d' || s[0] == '\x0a') {
			buf.WriteString(s)
		} else {
			buf.WriteString(margin(s))
		}
	}
	nl := func() { buf.WriteString(cmdFeed) }

	w(cmdInit)
	w(cmdBaseSz) // Set base DoubleHeight size for entire receipt

	// --- Header ---
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdQuadSz) // Temporarily larger for header (3x3)
	serviceLabel := formatServiceType(o.ServiceType)
	w(serviceLabel)
	nl()
	w(cmdBoldOff)
	nl()

	// --- Customer info ---
	cust := o.Customer
	fullName := strings.ToUpper(fmt.Sprintf("%s, %s", cust.LastName, cust.FirstName))
	if cust.FirstName != "" || cust.LastName != "" {
		w(cmdBoldOn)
		w(cmdDoubleSz)
		w(fmt.Sprintf("%s", fullName))
		w(cmdBaseSz)
		w(cmdBoldOff)
		nl()
	}

	if cust.Phone != "" {
		nl()
		w(cmdBoldOn)
		w(cmdDoubleSz)
		// Phone number format
		phoneNum := fmt.Sprintf("%s-%s-%s", cust.Phone[:3], cust.Phone[3:6], cust.Phone[6:])
		w(fmt.Sprintf("Phone: %s", phoneNum))
		w(cmdBoldOff)
		w(cmdBaseSz)
		nl()
	}

	// -- Placed On ---
	w(separator())
	nl()
	w(cmdBoldOn)
	placedOn := formatDate(o.SubmittedDate)
	w(placedOn)
	w(cmdBoldOff)
	nl()

	// --- Delivery address ---
	if o.DeliveryAddress != nil {
		da := o.DeliveryAddress
		if da.Street != "" {
			w(separator())
			nl()
			w(cmdBoldOn)
			w(fmt.Sprintf("Street: %s", da.Street))
			w(cmdBoldOff)
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
	w(cmdCenter)
	nl()
	w(cmdBoldOn)
	w(cmdDoubleSz)
	rLogger.Debug().Msg(fmt.Sprintf("Payments: %v", o.Payments))
	if o.Payments == nil {
		rLogger.Debug().Msg("No payments")
		w(fmt.Sprintf("CASH - $%.2f", o.OrderTotal))
	} else {
		rLogger.Debug().Msg("Payments")
		w("PAID - CARD")
	}
	w(cmdBaseSz)
	w(cmdLeft)
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

		w(cmdBoldOn)
		w(itemLine(item.Quantity, item.SizeName, item.Price))
		w(cmdBoldOff)
		nl()
		w(cmdRight)
		w(item.Name)
		nl()
		w(cmdLeft)

		// Modifiers
		for _, mod := range item.Modifiers {
			modLabel := mod.Name
			if strings.Contains(mod.Name, "fries") ||
				strings.Contains(mod.Name, "salad") ||
				strings.Contains(mod.Name, "Fries") ||
				strings.Contains(mod.Name, "Salad") {
				w(cmdBoldOn)
			}

			w(modifierLine(modLabel, mod.Price))
			w(cmdBoldOff)
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
		label := mc.MiscChargeDesc
		if o.ServiceType != "delivery" && mc.MiscChargeName == "Delivery" {
			continue
		}
		w(rightPair(label+":", fmt.Sprintf("$%.2f", mc.MiscChargeAmount)))
		nl()
	}

	// --- Totals ---
	subtotal := itemsSubtotal
	w(rightPair("Subtotal:", fmt.Sprintf("$%.2f", subtotal)))
	nl()

	for _, tax := range o.Taxes {
		w(rightPair(tax.TaxName+" (6.0%):", fmt.Sprintf("$%.2f", tax.TaxAmount)))
		nl()
	}

	w(cmdBoldOn)

	w(rightPair("Total:", fmt.Sprintf("$%.2f", o.OrderTotal)))
	nl()
	w(cmdBoldOff)

	if o.Tip > 0 {
		w(rightPair("Tips :", fmt.Sprintf("$%.2f", o.Tip)))
		nl()
	}
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w(cmdCenter)
	nl()
	w("Order Total:" + fmt.Sprintf("$%.2f", o.OrderTotal))

	w(cmdBoldOff)
	w(cmdBaseSz)
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

// margin adds equal spacing on both sides of content for centering
func margin(s string) string {
	margins := strings.Repeat(" ", marginSpaces)
	return margins + s
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
	return leftRight(strings.Repeat(" ", 19)+label, value, lineWidth)
}

// columnHeader returns the items table header.
func columnHeader() string {
	// Qty(3) + space(1) + Item(29) + Price(7)
	return fmt.Sprintf("%-3s %-29s %7s", "Qty", "Item", "Price")
}

// itemLine formats a single item row.
func itemLine(qty int, name string, price float64) string {
	maxName := 29
	if len(name) > maxName {
		name = name[:maxName]
	}
	return fmt.Sprintf("%-3d %-29s %7s", qty, name, fmt.Sprintf("$%.2f", price))
}

// modifierLine formats a modifier indented under an item.
func modifierLine(name string, price float64) string {
	indent := "      " // 6 spaces (align under item name)
	maxName := 27
	if len(name) > maxName {
		name = name[:maxName]
	}
	return fmt.Sprintf("%s%-27s %7s", indent, name, fmt.Sprintf("$%.2f", price))
}

// formatServiceType returns a human-readable header for the service type.
func formatServiceType(t string) string {
	switch strings.ToLower(t) {
	case "delivery":
		return "Delivery"
	case "pickup":
		return "Pickup"
	default:
		if t != "" {
			return strings.ToUpper(t)
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
			return t.Format("Jan 2, 2006 ----------  3:04 PM")
			// return t.Format("Mon, Jan 2 2006 @ 3:04 PM")
		}
	}
	return s
}
