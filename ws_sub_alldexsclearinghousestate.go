package hyperliquid

import "fmt"

type AllDexsClearinghouseStateSubscriptionParams struct {
	User string
}

func (w *WebsocketClient) AllDexsClearinghouseState(
	params AllDexsClearinghouseStateSubscriptionParams,
	callback func(AllDexsClearinghouseState, error),
) (*Subscription, error) {
	payload := remoteAllDexsClearinghouseStateSubscriptionPayload{
		Type: ChannelAllDexsClearinghouseState,
		User: params.User,
	}

	return w.subscribe(payload, func(msg any) {
		data, ok := msg.(AllDexsClearinghouseState)
		if !ok {
			callback(AllDexsClearinghouseState{}, fmt.Errorf("invalid message type"))
			return
		}
		callback(data, nil)
	})
}
