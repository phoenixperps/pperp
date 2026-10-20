package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/phoenixperps/pperp/internal/mcp"
	"github.com/phoenixperps/pperp/internal/ops"
)

/*
One command per API operation (design 15 section 3 "Generated commands"), from the operation table the MCP tools come
from too: the same name (place-order), the same arguments. The account comes from --account (or the default one);
the other path parameters are positional; query parameters and the body's top-level scalars are flags; the rest of a
body is --data (@file, - for stdin, or the JSON itself) and --set a.b=value, flags last.
*/

// typedConfirm: operations that cannot be undone ask for the account's name typed back.
var typedConfirm = map[string]bool{"flatten": true, "cancel-all-orders": true, "create-sandbox-account": true}

// schema is the part of a JSON Schema the flags are built from.
type schema struct {
	Type        any                `json:"type"`
	Description string             `json:"description"`
	Enum        []any              `json:"enum"`
	Properties  map[string]*schema `json:"properties"`
	Items       *schema            `json:"items"`
	Required    []string           `json:"required"`
}

func (s *schema) is(t string) bool {
	if s == nil {
		return false
	}
	switch x := s.Type.(type) {
	case string:
		return x == t
	case []any:
		for _, e := range x {
			if e == t {
				return true
			}
		}
	}
	return false
}

// scalar tells a field a flag can carry (not an object or a list).
func (s *schema) scalar() bool { return !s.is("object") && !s.is("array") }

func kebab(s string) string { return strings.ReplaceAll(s, "_", "-") }

// convert is a value typed on the command line as its field takes it: integers and plain numbers as JSON numbers,
// decimals as the strings they are, true/false, null where allowed, objects and lists as JSON.
func convert(s *schema, raw string) (any, error) {
	if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") {
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, usage("%q is not valid JSON: %v", raw, err)
		}
		return v, nil
	}
	if s == nil {
		return raw, nil
	}
	if raw == "null" && s.is("null") {
		return nil, nil
	}
	switch {
	case s.is("string"):
		return raw, nil
	case s.is("integer"):
		if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
			return nil, usage("%q is not a whole number", raw)
		}
		return json.Number(raw), nil
	case s.is("number"):
		if !json.Valid([]byte(raw)) || strings.ContainsAny(raw, `"{}[]tfn`) {
			return nil, usage("%q is not a number", raw)
		}
		return json.Number(raw), nil
	case s.is("boolean"):
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, usage("%q is not true or false", raw)
		}
		return b, nil
	case s.is("object"), s.is("array"):
		return nil, usage("%q: this field takes JSON ({...} or [...])", raw)
	}
	return raw, nil
}

// setIn sets a dotted path (exits.take_profit.0.price) in a body, typed by the schema along it.
func setIn(cur any, s *schema, parts []string, raw string) (any, error) {
	if len(parts) == 0 {
		return convert(s, raw)
	}
	p := parts[0]
	if n, err := strconv.Atoi(p); err == nil && n >= 0 && n < 1000 {
		arr, ok := cur.([]any)
		if cur != nil && !ok {
			return nil, usage("--set: %s is not a list", p)
		}
		for len(arr) <= n {
			arr = append(arr, nil)
		}
		var item *schema
		if s != nil {
			item = s.Items
		}
		v, err := setIn(arr[n], item, parts[1:], raw)
		arr[n] = v
		return arr, err
	}
	obj, ok := cur.(map[string]any)
	if cur != nil && !ok {
		return nil, usage("--set: %s is inside a value that is not an object", p)
	}
	if obj == nil {
		obj = map[string]any{}
	}
	var sub *schema
	if s != nil {
		sub = s.Properties[p]
	}
	v, err := setIn(obj[p], sub, parts[1:], raw)
	obj[p] = v
	return obj, err
}

// readData is --data: @file, - or @- for stdin, else the JSON itself; an object.
func (a *App) readData(spec string) (map[string]any, error) {
	var raw []byte
	var err error
	switch {
	case spec == "-" || spec == "@-":
		raw, err = io.ReadAll(a.reader)
	case strings.HasPrefix(spec, "@"):
		raw, err = os.ReadFile(spec[1:])
	default:
		raw = []byte(spec)
	}
	if err != nil {
		return nil, usage("--data: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, usage("--data is not a JSON object")
	}
	return m, nil
}

// request is one operation's call, put together from the command line.
type request struct {
	op    ops.Op
	path  string
	query url.Values
	body  map[string]any
	args  map[string]any // every argument flattened (the confirmation's summary)
	asked bool           // the command asked already (its own question)
}

func (a *App) addOps(root *cobra.Command) {
	taken := map[string]bool{}
	for _, c := range root.Commands() {
		taken[c.Name()] = true
	}
	for _, o := range ops.All {
		if !taken[o.Name] {
			root.AddCommand(a.opCommand(o))
		}
	}
}

func (a *App) opCommand(o ops.Op) *cobra.Command {
	var in schema
	_ = json.Unmarshal([]byte(o.InputSchema), &in)
	var positional []string
	for _, p := range o.PathParams {
		if p != "account_id" {
			positional = append(positional, p)
		}
	}
	use := o.Name
	for _, p := range positional {
		use += " <" + p + ">"
	}
	long := o.Summary + "."
	if o.Description != "" {
		long += "\n\n" + o.Description
	}
	if slices.Contains(o.PathParams, "account_id") {
		long += "\n\nThe account: --account, else the profile's default account, else your only one."
	}
	var nested []string
	if o.Body {
		for _, f := range o.BodyFields {
			if s := in.Properties[f]; s != nil && !s.scalar() {
				nested = append(nested, f)
			}
		}
	}
	if len(nested) > 0 {
		long += fmt.Sprintf("\n\n%s: in --data, or one field at a time with --set (--set %s.<field>=value).", strings.Join(nested, ", "), nested[0])
	}
	if o.Idempotent {
		long += "\n\nIt is sent with an Idempotency-Key (a new one, or --idempotency-key to repeat a call safely)."
	}
	cmd := &cobra.Command{
		Use: use, Short: o.Summary, Long: long, GroupID: "api",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != len(positional) {
				return usage("%s takes %d argument(s): %s", o.Name, len(positional), strings.Join(positional, " "))
			}
			return nil
		},
	}
	fields := []string{}
	for _, q := range o.Query {
		if q != "account_id" {
			fields = append(fields, q)
		}
	}
	if o.Body {
		for _, f := range o.BodyFields {
			if s := in.Properties[f]; s == nil || s.scalar() {
				fields = append(fields, f)
			}
		}
	}
	values := map[string]*string{}
	bools := map[string]*bool{}
	for _, f := range fields {
		s := in.Properties[f]
		help := ""
		if s != nil {
			help = s.Description
			if len(s.Enum) > 0 {
				var opts []string
				for _, e := range s.Enum {
					if e != nil && e != "" {
						opts = append(opts, fmt.Sprint(e))
					}
				}
				help += " (" + strings.Join(opts, ", ") + ")"
			}
			if slices.Contains(in.Required, f) && o.Body && slices.Contains(o.BodyFields, f) {
				help += " [required]"
			}
		}
		if s.is("boolean") && !s.is("string") {
			bools[f] = cmd.Flags().Bool(kebab(f), false, help)
		} else {
			values[f] = cmd.Flags().String(kebab(f), "", help)
		}
	}
	var dataSpec string
	var sets []string
	if o.Body {
		cmd.Flags().StringVar(&dataSpec, "data", "", "the body: @file.json, - for stdin, or the JSON itself")
		cmd.Flags().StringArrayVar(&sets, "set", nil, "one body field by its dotted path: --set exits.stop_loss.price=61000")
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		k, err := a.connect()
		if err != nil {
			return err
		}
		r := request{op: o, path: o.Path, query: url.Values{}, args: map[string]any{}}
		for i, p := range positional {
			r.path = strings.Replace(r.path, "{"+p+"}", url.PathEscape(args[i]), 1)
			r.args[p] = args[i]
		}
		if slices.Contains(o.PathParams, "account_id") {
			acct, err := a.account(ctx, k)
			if err != nil {
				return err
			}
			r.path = strings.Replace(r.path, "{account_id}", url.PathEscape(acct), 1)
			r.args["account_id"] = acct
		} else if slices.Contains(o.Query, "account_id") && a.flags.account != "" {
			r.query.Set("account_id", strings.ToUpper(a.flags.account))
		}
		if o.Body {
			r.body = map[string]any{}
			if dataSpec != "" {
				if r.body, err = a.readData(dataSpec); err != nil {
					return err
				}
			}
			for _, s := range sets {
				path, raw, ok := strings.Cut(s, "=")
				if !ok || path == "" {
					return usage("--set takes path=value, not %q", s)
				}
				v, err := setIn(r.body, &in, strings.Split(path, "."), raw)
				if err != nil {
					return err
				}
				r.body = v.(map[string]any)
			}
		}
		for _, f := range fields {
			flag := cmd.Flags().Lookup(kebab(f))
			if !flag.Changed {
				continue
			}
			var v any = true
			if b, ok := bools[f]; ok {
				v = *b
			} else if v, err = convert(in.Properties[f], *values[f]); err != nil {
				return usage("--%s: %v", kebab(f), err)
			}
			if slices.Contains(o.Query, f) {
				r.query.Set(f, fmt.Sprint(v))
			} else {
				r.body[f] = v
			}
		}
		for f, v := range r.body {
			r.args[f] = v
		}
		return a.runOp(ctx, k, r)
	}
	return cmd
}

// runOp asks where the operation needs it, sends it (every page with --all-pages) and prints its answer.
func (a *App) runOp(ctx context.Context, k *conn, r request) error {
	o := r.op
	acct, _ := r.args["account_id"].(string)
	switch {
	case r.asked:
	case typedConfirm[o.Name]:
		word := firstOf(acct, "open")
		if err := a.confirmTyped(mcp.Summary(o, r.args), word); err != nil {
			return err
		}
	case o.Scope == "trade" && o.Writes() && k.p.confirms():
		if err := a.confirm(mcp.Summary(o, r.args) + " Go ahead?"); err != nil {
			return err
		}
	}
	key := ""
	if o.Idempotent {
		key = firstOf(a.flags.idempotencyKey, newKey())
	}
	var body any
	if o.Body {
		body = r.body
	}
	res, v, err := a.call(ctx, k, o.HTTP, r.path, r.query, body, key)
	if err != nil {
		return err
	}
	if o.Paged {
		items, _ := data(v).([]any)
		for page := 1; str(v, "has_more") == "true" && str(v, "next_cursor") != ""; page++ {
			if !a.flags.allPages {
				a.note("More follow: --all-pages for every page, or --cursor %s for the next one.", str(v, "next_cursor"))
				break
			}
			if page >= a.flags.maxPages {
				a.note("Stopped after %d pages (--max-pages): more follow.", page)
				break
			}
			r.query.Set("cursor", str(v, "next_cursor"))
			if _, v, err = a.call(ctx, k, http.MethodGet, r.path, r.query, nil, ""); err != nil {
				return err
			}
			more, _ := data(v).([]any)
			items = append(items, more...)
		}
		return a.show(o.Name, items)
	}
	if res.StatusCode == http.StatusAccepted && key != "" {
		a.note("Accepted and still running: repeat with --idempotency-key %s to follow it.", key)
	}
	return a.show(o.Name, data(v))
}

// show prints an answer with the columns of the operation it came from.
func (a *App) show(opName string, v any) error {
	if v == nil {
		return nil
	}
	p := *a.printer
	p.Columns = columns[opName]
	return p.Print(v)
}

// newKey is a new Idempotency-Key (a UUID v4).
func newKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
