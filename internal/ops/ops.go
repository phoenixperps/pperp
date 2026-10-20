// Package ops is the API's operations, one table (ops_gen.go, from spec/openapi.json) for the CLI's commands and the
// MCP server's tools: the same name, the same arguments, the same rules.
package ops

//go:generate go run ../../tools/gen

// Op is one operation of the API.
type Op struct {
	ID          string   // its operationId (placeOrder)
	Name        string   // the command and tool name (place-order)
	Resource    string   // the SDKs' resource (orders)
	Method      string   // the SDKs' method (create)
	HTTP        string   // GET, POST, PUT, PATCH, DELETE
	Path        string   // /v1/accounts/{account_id}/orders
	Summary     string   // Place an order
	Description string   // what it does
	Scope       string   // public, read or trade
	Idempotent  bool     // it takes an Idempotency-Key (idempotency_key)
	RetrySafe   bool     // it may be sent again as it is
	List        bool     // its answer is a list
	Paged       bool     // ... in pages
	Envs        []string // the environments it exists in (nil: both)
	PathParams  []string
	Query       []string
	Body        bool
	BodyFields  []string // the body's top-level fields (the rest of the flattened arguments)
	InputSchema string   // the flattened arguments as one JSON Schema object
}

// ByName is the operation of a command or tool name.
func ByName(name string) (Op, bool) {
	for _, o := range All {
		if o.Name == name {
			return o, true
		}
	}
	return Op{}, false
}

// In tells whether the operation exists in an environment.
func (o Op) In(env string) bool {
	if len(o.Envs) == 0 {
		return true
	}
	for _, e := range o.Envs {
		if e == env {
			return true
		}
	}
	return false
}

// Writes tells whether the operation changes anything (any method but GET).
func (o Op) Writes() bool { return o.HTTP != "GET" }
