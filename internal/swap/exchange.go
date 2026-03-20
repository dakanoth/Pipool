package swap

import (
	"fmt"
	"strconv"
	"strings"
)

// Exchange is the common interface for all exchange API clients.
// Both TOExchange (TradeOgre) and XegClient (XeggeX) implement this.
type Exchange interface {
	// Name returns the human-readable exchange name.
	Name() string

	// GetTicker returns the current best bid and ask for a market.
	// Market format is exchange-specific (use MarketName to build it).
	GetTicker(market string) (bid, ask float64, err error)

	// GetBalance returns the available (unlocked) balance for a coin.
	GetBalance(coin string) (available float64, err error)

	// SubmitSell places a limit sell order, returns the order ID.
	SubmitSell(market, price, quantity string) (orderID string, err error)

	// SubmitBuy places a limit buy order, returns the order ID.
	SubmitBuy(market, price, quantity string) (orderID string, err error)

	// GetOrder checks whether an order has been filled.
	GetOrder(orderID string) (filled bool, err error)

	// CancelOrder cancels a pending order.
	CancelOrder(orderID string) error

	// Withdraw sends funds to an external address. Returns the txid if
	// the exchange provides one.
	Withdraw(coin string, amount float64, address string) (txid string, err error)

	// MarketName builds the exchange-specific market string from a
	// source (base) and dest (quote) ticker, e.g. ("LTC","BTC").
	MarketName(source, dest string) string
}

// ─── TOExchange adapts TOClient to the Exchange interface ────────────────────

// TOExchange wraps a TOClient and implements the Exchange interface.
// The underlying TOClient methods are preserved for direct use.
type TOExchange struct {
	*TOClient
}

// NewTOExchange creates a TradeOgre Exchange from API credentials.
func NewTOExchange(apiKey, apiSecret string) *TOExchange {
	return &TOExchange{TOClient: NewTOClient(apiKey, apiSecret)}
}

func (e *TOExchange) Name() string { return "TradeOgre" }

func (e *TOExchange) GetTicker(market string) (bid, ask float64, err error) {
	t, err := e.TOClient.GetTicker(market)
	if err != nil {
		return 0, 0, err
	}
	bid, err = strconv.ParseFloat(t.Bid, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse bid: %w", err)
	}
	ask, err = strconv.ParseFloat(t.Ask, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse ask: %w", err)
	}
	return bid, ask, nil
}

func (e *TOExchange) GetBalance(coin string) (float64, error) {
	b, err := e.TOClient.GetBalance(coin)
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(b.Available, 64)
}

func (e *TOExchange) SubmitSell(market, price, quantity string) (string, error) {
	res, err := e.TOClient.SubmitSell(market, price, quantity)
	if err != nil {
		return "", err
	}
	return res.UUID, nil
}

func (e *TOExchange) SubmitBuy(market, price, quantity string) (string, error) {
	res, err := e.TOClient.SubmitBuy(market, price, quantity)
	if err != nil {
		return "", err
	}
	return res.UUID, nil
}

func (e *TOExchange) GetOrder(orderID string) (bool, error) {
	info, err := e.TOClient.GetOrder(orderID)
	if err != nil {
		return false, err
	}
	// TradeOgre removes filled orders from the book; a "not found"
	// response means the order was fully filled.
	if !info.Success && strings.Contains(strings.ToLower(info.Error), "not found") {
		return true, nil
	}
	return false, nil
}

func (e *TOExchange) CancelOrder(orderID string) error {
	return e.TOClient.CancelOrder(orderID)
}

func (e *TOExchange) Withdraw(coin string, amount float64, address string) (string, error) {
	return "", fmt.Errorf("TradeOgre does not support API withdrawals — manual withdrawal required")
}

// MarketName returns TradeOgre format: "QUOTE-BASE" (e.g. "BTC-LTC").
func (e *TOExchange) MarketName(source, dest string) string {
	return strings.ToUpper(dest) + "-" + strings.ToUpper(source)
}

// Compile-time interface checks.
var _ Exchange = (*TOExchange)(nil)
var _ Exchange = (*XegClient)(nil)
