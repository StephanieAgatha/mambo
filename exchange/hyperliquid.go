package exchange

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	hyperliquid "github.com/sonirico/go-hyperliquid"

	"mambo/config"
)

// OrderSide represents the direction of a futures trade.
type OrderSide string

const (
	OrderSideLong  OrderSide = "long"
	OrderSideShort OrderSide = "short"
)

// OrderResult holds the result of a successfully placed limit order.
type OrderResult struct {
	OrderID  uint64
	Pair     string
	Side     OrderSide
	Price    float64
	SizeUSD  float64
	Leverage int
	PlacedAt time.Time
}

// Position represents an open futures position fetched from Hyperliquid.
type Position struct {
	Pair          string
	Side          OrderSide
	EntryPrice    float64
	SizeUSD       float64
	Leverage      int
	UnrealizedPnL float64
	LiquidationPx float64
}

// Client wraps the go-hyperliquid SDK exposing only what Mambo needs.
// info is used for all read-only queries (balance, positions, candles).
// ex is used for all write operations (orders, leverage, cancel).
type Client struct {
	info *hyperliquid.Info
	ex   *hyperliquid.Exchange
	cfg  *config.Config
}

// New initializes both the Info (read) and Exchange (write) SDK clients
// using the agent wallet private key. Logs network mode clearly.
func New(ctx context.Context, cfg *config.Config) (*Client, error) {
	// strip "0x" prefix — crypto.HexToECDSA does not accept it
	rawKey := strings.TrimPrefix(cfg.AgentPrivateKey, "0x")

	privateKey, err := crypto.HexToECDSA(rawKey)
	if err != nil {
		return nil, fmt.Errorf("exchange: invalid agent private key: %w", err)
	}

	apiURL := hyperliquid.MainnetAPIURL
	if cfg.Testnet {
		apiURL = hyperliquid.TestnetAPIURL
	}

	// Info client — read-only, no signing needed
	// skipWS=true: we use REST polling, not WebSocket
	// nil meta/spotMeta/perpDexs → SDK fetches them automatically
	info := hyperliquid.NewInfo(ctx, apiURL, true, nil, nil, nil)

	// Exchange client — for signing and submitting orders
	// vault address is empty string (not using a vault)
	// meta=nil → SDK fetches automatically from the Info instance it creates internally
	ex := hyperliquid.NewExchange(
		ctx,
		privateKey,
		apiURL,
		nil,               // meta — fetched automatically
		"",                // vault address — not using vault
		cfg.AccountAddress,
		nil,               // spotMeta — fetched automatically
		nil,               // perpDexs — fetched automatically
	)

	if cfg.Testnet {
		slog.Warn("⚠️  exchange: TESTNET MODE — no real funds at risk",
			"api_url", apiURL,
			"account", cfg.AccountAddress,
		)
	} else {
		slog.Warn("🔴 exchange: MAINNET MODE — real funds in play",
			"api_url", apiURL,
			"account", cfg.AccountAddress,
		)
	}

	slog.Info("exchange client initialized",
		"network", cfg.NetworkLabel(),
		"account", cfg.AccountAddress,
	)

	return &Client{
		info: info,
		ex:   ex,
		cfg:  cfg,
	}, nil
}

// FetchBalance returns the available USDC balance (AccountValue) of the main account.
// AccountValue includes unrealized PnL.
func (c *Client) FetchBalance(ctx context.Context) (float64, error) {
	state, err := c.info.UserState(ctx, c.cfg.AccountAddress)
	if err != nil {
		return 0, fmt.Errorf("exchange: fetch balance failed account=%s: %w", c.cfg.AccountAddress, err)
	}

	// sonirico SDK: MarginSummary.AccountValue is a raw string — parse it
	balance, err := strconv.ParseFloat(state.MarginSummary.AccountValue, 64)
	if err != nil {
		return 0, fmt.Errorf("exchange: parse account value %q: %w", state.MarginSummary.AccountValue, err)
	}

	slog.Debug("balance fetched",
		"account", c.cfg.AccountAddress,
		"balance_usd", balance,
		"network", c.cfg.NetworkLabel(),
	)

	return balance, nil
}

// FetchPositions returns all open perp positions for the main account.
// Positions with zero size are skipped.
func (c *Client) FetchPositions(ctx context.Context) ([]Position, error) {
	state, err := c.info.UserState(ctx, c.cfg.AccountAddress)
	if err != nil {
		return nil, fmt.Errorf("exchange: fetch positions failed account=%s: %w", c.cfg.AccountAddress, err)
	}

	positions := make([]Position, 0, len(state.AssetPositions))

	for _, ap := range state.AssetPositions {
		pos := ap.Position

		// sonirico SDK: Szi is a raw string — negative = short, positive = long
		szi, err := strconv.ParseFloat(pos.Szi, 64)
		if err != nil {
			return nil, fmt.Errorf("exchange: parse Szi %q for %s: %w", pos.Szi, pos.Coin, err)
		}

		// skip positions with zero or near-zero size
		if math.Abs(szi) < 1e-9 {
			continue
		}

		side := OrderSideLong
		if szi < 0 {
			side = OrderSideShort
		}

		// EntryPx is *string — nil means position has no entry price yet
		var entryPrice float64
		if pos.EntryPx != nil {
			entryPrice, err = strconv.ParseFloat(*pos.EntryPx, 64)
			if err != nil {
				return nil, fmt.Errorf("exchange: parse EntryPx %q for %s: %w", *pos.EntryPx, pos.Coin, err)
			}
		}

		posValue, err := strconv.ParseFloat(pos.PositionValue, 64)
		if err != nil {
			return nil, fmt.Errorf("exchange: parse PositionValue %q for %s: %w", pos.PositionValue, pos.Coin, err)
		}

		unrealizedPnL, err := strconv.ParseFloat(pos.UnrealizedPnl, 64)
		if err != nil {
			return nil, fmt.Errorf("exchange: parse UnrealizedPnl %q for %s: %w", pos.UnrealizedPnl, pos.Coin, err)
		}

		// LiquidationPx is *string — nil if no liquidation price set
		var liqPx float64
		if pos.LiquidationPx != nil {
			liqPx, err = strconv.ParseFloat(*pos.LiquidationPx, 64)
			if err != nil {
				return nil, fmt.Errorf("exchange: parse LiquidationPx %q for %s: %w", *pos.LiquidationPx, pos.Coin, err)
			}
		}

		positions = append(positions, Position{
			Pair:          pos.Coin,
			Side:          side,
			EntryPrice:    entryPrice,
			SizeUSD:       math.Abs(posValue),
			Leverage:      pos.Leverage.Value,
			UnrealizedPnL: unrealizedPnL,
			LiquidationPx: liqPx,
		})
	}

	slog.Debug("positions fetched",
		"account", c.cfg.AccountAddress,
		"open_count", len(positions),
	)

	return positions, nil
}

// PlaceLimitOrder places a cross margin limit order on Hyperliquid.
// sizeUSD is the notional value in USDC. Price is the limit price.
// Leverage is set via UpdateLeverage before placing the order.
func (c *Client) PlaceLimitOrder(
	ctx context.Context,
	pair string,
	side OrderSide,
	sizeUSD float64,
	price float64,
	leverage int,
) (OrderResult, error) {
	if _, err := c.ex.UpdateLeverage(ctx, leverage, pair, true); err != nil {
		return OrderResult{}, fmt.Errorf("exchange: set leverage %dx failed pair=%s: %w", leverage, pair, err)
	}

	isBuy := side == OrderSideLong
	sizeCoins := sizeUSD / price

	req := hyperliquid.CreateOrderRequest{
    Coin:       pair,
    IsBuy:      isBuy,
    Size:       sizeCoins,   // was Sz
    Price:      price,       // was LimitPx
    OrderType:  hyperliquid.OrderType{
        Limit: &hyperliquid.LimitOrderType{Tif: "Gtc"},
    },
    ReduceOnly: false,
}

	// The 2nd arg is *BuilderInfo — pass nil (no vault, no custom nonce)
	resp, err := c.ex.Order(ctx, req, nil)
	if err != nil {
		return OrderResult{}, fmt.Errorf("exchange: place limit order failed pair=%s side=%s price=%.4f: %w",
			pair, side, price, err)
	}

	var orderID uint64
	if resp.Resting != nil {
		orderID = uint64(resp.Resting.Oid)
	}

	result := OrderResult{
		OrderID:  orderID,
		Pair:     pair,
		Side:     side,
		Price:    price,
		SizeUSD:  sizeUSD,
		Leverage: leverage,
		PlacedAt: time.Now(),
	}

	slog.Info("limit order placed",
		"pair", pair,
		"side", side,
		"price", price,
		"size_usd", sizeUSD,
		"size_coins", sizeCoins,
		"leverage", leverage,
		"order_id", orderID,
		"network", c.cfg.NetworkLabel(),
	)

	return result, nil
}

// CancelOrder cancels an open order by its order ID.
// CancelOrder cancels an open order by its order ID.
func (c *Client) CancelOrder(ctx context.Context, pair string, orderID uint64) error {
	if _, err := c.ex.Cancel(ctx, pair, int64(orderID)); err != nil {
		return fmt.Errorf("exchange: cancel order failed pair=%s order_id=%d: %w", pair, orderID, err)
	}

	slog.Info("order cancelled",
		"pair", pair,
		"order_id", orderID,
	)
	return nil
}

// ClosePosition closes an open position using a reduce-only market order.
// Used by the monitor goroutine when TP/SL/hard rules trigger.
func (c *Client) ClosePosition(ctx context.Context, pair string, side OrderSide) error {
	isBuy := side == OrderSideShort // closing long = sell, closing short = buy

	req := hyperliquid.CreateOrderRequest{
		Coin:       pair,
		IsBuy:      isBuy,
		Size:       0,                      // 0 + ReduceOnly = close entire position
		Price:      0,                      // 0 = market order
		OrderType:  hyperliquid.OrderType{}, // no limit fields → market
		ReduceOnly: true,
	}

	if _, err := c.ex.Order(ctx, req, nil); err != nil {
		return fmt.Errorf("exchange: close position failed pair=%s side=%s: %w", pair, side, err)
	}

	slog.Info("position closed",
		"pair", pair,
		"side", side,
		"network", c.cfg.NetworkLabel(),
	)
	return nil
}

// FetchCurrentPrice returns the current mid price for a pair using AllMids.
// Used by the monitor goroutine to check TP/SL conditions every 10 seconds.
func (c *Client) FetchCurrentPrice(ctx context.Context, pair string) (float64, error) {
	mids, err := c.info.AllMids(ctx)
	if err != nil {
		return 0, fmt.Errorf("exchange: fetch current price failed pair=%s: %w", pair, err)
	}

	priceStr, ok := mids[pair]
	if !ok {
		return 0, fmt.Errorf("exchange: pair %s not found in AllMids", pair)
	}

	price, err := strconv.ParseFloat(priceStr, 64)
	if err != nil {
		return 0, fmt.Errorf("exchange: parse price %q for pair=%s: %w", priceStr, pair, err)
	}

	return price, nil
}