package mcp

import (
	"context"
	"crypto/rand"
	"errors"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Local is the MCP server of `pperp mcp` (design 15 section 5.6): the hosted server's tools, over stdio on the user's
// computer, for the one credential of a CLI profile, through the public API (NewRemote). Read only unless trading is
// allowed; with confirmTrades every trade is previewed first, as on a grant that asked for previews.
func Local(ctx context.Context, gw *Gateway, env, token, version string, allowTrading, confirmTrades bool) (*sdk.Server, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	h := New(Config{Version: version}, gw, NewConfirmations(secret, nil))
	me, aerr := h.me(ctx, env, token, "")
	if aerr != nil {
		return nil, errors.New(firstOf(aerr.text, "The API refused this credential ("+aerr.code+")."))
	}
	if !allowTrading {
		me.Credential.Access = "read"
	}
	if me.Assistant == nil {
		me.Assistant = &Assistant{ConfirmTrades: confirmTrades, ConfirmSeconds: 120}
	}
	srv := sdk.NewServer(&sdk.Implementation{Name: "phoenix-perpetuals", Title: "Phoenix Perpetuals", Version: version}, &sdk.ServerOptions{Instructions: Instructions})
	h.tools(srv, &session{env: env, token: token, me: me})
	return srv, nil
}
