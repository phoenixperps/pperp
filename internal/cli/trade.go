package cli

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/phoenixperps/pperp/internal/mcp"
	"github.com/phoenixperps/pperp/internal/ops"
	"github.com/phoenixperps/pperp/internal/output"
)

func (a *App) addTrade(root *cobra.Command) {
	root.AddCommand(a.orderCommand("buy"), a.orderCommand("sell"), a.closeCommand(), a.cancelCommand(), a.flattenCommand())
}

// level is a take-profit or stop-loss level: a price (64000) or a percent from the fill (2%).
func level(v string) map[string]any {
	if p, ok := strings.CutSuffix(v, "%"); ok {
		return map[string]any{"percent": p}
	}
	return map[string]any{"price": v}
}

func (a *App) orderCommand(side string) *cobra.Command {
	var f struct {
		limit, stop, leverage, sl, trail, slippage, clientID, tif string
		tp                                                        []string
		reduceOnly, postOnly, dryRun                              bool
	}
	verb := map[string]string{"buy": "Buy (long)", "sell": "Sell (short)"}[side]
	cmd := &cobra.Command{
		Use:     side + " <market> <size|$usd>",
		Short:   verb + ": a market, limit or stop order, with exits if you like",
		GroupID: "trade",
		Example: fmt.Sprintf(`  pperp %[1]s BTC-USD 0.01                       at the market
  pperp %[1]s BTC-USD $5000 --limit 62000        a limit order for 5,000 USD of BTC
  pperp %[1]s ETH-USD 1 --stop 3500 --limit 3510 a stop-limit order
  pperp %[1]s BTC-USD 0.01 --tp 2%% --sl 1%%      with a take-profit and a stop-loss from the fill
  pperp %[1]s BTC-USD 0.01 --dry-run             what it would do, nothing sent`, side),
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 2 {
				return usage("pperp %s MARKET SIZE (or $USD)", side)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			k, err := a.connect()
			if err != nil {
				return err
			}
			acct, err := a.account(ctx, k)
			if err != nil {
				return err
			}
			market := strings.ToUpper(args[0])
			body := map[string]any{"market": market, "side": side, "type": "market"}
			sizeField(body, args[1])
			switch {
			case f.stop != "" && f.limit != "":
				body["type"], body["stop_price"], body["limit_price"] = "stop_limit", f.stop, f.limit
			case f.stop != "":
				body["type"], body["stop_price"] = "stop_market", f.stop
			case f.limit != "":
				body["type"], body["limit_price"] = "limit", f.limit
			}
			if f.reduceOnly {
				body["reduce_only"] = true
			}
			if f.postOnly {
				body["post_only"] = true
			}
			for k, v := range map[string]string{"time_in_force": f.tif, "client_order_id": f.clientID, "max_slippage_pct": f.slippage} {
				if v != "" {
					body[k] = v
				}
			}
			if len(f.tp) > 0 || f.sl != "" || f.trail != "" {
				exits := map[string]any{}
				if len(f.tp) > 0 {
					var tps []any
					for _, t := range f.tp {
						tps = append(tps, level(t))
					}
					exits["take_profit"] = tps
				}
				if f.sl != "" || f.trail != "" {
					sl := map[string]any{}
					if f.sl != "" {
						sl = level(f.sl)
					}
					if f.trail != "" {
						trail := level(f.trail)
						if p, ok := trail["price"]; ok { // a trail is a distance, or a percent
							trail = map[string]any{"distance": p}
						}
						sl["trail"] = trail
						if f.sl == "" {
							for k, v := range trail {
								sl[k] = v
							}
						}
					}
					exits["stop_loss"] = sl
				}
				body["exits"] = exits
			}
			path := map[string]string{"account_id": acct}
			if f.dryRun {
				return a.runNamed(ctx, k, "test-order", path, nil, body)
			}
			r := named("place-order", path, nil, body)
			if k.p.confirms() {
				if err := a.preview(ctx, k, r, f.leverage); err != nil {
					return err
				}
				r.asked = true
			}
			if f.leverage != "" {
				lev, err := strconv.Atoi(strings.TrimSuffix(f.leverage, "x"))
				if err != nil || lev < 1 {
					return usage("--leverage is a whole number (10 or 10x)")
				}
				if _, _, err := a.call(ctx, k, http.MethodPut, "/v1/accounts/"+url.PathEscape(acct)+"/margin/"+url.PathEscape(market), nil, map[string]any{"leverage": lev}, ""); err != nil {
					return err
				}
				a.note("Leverage on %s: %dx.", market, lev)
			}
			return a.runOp(ctx, k, r)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.limit, "limit", "", "a limit price (a limit order; with --stop, a stop-limit)")
	fl.StringVar(&f.stop, "stop", "", "a stop price (a stop order)")
	fl.StringVar(&f.leverage, "leverage", "", "set the market's leverage first (10 or 10x)")
	fl.StringArrayVar(&f.tp, "tp", nil, "a take-profit: a price, or a percent from the fill (2%); repeat it to split the size")
	fl.StringVar(&f.sl, "sl", "", "a stop-loss: a price, or a percent from the fill (1%)")
	fl.StringVar(&f.trail, "trail", "", "a trailing stop-loss: a price distance (500), or a percent (1%)")
	fl.BoolVar(&f.reduceOnly, "reduce-only", false, "only reduce a position")
	fl.BoolVar(&f.postOnly, "post-only", false, "a limit order that only rests (never takes)")
	fl.StringVar(&f.tif, "tif", "", "time in force: gtc, gtd, ioc or fok")
	fl.StringVar(&f.slippage, "slippage", "", "a market order's slippage bound, in percent")
	fl.StringVar(&f.clientID, "client-order-id", "", "your own id for the order")
	fl.BoolVar(&f.dryRun, "dry-run", false, "only say what it would do (the what-if: nothing is sent)")
	return cmd
}

// preview prints what an order would do (the what-if) and asks before it is placed.
func (a *App) preview(ctx context.Context, k *conn, r request, leverage string) error {
	acct := r.args["account_id"].(string)
	_, v, err := a.call(ctx, k, http.MethodPost, "/v1/accounts/"+url.PathEscape(acct)+"/orders/test", nil, r.body, "")
	if err != nil {
		return err
	}
	w := data(v)
	line := mcp.Summary(r.op, r.args)
	if leverage != "" {
		line += " Leverage " + strings.TrimSuffix(leverage, "x") + "x first."
	}
	a.note("%s", line)
	if str(w, "result") == "would_reject" {
		return &exitError{ExitRule, "It would be refused: " + firstOf(str(w, "reject.message"), str(w, "reject"))}
	}
	var facts []string
	add := func(label, path string) {
		if s := str(w, path); s != "" {
			facts = append(facts, label+" "+s)
		}
	}
	add("price", "execution.expected_price")
	add("worst", "execution.worst_price")
	add("size", "size")
	add("notional", "notional")
	add("fee", "fee")
	if leverage == "" { // at another leverage the margin is not this one
		add("margin", "margin.required")
		add("margin left", "margin.available_after")
	}
	add("position after", "position_after.size")
	if len(facts) > 0 {
		a.note("  %s: %s", strings.ReplaceAll(str(w, "result"), "_", " "), strings.Join(facts, ", "))
	}
	return a.confirm("Place it on " + acct + " (" + k.env + ")?")
}

func (a *App) closeCommand() *cobra.Command {
	var percent, size, clientID string
	cmd := &cobra.Command{
		Use: "close <market>", Short: "Close a position at the market (all of it, --percent or --size)", GroupID: "trade",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usage("pperp close MARKET [--percent N | --size N]")
			}
			if percent != "" && size != "" {
				return usage("--percent or --size, not both")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			k, err := a.connect()
			if err != nil {
				return err
			}
			acct, err := a.account(ctx, k)
			if err != nil {
				return err
			}
			market := strings.ToUpper(args[0])
			body := map[string]any{}
			if size != "" {
				body["size"] = size
			}
			if percent != "" && strings.TrimSuffix(percent, "%") != "100" {
				part, err := a.partOf(ctx, k, acct, market, strings.TrimSuffix(percent, "%"))
				if err != nil {
					return err
				}
				body["size"] = part
				a.note("%s%% of the %s position: %s.", strings.TrimSuffix(percent, "%"), market, part)
			}
			if clientID != "" {
				body["client_order_id"] = clientID
			}
			return a.runNamed(ctx, k, "close-position", map[string]string{"account_id": acct, "market": market}, nil, body)
		},
	}
	cmd.Flags().StringVar(&percent, "percent", "", "close this percent of it, rounded down to the market's size step")
	cmd.Flags().StringVar(&size, "size", "", "close this size of it")
	cmd.Flags().StringVar(&clientID, "client-order-id", "", "your own id for the closing order")
	return cmd
}

// partOf is a percent of a position's size, rounded down to the market's size step (exact decimals).
func (a *App) partOf(ctx context.Context, k *conn, acct, market, percent string) (string, error) {
	pct, ok := new(big.Rat).SetString(percent)
	if !ok || pct.Sign() <= 0 || pct.Cmp(big.NewRat(100, 1)) > 0 {
		return "", usage("--percent is above 0 and at most 100")
	}
	pos, err := a.get(ctx, k, "/v1/accounts/"+url.PathEscape(acct)+"/positions/"+url.PathEscape(market), nil)
	if err != nil {
		return "", err
	}
	m, err := a.get(ctx, k, "/v1/markets/"+url.PathEscape(market), nil)
	if err != nil {
		return "", err
	}
	size, ok1 := new(big.Rat).SetString(strings.TrimPrefix(str(pos, "size"), "-"))
	step, ok2 := new(big.Rat).SetString(str(m, "size_step"))
	if !ok1 || !ok2 || step.Sign() <= 0 {
		return "", fmt.Errorf("the position's size or the market's step could not be read")
	}
	part := new(big.Rat).Mul(size, pct)
	part.Quo(part, big.NewRat(100, 1))
	steps := new(big.Rat).Quo(part, step)
	n := new(big.Int).Quo(steps.Num(), steps.Denom()) // whole steps, rounded down
	if n.Sign() == 0 {
		return "", &exitError{ExitRule, fmt.Sprintf("%s%% of %s is below the market's size step (%s).", percent, str(pos, "size"), str(m, "size_step"))}
	}
	out := new(big.Rat).Mul(new(big.Rat).SetInt(n), step)
	return out.FloatString(decimals(str(m, "size_step"))), nil
}

// decimals is how many decimals a step has (0.001: 3).
func decimals(step string) int {
	if i := strings.IndexByte(step, '.'); i >= 0 {
		return len(strings.TrimRight(step[i+1:], "0"))
	}
	return 0
}

func (a *App) cancelCommand() *cobra.Command {
	var all bool
	var market string
	cmd := &cobra.Command{
		Use: "cancel <order>... | --all [--market M]", Short: "Cancel orders (--all: every working order)", GroupID: "trade",
		Args: func(_ *cobra.Command, args []string) error {
			if all == (len(args) > 0) {
				return usage("pperp cancel ORDER_ID..., or pperp cancel --all [--market M]")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			k, err := a.connect()
			if err != nil {
				return err
			}
			acct, err := a.account(ctx, k)
			if err != nil {
				return err
			}
			if all {
				body := map[string]any{}
				if market != "" {
					body["market"] = strings.ToUpper(market)
				}
				return a.runNamed(ctx, k, "cancel-all-orders", map[string]string{"account_id": acct}, nil, body)
			}
			for _, id := range args {
				r := named("cancel-order", map[string]string{"account_id": acct, "order_id": id}, nil, nil)
				r.asked = true // a cancel takes risk away: never asked
				if err := a.runOp(ctx, k, r); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "every working order of the account")
	cmd.Flags().StringVar(&market, "market", "", "with --all: only this market's")
	return cmd
}

func (a *App) flattenCommand() *cobra.Command {
	var every bool
	cmd := &cobra.Command{
		Use: "flatten", Short: "Cancel every working order and close every position (--every-account: on all of them)", GroupID: "trade", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			k, err := a.connect()
			if err != nil {
				return err
			}
			var accounts []string
			if every {
				list, err := a.get(ctx, k, "/v1/accounts", nil)
				if err != nil {
					return err
				}
				items, _ := list.([]any)
				for _, it := range items {
					if str(it, "status") == "active" {
						accounts = append(accounts, str(it, "id"))
					}
				}
				if len(accounts) == 0 {
					return &exitError{ExitNotFound, "No active account to flatten."}
				}
				if err := a.confirmTyped(fmt.Sprintf("Cancel every working order and close every position on %d accounts (%s): %s.", len(accounts), k.env, strings.Join(accounts, ", ")), "flatten"); err != nil {
					return err
				}
			} else {
				acct, err := a.account(ctx, k)
				if err != nil {
					return err
				}
				accounts = []string{acct}
				o, _ := ops.ByName("flatten")
				if err := a.confirmTyped(mcp.Summary(o, map[string]any{"account_id": acct}), acct); err != nil {
					return err
				}
			}
			var results []any
			for _, acct := range accounts {
				res, err := a.flatten(ctx, k, acct)
				if err != nil {
					return err
				}
				results = append(results, res)
			}
			if every {
				return a.show("flatten-all", results)
			}
			return a.show("flatten", results[0])
		},
	}
	cmd.Flags().BoolVar(&every, "every-account", false, "every active account this credential reaches")
	return cmd
}

// flatten flattens one account: the same call (its Idempotency-Key) repeated while the API answers 202 (still running).
func (a *App) flatten(ctx context.Context, k *conn, acct string) (any, error) {
	key := newKey()
	if a.flags.idempotencyKey != "" {
		key = a.flags.idempotencyKey
	}
	path := "/v1/accounts/" + url.PathEscape(acct) + "/flatten"
	for {
		res, v, err := a.call(ctx, k, http.MethodPost, path, nil, nil, key)
		if err != nil {
			return nil, err
		}
		if res.StatusCode != http.StatusAccepted {
			return data(v), nil
		}
		wait := 2 * time.Second
		if s, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && s > 0 && s <= 60 {
			wait = time.Duration(s) * time.Second
		}
		a.note("%s: flattening, %s so far...", acct, progress(data(v)))
		select {
		case <-ctx.Done():
			return nil, &exitError{ExitNetwork, fmt.Sprintf("Stopped while %s was still flattening: pperp flatten --account %s --idempotency-key %s follows it.", acct, acct, key)}
		case <-time.After(wait):
		}
	}
}

func progress(v any) string {
	o, ok := v.(*output.Object)
	if !ok {
		return "working"
	}
	closes, _ := o.Vals["closes"].([]any)
	return fmt.Sprintf("%s orders canceled, %d closes", firstOf(str(o, "orders_canceled"), "0"), len(closes))
}
