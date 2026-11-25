package mcp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Confirmations (docs/api/design/15-cli-mcp.md section 5.6): a trade an assistant asks for on a grant that wants
// previews is first answered with what it would do and a confirmation; the same call repeated with it, the same
// arguments, before it expires, is placed. Stateless: the confirmation is an HMAC over the credential, the tool, the
// arguments and the expiry.
type Confirmations struct {
	secret []byte
	now    func() time.Time
}

// NewConfirmations signs with secret (hex or raw, at least 32 bytes in production).
func NewConfirmations(secret []byte, now func() time.Time) *Confirmations {
	if now == nil {
		now = time.Now
	}
	return &Confirmations{secret: secret, now: now}
}

// canonical is the arguments as one string: sorted keys, the confirmation itself left out.
func canonical(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		if k != "confirmation" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		v, _ := json.Marshal(args[k]) // maps marshal with sorted keys: nested objects are canonical too
		fmt.Fprintf(&b, "%s=%s;", k, v)
	}
	return b.String()
}

func (c *Confirmations) sign(credential, tool string, args map[string]any, expires int64) string {
	m := hmac.New(sha256.New, c.secret)
	fmt.Fprintf(m, "%s|%s|%s|%d", credential, tool, canonical(args), expires)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)) + "." + strconv.FormatInt(expires, 10)
}

// Issue is the confirmation of these arguments, good for ttl.
func (c *Confirmations) Issue(credential, tool string, args map[string]any, ttl time.Duration) (string, time.Time) {
	exp := c.now().Add(ttl)
	return c.sign(credential, tool, args, exp.Unix()), exp
}

// Check tells whether a confirmation was issued for exactly these arguments and has not expired.
func (c *Confirmations) Check(credential, tool string, args map[string]any, confirmation string) bool {
	i := strings.LastIndexByte(confirmation, '.')
	if i < 0 {
		return false
	}
	exp, err := strconv.ParseInt(confirmation[i+1:], 10, 64)
	if err != nil || c.now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(c.sign(credential, tool, args, exp)), []byte(confirmation))
}
