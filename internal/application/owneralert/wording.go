package owneralert

import (
	"fmt"
	"strings"
	"time"
)

// The owner's words: plain English, no technical terms, what to do. Each
// builder returns the Alert with its dedup key.

func printerName(name string) string {
	n := strings.TrimSpace(name)
	if n == "" {
		n = "a"
	}
	if strings.HasSuffix(strings.ToLower(n), "printer") {
		return n
	}
	return n + " printer"
}

// PrinterDown: a printer failing for a while.
func PrinterDown(name string) Alert {
	p := printerName(name)
	return Alert{
		Key:   "printer_down:" + name,
		Text:  fmt.Sprintf("The %s is off or out of paper. Check it: turn it on, add paper, and check its cable.", p),
		Short: fmt.Sprintf("the %s was off", p),
	}
}

// PrinterBack: the printer works again (only after PrinterDown was sent).
func PrinterBack(name string) Alert {
	p := printerName(name)
	return Alert{
		Key:   "printer_back:" + name,
		Text:  fmt.Sprintf("The %s is working again.", p),
		Short: fmt.Sprintf("the %s came back", p),
	}
}

// NotPrinted: an order printed on no printer. label is the order's number
// ("#1104"), never the customer.
func NotPrinted(orderID, label string) Alert {
	if label == "" {
		label = "an order"
	} else {
		label = "Order " + label
	}
	return Alert{
		Key:   "not_printed:" + orderID,
		Text:  fmt.Sprintf("%s did not print on any printer. Check the printers, then print it again from the tablet.", label),
		Short: strings.ToLower(label[:1]) + label[1:] + " did not print",
		Sound: "siren",
	}
}

// Offline: the store's computer can't reach the ordering system.
func Offline(d time.Duration) Alert {
	return Alert{
		Key: "offline",
		Text: fmt.Sprintf("The store has been offline for %s: new online orders can't reach the store. "+
			"Check the internet: turn the router off and on again.", minutes(d)),
		Short: "the store was offline",
	}
}

// Online: the connection is back.
func Online() Alert {
	return Alert{
		Key:   "online",
		Text:  "The store is back online. New online orders are coming in again.",
		Short: "the store came back online",
	}
}

// Waiting: orders nobody accepted.
func Waiting(n int, oldest time.Duration) Alert {
	text := fmt.Sprintf("An order has been waiting %s to be accepted. Accept it on the tablet.", minutes(oldest))
	if n > 1 {
		text = fmt.Sprintf("%d orders are waiting to be accepted (the oldest for %s). Accept them on the tablet.", n, minutes(oldest))
	}
	return Alert{Key: "orders_waiting", Text: text, Short: "orders were waiting to be accepted", Sound: "siren"}
}

// CloseBatch: yesterday's orders still open in the morning. day = the
// store-local date (one a day).
func CloseBatch(day string) Alert {
	return Alert{
		Key:   "close_batch:" + day,
		Text:  "Yesterday's orders were not closed out. On the tablet, tap Close batch so the day's reports print.",
		Short: "yesterday was not closed out",
	}
}

func minutes(d time.Duration) string {
	m := int(d / time.Minute)
	if m <= 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", m)
}
