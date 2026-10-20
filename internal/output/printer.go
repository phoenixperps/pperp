// Package output prints the API's answers (design 15 section 3 "Output and errors"): a table on a terminal, JSON when
// piped, or JSON lines or CSV; --jq first when given. Decimals stay exactly as the API sent them, and objects keep the
// API's own key order.
package output

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/itchyny/gojq"
)

// Formats are the -o values.
var Formats = []string{"table", "json", "jsonl", "csv"}

// Column is one column of a table or CSV: its title and the dotted path to its value in an item.
type Column struct {
	Title string
	Path  string
}

// Printer prints answers in one format.
type Printer struct {
	Out     io.Writer
	Err     io.Writer // where "Nothing to show." goes
	Format  string    // table, json, jsonl or csv
	JQ      *gojq.Code
	Columns []Column // a list's columns; none: every scalar field of its first item
	Color   bool
	Zone    *time.Location // times in a table (nil: local)
}

// ParseJQ compiles a --jq expression.
func ParseJQ(expr string) (*gojq.Code, error) {
	q, err := gojq.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("--jq: %w", err)
	}
	code, err := gojq.Compile(q)
	if err != nil {
		return nil, fmt.Errorf("--jq: %w", err)
	}
	return code, nil
}

// Print prints one value (from Decode).
func (p *Printer) Print(v any) error {
	if p.JQ != nil {
		return p.jq(v)
	}
	switch p.Format {
	case "json":
		return p.json(v)
	case "jsonl":
		if arr, ok := v.([]any); ok {
			for _, e := range arr {
				fmt.Fprintln(p.Out, Compact(e))
			}
			return nil
		}
		_, err := fmt.Fprintln(p.Out, Compact(v))
		return err
	case "csv":
		return p.csv(v)
	default:
		return p.table(v)
	}
}

func (p *Printer) json(v any) error {
	var compact, out bytes.Buffer
	Encode(&compact, v)
	if err := json.Indent(&out, compact.Bytes(), "", "  "); err != nil {
		return err
	}
	out.WriteByte('\n')
	_, err := p.Out.Write(out.Bytes())
	return err
}

func (p *Printer) jq(v any) error {
	iter := p.JQ.Run(Plain(v))
	for {
		r, ok := iter.Next()
		if !ok {
			return nil
		}
		if err, ok := r.(error); ok {
			if h, ok := err.(*gojq.HaltError); ok && h.Value() == nil {
				return nil
			}
			return fmt.Errorf("--jq: %w", err)
		}
		if s, ok := r.(string); ok {
			fmt.Fprintln(p.Out, s)
			continue
		}
		b := marshal(r)
		if p.Format != "jsonl" {
			var out bytes.Buffer
			if json.Indent(&out, b, "", "  ") == nil {
				b = out.Bytes()
			}
		}
		fmt.Fprintln(p.Out, string(b))
	}
}

// columns are a list's columns: the printer's, or the first item's scalar fields.
func (p *Printer) columns(items []any) []Column {
	if len(p.Columns) > 0 {
		return p.Columns
	}
	first, ok := items[0].(*Object)
	if !ok {
		return []Column{{Title: "VALUE", Path: ""}}
	}
	var cols []Column
	for _, k := range first.Keys {
		switch first.Vals[k].(type) {
		case *Object, []any:
			continue
		}
		cols = append(cols, Column{Title: strings.ToUpper(k), Path: k})
	}
	return cols
}

func lookup(item any, path string) any {
	if path == "" {
		return item
	}
	if o, ok := item.(*Object); ok {
		v, _ := o.Get(path)
		return v
	}
	return nil
}

func (p *Printer) table(v any) error {
	arr, isList := v.([]any)
	if !isList {
		if o, ok := v.(*Object); ok {
			return p.record(o)
		}
		_, err := fmt.Fprintln(p.Out, p.cell(v, "", true))
		return err
	}
	if len(arr) == 0 {
		if p.Err != nil {
			fmt.Fprintln(p.Err, "Nothing to show.")
		}
		return nil
	}
	cols := p.columns(arr)
	rows := make([][]string, len(arr))
	width := make([]int, len(cols))
	for i, c := range cols {
		width[i] = utf8.RuneCountInString(c.Title)
	}
	for r, item := range arr {
		rows[r] = make([]string, len(cols))
		for i, c := range cols {
			s := p.cell(lookup(item, c.Path), c.Path, true)
			rows[r][i] = s
			width[i] = max(width[i], utf8.RuneCountInString(s))
		}
	}
	var b strings.Builder
	line := func(cells []string, style func(i int, s string) string) {
		for i, s := range cells {
			pad := s
			if i < len(cells)-1 {
				pad += strings.Repeat(" ", width[i]-utf8.RuneCountInString(s)+2)
			}
			b.WriteString(style(i, pad))
		}
		b.WriteString("\n")
	}
	titles := make([]string, len(cols))
	for i, c := range cols {
		titles[i] = c.Title
	}
	line(titles, func(_ int, s string) string { return p.paint("1", s) })
	for r := range rows {
		line(rows[r], func(i int, s string) string { return p.tone(cols[i].Path, rows[r][i], s) })
	}
	_, err := io.WriteString(p.Out, b.String())
	return err
}

// record prints one object as name and value lines, nested fields by their dotted names.
func (p *Printer) record(o *Object) error {
	type kv struct{ k, v string }
	var lines []kv
	var walk func(prefix string, o *Object)
	walk = func(prefix string, o *Object) {
		for _, k := range o.Keys {
			name := prefix + k
			if sub, ok := o.Vals[k].(*Object); ok && len(sub.Keys) > 0 {
				walk(name+".", sub)
				continue
			}
			lines = append(lines, kv{name, p.cell(o.Vals[k], k, true)})
		}
	}
	walk("", o)
	w := 0
	for _, l := range lines {
		w = max(w, utf8.RuneCountInString(l.k))
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(p.paint("2", l.k+strings.Repeat(" ", w-utf8.RuneCountInString(l.k)+2)))
		leaf := l.k[strings.LastIndexByte(l.k, '.')+1:]
		b.WriteString(p.tone(leaf, l.v, l.v))
		b.WriteString("\n")
	}
	_, err := io.WriteString(p.Out, b.String())
	return err
}

func (p *Printer) csv(v any) error {
	w := csv.NewWriter(p.Out)
	arr, ok := v.([]any)
	if !ok {
		arr = []any{v}
	}
	if len(arr) > 0 {
		cols := p.columns(arr)
		head := make([]string, len(cols))
		for i, c := range cols {
			head[i] = firstOf(c.Path, "value")
		}
		_ = w.Write(head)
		for _, item := range arr {
			row := make([]string, len(cols))
			for i, c := range cols {
				row[i] = p.cell(lookup(item, c.Path), c.Path, false)
			}
			_ = w.Write(row)
		}
	}
	w.Flush()
	return w.Error()
}

// timeField tells a field holding a time in ms (created_at, time, as_of...).
func timeField(path string) bool {
	k := path[strings.LastIndexByte(path, '.')+1:]
	return strings.HasSuffix(k, "_at") || k == "time" || k == "as_of" || k == "server_time"
}

// cell is a value as text: in a table, times are dates and long nested values are cut; in CSV, everything as sent.
func (p *Printer) cell(v any, path string, table bool) string {
	switch x := v.(type) {
	case nil:
		if table {
			return "-"
		}
		return ""
	case string:
		return strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return ' '
			}
			return r
		}, x)
	case json.Number:
		if table && timeField(path) {
			if ms, err := x.Int64(); err == nil && ms > 1e11 {
				zone := p.Zone
				if zone == nil {
					zone = time.Local
				}
				return time.UnixMilli(ms).In(zone).Format("2006-01-02 15:04:05")
			}
		}
		return string(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case []any:
		if table && len(x) == 0 {
			return "-"
		}
		var parts []string
		scalar := true
		for _, e := range x {
			switch e.(type) {
			case *Object, []any:
				scalar = false
			}
			parts = append(parts, p.cell(e, "", false))
		}
		if scalar && table {
			return strings.Join(parts, ", ")
		}
		return cut(Compact(x), table)
	default:
		return cut(Compact(x), table)
	}
}

func cut(s string, table bool) string {
	if table && utf8.RuneCountInString(s) > 60 {
		return string([]rune(s)[:57]) + "..."
	}
	return s
}

func (p *Printer) paint(code, s string) string {
	if !p.Color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// tone colours a cell by what it says: a side, a profit or a loss.
func (p *Printer) tone(path, value, padded string) string {
	if !p.Color {
		return padded
	}
	k := path[strings.LastIndexByte(path, '.')+1:]
	switch {
	case k == "side" && (value == "buy" || value == "long"):
		return p.paint("32", padded)
	case k == "side" && (value == "sell" || value == "short"):
		return p.paint("31", padded)
	case strings.Contains(k, "pnl") && strings.HasPrefix(value, "-"):
		return p.paint("31", padded)
	case strings.Contains(k, "pnl") && value != "-" && value != "0":
		return p.paint("32", padded)
	}
	return padded
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
