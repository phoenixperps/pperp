package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	pp "github.com/phoenixperps/sdk/go"
	"github.com/spf13/cobra"

	"github.com/phoenixperps/pperp/internal/output"
)

/*
pperp stream: the account stream (snapshots, then every event once, resumed after a reconnect by the SDK) and the
market stream. Piped: one JSON object a line (NDJSON). On a terminal: a live log. It runs until Ctrl+C.
*/

func (a *App) addStream(root *cobra.Command) {
	stream := &cobra.Command{Use: "stream", Short: "Live: your accounts' events, or market data", GroupID: "stream"}

	var resume string
	var risk bool
	account := &cobra.Command{
		Use: "account", Short: "Every event of your accounts as it happens, after a snapshot of each (--account: one)", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			k, err := a.connect()
			if err != nil {
				return err
			}
			return a.streamAccount(cmd.Context(), k, resume, risk)
		},
	}
	account.Flags().StringVar(&resume, "resume", "", "start after this event id (the last one you have)")
	account.Flags().BoolVar(&risk, "risk", false, "live risk too: equity, margin and the room to the limits, on every change")
	stream.AddCommand(account)

	type channel struct{ use, channel, short string }
	for _, ch := range []channel{
		{"prices", "ticker", "Bid, ask and last price"},
		{"book", "book", "The order book's top levels"},
		{"trades", "trades", "Every trade"},
		{"candles", "candles", "Candles, the last one as it forms"},
		{"stats", "stats", "24-hour change, range and volume"},
	} {
		var intervalMs, depth, history int
		var interval string
		c := &cobra.Command{
			Use: ch.use + " <market>...", Short: ch.short,
			Args: func(_ *cobra.Command, args []string) error {
				if len(args) == 0 {
					return usage("pperp stream %s BTC-USD [ETH-USD...]", ch.use)
				}
				return nil
			},
			RunE: func(cmd *cobra.Command, args []string) error {
				k, err := a.connect()
				if err != nil {
					return err
				}
				params := map[string]any{}
				switch ch.channel {
				case "ticker":
					params["interval_ms"] = intervalMs
				case "book":
					params["depth"] = depth
				case "trades":
					params["history"] = history
				case "candles":
					params["interval"], params["history"] = interval, history
				}
				markets := make([]string, len(args))
				for i, m := range args {
					markets[i] = strings.ToUpper(m)
				}
				return a.streamMarket(cmd.Context(), k, ch.channel, markets, params)
			},
		}
		switch ch.channel {
		case "ticker":
			c.Flags().IntVar(&intervalMs, "interval-ms", 250, "at most one price per window: 100, 250, 500 or 1000")
		case "book":
			c.Aliases = []string{"depth"}
			c.Flags().IntVar(&depth, "depth", 5, "levels: 1, 5, 10 or 20")
		case "trades":
			c.Flags().IntVar(&history, "history", 0, "the last trades first (0 to 200)")
		case "candles":
			c.Flags().StringVar(&interval, "interval", "1m", "1m, 3m, 5m, 15m, 30m, 1h, 2h, 4h, 6h, 12h, 1d, 1w or 1M")
			c.Flags().IntVar(&history, "history", 0, "the last candles first (0 to 500)")
		}
		stream.AddCommand(c)
	}
	root.AddCommand(stream)
}

func (a *App) printLine(v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintln(a.Out, string(b))
}

func (a *App) streamAccount(ctx context.Context, k *conn, resume string, risk bool) error {
	var accounts []string
	if a.flags.account != "" {
		accounts = []string{strings.ToUpper(a.flags.account)}
	}
	s := k.c.AccountStream(ctx, pp.AccountStreamOptions{Accounts: accounts, Risk: risk, Resume: resume})
	defer s.Close()
	live := a.OutTTY && a.printer.Format == "table"
	select {
	case <-s.Ready():
	case <-ctx.Done():
		return ctx.Err()
	}
	if len(accounts) == 0 {
		list, err := a.get(ctx, k, "/v1/accounts", nil)
		if err != nil {
			return err
		}
		items, _ := list.([]any)
		for _, it := range items {
			accounts = append(accounts, str(it, "id"))
		}
	}
	for _, acct := range accounts {
		snap := s.Snapshot(acct)
		if snap == nil {
			continue
		}
		if !live {
			fmt.Fprintln(a.Out, string(snap))
			continue
		}
		v, _ := output.Decode(snap)
		positions, _ := lookupAny(v, "positions").([]any)
		orders, _ := lookupAny(v, "orders").([]any)
		fmt.Fprintf(a.Out, "%s  %s: %d positions, %d working orders\n", time.Now().Format("15:04:05.000"), acct, len(positions), len(orders))
	}
	if live {
		a.note("Following %s. Ctrl+C to stop.", strings.Join(accounts, ", "))
	}
	risks := map[string]string{}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	events := make(chan pp.StreamEvent)
	failed := make(chan error, 1)
	go func() {
		for {
			e, err := s.Next(ctx)
			if err != nil {
				failed <- err
				return
			}
			select {
			case events <- e:
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failed:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case e := <-events:
			// the event itself, as the CLI always printed it (the stream's seq and replay flag stay out)
			if !live {
				a.printLine(e.Event)
				continue
			}
			fmt.Fprintf(a.Out, "%s  %-24s %s  %s\n", time.UnixMilli(e.Time).Format("15:04:05.000"), e.Type, e.AccountID, eventText(e.Event))
		case <-tick.C:
			if !risk {
				continue
			}
			for _, acct := range accounts {
				r := s.Risk(acct)
				if r == nil || risks[acct] == string(r) {
					continue
				}
				risks[acct] = string(r)
				if !live {
					fmt.Fprintln(a.Out, string(r))
					continue
				}
				a.riskLine(acct, r)
			}
		}
	}
}

func lookupAny(v any, path string) any {
	if o, ok := v.(*output.Object); ok {
		x, _ := o.Get(path)
		return x
	}
	return nil
}

// eventText is an event in a few words: its order, fill or position.
func eventText(e pp.Event) string {
	for _, kind := range []string{"fill", "order", "position"} {
		m, ok := e.Data[kind].(map[string]any)
		if !ok {
			continue
		}
		get := func(k string) string {
			if v, ok := m[k]; ok && v != nil {
				return fmt.Sprint(v)
			}
			return ""
		}
		parts := []string{get("market"), get("side")}
		if s := get("size"); s != "" {
			parts = append(parts, s)
		}
		switch kind {
		case "fill":
			parts = append(parts, "@ "+get("price"))
		case "order":
			if p := firstOf(get("limit_price"), get("stop_price")); p != "" {
				parts = append(parts, "@ "+p)
			}
			parts = append(parts, "("+get("status")+")")
		case "position":
			if p := get("entry_price"); p != "" {
				parts = append(parts, "entry "+p)
			}
		}
		return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
	}
	return ""
}

// riskLine is a risk frame on a terminal: yellow when it warns.
func (a *App) riskLine(acct string, raw json.RawMessage) {
	v, err := output.Decode(raw)
	if err != nil {
		return
	}
	d := lookupAny(v, "data")
	if d == nil {
		d = v
	}
	line := fmt.Sprintf("%s  %-24s %s  equity %s, daily loss room %s, drawdown room %s", time.Now().Format("15:04:05.000"), "risk", acct,
		firstOf(str(d, "equity"), "-"), firstOf(str(d, "daily_loss.room"), "-"), firstOf(str(d, "max_drawdown.room"), "-"))
	if w, _ := lookupAny(d, "warnings").([]any); len(w) > 0 {
		line += "  " + output.Compact(w)
		if a.printer.Color {
			line = "\x1b[33m" + line + "\x1b[0m"
		}
	}
	fmt.Fprintln(a.Out, line)
}

func (a *App) streamMarket(ctx context.Context, k *conn, channel string, markets []string, params map[string]any) error {
	ms := k.c.MarketStream(ctx)
	defer ms.Close()
	reply, err := ms.Subscribe(ctx, channel, markets, params)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	var sub struct {
		Rejected []struct{ Symbol, Code string } `json:"rejected"`
	}
	_ = json.Unmarshal(reply, &sub)
	for _, r := range sub.Rejected {
		a.note("%s: %s", r.Symbol, strings.ReplaceAll(r.Code, "_", " "))
	}
	if len(sub.Rejected) == len(markets) {
		return &exitError{ExitNotFound, "Nothing to follow (pperp markets lists the markets)."}
	}
	live := a.OutTTY && a.printer.Format == "table"
	for {
		f, err := ms.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if f.Type == "snapshot_end" {
			continue
		}
		if !live {
			fmt.Fprintln(a.Out, string(bytes.TrimSpace(f.Raw)))
			continue
		}
		for _, line := range marketLines(f) {
			fmt.Fprintln(a.Out, line)
		}
	}
}

// marketLines is a market frame as log lines.
func marketLines(f pp.MarketFrame) []string {
	v, err := output.Decode(f.Data)
	if err != nil {
		return nil
	}
	now := time.Now().Format("15:04:05.000")
	at := func(x any) string {
		if ms, err := json.Number(str(x, "time")).Int64(); err == nil && ms > 0 {
			return time.UnixMilli(ms).Format("15:04:05.000")
		}
		return now
	}
	switch f.Type {
	case "ticker":
		return []string{fmt.Sprintf("%s  %-10s bid %s  ask %s  last %s", at(v), f.Symbol, str(v, "bid"), str(v, "ask"), str(v, "last"))}
	case "book":
		side := func(levels any) string {
			var parts []string
			arr, _ := levels.([]any)
			for _, l := range arr {
				if pq, ok := l.([]any); ok && len(pq) == 2 {
					parts = append(parts, fmt.Sprintf("%v x %v", pq[0], pq[1]))
				}
			}
			return strings.Join(parts, "  ")
		}
		return []string{fmt.Sprintf("%s  %-10s bids %s | asks %s", at(v), f.Symbol, side(lookupAny(v, "bids")), side(lookupAny(v, "asks")))}
	case "trades", "candles":
		arr, ok := v.([]any)
		if !ok {
			arr = []any{v}
		}
		var out []string
		for _, t := range arr {
			if f.Type == "trades" {
				out = append(out, fmt.Sprintf("%s  %-10s %-4s %s @ %s", at(t), f.Symbol, str(t, "side"), str(t, "size"), str(t, "price")))
				continue
			}
			state := "forming"
			if str(t, "final") == "true" {
				state = "final"
			}
			out = append(out, fmt.Sprintf("%s  %-10s o %s  h %s  l %s  c %s  v %s  (%s)", at(t), f.Symbol, str(t, "open"), str(t, "high"), str(t, "low"), str(t, "close"), str(t, "volume"), state))
		}
		return out
	case "stats":
		return []string{fmt.Sprintf("%s  %-10s last %s  24h %s%%  high %s  low %s  volume %s", now, f.Symbol, str(v, "last"), str(v, "change_24h_pct"), str(v, "high_24h"), str(v, "low_24h"), str(v, "volume_24h"))}
	}
	return []string{fmt.Sprintf("%s  %s %s %s", now, f.Type, f.Symbol, output.Compact(v))}
}
