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
	info       *hyperliquid.Info
	ex         *hyperliquid.Exchange
	cfg        *config.Config
	szDecimals map[string]int // pair → szDecimals, cached after first fetch
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
		nil, // meta — fetched automatically
		"",  // vault address — not using vault
		cfg.AccountAddress,
		nil, // spotMeta — fetched automatically
		nil, // perpDexs — fetched automatically
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
		info:       info,
		ex:         ex,
		cfg:        cfg,
		szDecimals: make(map[string]int),
	}, nil
}

// FetchBalance returns the available USDC balance from spot wallet.
func (c *Client) FetchBalance(ctx context.Context) (float64, error) {
	spotState, err := c.info.SpotUserState(ctx, c.cfg.AccountAddress)
	if err != nil {
		return 0, fmt.Errorf("exchange: fetch spot balance failed account=%s: %w", c.cfg.AccountAddress, err)
	}

	var usdcBalance float64
	foundUSDC := false

	if spotState != nil && spotState.Balances != nil {
		for _, balance := range spotState.Balances {
			if balance.Coin == "USDC" {
				usdcBalance, err = strconv.ParseFloat(balance.Total, 64)
				if err != nil {
					return 0, fmt.Errorf("exchange: parse USDC total %q: %w", balance.Total, err)
				}
				foundUSDC = true
				break
			}
		}
	}

	if !foundUSDC {
		state, err := c.info.UserState(ctx, c.cfg.AccountAddress)
		if err != nil {
			return 0, fmt.Errorf("exchange: fetch perp balance failed account=%s: %w", c.cfg.AccountAddress, err)
		}

		usdcBalance, err = strconv.ParseFloat(state.MarginSummary.AccountValue, 64)
		if err != nil {
			return 0, fmt.Errorf("exchange: parse account value %q: %w", state.MarginSummary.AccountValue, err)
		}

		slog.Warn("USDC balance not found in spot wallet, using perp account value",
			"account", c.cfg.AccountAddress,
			"balance_usd", usdcBalance,
		)
	}

	slog.Debug("balance fetched",
		"account", c.cfg.AccountAddress,
		"balance_usd", usdcBalance,
		"network", c.cfg.NetworkLabel(),
		"source", map[bool]string{true: "spot", false: "perp"}[foundUSDC],
	)

	return usdcBalance, nil
}

// FetchPositions returns all open perp positions for the main account.
func (c *Client) FetchPositions(ctx context.Context) ([]Position, error) {
	state, err := c.info.UserState(ctx, c.cfg.AccountAddress)
	if err != nil {
		return nil, fmt.Errorf("exchange: fetch positions failed account=%s: %w", c.cfg.AccountAddress, err)
	}

	positions := make([]Position, 0, len(state.AssetPositions))

	for _, ap := range state.AssetPositions {
		pos := ap.Position

		szi, err := strconv.ParseFloat(pos.Szi, 64)
		if err != nil {
			return nil, fmt.Errorf("exchange: parse Szi %q for %s: %w", pos.Szi, pos.Coin, err)
		}

		if math.Abs(szi) < 1e-9 {
			continue
		}

		side := OrderSideLong
		if szi < 0 {
			side = OrderSideShort
		}

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

	// round size to the pair's szDecimals to avoid Hyperliquid rounding errors
	decimals := c.szDecimalsForPair(ctx, pair)
	roundFactor := math.Pow(10, float64(decimals))
	sizeCoins = math.Round(sizeCoins*roundFactor) / roundFactor

	req := hyperliquid.CreateOrderRequest{
		Coin:  pair,
		IsBuy: isBuy,
		Size:  sizeCoins,
		Price: price,
		OrderType: hyperliquid.OrderType{
			Limit: &hyperliquid.LimitOrderType{Tif: "Gtc"},
		},
		ReduceOnly: false,
	}

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

// PlaceTriggerOrder places a TP or SL trigger order on Hyperliquid.
// tpsl must be hyperliquid.TakeProfit or hyperliquid.StopLoss.
// coinSize is the position size in coins, used for the reduce-only closure.
func (c *Client) PlaceTriggerOrder(
	ctx context.Context,
	pair string,
	side OrderSide,
	coinSize float64,
	triggerPrice float64,
	tpsl hyperliquid.Tpsl,
) error {
	// Trigger orders reverse the side: a long position closes with a sell
	isBuy := side == OrderSideShort

	// Round size to the pair's szDecimals — same as entry order
	decimals := c.szDecimalsForPair(ctx, pair)
	roundFactor := math.Pow(10, float64(decimals))
	coinSize = math.Round(coinSize*roundFactor) / roundFactor

	if coinSize <= 0 {
		return fmt.Errorf("exchange: place %s trigger failed pair=%s: coinSize rounded to 0", tpsl, pair)
	}

	req := hyperliquid.CreateOrderRequest{
		Coin:       pair,
		IsBuy:      isBuy,
		Size:       coinSize,
		Price:      0,
		ReduceOnly: true,
		OrderType: hyperliquid.OrderType{
			Trigger: &hyperliquid.TriggerOrderType{
				TriggerPx: triggerPrice,
				IsMarket:  true,
				Tpsl:      tpsl,
			},
		},
	}

	resp, err := c.ex.Order(ctx, req, nil)
	if err != nil {
		return fmt.Errorf("exchange: place %s trigger failed pair=%s px=%.4f: %w", tpsl, pair, triggerPrice, err)
	}

	if resp.Error != nil {
		return fmt.Errorf("exchange: place %s trigger rejected pair=%s: %s", tpsl, pair, *resp.Error)
	}

	var orderID int64
	if resp.Resting != nil {
		orderID = resp.Resting.Oid
	} else if resp.Filled != nil {
		orderID = int64(resp.Filled.Oid)
	}

	if orderID == 0 {
		return fmt.Errorf("exchange: place %s trigger returned no order ID pair=%s — order may have been silently rejected", tpsl, pair)
	}

	slog.Info("trigger order placed",
		"pair", pair,
		"tpsl", tpsl,
		"trigger_px", triggerPrice,
		"size_coins", coinSize,
		"order_id", orderID,
		"network", c.cfg.NetworkLabel(),
	)

	return nil
}

// FilledTrade represents a completed fill from Hyperliquid's userFills endpoint.
type FilledTrade struct {
	Coin      string
	Side      string // "A" = sell/short, "B" = buy/long
	Price     float64
	Size      float64
	ClosedPnL float64
	Fee       float64
	Time      time.Time
	Oid       int64
	Tid       int64
	Dir       string // direction of the trade
	Hash      string
}

// FetchFilledTrades fetches all filled trades for the account within a time range.
// startTime/endTime are unix timestamps in milliseconds.
func (c *Client) FetchFilledTrades(ctx context.Context, startTime int64, endTime *int64) ([]FilledTrade, error) {
	fills, err := c.info.UserFillsByTime(ctx, c.cfg.AccountAddress, startTime, endTime, nil)
	if err != nil {
		return nil, fmt.Errorf("exchange: fetch filled trades failed account=%s: %w", c.cfg.AccountAddress, err)
	}

	trades := make([]FilledTrade, 0, len(fills))
	for _, f := range fills {
		price, err := strconv.ParseFloat(f.Price, 64)
		if err != nil {
			slog.Warn("exchange: parse fill price failed", "px", f.Price, "coin", f.Coin, "err", err)
			continue
		}
		size, err := strconv.ParseFloat(f.Size, 64)
		if err != nil {
			slog.Warn("exchange: parse fill size failed", "sz", f.Size, "coin", f.Coin, "err", err)
			continue
		}
		pnl, err := strconv.ParseFloat(f.ClosedPnl, 64)
		if err != nil {
			slog.Warn("exchange: parse fill pnl failed", "pnl", f.ClosedPnl, "coin", f.Coin, "err", err)
			continue
		}
		fee, err := strconv.ParseFloat(f.Fee, 64)
		if err != nil {
			fee = 0
		}

		trades = append(trades, FilledTrade{
			Coin:      f.Coin,
			Side:      f.Side,
			Price:     price,
			Size:      size,
			ClosedPnL: pnl,
			Fee:       fee,
			Time:      time.UnixMilli(f.Time),
			Oid:       f.Oid,
			Tid:       f.Tid,
			Dir:       f.Dir,
			Hash:      f.Hash,
		})
	}

	slog.Info("filled trades fetched",
		"account", c.cfg.AccountAddress,
		"count", len(trades),
		"network", c.cfg.NetworkLabel(),
	)

	return trades, nil
}

// FetchDailyPnL returns today's loss, win, and consecutive loss count from live fills.
func (c *Client) FetchDailyPnL(ctx context.Context) (lossUSD float64, winUSD float64, consecLosses int, err error) {
	wib := time.FixedZone("WIB", 7*60*60)
	nowWIB := time.Now().In(wib)
	todayStart := time.Date(nowWIB.Year(), nowWIB.Month(), nowWIB.Day(), 0, 0, 0, 0, wib)
	startTime := todayStart.UnixMilli()

	fills, err := c.FetchFilledTrades(ctx, startTime, nil)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("exchange: fetch daily PnL: %w", err)
	}

	// calculate consecutive losses from the end
	for i := len(fills) - 1; i >= 0; i-- {
		if fills[i].ClosedPnL < 0 {
			consecLosses++
		} else {
			break
		}
	}

	for _, f := range fills {
		if f.ClosedPnL < 0 {
			lossUSD += -f.ClosedPnL
		} else {
			winUSD += f.ClosedPnL
		}
	}

	return lossUSD, winUSD, consecLosses, nil
}

// ClosePosition closes an open position using a reduce-only market order.
func (c *Client) ClosePosition(ctx context.Context, pair string, side OrderSide) error {
	isBuy := side == OrderSideShort

	req := hyperliquid.CreateOrderRequest{
		Coin:       pair,
		IsBuy:      isBuy,
		Size:       0,
		Price:      0,
		OrderType:  hyperliquid.OrderType{},
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

// szDecimalsForPair returns the size decimals for a given pair.
// Cached after first fetch from Hyperliquid metadata.
func (c *Client) szDecimalsForPair(ctx context.Context, pair string) int {
	if d, ok := c.szDecimals[pair]; ok {
		return d
	}

	meta, err := c.info.Meta(ctx)
	if err != nil {
		slog.Warn("exchange: fetch meta failed — defaulting to 0 szDecimals", "err", err)
		return 0
	}

	for _, asset := range meta.Universe {
		c.szDecimals[asset.Name] = asset.SzDecimals
	}

	d, ok := c.szDecimals[pair]
	if !ok {
		return 0
	}
	return d
}
