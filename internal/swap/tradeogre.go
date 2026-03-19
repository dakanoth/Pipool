package swap

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TradeOgre API client — simple REST, no KYC exchange.
// All markets are quoted in BTC (e.g. BTC-LTC, BTC-DGB, BTC-DOGE).

const tradeOgreBase = "https://tradeogre.com/api/v1"

// TOClient wraps the TradeOgre REST API.
type TOClient struct {
	apiKey    string
	apiSecret string
	http      *http.Client
}

// NewTOClient creates a new TradeOgre API client.
func NewTOClient(apiKey, apiSecret string) *TOClient {
	return &TOClient{
		apiKey:    apiKey,
		apiSecret: apiSecret,
		http:      &http.Client{Timeout: 15 * time.Second},
	}
}

// ─── Public endpoints ────────────────────────────────────────────────────────

// TOTicker holds market ticker data.
type TOTicker struct {
	InitPrice string `json:"initialprice"`
	Price     string `json:"price"`
	High      string `json:"high"`
	Low       string `json:"low"`
	Volume    string `json:"volume"`
	Bid       string `json:"bid"`
	Ask       string `json:"ask"`
}

// GetTicker fetches the current ticker for a market (e.g. "BTC-LTC").
func (c *TOClient) GetTicker(market string) (*TOTicker, error) {
	resp, err := c.http.Get(tradeOgreBase + "/ticker/" + market)
	if err != nil {
		return nil, fmt.Errorf("ticker request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read ticker: %w", err)
	}

	// TradeOgre returns {"success":false} for invalid markets
	if strings.Contains(string(body), `"success":false`) {
		return nil, fmt.Errorf("market %s not found on TradeOgre", market)
	}

	var t TOTicker
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("decode ticker: %w", err)
	}
	return &t, nil
}

// TOOrderBook holds bids/asks for a market.
type TOOrderBook struct {
	Success bool               `json:"success"`
	Buy     map[string]string  `json:"buy"`  // price → quantity
	Sell    map[string]string  `json:"sell"` // price → quantity
}

// GetOrderBook fetches the order book for a market.
func (c *TOClient) GetOrderBook(market string) (*TOOrderBook, error) {
	resp, err := c.http.Get(tradeOgreBase + "/orders/" + market)
	if err != nil {
		return nil, fmt.Errorf("orderbook request: %w", err)
	}
	defer resp.Body.Close()

	var ob TOOrderBook
	if err := json.NewDecoder(resp.Body).Decode(&ob); err != nil {
		return nil, fmt.Errorf("decode orderbook: %w", err)
	}
	if !ob.Success {
		return nil, fmt.Errorf("orderbook failed for %s", market)
	}
	return &ob, nil
}

// ─── Authenticated endpoints ─────────────────────────────────────────────────

func (c *TOClient) authPost(endpoint string, params url.Values) ([]byte, error) {
	req, err := http.NewRequest("POST", tradeOgreBase+endpoint, strings.NewReader(params.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(c.apiKey, c.apiSecret)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (c *TOClient) authGet(endpoint string) ([]byte, error) {
	req, err := http.NewRequest("GET", tradeOgreBase+endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.apiKey, c.apiSecret)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// TOBalance holds balance info for a single coin.
type TOBalance struct {
	Balance   string `json:"balance"`
	Available string `json:"available"`
}

// GetBalance fetches balance for a specific coin on TradeOgre.
func (c *TOClient) GetBalance(coin string) (*TOBalance, error) {
	body, err := c.authPost("/account/balance", url.Values{"currency": {strings.ToUpper(coin)}})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Success   bool   `json:"success"`
		Balance   string `json:"balance"`
		Available string `json:"available"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode balance: %w", err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("balance error: %s", resp.Error)
	}
	return &TOBalance{Balance: resp.Balance, Available: resp.Available}, nil
}

// GetBalances fetches all balances on TradeOgre.
func (c *TOClient) GetBalances() (map[string]string, error) {
	body, err := c.authGet("/account/balances")
	if err != nil {
		return nil, err
	}

	var resp struct {
		Success  bool              `json:"success"`
		Balances map[string]string `json:"balances"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode balances: %w", err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("balances request failed")
	}
	return resp.Balances, nil
}

// TOOrderResult holds the result of a buy/sell order.
type TOOrderResult struct {
	Success bool   `json:"success"`
	UUID    string `json:"uuid"`
	Bnewbal string `json:"bnewbal"` // new base balance
	Snewbal string `json:"snewbal"` // new second balance
	Error   string `json:"error"`
}

// SubmitBuy places a limit buy order on a market.
// market is e.g. "BTC-LTC", price and quantity are strings.
func (c *TOClient) SubmitBuy(market, price, quantity string) (*TOOrderResult, error) {
	body, err := c.authPost("/order/buy", url.Values{
		"market":   {market},
		"price":    {price},
		"quantity": {quantity},
	})
	if err != nil {
		return nil, err
	}

	var res TOOrderResult
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("decode buy result: %w", err)
	}
	if !res.Success {
		return nil, fmt.Errorf("buy order failed: %s", res.Error)
	}
	return &res, nil
}

// SubmitSell places a limit sell order on a market.
func (c *TOClient) SubmitSell(market, price, quantity string) (*TOOrderResult, error) {
	body, err := c.authPost("/order/sell", url.Values{
		"market":   {market},
		"price":    {price},
		"quantity": {quantity},
	})
	if err != nil {
		return nil, err
	}

	var res TOOrderResult
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("decode sell result: %w", err)
	}
	if !res.Success {
		return nil, fmt.Errorf("sell order failed: %s", res.Error)
	}
	return &res, nil
}

// TOOrderInfo holds info about a placed order.
type TOOrderInfo struct {
	Success   bool   `json:"success"`
	Date      string `json:"date"`
	Type      string `json:"type"` // "buy" or "sell"
	Market    string `json:"market"`
	Price     string `json:"price"`
	Quantity  string `json:"quantity"`
	Fulfilled string `json:"fulfilled"`
	Error     string `json:"error"`
}

// GetOrder checks the status of an order by UUID.
func (c *TOClient) GetOrder(uuid string) (*TOOrderInfo, error) {
	body, err := c.authPost("/account/order/"+uuid, nil)
	if err != nil {
		return nil, err
	}

	var info TOOrderInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("decode order: %w", err)
	}
	// If success is false and error is "not found", order was filled and removed
	return &info, nil
}

// CancelOrder cancels a pending order.
func (c *TOClient) CancelOrder(uuid string) error {
	body, err := c.authPost("/order/cancel", url.Values{"uuid": {uuid}})
	if err != nil {
		return err
	}

	var resp struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("decode cancel: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("cancel failed: %s", resp.Error)
	}
	return nil
}

// DepositAddress fetches the deposit address for a coin on TradeOgre.
// Note: TradeOgre doesn't have a public API for this — deposit addresses
// must be configured manually from the TradeOgre web interface.
// This is a placeholder that returns the configured address.
func DepositAddress(configuredAddr string) string {
	return configuredAddr
}
