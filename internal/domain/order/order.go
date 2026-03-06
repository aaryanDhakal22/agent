package order

// OrderRequest is the aggregate root representing a full order payload received from SQS.
type OrderSQSMessage struct {
	ID          string
	Payload     string
	DateCreated string
	CreatedAt   string
}

type OrderRequest struct {
	TVer          string `json:"tVer"`
	OrderID       int    `json:"order_id"`
	StoreID       int64  `json:"store_id"`
	VendorStoreID string `json:"vendor_store_id"`
	StoreName     string `json:"store_name"`
	ServiceType   string `json:"service_type"`
	SubmittedDate string `json:"submitted_date"`
	PrintDate     string `json:"print_date"`
	DeferredDate  string `json:"deferred_date"`

	MiscCharges []MiscCharge `json:"misc_charges"`
	Tip         float64      `json:"tip"`
	Taxes       []Tax        `json:"taxes"`

	IsTaxExempt  bool    `json:"is_tax_exempt"`
	OrderTotal   float64 `json:"order_total"`
	BalanceOwing float64 `json:"balance_owing"`
	Notes        string  `json:"notes"`

	Customer         Customer          `json:"customer"`
	DeliveryAddress  *DeliveryAddress  `json:"delivery_address,omitempty"`
	DeliveryProvider *DeliveryProvider `json:"delivery_provider,omitempty"`

	Payments []Payment `json:"payments"`
	Items    []Item    `json:"items"`
	Coupons  []Coupon  `json:"coupons"`
}

type MiscCharge struct {
	MiscChargeName   string  `json:"misc_charge_name"`
	MiscChargeDesc   string  `json:"misc_charge_desc"`
	MiscChargeAmount float64 `json:"misc_charge_amount"`
}

type Tax struct {
	TaxName   string  `json:"tax_name"`
	TaxAmount float64 `json:"tax_amount"`
}

type Customer struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Company   string `json:"company"`
	Phone     string `json:"phone"`
	Ext       string `json:"ext"`
	Email     string `json:"email"`
}

type DeliveryAddress struct {
	Street       string `json:"street"`
	CrossStreets string `json:"cross_streets"`
	Suite        string `json:"suite"`
	Buz          string `json:"buz"`
	City         string `json:"city"`
	State        string `json:"state"`
	Zip          string `json:"zip"`
}

type DeliveryProvider struct {
	ProviderName string `json:"provider_name"`
	Status       string `json:"status"`
	DeliveryID   string `json:"delivery_id"`
	TrackingURL  string `json:"tracking_url"`
	PickupDate   string `json:"pickup_date"`
}

type Payment struct {
	Type          string  `json:"type"`
	Amount        float64 `json:"amount"`
	CardNumber    string  `json:"card_number"`
	CardHolder    string  `json:"card_holder"`
	AuthCode      string  `json:"auth_code"`
	TransactionID string  `json:"transaction_id"`
	Token         string  `json:"token"`
}

type Item struct {
	Name      string     `json:"name"`
	SizeID    int        `json:"size_id"`
	SizeName  string     `json:"size_name"`
	Quantity  int        `json:"quantity"`
	Price     float64    `json:"price"`
	PLU       string     `json:"plu"`
	Who       string     `json:"who"`
	GroupID   string     `json:"group_id"`
	Notes     string     `json:"notes"`
	Modifiers []Modifier `json:"modifiers"`
}

type Modifier struct {
	Side     string  `json:"side"`
	Name     string  `json:"name"`
	Quantity int     `json:"quantity"`
	PLU      string  `json:"plu"`
	Price    float64 `json:"price"`
	Action   string  `json:"action"`
}

type Coupon struct {
	Serial  string  `json:"serial"`
	PLU     string  `json:"plu"`
	Name    string  `json:"name"`
	Value   float64 `json:"value"`
	GroupID string  `json:"group_id"`
}
