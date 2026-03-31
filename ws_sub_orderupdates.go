package hyperliquid

import "fmt"

type OrderUpdatesSubscriptionParams struct {
	User string
}

func (w *WebsocketClient) OrderUpdates(
	params OrderUpdatesSubscriptionParams,
	callback func([]WsOrderWithUser, error),
) (*Subscription, error) {
	payload := remoteOrderUpdatesSubscriptionPayload{
		Type: ChannelOrderUpdates,
		User: params.User,
	}

	return w.subscribe(payload, func(msg any) {
		orders, ok := msg.(WsOrders)
		if !ok {
			callback(nil, fmt.Errorf("invalid message type"))
			return
		}

		callbackOrders := make([]WsOrderWithUser, len(orders))
		for i := range orders {
			callbackOrders[i] = WsOrderWithUser{
				WsOrder: orders[i],
				User:    params.User,
			}
		}

		callback(callbackOrders, nil)
	})
}
