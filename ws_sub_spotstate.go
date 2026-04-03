package hyperliquid

import "fmt"

type SpotStateSubscriptionParams struct {
	User string
}

func (w *WebsocketClient) SpotState(
	params SpotStateSubscriptionParams,
	callback func(SpotStateMessage, error),
) (*Subscription, error) {
	payload := remoteSpotStateSubscriptionPayload{
		Type: ChannelSpotState,
		User: params.User,
	}

	return w.subscribe(payload, func(msg any) {
		data, ok := msg.(SpotStateMessage)
		if !ok {
			callback(SpotStateMessage{}, fmt.Errorf("invalid message type"))
			return
		}
		callback(data, nil)
	})
}
