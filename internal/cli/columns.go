package cli

import "github.com/phoenixperps/pperp/internal/output"

func cols(pairs ...string) []output.Column {
	out := make([]output.Column, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, output.Column{Title: pairs[i], Path: pairs[i+1]})
	}
	return out
}

// columns are a list's table and CSV columns, by the operation it comes from (any other list: every scalar field).
var columns = map[string][]output.Column{
	"list-accounts":            cols("ACCOUNT", "id", "STAGE", "stage", "STATUS", "status", "PRODUCT", "product", "BALANCE", "balance", "START", "start_balance", "PARKED", "parked"),
	"list-markets":             cols("MARKET", "market", "NAME", "name", "STATUS", "status", "TICK", "tick_size", "STEP", "size_step", "MIN SIZE", "min_size", "MAX LEV", "max_leverage", "FEE %", "fee_rate_pct"),
	"list-positions":           cols("MARKET", "market", "SIDE", "side", "SIZE", "size", "ENTRY", "entry_price", "MARK", "mark_price", "UPNL", "unrealized_pnl", "LEV", "leverage", "MARGIN", "margin_mode", "LIQ", "liquidation.price", "PROTECTED", "protected"),
	"list-closed-positions":    cols("MARKET", "market", "SIDE", "side", "SIZE", "size", "ENTRY", "entry_price", "EXIT", "exit_price", "PNL", "pnl", "FEES", "fees", "OPENED", "opened_at", "CLOSED", "closed_at"),
	"list-orders":              cols("ORDER", "id", "MARKET", "market", "SIDE", "side", "TYPE", "type", "SIZE", "size", "FILLED", "filled_size", "LIMIT", "limit_price", "STOP", "stop_price", "STATUS", "status", "SOURCE", "source", "CREATED", "created_at"),
	"list-fills":               cols("TIME", "time", "MARKET", "market", "SIDE", "side", "SIZE", "size", "PRICE", "price", "FEE", "fee", "ORDER", "order_id", "FILL", "id"),
	"list-strategies":          cols("STRATEGY", "id", "TYPE", "type", "MARKET", "market", "SIDE", "side", "STATUS", "status", "ENDED", "end_reason", "CREATED", "created_at"),
	"list-events":              cols("TIME", "time", "TYPE", "type", "ACCOUNT", "account_id", "SOURCE", "source", "EVENT", "id"),
	"list-daily-results":       cols("DATE", "date", "TRADES", "trades", "GROSS", "gross_pnl", "FEES", "fees", "PNL", "pnl", "BALANCE", "balance_end"),
	"list-margin-settings":     cols("MARKET", "market", "LEVERAGE", "leverage", "MODE", "margin_mode", "MAX", "max_leverage", "FROM", "source", "LOCKED", "locked"),
	"list-webhook-endpoints":   cols("ENDPOINT", "id", "URL", "url", "FORMAT", "format", "STATUS", "status", "EVENTS", "events"),
	"list-webhook-deliveries":  cols("DELIVERY", "id", "EVENT", "event_type", "STATUS", "status", "TEST", "test", "CREATED", "created_at", "DELIVERED", "delivered_at"),
	"list-trading-view-hooks":  cols("HOOK", "id", "NAME", "name", "ACCOUNT", "account_id", "STATUS", "status", "MODE", "position_mode", "MARKETS", "markets"),
	"list-trading-view-alerts": cols("ALERT", "id", "STATUS", "status", "MARKET", "market", "ACTION", "action", "REASON", "reason", "RECEIVED", "received_at"),
	"list-webhook-event-types": cols("TYPE", "type", "GROUP", "group", "WHAT", "description"),
	"quote":                    cols("MARKET", "market", "BID", "bid", "ASK", "ask", "LAST", "last", "TIME", "time"),
	"flatten-all":              cols("ACCOUNT", "account_id", "ORDERS CANCELED", "orders_canceled", "CLOSES", "closes"),
}
