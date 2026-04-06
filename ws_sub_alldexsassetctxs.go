package hyperliquid

import "fmt"

func (w *WebsocketClient) AllDexsAssetCtxs(
	callback func(AllDexsAssetCtxs, error),
) (*Subscription, error) {
	payload := remoteAllDexsAssetCtxsSubscriptionPayload{
		Type: ChannelAllDexsAssetCtxs,
	}

	return w.subscribe(payload, func(msg any) {
		data, ok := msg.(AllDexsAssetCtxs)
		if !ok {
			callback(AllDexsAssetCtxs{}, fmt.Errorf("invalid message type"))
			return
		}
		callback(data, nil)
	})
}
