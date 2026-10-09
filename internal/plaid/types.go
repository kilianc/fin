package plaid

import "time"

type LinkTokenCreateRequest struct {
	ClientName   string   `json:"client_name"`
	Language     string   `json:"language"`
	CountryCodes []string `json:"country_codes"`
	User         LinkUser `json:"user"`
	Products     []string `json:"products,omitempty"`
	// RequiredIfSupportedProducts are added when the chosen institution
	// supports them, without hiding institutions that do not.
	RequiredIfSupportedProducts []string `json:"required_if_supported_products,omitempty"`
	AccessToken                 string   `json:"access_token,omitempty"`
	// AdditionalConsentedProducts asks for consent to more products on an
	// existing Item in update mode.
	AdditionalConsentedProducts []string          `json:"additional_consented_products,omitempty"`
	Transactions                *LinkTransactions `json:"transactions,omitempty"`
	HostedLink                  *HostedLink       `json:"hosted_link,omitempty"`
}

type LinkUser struct {
	ClientUserID string `json:"client_user_id"`
}

type LinkTransactions struct {
	DaysRequested int `json:"days_requested,omitempty"`
}

type HostedLink struct {
	URLLifetimeSeconds int `json:"url_lifetime_seconds,omitempty"`
}

type LinkTokenCreateResponse struct {
	LinkToken     string    `json:"link_token"`
	Expiration    time.Time `json:"expiration"`
	HostedLinkURL string    `json:"hosted_link_url"`
}

type LinkTokenGetResponse struct {
	LinkToken    string        `json:"link_token"`
	Expiration   time.Time     `json:"expiration"`
	LinkSessions []LinkSession `json:"link_sessions"`
}

type LinkSession struct {
	LinkSessionID string       `json:"link_session_id"`
	StartedAt     *time.Time   `json:"started_at"`
	FinishedAt    *time.Time   `json:"finished_at"`
	Results       *LinkResults `json:"results"`
	Exit          *LinkExit    `json:"exit"`
	// OnSuccess and OnExit are deprecated by Plaid but still populated, and
	// finished_at is not always set, notably for update mode.
	OnSuccess *LinkOnSuccess `json:"on_success"`
	OnExit    *LinkExit      `json:"on_exit"`
	Events    []LinkEvent    `json:"events"`
}

type LinkOnSuccess struct {
	PublicToken string `json:"public_token"`
}

type LinkEvent struct {
	EventName string `json:"event_name"`
}

type LinkResults struct {
	ItemAddResults []ItemAddResult `json:"item_add_results"`
}

type ItemAddResult struct {
	PublicToken string       `json:"public_token"`
	Institution *Institution `json:"institution"`
}

type LinkExit struct {
	Error    *Error `json:"error"`
	Metadata struct {
		Status string `json:"status"`
	} `json:"metadata"`
}

type ExchangeResponse struct {
	AccessToken string `json:"access_token"`
	ItemID      string `json:"item_id"`
}

type Institution struct {
	InstitutionID string `json:"institution_id"`
	Name          string `json:"name"`
}

type InstitutionDetail struct {
	InstitutionID string   `json:"institution_id"`
	Name          string   `json:"name"`
	Products      []string `json:"products"`
	OAuth         bool     `json:"oauth"`
	URL           *string  `json:"url"`
}

type Item struct {
	ItemID                string     `json:"item_id"`
	InstitutionID         *string    `json:"institution_id"`
	Error                 *Error     `json:"error"`
	Products              []string   `json:"products"`
	ConsentExpirationTime *time.Time `json:"consent_expiration_time"`
}

type ItemGetResponse struct {
	Item   Item        `json:"item"`
	Status *ItemStatus `json:"status"`
}

type ItemStatus struct {
	Transactions *ProductStatus `json:"transactions"`
	Investments  *ProductStatus `json:"investments"`
}

type ProductStatus struct {
	LastSuccessfulUpdate *time.Time `json:"last_successful_update"`
	LastFailedUpdate     *time.Time `json:"last_failed_update"`
}

type Account struct {
	AccountID    string   `json:"account_id"`
	Name         string   `json:"name"`
	OfficialName *string  `json:"official_name"`
	Mask         *string  `json:"mask"`
	Type         string   `json:"type"`
	Subtype      *string  `json:"subtype"`
	Balances     Balances `json:"balances"`
}

type Balances struct {
	Available              *float64 `json:"available"`
	Current                *float64 `json:"current"`
	Limit                  *float64 `json:"limit"`
	IsoCurrencyCode        *string  `json:"iso_currency_code"`
	UnofficialCurrencyCode *string  `json:"unofficial_currency_code"`
}

type AccountsResponse struct {
	Accounts []Account `json:"accounts"`
	Item     Item      `json:"item"`
}

type TransactionsSyncResponse struct {
	Added                    []Transaction        `json:"added"`
	Modified                 []Transaction        `json:"modified"`
	Removed                  []RemovedTransaction `json:"removed"`
	NextCursor               string               `json:"next_cursor"`
	HasMore                  bool                 `json:"has_more"`
	TransactionsUpdateStatus string               `json:"transactions_update_status"`
	Accounts                 []Account            `json:"accounts"`
}

// Transaction amounts follow Plaid's sign: positive is money leaving the
// account (a purchase), negative is money coming in (a refund or deposit).
type Transaction struct {
	TransactionID           string                   `json:"transaction_id"`
	AccountID               string                   `json:"account_id"`
	Amount                  float64                  `json:"amount"`
	IsoCurrencyCode         *string                  `json:"iso_currency_code"`
	Date                    string                   `json:"date"`
	AuthorizedDate          *string                  `json:"authorized_date"`
	Name                    string                   `json:"name"`
	MerchantName            *string                  `json:"merchant_name"`
	Pending                 bool                     `json:"pending"`
	PaymentChannel          string                   `json:"payment_channel"`
	PersonalFinanceCategory *PersonalFinanceCategory `json:"personal_finance_category"`
}

type PersonalFinanceCategory struct {
	Primary  string `json:"primary"`
	Detailed string `json:"detailed"`
}

type RemovedTransaction struct {
	TransactionID string `json:"transaction_id"`
	AccountID     string `json:"account_id"`
}

type Security struct {
	SecurityID       string   `json:"security_id"`
	Name             *string  `json:"name"`
	TickerSymbol     *string  `json:"ticker_symbol"`
	Type             *string  `json:"type"`
	IsCashEquivalent *bool    `json:"is_cash_equivalent"`
	ClosePrice       *float64 `json:"close_price"`
	IsoCurrencyCode  *string  `json:"iso_currency_code"`
}

type Holding struct {
	AccountID            string   `json:"account_id"`
	SecurityID           string   `json:"security_id"`
	Quantity             float64  `json:"quantity"`
	InstitutionPrice     float64  `json:"institution_price"`
	InstitutionPriceAsOf *string  `json:"institution_price_as_of"`
	InstitutionValue     float64  `json:"institution_value"`
	CostBasis            *float64 `json:"cost_basis"`
	IsoCurrencyCode      *string  `json:"iso_currency_code"`
	TaxLots              []TaxLot `json:"tax_lots"`
}

// TaxLot is one purchase lot of a holding. Institutions that do not report
// lots return an empty list.
type TaxLot struct {
	InstitutionLotID         *string  `json:"institution_lot_id"`
	OriginalPurchaseDatetime *string  `json:"original_purchase_datetime"`
	Quantity                 *float64 `json:"quantity"`
	PurchasePrice            *float64 `json:"purchase_price"`
	CostBasis                *float64 `json:"cost_basis"`
	CurrentValue             *float64 `json:"current_value"`
	PositionType             *string  `json:"position_type"`
}

type HoldingsResponse struct {
	Accounts   []Account  `json:"accounts"`
	Holdings   []Holding  `json:"holdings"`
	Securities []Security `json:"securities"`
	Item       Item       `json:"item"`
}

type InvestmentTransaction struct {
	InvestmentTransactionID string   `json:"investment_transaction_id"`
	AccountID               string   `json:"account_id"`
	SecurityID              *string  `json:"security_id"`
	Date                    string   `json:"date"`
	Name                    string   `json:"name"`
	Quantity                float64  `json:"quantity"`
	Amount                  float64  `json:"amount"`
	Price                   float64  `json:"price"`
	Fees                    *float64 `json:"fees"`
	Type                    string   `json:"type"`
	Subtype                 string   `json:"subtype"`
	IsoCurrencyCode         *string  `json:"iso_currency_code"`
}

type InvestmentTransactionsResponse struct {
	Accounts                    []Account               `json:"accounts"`
	Securities                  []Security              `json:"securities"`
	InvestmentTransactions      []InvestmentTransaction `json:"investment_transactions"`
	TotalInvestmentTransactions int                     `json:"total_investment_transactions"`
	Item                        Item                    `json:"item"`
}
