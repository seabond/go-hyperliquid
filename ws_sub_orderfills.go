package hyperliquid

import "fmt"

type OrderFillsSubscriptionParams struct {
	User string
	// AggregateByTime asks HL to merge fills that share order/liquidation
	// within a short time bucket server-side. Defaults to nil (field omitted
	// from wire, HL treats as false). Set a pointer to true to opt in.
	AggregateByTime *bool
}

func (w *WebsocketClient) OrderFills(
	params OrderFillsSubscriptionParams,
	callback func(WsOrderFills, error),
) (*Subscription, error) {
	payload := remoteOrderFillsSubscriptionPayload{
		Type:            ChannelUserFills,
		User:            params.User,
		AggregateByTime: params.AggregateByTime,
	}

	return w.subscribe(payload, func(msg any) {
		orders, ok := msg.(WsOrderFills)
		if !ok {
			callback(WsOrderFills{}, fmt.Errorf("invalid message type"))
			return
		}

		callback(orders, nil)
	})
}
