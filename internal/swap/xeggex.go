package swap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// XeggeX API client — full-featured exchange with withdrawal support.
// Market format: "BASE/QUOTE" (e.g. "LTC/BTC").
// API v2 docs: https://xeggex.com/api

const xeggexBase = "https://api.xeggex.com/api/v2"

// XegClient wraps the XeggeX REST API (v2).
type XegClient struct {
	apiKey    string
	apiSecret string
	http      *http.Client
}

// NewXegClient creates a new XeggeX API client.
func NewXegClient(apiKey, apiSecret string) *XegClient {
	return &XegClient{
		apiKey:    apiKey,
		apiSecret: apiSecret,
		http:      &http.Client{Timeout: 15 * time.Second},
	}
}

// ─── Internal helpers ────────────────────────────────────────────────────────

func (c *XegClient) authGet(endpoint string) ([]byte, error) {
	req, err := http.NewRequest("GET", xeggexBase+endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("x-api-secret", c.apiSecret)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response %s: %w", endpoint, err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GET %s: HTTP %d: %s", endpoint, resp.StatusCode, string(body))
	}
	return body, nil
}

func (c *XegClient) authPostJSON(endpoint string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	req, err := http.NewRequest("POST", xeggexBase+endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("x-api-secret", c.apiSecret)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response %s: %w", endpoint, err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("POST %s: HTTP %d: %s", endpoint, resp.StatusCode, string(body))
	}
	return body, nil
}

func (c *XegClient) publicGet(endpoint string) ([]byte, error) {
	resp, err := c.http.Get(xeggexBase + endpoint)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response %s: %w", endpoint, err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GET %s: HTTP %d: %s", endpoint, resp.StatusCode, string(body))
	}
	return body, nil
}

// ─── Exchange interface implementation ───────────────────────────────────────

func (c *XegClient) Name() string { return "XeggeX" }

// xegMarketInfo is the response from /market/getbysymbol.
type xegMarketInfo struct {
	BestBid     string `json:"bestBid"`
	BestAsk     string `json:"bestAsk"`
	LastPrice   string `json:"lastPrice"`
	Volume      string `json:"volume"`
	HighPrice   string `json:"highPrice"`
	LowPrice    string `json:"lowPrice"`
	Symbol      string `json:"symbol"`
	PrimaryCurrency  string `json:"primaryCurrency"`
	SecondaryCurrency string `json:"secondaryCurrency"`
}

// GetTicker returns the best bid and ask for a market.
// market should be in XeggeX format: "LTC/BTC".
func (c *XegClient) GetTicker(market string) (bid, ask float64, err error) {
	// URL-encode the slash: "LTC/BTC" → "LTC%2FBTC"
	encoded := strings.Replace(market, "/", "%2F", 1)
	body, err := c.publicGet("/market/getbysymbol/" + encoded)
	if err != nil {
		return 0, 0, fmt.Errorf("xeggex ticker %s: %w", market, err)
	}

	var info xegMarketInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return 0, 0, fmt.Errorf("decode ticker: %w", err)
	}

	bid, err = strconv.ParseFloat(info.BestBid, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse bid %q: %w", info.BestBid, err)
	}
	ask, err = strconv.ParseFloat(info.BestAsk, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse ask %q: %w", info.BestAsk, err)
	}
	return bid, ask, nil
}

// xegBalance is the response from /balance/{ticker}.
type xegBalance struct {
	Asset     string `json:"asset"`
	Available string `json:"available"`
	Pending   string `json:"pending"`
	Held      string `json:"held"`
}

// GetBalance returns the available balance for a coin.
func (c *XegClient) GetBalance(coin string) (float64, error) {
	body, err := c.authGet("/balance/" + strings.ToUpper(coin))
	if err != nil {
		return 0, fmt.Errorf("xeggex balance %s: %w", coin, err)
	}

	var bal xegBalance
	if err := json.Unmarshal(body, &bal); err != nil {
		return 0, fmt.Errorf("decode balance: %w", err)
	}

	avail, err := strconv.ParseFloat(bal.Available, 64)
	if err != nil {
		return 0, fmt.Errorf("parse available %q: %w", bal.Available, err)
	}
	return avail, nil
}

// GetBalances returns all balances on XeggeX.
func (c *XegClient) GetBalances() (map[string]float64, error) {
	body, err := c.authGet("/balances")
	if err != nil {
		return nil, err
	}

	var bals []xegBalance
	if err := json.Unmarshal(body, &bals); err != nil {
		return nil, fmt.Errorf("decode balances: %w", err)
	}

	result := make(map[string]float64, len(bals))
	for _, b := range bals {
		if v, err := strconv.ParseFloat(b.Available, 64); err == nil && v > 0 {
			result[b.Asset] = v
		}
	}
	return result, nil
}

// xegOrderResult is the response from /createorder.
type xegOrderResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

// SubmitBuy places a limit buy order on XeggeX.
func (c *XegClient) SubmitBuy(market, price, quantity string) (string, error) {
	return c.submitOrder(market, "buy", price, quantity)
}

// SubmitSell places a limit sell order on XeggeX.
func (c *XegClient) SubmitSell(market, price, quantity string) (string, error) {
	return c.submitOrder(market, "sell", price, quantity)
}

func (c *XegClient) submitOrder(market, side, price, quantity string) (string, error) {
	payload := map[string]string{
		"symbol":   market,
		"side":     side,
		"type":     "limit",
		"quantity": quantity,
		"price":    price,
	}

	body, err := c.authPostJSON("/createorder", payload)
	if err != nil {
		return "", fmt.Errorf("xeggex %s order: %w", side, err)
	}

	var res xegOrderResult
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("decode order result: %w", err)
	}
	if res.Error != "" {
		return "", fmt.Errorf("xeggex %s order: %s", side, res.Error)
	}
	if res.ID == "" {
		return "", fmt.Errorf("xeggex %s order: empty order ID in response: %s", side, string(body))
	}
	return res.ID, nil
}

// xegOrderInfo is the response from /getorder/{id}.
type xegOrderInfo struct {
	ID            string `json:"id"`
	Status        string `json:"status"` // "active", "filled", "cancelled", "partially-filled"
	Side          string `json:"side"`
	Symbol        string `json:"symbol"`
	Price         string `json:"price"`
	Quantity      string `json:"quantity"`
	ExecutedQty   string `json:"executedQuantity"`
	Error         string `json:"error"`
}

// GetOrder checks whether an order has been fully filled.
func (c *XegClient) GetOrder(orderID string) (bool, error) {
	body, err := c.authGet("/getorder/" + orderID)
	if err != nil {
		return false, fmt.Errorf("xeggex get order: %w", err)
	}

	var info xegOrderInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return false, fmt.Errorf("decode order: %w", err)
	}
	if info.Error != "" {
		return false, fmt.Errorf("xeggex order %s: %s", orderID, info.Error)
	}

	filled := strings.EqualFold(info.Status, "filled")
	return filled, nil
}

// CancelOrder cancels a pending order on XeggeX.
func (c *XegClient) CancelOrder(orderID string) error {
	payload := map[string]string{"id": orderID}

	body, err := c.authPostJSON("/cancelorder", payload)
	if err != nil {
		return fmt.Errorf("xeggex cancel order: %w", err)
	}

	// Check for error in response
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("decode cancel response: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("xeggex cancel %s: %s", orderID, resp.Error)
	}
	return nil
}

// xegWithdrawResult is the response from /withdraw.
type xegWithdrawResult struct {
	ID    string `json:"id"`
	TxID  string `json:"txId"`
	Error string `json:"error"`
}

// Withdraw sends funds to an external address via XeggeX.
func (c *XegClient) Withdraw(coin string, amount float64, address string) (string, error) {
	payload := map[string]string{
		"ticker":   strings.ToUpper(coin),
		"quantity": strconv.FormatFloat(amount, 'f', -1, 64),
		"address":  address,
	}

	body, err := c.authPostJSON("/withdraw", payload)
	if err != nil {
		return "", fmt.Errorf("xeggex withdraw %s: %w", coin, err)
	}

	var res xegWithdrawResult
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("decode withdraw result: %w", err)
	}
	if res.Error != "" {
		return "", fmt.Errorf("xeggex withdraw %s: %s", coin, res.Error)
	}

	// txId may be empty initially — XeggeX queues withdrawals.
	// Return the withdrawal ID if no txid yet.
	txid := res.TxID
	if txid == "" {
		txid = res.ID
	}
	return txid, nil
}

// MarketName returns XeggeX format: "BASE/QUOTE" (e.g. "LTC/BTC").
func (c *XegClient) MarketName(source, dest string) string {
	return strings.ToUpper(source) + "/" + strings.ToUpper(dest)
}
