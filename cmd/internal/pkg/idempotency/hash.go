/*
@Author: Franco Ribeiro Borba
@Description: Canonical payload hashing for persistent idempotency. The API
contract requires a deterministic hash of the business fields of an operation,
so that the same Idempotency-Key replayed with an equivalent body returns the
stored result, while the same key reused with a different body is a conflict.
The hash is SHA-256 over a canonical JSON rendering of the payload: keys are
serialized in alphabetical order at every level, insignificant whitespace is
dropped, numbers keep the literal the client sent and transport metadata is
removed before hashing. Removing the transport fields is what makes the HTTP
body and the SQS data object produce the same hash for the same operation,
since only the SQS envelope carries idempotencyKey, messageId and friends. The
hash never depends on the Idempotency-Key itself, which is compared separately
as the identity of the request.
@Date : 20/09/2026
@Update: -
*/
package idempotency

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)


const Algorithm = "sha256-canonical-json-v1"

var (
	ErrEmptyPayload     = errors.New("payload is empty")
	ErrInvalidPayload   = errors.New("payload is not valid JSON")
	ErrPayloadNotObject = errors.New("payload must be a JSON object")
	ErrNoBusinessFields = errors.New("payload has no business fields to hash")
)


var transportFields = map[string]struct{}{
	"idempotencykey":         {},
	"messageid":              {},
	"messagegroupid":         {},
	"messagededuplicationid": {},
	"sequencenumber":         {},
	"receipthandle":          {},
	"type":                   {},
	"occurredat":             {},
	"sentat":                 {},
	"timestamp":              {},
	"requestid":              {},
	"correlationid":          {},
	"traceid":                {},
}

// CanonicalHash returns the hexadecimal SHA-256 of the canonical form of the
// payload. Two payloads hash to the same value when they describe the same
// operation, whatever the order of their keys, their indentation or the
// transport that carried them.
func CanonicalHash(payload []byte) (string, error) {
	canonical, err := CanonicalJSON(payload)
	if err != nil {
		return "", err
	}

	hash := sha256.Sum256(canonical)

	return hex.EncodeToString(hash[:]), nil
}

// CanonicalJSON returns the bytes that CanonicalHash hashes. It is exported so
// that a rejected replay can show what was compared, instead of two opaque
// hashes that nobody can tell apart.
func CanonicalJSON(payload []byte) ([]byte, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil, ErrEmptyPayload
	}

	// UseNumber keeps every number as the literal text that was received.
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()

	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPayload, err)
	}

	// A well formed document is a single value. Anything after it means the
	// body was truncated or concatenated, and we refuse to hash half of it.
	if decoder.More() {
		return nil, fmt.Errorf("%w: unexpected content after the JSON value", ErrInvalidPayload)
	}

	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, ErrPayloadNotObject
	}

	business := canonicalObject(object, true)
	if len(business) == 0 {
		return nil, ErrNoBusinessFields
	}

	return json.Marshal(business)
}

// canonicalObject copies an object without the values that must not reach the
// hash. The top flag is true only for the outermost object, because transport
// metadata is a property of the envelope and never of a nested value.
func canonicalObject(object map[string]any, top bool) map[string]any {
	canonical := make(map[string]any, len(object))

	for key, value := range object {
		if value == nil {
			continue
		}
		if top && isTransportField(key) {
			continue
		}

		canonical[key] = canonicalValue(value)
	}

	return canonical
}

// canonicalValue walks nested objects and arrays so that the whole payload is
// normalized, not only its first level.
func canonicalValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return canonicalObject(typed, false)

	case []any:
		items := make([]any, len(typed))
		for i, item := range typed {
			// A null item is kept: dropping it would shift every item after
			// it and change the meaning of the array.
			items[i] = canonicalValue(item)
		}

		return items

	default:
		return typed
	}
}

// isTransportField reports whether a key is transport metadata. The comparison
// ignores case because encoding/json matches field names case insensitively
// when it fills a struct: a producer sending IdempotencyKey is understood by
// the handler, so it must also be excluded from the hash.
func isTransportField(key string) bool {
	_, found := transportFields[strings.ToLower(key)]

	return found
}
