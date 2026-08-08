package hyperliquid

import "testing"

// The payload below is a verbatim capture from mainnet on 2026-08-06: an
// openOrders subscription for 0x9bbc…9b7a, whose only resting order was a
// UI-placed take-profit. It is the contract this decoder has to honour — the
// channel sends the frontendOpenOrders field set even though the REST
// `openOrders` request for the same account answers with eight fields.
//
// Every expectation here is read off that capture, not off the decoder.
const liveOpenOrdersPush = `{"dex":"","user":"0x9bbcc6a19240d683a1e3b2087c4eeb276f3a9b7a","orders":[` +
	`{"coin":"HYPE","side":"A","limitPx":"65.32","sz":"0.0","oid":501149851930,` +
	`"timestamp":1784796308133,"triggerCondition":"Price above 71","isTrigger":true,` +
	`"triggerPx":"71.0","children":[],"isPositionTpsl":true,"reduceOnly":true,` +
	`"orderType":"Take Profit Market","origSz":"0.0","tif":null,"cloid":null}]}`

func TestOpenOrders_DecodesFrontendFields(t *testing.T) {
	var got OpenOrders
	if err := got.UnmarshalJSON([]byte(liveOpenOrdersPush)); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Orders) != 1 {
		t.Fatalf("orders = %d, want 1", len(got.Orders))
	}
	o := got.Orders[0]

	// A leader's UI-placed stop: no cloid, zero size, and the two trigger
	// flags that tell it apart from an entry order.
	if o.Cloid != nil {
		t.Errorf("Cloid = %q, want nil (HL sends null for orders placed without one)", *o.Cloid)
	}
	if o.Sz != "0.0" || o.OrigSz != "0.0" {
		t.Errorf("Sz/OrigSz = %q/%q, want 0.0/0.0", o.Sz, o.OrigSz)
	}
	if !o.IsTrigger {
		t.Error("IsTrigger = false, want true — a stop must be distinguishable from an entry limit order")
	}
	if !o.IsPositionTpsl {
		t.Error("IsPositionTpsl = false, want true for a UI-placed position stop")
	}
	if !o.ReduceOnly {
		t.Error("ReduceOnly = false, want true")
	}
	if o.OrderType != "Take Profit Market" {
		t.Errorf("OrderType = %q, want %q", o.OrderType, "Take Profit Market")
	}
	if o.TriggerPx != "71.0" {
		t.Errorf("TriggerPx = %q, want %q", o.TriggerPx, "71.0")
	}
	if o.Tif != nil {
		t.Errorf("Tif = %q, want nil — HL sends tif:null on trigger orders", *o.Tif)
	}
}

// An ordinary resting entry order carries a tif and no trigger flags. Shape
// taken from HL's documented ALO/GTC limit order response.
func TestOpenOrders_EntryOrderHasNoTriggerFlags(t *testing.T) {
	const push = `{"dex":"","user":"0xabc","orders":[` +
		`{"coin":"BTC","side":"B","limitPx":"60000.0","sz":"0.1","oid":42,` +
		`"timestamp":1,"origSz":"0.1","cloid":"0x00000000000000000000000000000001",` +
		`"isTrigger":false,"isPositionTpsl":false,"reduceOnly":false,` +
		`"orderType":"Limit","tif":"Alo"}]}`

	var got OpenOrders
	if err := got.UnmarshalJSON([]byte(push)); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	o := got.Orders[0]
	if o.IsTrigger || o.IsPositionTpsl || o.ReduceOnly {
		t.Errorf("entry order classified as trigger/reduce-only: %+v", o)
	}
	if o.Tif == nil || *o.Tif != "Alo" {
		t.Errorf("Tif = %v, want Alo", o.Tif)
	}
	if o.Cloid == nil || *o.Cloid != "0x00000000000000000000000000000001" {
		t.Errorf("Cloid = %v, want the echoed client id", o.Cloid)
	}
}
