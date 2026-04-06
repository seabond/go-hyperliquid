package hyperliquid

import (
	"encoding/json"
)

//go:generate easyjson -all

const (
	ChannelPong               string = "pong"
	ChannelTrades             string = "trades"
	ChannelActiveAssetCtx     string = "activeAssetCtx"
	ChannelL2Book             string = "l2Book"
	ChannelCandle             string = "candle"
	ChannelAllMids            string = "allMids"
	ChannelNotification       string = "notification"
	ChannelOrderUpdates       string = "orderUpdates"
	ChannelUserFills          string = "userFills"
	ChannelWebData2           string = "webData2"
	ChannelBbo                string = "bbo"
	ChannelSubResponse        string = "subscriptionResponse"
	ChannelClearinghouseState string = "clearinghouseState"
	ChannelOpenOrders         string = "openOrders"
	ChannelTwapStates         string = "twapStates"
	ChannelWebData3                      string = "webData3"
	ChannelAllDexsClearinghouseState     string = "allDexsClearinghouseState"
	ChannelSpotState                    string = "spotState"
	ChannelAllDexsAssetCtxs             string = "allDexsAssetCtxs"
)

type wsMessage struct {
	Channel string          `json:"channel"`
	Data    json.RawMessage `json:"data"`
}

type wsCommand struct {
	Method       string `json:"method"`
	Subscription any    `json:"subscription,omitempty"`
}

type (
	Trade struct {
		Coin  string   `json:"coin"`
		Side  string   `json:"side"`
		Px    string   `json:"px"`
		Sz    string   `json:"sz"`
		Time  int64    `json:"time"`
		Hash  string   `json:"hash"`
		Tid   int64    `json:"tid"`
		Users []string `json:"users"`
	}

	ActiveAssetCtx struct {
		Coin string         `json:"coin"`
		Ctx  SharedAssetCtx `json:"ctx"`
	}

	SharedAssetCtx struct {
		DayNtlVlm float64 `json:"dayNtlVlm,string"`
		PrevDayPx float64 `json:"prevDayPx,string"`
		MarkPx    float64 `json:"markPx,string"`
		MidPx     float64 `json:"midPx,string"`

		// PerpsAssetCtx
		Funding      float64 `json:"funding,string,omitempty"`
		OpenInterest float64 `json:"openInterest,string,omitempty"`
		OraclePx     float64 `json:"oraclePx,string,omitempty"`

		// SpotAssetCtx
		CirculatingSupply float64 `json:"circulatingSupply,string,omitempty"`
	}

	AllMids struct {
		Mids map[string]string `json:"mids"`
	}

	Notification struct {
		Notification string `json:"notification"`
	}

	//easyjson:skip
	WebData2 struct {
		ClearinghouseState     *ClearinghouseState `json:"clearinghouseState,omitempty"`
		LeadingVaults          []any               `json:"leadingVaults,omitempty"`
		TotalVaultEquity       string              `json:"totalVaultEquity,omitempty"`
		OpenOrders             []WsBasicOrder      `json:"openOrders,omitempty"`
		AgentAddress           *string             `json:"agentAddress,omitempty"`
		AgentValidUntil        *int64              `json:"agentValidUntil,omitempty"`
		CumLedger              string              `json:"cumLedger,omitempty"`
		Meta                   *WebData2Meta       `json:"meta,omitempty"`
		AssetCtxs              []AssetCtx          `json:"assetCtxs,omitempty"`
		ServerTime             int64               `json:"serverTime,omitempty"`
		IsVault                bool                `json:"isVault,omitempty"`
		User                   string              `json:"user,omitempty"`
		TwapStates             []any               `json:"twapStates,omitempty"`
		SpotState              *SpotState          `json:"spotState,omitempty"`
		SpotAssetCtxs          []SpotAssetCtx      `json:"spotAssetCtxs,omitempty"`
		PerpsAtOpenInterestCap []string            `json:"perpsAtOpenInterestCap,omitempty"`
	}

	//easyjson:skip
	WebData2Meta struct {
		Universe     []WebData2AssetInfo                `json:"universe,omitempty"`
		MarginTables []Tuple2[int, WebData2MarginTable] `json:"marginTables,omitempty"`
	}

	WebData2AssetInfo struct {
		SzDecimals    int    `json:"szDecimals,omitempty"`
		Name          string `json:"name,omitempty"`
		MaxLeverage   int    `json:"maxLeverage,omitempty"`
		MarginTableID int    `json:"marginTableId,omitempty"`
		IsDelisted    bool   `json:"isDelisted,omitempty"`
		OnlyIsolated  bool   `json:"onlyIsolated,omitempty"`
	}

	WebData2MarginTable struct {
		Description string               `json:"description,omitempty"`
		MarginTiers []WebData2MarginTier `json:"marginTiers,omitempty"`
	}

	WebData2MarginTier struct {
		LowerBound  string `json:"lowerBound,omitempty"`
		MaxLeverage int    `json:"maxLeverage,omitempty"`
	}

	ClearinghouseState struct {
		MarginSummary              *MarginSummary  `json:"marginSummary,omitempty"`
		CrossMarginSummary         *MarginSummary  `json:"crossMarginSummary,omitempty"`
		CrossMaintenanceMarginUsed string          `json:"crossMaintenanceMarginUsed,omitempty"`
		Withdrawable               string          `json:"withdrawable,omitempty"`
		AssetPositions             []AssetPosition `json:"assetPositions,omitempty"`
		Time                       int64           `json:"time,omitempty"`
	}

	ClearinghouseStateMessage struct {
		Dex                string             `json:"dex,omitempty"`
		User               string             `json:"user,omitempty"`
		ClearinghouseState ClearinghouseState `json:"clearinghouseState"`
	}

	SpotState struct {
		Balances []SpotBalance `json:"balances,omitempty"`
	}

	OpenOrders struct {
		Dex    string         `json:"dex"`
		User   string         `json:"user"`
		Orders []WsBasicOrder `json:"orders"`
	}

	WsOrder struct {
		Order           WsBasicOrder     `json:"order"`
		Status          OrderStatusValue `json:"status"`
		StatusTimestamp int64            `json:"statusTimestamp"`
	}

	WsOrderWithUser struct {
		WsOrder
		User string `json:"user"`
	}

	WsBasicOrder struct {
		Coin      string  `json:"coin"`
		Side      string  `json:"side"`
		LimitPx   string  `json:"limitPx"`
		Sz        string  `json:"sz"`
		Oid       int64   `json:"oid"`
		Timestamp int64   `json:"timestamp"`
		OrigSz    string  `json:"origSz"`
		Cloid     *string `json:"cloid"`
	}

	WsOrderFills struct {
		IsSnapshot bool          `json:"isSnapshot"`
		User       string        `json:"user"`
		Fills      []WsOrderFill `json:"fills"`
	}

	WsOrderFill struct {
		Coin          string           `json:"coin"`
		Px            string           `json:"px"` // price
		Sz            string           `json:"sz"` // size
		Side          string           `json:"side"`
		Time          int64            `json:"time"`
		StartPosition string           `json:"startPosition"`
		Dir           string           `json:"dir"` // used for frontend display
		ClosedPnl     string           `json:"closedPnl"`
		Hash          string           `json:"hash"`    // L1 transaction hash
		Oid           int64            `json:"oid"`     // order id
		Crossed       bool             `json:"crossed"` // whether order crossed the spread (was taker)
		Fee           string           `json:"fee"`     // negative means rebate
		Tid           int64            `json:"tid"`     // unique trade id
		Liquidation   *FillLiquidation `json:"liquidation,omitempty"`
		FeeToken      string           `json:"feeToken"`             // the token the fee was paid in
		BuilderFee    *string          `json:"builderFee,omitempty"` // amount paid to builder, also included in fee
	}

	FillLiquidation struct {
		LiquidatedUser *string `json:"liquidatedUser,omitempty"`
		MarkPx         string  `json:"markPx"`
		Method         string  `json:"method"`
	}

	L2Book struct {
		Coin   string    `json:"coin"`
		Levels [][]Level `json:"levels"`
		Time   int64     `json:"time"`
	}

	Bbo struct {
		Coin string  `json:"coin"`
		Time int64   `json:"time"`
		Bbo  []Level `json:"bbo"`
	}

	Level struct {
		N  int     `json:"n"`
		Px float64 `json:"px,string"`
		Sz float64 `json:"sz,string"`
	}

	Candle struct {
		TimeOpen    int64  `json:"t"` // open millis
		TimeClose   int64  `json:"T"` // close millis
		Interval    string `json:"i"` // interval
		TradesCount int    `json:"n"` // number of trades
		Open        string `json:"o"` // open price
		High        string `json:"h"` // high price
		Low         string `json:"l"` // low price
		Close       string `json:"c"` // close price
		Symbol      string `json:"s"` // coin
		Volume      string `json:"v"` // volume (base unit)
	}

	//easyjson:skip
	TwapStates struct {
		Dex    string                   `json:"dex"`
		User   string                   `json:"user"`
		States []Tuple2[int, TwapState] `json:"states"`
	}

	TwapState struct {
		Coin        string  `json:"coin"`
		User        string  `json:"user"`
		Side        string  `json:"side"`
		Sz          float64 `json:"sz,string"`
		ExecutedSz  float64 `json:"executedSz,string"`
		ExecutedNtl float64 `json:"executedNtl,string"`
		Minutes     int     `json:"minutes"`
		ReduceOnly  bool    `json:"reduceOnly"`
		Randomize   bool    `json:"randomize"`
		Timestamp   int64   `json:"timestamp"`
	}

	//easyjson:skip
	WebData3 struct {
		UserState     WebData3UserState `json:"userState"`
		PerpDexStates []PerpDexState    `json:"perpDexStates"`
	}

	WebData3UserState struct {
		AgentAddress          *string `json:"agentAddress,omitempty"`
		AgentValidUntil       *int64  `json:"agentValidUntil,omitempty"`
		ServerTime            int64   `json:"serverTime"`
		CumLedger             float64 `json:"cumLedger,string"`
		IsVault               bool    `json:"isVault"`
		User                  string  `json:"user"`
		OptOutOfSpotDusting   *bool   `json:"optOutOfSpotDusting,omitempty"`
		DexAbstractionEnabled *bool   `json:"dexAbstractionEnabled,omitempty"`
	}

	PerpDexState struct {
		TotalVaultEquity       float64         `json:"totalVaultEquity,string"`
		PerpsAtOpenInterestCap *[]string       `json:"perpsAtOpenInterestCap,omitempty"`
		LeadingVaults          *[]LeadingVault `json:"leadingVaults,omitempty"`
	}

	LeadingVault struct {
		Address string `json:"address"`
		Name    string `json:"name"`
	}

	//easyjson:skip
	AllDexsClearinghouseState struct {
		User                string       `json:"user"`
		ClearinghouseStates DexStatesMap `json:"clearinghouseStates"`
	}

	//easyjson:skip
	SpotStateMessage struct {
		User      string        `json:"user"`
		SpotState SpotUserState `json:"spotState"`
	}

	// AllDexsAssetCtxs is the response for the allDexsAssetCtxs channel.
	// HL format: {"ctxs": [["dexName", [SharedAssetCtx...]], ...]}.
	// Custom UnmarshalJSON because jsoniter doesn't correctly delegate to
	// DexAssetCtxsMap.UnmarshalJSON when nested inside a struct.
	//easyjson:skip
	AllDexsAssetCtxs struct {
		Dexes DexAssetCtxsMap
	}
)

// DexStatesMap maps dex name to ClearinghouseState.
// Hyperliquid serializes this as an array of [key, value] tuples.
type DexStatesMap map[string]ClearinghouseState

func (m *DexStatesMap) UnmarshalJSON(data []byte) error {
	var tuples [][2]json.RawMessage
	if err := json.Unmarshal(data, &tuples); err != nil {
		return err
	}
	*m = make(DexStatesMap, len(tuples))
	for _, t := range tuples {
		var key string
		if err := json.Unmarshal(t[0], &key); err != nil {
			return err
		}
		var state ClearinghouseState
		if err := json.Unmarshal(t[1], &state); err != nil {
			return err
		}
		(*m)[key] = state
	}
	return nil
}

// DexAssetCtxsMap maps dex name to a slice of SharedAssetCtx.
// Hyperliquid serializes Record<string, T[]> as [[key, values], ...] tuples.
// Each ctx does NOT contain a coin field; the caller must correlate by index
// against the meta universe for each dex.
type DexAssetCtxsMap map[string][]SharedAssetCtx

func (m *DexAssetCtxsMap) UnmarshalJSON(data []byte) error {
	var tuples []json.RawMessage
	if err := json.Unmarshal(data, &tuples); err != nil {
		return err
	}
	*m = make(DexAssetCtxsMap, len(tuples))
	for _, raw := range tuples {
		// Each tuple is [string, [...ctxs]].
		var pair [2]json.RawMessage
		if err := json.Unmarshal(raw, &pair); err != nil {
			return err
		}
		var key string
		if err := json.Unmarshal(pair[0], &key); err != nil {
			return err
		}
		var ctxs []SharedAssetCtx
		if err := json.Unmarshal(pair[1], &ctxs); err != nil {
			return err
		}
		(*m)[key] = ctxs
	}
	return nil
}

func (a *AllDexsAssetCtxs) UnmarshalJSON(data []byte) error {
	var wrapper struct {
		Ctxs DexAssetCtxsMap `json:"ctxs"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return err
	}
	a.Dexes = wrapper.Ctxs
	return nil
}

func (a AllDexsAssetCtxs) Key() string {
	return key(ChannelAllDexsAssetCtxs)
}


var (
	candleNoop = Candle{}
)
