// Package menuview is the agent's copy of the few types of main's
// internal/domain/menuview that the pricing engine reads: a channel menu's
// categories, items and modifier groups (a view build). The pricing package
// next door is a byte copy of main's (agent/scripts/sync-pricing.sh); its
// only change is the import path of this package. Keep these types' JSON in
// step with main's menuview/compile.go (the shared pricing cases decode
// through them, so a missing field fails TestSharedCases).
package menuview

import "github.com/google/uuid"

// BuildCategory is one category of a channel menu.
type BuildCategory struct {
	Key      string      `json:"key"`
	Name     string      `json:"name"`
	Position int         `json:"position"`
	Items    []BuildItem `json:"items"`
}

// BuildItem is one thing a channel can sell. ID is the bank item's id for an
// item the view shows as is (or changed by an item entry), the entry's id for
// a merge or an augment; Anchor says which bank items it is.
type BuildItem struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Photo       string          `json:"photo"`
	Anchor      Anchor          `json:"anchor"`
	PriceCents  int64           `json:"price_cents"`
	Sizes       []BuildItemSize `json:"sizes"`
	SoldOut     bool            `json:"sold_out"`
	// ModifierGroupIDs are the item's groups, in order.
	ModifierGroupIDs []uuid.UUID   `json:"modifier_group_ids"`
	Choices          []BuildChoice `json:"choices,omitempty"`
}

// Anchor names the bank items (and size) a view item stands for.
type Anchor struct {
	ItemIDs []uuid.UUID `json:"item_ids"`
	SizeID  *uuid.UUID  `json:"size_id,omitempty"`
}

type BuildItemSize struct {
	SizeID     uuid.UUID `json:"size_id"`
	Name       string    `json:"name"`
	PriceCents int64     `json:"price_cents"`
}

// BuildChoice is one bank item a merged view item stands for.
type BuildChoice struct {
	ItemID     uuid.UUID       `json:"item_id"`
	Name       string          `json:"name"`
	PriceCents int64           `json:"price_cents"`
	Sizes      []BuildItemSize `json:"sizes"`
	SoldOut    bool            `json:"sold_out"`
}

type BuildGroup struct {
	ID            uuid.UUID     `json:"id"`
	Name          string        `json:"name"`
	Required      bool          `json:"required"`
	MinSelect     int           `json:"min_select"`
	MaxSelect     int           `json:"max_select"`
	AllowMultiple bool          `json:"allow_multiple"`
	Portions      bool          `json:"portions"`
	Options       []BuildOption `json:"options"`
}

type BuildOption struct {
	ID         uuid.UUID     `json:"id"`
	Name       string        `json:"name"`
	PriceCents int64         `json:"price_cents"`
	Prices     []OptionPrice `json:"prices"`
	IsDefault  bool          `json:"is_default"`
	SoldOut    bool          `json:"sold_out"`
}

// OptionPrice is a modifier option's price on an item of one size (main's
// menu.OptionPrice).
type OptionPrice struct {
	SizeID     uuid.UUID `json:"size_id"`
	PriceCents int64     `json:"price_cents"`
}
