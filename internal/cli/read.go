package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/phoenixperps/pperp/internal/ops"
	"github.com/phoenixperps/pperp/internal/output"
)

// named is an operation's call for a friendly command: the account and other path values filled in, the query given.
func named(name string, path map[string]string, query url.Values, body map[string]any) request {
	o, ok := ops.ByName(name)
	if !ok {
		panic("pperp: no operation " + name) // the table and the commands are built together
	}
	r := request{op: o, path: o.Path, query: query, body: body, args: map[string]any{}}
	if r.query == nil {
		r.query = url.Values{}
	}
	for p, v := range path {
		r.path = strings.Replace(r.path, "{"+p+"}", url.PathEscape(v), 1)
		r.args[p] = v
	}
	for f, v := range body {
		r.args[f] = v
	}
	return r
}

// runNamed runs an operation for a friendly command.
func (a *App) runNamed(ctx context.Context, k *conn, name string, path map[string]string, query url.Values, body map[string]any) error {
	return a.runOp(ctx, k, named(name, path, query, body))
}

// withAccount runs a command against this run's account.
func (a *App) withAccount(fn func(ctx context.Context, k *conn, acct string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		k, err := a.connect()
		if err != nil {
			return err
		}
		acct, err := a.account(cmd.Context(), k)
		if err != nil {
			return err
		}
		return fn(cmd.Context(), k, acct)
	}
}

func noArgs(_ *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usage("this command takes no arguments (got %s)", strings.Join(args, " "))
	}
	return nil
}

func (a *App) addRead(root *cobra.Command) {
	accounts := &cobra.Command{
		Use: "accounts", Short: "Your accounts", GroupID: "read", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			k, err := a.connect()
			if err != nil {
				return err
			}
			return a.runNamed(cmd.Context(), k, "list-accounts", nil, nil, nil)
		},
	}

	use := &cobra.Command{
		Use: "use <account>", Short: "Make an account the profile's default", GroupID: "read",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usage("pperp use ACCOUNT (pperp accounts lists them)")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := a.connect()
			if err != nil {
				return err
			}
			acct := strings.ToUpper(strings.TrimSpace(args[0]))
			if _, err := a.get(cmd.Context(), k, "/v1/accounts/"+url.PathEscape(acct), nil); err != nil {
				return err
			}
			k.p.DefaultAccount = acct
			a.keepProfile(k.name, k.p)
			if err := a.saveConfig(); err != nil {
				return err
			}
			a.note("%s is now the default account of profile %s.", acct, k.name)
			return nil
		},
	}

	var search string
	markets := &cobra.Command{
		Use: "markets", Short: "The markets and their rules", GroupID: "read", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			k, err := a.connect()
			if err != nil {
				return err
			}
			list, err := a.get(cmd.Context(), k, "/v1/markets", nil)
			if err != nil {
				return err
			}
			items, _ := list.([]any)
			if search != "" {
				q := strings.ToLower(search)
				items = slices.DeleteFunc(items, func(m any) bool {
					return !strings.Contains(strings.ToLower(str(m, "market")+" "+str(m, "name")), q)
				})
			}
			return a.show("list-markets", items)
		},
	}
	markets.Flags().StringVar(&search, "search", "", "only markets whose id or name has this")

	var posMarket string
	positions := &cobra.Command{
		Use: "positions", Short: "Open positions", GroupID: "read", Args: noArgs,
		RunE: a.withAccount(func(ctx context.Context, k *conn, acct string) error {
			return a.runNamed(ctx, k, "list-positions", map[string]string{"account_id": acct}, query("market", strings.ToUpper(posMarket)), nil)
		}),
	}
	positions.Flags().StringVar(&posMarket, "market", "", "only this market")

	var ordStatus, ordMarket string
	orders := &cobra.Command{
		Use: "orders", Short: "Working orders (--status history or all for the others)", GroupID: "read", Args: noArgs,
		RunE: a.withAccount(func(ctx context.Context, k *conn, acct string) error {
			return a.runNamed(ctx, k, "list-orders", map[string]string{"account_id": acct}, query("status", ordStatus, "market", strings.ToUpper(ordMarket)), nil)
		}),
	}
	orders.Flags().StringVar(&ordStatus, "status", "working", "working, history or all")
	orders.Flags().StringVar(&ordMarket, "market", "", "only this market")

	var fillMarket, fillLimit string
	fills := &cobra.Command{
		Use: "fills", Short: "Your fills, newest first", GroupID: "read", Args: noArgs,
		RunE: a.withAccount(func(ctx context.Context, k *conn, acct string) error {
			return a.runNamed(ctx, k, "list-fills", map[string]string{"account_id": acct}, query("market", strings.ToUpper(fillMarket), "limit", fillLimit), nil)
		}),
	}
	fills.Flags().StringVar(&fillMarket, "market", "", "only this market")
	fills.Flags().StringVar(&fillLimit, "limit", "", "how many on a page (1 to 200)")

	risk := &cobra.Command{
		Use: "risk", Short: "Live risk: equity, margin, the daily loss and drawdown room", GroupID: "read", Args: noArgs,
		RunE: a.withAccount(func(ctx context.Context, k *conn, acct string) error {
			return a.runNamed(ctx, k, "get-risk", map[string]string{"account_id": acct}, nil, nil)
		}),
	}

	var resolution string
	history := &cobra.Command{
		Use: "history equity|daily", Short: "Equity over time, or results by trading day", GroupID: "read",
		ValidArgs: []string{"equity", "daily"},
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 || (args[0] != "equity" && args[0] != "daily") {
				return usage("pperp history equity, or pperp history daily")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withAccount(func(ctx context.Context, k *conn, acct string) error {
				if args[0] == "daily" {
					return a.runNamed(ctx, k, "list-daily-results", map[string]string{"account_id": acct}, nil, nil)
				}
				return a.runNamed(ctx, k, "get-equity-history", map[string]string{"account_id": acct}, query("resolution", resolution), nil)
			})(cmd, args)
		},
	}
	history.Flags().StringVar(&resolution, "resolution", "", "equity: trade (a point per closed trade) or day")

	whoami := &cobra.Command{
		Use: "whoami", Short: "What you are signed in as: the credential, its access, accounts and limits", GroupID: "start", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			k, err := a.connect()
			if err != nil {
				return err
			}
			me, err := a.get(cmd.Context(), k, "/v1/me", nil)
			if err != nil {
				return err
			}
			o, ok := me.(*output.Object)
			if !ok {
				return a.show("get-me", me)
			}
			from := "profile " + k.name
			if k.fromEnv {
				from = "PHOENIXPERPS_API_KEY"
			}
			out := &output.Object{Keys: []string{"signed_in_with", "api"}, Vals: map[string]any{"signed_in_with": from, "api": k.c.BaseURL()}}
			out.Keys = append(out.Keys, o.Keys...)
			for _, key := range o.Keys {
				out.Vals[key] = o.Vals[key]
			}
			return a.show("get-me", out)
		},
	}

	var qSide, qSize string
	quote := &cobra.Command{
		Use: "quote <market>...", Short: "Bid, ask and last price (with --side and --size: what a market order would do)", GroupID: "read",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usage("pperp quote BTC-USD [ETH-USD...]")
			}
			if (qSide == "") != (qSize == "") {
				return usage("--side and --size go together")
			}
			if qSide != "" && len(args) != 1 {
				return usage("--side and --size: one market")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := a.connect()
			if err != nil {
				return err
			}
			markets := make([]string, len(args))
			for i, m := range args {
				markets[i] = strings.ToUpper(m)
			}
			if qSide != "" {
				acct, err := a.account(cmd.Context(), k)
				if err != nil {
					return err
				}
				body := map[string]any{"market": markets[0], "side": strings.ToLower(qSide), "type": "market"}
				sizeField(body, qSize)
				return a.runNamed(cmd.Context(), k, "test-order", map[string]string{"account_id": acct}, nil, body)
			}
			quotes, err := a.quotes(cmd.Context(), k, markets)
			if err != nil {
				return err
			}
			return a.show("quote", quotes)
		},
	}
	quote.Flags().StringVar(&qSide, "side", "", "buy or sell")
	quote.Flags().StringVar(&qSize, "size", "", "a size, or $USD for a notional ($5000)")

	root.AddCommand(whoami, accounts, use, markets, quote, positions, orders, fills, risk, history)
}

// query is url.Values from name, value pairs, leaving out the empty ones.
func query(pairs ...string) url.Values {
	q := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			q.Set(pairs[i], pairs[i+1])
		}
	}
	return q
}

// quotes are the markets' prices from the market stream (its first ticker of each).
func (a *App) quotes(ctx context.Context, k *conn, markets []string) ([]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ms := k.c.MarketStream(ctx)
	defer ms.Close()
	reply, err := ms.Subscribe(ctx, "ticker", markets, map[string]any{"interval_ms": 250})
	if err != nil {
		if ctx.Err() != nil {
			return nil, &exitError{ExitNetwork, "The market stream did not answer in time."}
		}
		return nil, err
	}
	var sub struct {
		Rejected []struct{ Symbol, Code string } `json:"rejected"`
	}
	_ = json.Unmarshal(reply, &sub)
	if len(sub.Rejected) > 0 {
		r := sub.Rejected[0]
		return nil, &exitError{ExitNotFound, fmt.Sprintf("%s: %s (pperp markets lists them)", r.Symbol, strings.ReplaceAll(r.Code, "_", " "))}
	}
	got := map[string]any{}
	for len(got) < len(markets) {
		f, err := ms.Next(ctx)
		if err != nil {
			var missing []string
			for _, m := range markets {
				if got[m] == nil {
					missing = append(missing, m)
				}
			}
			return nil, &exitError{ExitNetwork, "No price yet for " + strings.Join(missing, ", ") + "."}
		}
		if f.Type != "ticker" || got[f.Symbol] != nil {
			continue
		}
		d, err := output.Decode(f.Data)
		if err != nil {
			continue
		}
		row := &output.Object{Keys: []string{"market"}, Vals: map[string]any{"market": f.Symbol}}
		if o, ok := d.(*output.Object); ok {
			for _, key := range o.Keys {
				row.Keys = append(row.Keys, key)
				row.Vals[key] = o.Vals[key]
			}
		}
		got[f.Symbol] = row
	}
	out := make([]any, len(markets))
	for i, m := range markets {
		out[i] = got[m]
	}
	return out, nil
}

// sizeField puts a size in an order's body: $5000 is a notional in USD (the API sizes it down to the market's step).
func sizeField(body map[string]any, size string) {
	if usd, ok := strings.CutPrefix(size, "$"); ok {
		body["notional_usd"] = strings.ReplaceAll(usd, ",", "")
		return
	}
	body["size"] = size
}
