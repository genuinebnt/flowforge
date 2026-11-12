package graphstore

import (
	"encoding/base64"
	"encoding/json"

	"github.com/genuinebnt/flowforge/core"
)

// Page tokens are opaque to clients: base64url (unpadded) JSON holding the
// cursor, {"seq": n} for change-event pages (as in the spec 12.7 example)
// and {"id": "ent_..."} for entity pages. A malformed token is
// INVALID_ARGUMENT.

type seqCursor struct {
	Seq int64 `json:"seq"`
}

type idCursor struct {
	ID string `json:"id"`
}

// EncodeSeqToken returns the token that resumes after seq.
func EncodeSeqToken(seq int64) string { return encodeToken(seqCursor{seq}) }

// DecodeSeqToken returns the seq a token resumes after (0 for "").
func DecodeSeqToken(tok string) (int64, error) {
	if tok == "" {
		return 0, nil
	}
	var c seqCursor
	if err := decodeToken(tok, &c); err != nil || c.Seq < 0 {
		return 0, InvalidArgument("page_token", "malformed page token")
	}
	return c.Seq, nil
}

// EncodeIDToken returns the token that resumes after entity id.
func EncodeIDToken(id string) string { return encodeToken(idCursor{id}) }

// DecodeIDToken returns the entity id a token resumes after ("" for "").
func DecodeIDToken(tok string) (string, error) {
	if tok == "" {
		return "", nil
	}
	var c idCursor
	if err := decodeToken(tok, &c); err != nil {
		return "", InvalidArgument("page_token", "malformed page token")
	}
	if _, err := ParseID(core.PrefixEntity, "page_token", c.ID); err != nil {
		return "", InvalidArgument("page_token", "malformed page token")
	}
	return c.ID, nil
}

func encodeToken(v any) string {
	b, _ := json.Marshal(v) // plain structs of strings and ints cannot fail
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeToken(tok string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return err
	}
	return core.DecodeStrictBytes(b, v)
}
